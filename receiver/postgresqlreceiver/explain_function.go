// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/lib/pq"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadata"
)

// [DBM P8] explain_function: estimated plans from a hardened SECURITY
// DEFINER function (ADR-0004 amendment 2026-11-15, SEC-02 conditions C4, C6,
// C12 and C14) instead of the receiver's own PREPARE / EXPLAIN.

// explainFunctionName is the only function the receiver calls. Accepting
// one fixed name keeps every statement on this path a literal (BR-3 test).
const explainFunctionName = "coralogix_dbm.explain_statement"

const (
	defaultExplainFunctionTimeout           = time.Second
	maxExplainFunctionTimeout               = 2 * time.Second
	defaultExplainFunctionMaxCallsPerMinute = 30
	maxExplainFunctionMaxCallsPerMinute     = 600
	defaultExplainFunctionRecheckInterval   = time.Hour
	minExplainFunctionRecheckInterval       = 5 * time.Minute
	maxExplainFunctionSchemas               = 16
	// explainNoPlanSize bounds the statements remembered as refused
	// (DBM24-DBM26). One evicted from it is tried once more.
	explainNoPlanSize = 10000
	// explainNoPlanRetry is how long a refused statement is not requested
	// (requester decision D5, 2026-11-16): a bounded exception to C14's
	// "never retry DBM2x", one retry per statement per 24 h.
	explainNoPlanRetry = 24 * time.Hour
	// maxSchemaNameBytes is NAMEDATALEN - 1.
	maxSchemaNameBytes = 63
)

// The statements of the explain-function path. The function takes the
// pg_stat_statements queryid, reads the statement text itself, checks it,
// plans it as a generic plan and returns JSON; see
// grants/postgres-plans-definer.sql (W0-07b). The receiver never sends
// statement text (requester decision 2026-11-16). This is the only call.
const (
	explainFunctionTimeoutSQL = "/* otel-collector-ignore */ SET LOCAL statement_timeout = %d"
	explainFunctionCallSQL    = "/* otel-collector-ignore */ SELECT coralogix_dbm.explain_statement($1::bigint, $2::name[]) AS plan"
)

// errExplainFunctionQueryID is returned, before anything is sent, for a
// queryid that is not a bigint.
var errExplainFunctionQueryID = errors.New("explain function: queryid is not a bigint")

// explainFunctionOptions are the per-call settings.
type explainFunctionOptions struct {
	// timeoutMS is the statement_timeout of the call, 1..2000 (C6).
	timeoutMS int64
	// schemas is the optional schemas argument (C12); nil omits it.
	schemas []string
}

// explainFunctionTxOptions are the options of the call's transaction: read
// only, so neither the call nor anything it plans can write.
func explainFunctionTxOptions() *sql.TxOptions {
	return &sql.TxOptions{ReadOnly: true}
}

// explainViaFunction implements client. It runs, in one read-only
// transaction that is always rolled back: SET LOCAL statement_timeout (the
// function refuses to run without a 1-2000 ms caller timeout, C6), then the
// function call with the pg_stat_statements queryid and the optional
// schemas (C12). The statement text is never sent. SET LOCAL ends with the
// transaction, so nothing stays set on a pooled connection. There is no
// fallback to PREPARE / EXPLAIN: any error is returned and the statement
// gets no plan. The caller logs the error by SQLSTATE only.
func (c *postgreSQLClient) explainViaFunction(ctx context.Context, queryID string, opts explainFunctionOptions, _ *zap.Logger) (string, error) {
	id, err := strconv.ParseInt(queryID, 10, 64)
	if err != nil || strconv.FormatInt(id, 10) != queryID {
		// Only the canonical form pg_stat_statements prints.
		return "", errExplainFunctionQueryID
	}
	tx, err := c.client.BeginTx(ctx, explainFunctionTxOptions())
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	timeoutMS := opts.timeoutMS
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(explainFunctionTimeoutSQL, timeoutMS)); err != nil {
		return "", err
	}

	var raw sql.NullString
	if err := tx.QueryRowContext(ctx, explainFunctionCallSQL, id, pq.Array(opts.schemas)).Scan(&raw); err != nil {
		return "", err
	}
	if !raw.Valid || raw.String == "" {
		return "", nil
	}
	return obfuscateSQLExecPlan(raw.String)
}

// explainFunctionInput reports whether a statement is worth a call: a
// local filter on the text, which is never sent. It does no comment
// handling (the function strips comments itself, W0-07b): a statement that
// starts with a comment is left to the function. Otherwise only a first
// word of SELECT or WITH qualifies, the only statements the function plans
// (SEC-02 C5).
func explainFunctionInput(query string) bool {
	s := strings.TrimLeftFunc(query, unicode.IsSpace)
	if strings.HasPrefix(s, "/*") || strings.HasPrefix(s, "--") {
		return true
	}
	end := strings.IndexFunc(s, func(r rune) bool { return !isIdentRune(r) })
	if end < 0 {
		end = len(s)
	}
	switch strings.ToLower(s[:end]) {
	case "select", "with":
		return true
	}
	return false
}

func isIdentRune(r rune) bool {
	return r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// explainFunctionOutcome classifies a call result by the SQLSTATE the
// function raises (grants/postgres-plans-definer.sql, W0-07b): DBM10 caller
// timeout refused, DBM11-DBM14 and DBM16 request refused, DBM15 and
// DBM20-DBM23 (and any other DBM2x) database not eligible, DBM24/DBM25 plan
// unavailable for this statement (DBM26 is retired and reserved), DBM30
// planning failed (also 42P18, a $n type that cannot be inferred); 42883, 3F000 and
// 42501 function unavailable (database level). Anything else (for example
// 57014) is an error.
func explainFunctionOutcome(err error) metadata.AttributeExplainOutcome {
	if err == nil {
		return metadata.AttributeExplainOutcomeOk
	}
	pqErr, ok := errors.AsType[*pq.Error](err)
	if !ok {
		return metadata.AttributeExplainOutcomeError
	}
	code := string(pqErr.Code)
	switch {
	case code == "42883", code == "3F000", code == "42501":
		// The function or its schema is missing, or not executable: the
		// same for every statement of the database.
		return metadata.AttributeExplainOutcomeFunctionUnavailable
	case code == "DBM24", code == "DBM25", code == "DBM26":
		// DBM26 is retired by W0-07b I-1; kept for older installs.
		return metadata.AttributeExplainOutcomeStatementNotEligible
	case strings.HasPrefix(code, "DBM2"), code == "DBM15":
		// DBM20-DBM23, any other DBM2x, and DBM15 (a configured schema
		// the install does not allow): the database.
		return metadata.AttributeExplainOutcomeNotEligible
	case code == "DBM10":
		return metadata.AttributeExplainOutcomeTimeoutRefused
	case code == "DBM11", code == "DBM12", code == "DBM13", code == "DBM14", code == "DBM16":
		return metadata.AttributeExplainOutcomeInputRefused
	case code == "DBM30":
		return metadata.AttributeExplainOutcomePlanningFailed
	}
	return metadata.AttributeExplainOutcomeError
}

// explainProbeClears reports whether a probe result shows that the function
// ran for this database: ok, a statement-level refusal, a planning failure,
// or an input refusal past the size check (DBM12-DBM14, DBM16). A transport
// or other error, a timeout refusal (DBM10, 57014) or DBM11 does not.
func explainProbeClears(outcome metadata.AttributeExplainOutcome, err error) bool {
	switch outcome {
	case metadata.AttributeExplainOutcomeOk, metadata.AttributeExplainOutcomeStatementNotEligible, metadata.AttributeExplainOutcomePlanningFailed:
		return true
	case metadata.AttributeExplainOutcomeInputRefused:
		pqErr, ok := errors.AsType[*pq.Error](err)
		return ok && pqErr.Code != "DBM11"
	}
	return false
}

// explainFunctionHint is a fixed hint for a database-level refusal.
func explainFunctionHint(sqlState string) string {
	switch sqlState {
	case "DBM20", "DBM22":
		return "use a clean connection"
	case "DBM21":
		return "see coralogix_dbm.explain_preflight_report()"
	case "DBM23":
		return "install pg_stat_statements in this database (on PG 13 with track = top)"
	case "DBM15":
		return "a configured schema is not in the install's dbm_plan_schemas"
	case "42883":
		return "function not installed in this database"
	case "3F000":
		return "schema coralogix_dbm not installed in this database"
	case "42501":
		return "the monitoring role cannot execute the function"
	}
	return ""
}

// explainRateLimiter allows at most maxPerMinute calls in any 60 s window
// (C14), exactly: a sliding-window log, a ring of the last maxPerMinute call
// times. A call is allowed while fewer than maxPerMinute calls were made, or
// when the oldest of the last maxPerMinute is at least 60 s old. So up to N
// calls may come back to back inside one scrape. Memory: at most 600 times.
type explainRateLimiter struct {
	calls []time.Time // ring of the last len(calls) call times
	next  int         // index of the oldest call once the ring is full
	full  bool
}

const explainRateWindow = time.Minute

func newExplainRateLimiter(maxPerMinute int) *explainRateLimiter {
	return &explainRateLimiter{calls: make([]time.Time, max(maxPerMinute, 1))}
}

// allow records a call at now if the window allows it.
func (l *explainRateLimiter) allow(now time.Time) bool {
	if l.full && now.Sub(l.calls[l.next]) < explainRateWindow {
		// Also refuses when the clock stepped back (a negative age).
		return false
	}
	l.calls[l.next] = now
	l.next++
	if l.next == len(l.calls) {
		l.next = 0
		l.full = true
	}
	return true
}

// explainFunctionOptions returns the per-call settings, with defaults.
func (cfg DBMConfig) explainFunctionOptions() explainFunctionOptions {
	timeout := cfg.ExplainFunctionTimeout
	if timeout == 0 {
		timeout = defaultExplainFunctionTimeout
	}
	opts := explainFunctionOptions{timeoutMS: timeout.Milliseconds()}
	if len(cfg.ExplainFunctionSchemas) > 0 {
		opts.schemas = cfg.ExplainFunctionSchemas
	}
	return opts
}

// explainFunctionMaxCallsPerMinute returns the rate limit, with its default.
func (cfg DBMConfig) explainFunctionMaxCallsPerMinute() int {
	if cfg.ExplainFunctionMaxCallsPerMinute == 0 {
		return defaultExplainFunctionMaxCallsPerMinute
	}
	return cfg.ExplainFunctionMaxCallsPerMinute
}

// explainFunctionRecheckInterval returns the re-check interval, with its
// default.
func (cfg DBMConfig) explainFunctionRecheckInterval() time.Duration {
	if cfg.ExplainFunctionRecheckInterval == 0 {
		return defaultExplainFunctionRecheckInterval
	}
	return cfg.ExplainFunctionRecheckInterval
}

// validateExplainFunction checks the dbm.explain_function options.
func (cfg DBMConfig) validateExplainFunction() error {
	var errs []error
	if cfg.ExplainFunction == "" {
		if cfg.ExplainFunctionTimeout != 0 || len(cfg.ExplainFunctionSchemas) > 0 || cfg.ExplainFunctionMaxCallsPerMinute != 0 || cfg.ExplainFunctionRecheckInterval != 0 {
			errs = append(errs, errors.New("invalid config: each of 'dbm.explain_function_timeout', 'dbm.explain_function_schemas', 'dbm.explain_function_max_calls_per_minute' and 'dbm.explain_function_recheck_interval' requires 'dbm.explain_function'"))
		}
		return errors.Join(errs...)
	}
	if cfg.ExplainFunction != explainFunctionName {
		errs = append(errs, fmt.Errorf("invalid config: 'dbm.explain_function' must be %q", explainFunctionName))
	}
	if t := cfg.ExplainFunctionTimeout; t != 0 && (t < time.Millisecond || t > maxExplainFunctionTimeout || t%time.Millisecond != 0) {
		errs = append(errs, fmt.Errorf("invalid config: 'dbm.explain_function_timeout' must be a whole number of milliseconds from 1ms to %s, got %s", maxExplainFunctionTimeout, t))
	}
	if len(cfg.ExplainFunctionSchemas) > maxExplainFunctionSchemas {
		errs = append(errs, fmt.Errorf("invalid config: 'dbm.explain_function_schemas' must have at most %d entries, got %d", maxExplainFunctionSchemas, len(cfg.ExplainFunctionSchemas)))
	}
	for _, schema := range cfg.ExplainFunctionSchemas {
		if schema == "" || len(schema) > maxSchemaNameBytes {
			errs = append(errs, fmt.Errorf("invalid config: 'dbm.explain_function_schemas' entries must be 1 to %d bytes", maxSchemaNameBytes))
			break
		}
	}
	if n := cfg.ExplainFunctionMaxCallsPerMinute; n < 0 || n > maxExplainFunctionMaxCallsPerMinute {
		errs = append(errs, fmt.Errorf("invalid config: 'dbm.explain_function_max_calls_per_minute' must be from 1 to %d, or 0 for the default (%d), got %d", maxExplainFunctionMaxCallsPerMinute, defaultExplainFunctionMaxCallsPerMinute, n))
	}
	if r := cfg.ExplainFunctionRecheckInterval; r != 0 && r < minExplainFunctionRecheckInterval {
		errs = append(errs, fmt.Errorf("invalid config: 'dbm.explain_function_recheck_interval' must be at least %s, or 0 for the default (%s), got %s", minExplainFunctionRecheckInterval, defaultExplainFunctionRecheckInterval, r))
	}
	return errors.Join(errs...)
}

// planFromFunction returns the plan of one top-query statement from the
// explain function ("" when there is none), and whether a call was made
// (only calls use the per-scrape explain budget). It records the outcome and
// adds the result to the plan cache unless the statement should be tried
// again on a later scrape (rate limited) or the database is not eligible.
//
// A database the function refused (database level) gets no calls until
// the re-check interval has passed; then one probe call is allowed. One Warn
// is logged per transition (unavailable, available again).
func (p *postgreSQLScraper) planFromFunction(ctx context.Context, clientFactory postgreSQLClientFactory, database, rawQuery, queryID, planCacheKey string, logger *zap.Logger) (string, bool) {
	now := p.now()
	until, probe := p.explainNotEligible[database]
	if probe && now.Before(until) {
		p.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeSkippedNotEligible)
		return "", false
	}
	if retryAt, refused := p.explainNoPlan.Get(planCacheKey); refused {
		if now.Before(retryAt) {
			// DBM24-DBM26 earlier: not retried within 24 h.
			return "", false
		}
		p.explainNoPlan.Remove(planCacheKey)
	}
	// A local check only: the text is never sent. The function plans
	// SELECT and WITH only, so other statements are not worth a call.
	if !explainFunctionInput(rawQuery) {
		p.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeSkippedLocal)
		p.queryPlanCache.Add(planCacheKey, "")
		return "", false
	}
	if !p.explainLimiter.allow(now) {
		p.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeRateLimited)
		return "", false
	}
	dbClient, err := clientFactory.getClient(ctx, database)
	if err != nil {
		if probe {
			p.explainNotEligible[database] = now.Add(p.config.DBM.explainFunctionRecheckInterval())
		}
		p.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeError)
		logger.Error("failed to get a client for the explain function", append([]zap.Field{zap.String("queryID", queryID)}, sqlErrorFields(err)...)...)
		return "", false
	}
	defer func() {
		if closeErr := dbClient.Close(); closeErr != nil {
			logger.Error("failed to close", sqlErrorFields(closeErr)...)
		}
	}()

	plan, err := dbClient.explainViaFunction(ctx, queryID, p.config.DBM.explainFunctionOptions(), logger)
	outcome := explainFunctionOutcome(err)
	p.recordExplainOutcome(ctx, outcome)
	if outcome == metadata.AttributeExplainOutcomeNotEligible || outcome == metadata.AttributeExplainOutcomeFunctionUnavailable {
		// C14: no retry before the re-check interval; the collection status
		// shows why plans are missing.
		p.explainNotEligible[database] = now.Add(p.config.DBM.explainFunctionRecheckInterval())
		if !probe {
			sqlState := ""
			if pqErr, isPQ := errors.AsType[*pq.Error](err); isPQ {
				sqlState = string(pqErr.Code)
			}
			logger.Warn("plans unavailable: database not eligible for the explain function",
				zap.String("database", database), zap.String("sqlState", sqlState), zap.String("hint", explainFunctionHint(sqlState)))
		}
		return "", true
	}
	if probe {
		if !explainProbeClears(outcome, err) {
			// The function did not evidently run for this database: keep
			// the state and start the re-check interval again, silently.
			p.explainNotEligible[database] = now.Add(p.config.DBM.explainFunctionRecheckInterval())
		} else {
			delete(p.explainNotEligible, database)
			logger.Warn("plans available again from the explain function", zap.String("database", database))
		}
	}
	if outcome == metadata.AttributeExplainOutcomeStatementNotEligible {
		p.explainNoPlan.Add(planCacheKey, now.Add(explainNoPlanRetry))
		logger.Debug("plan unavailable for this statement from the explain function",
			append([]zap.Field{zap.String("queryID", queryID)}, sqlErrorFields(err)...)...)
		return "", true
	}
	if err != nil {
		// [DBM P7] the length only, never the statement text; the SQLSTATE
		// only, never the server message (C14, P7b).
		fields := append([]zap.Field{zap.String("queryID", queryID), zap.Int("queryLength", len(rawQuery))}, sqlErrorFields(err)...)
		logger.Error("failed to get a plan from the explain function", append(fields, zap.String("explainOutcome", outcome.String()))...)
	}
	// As on the EXPLAIN path, a failure is cached too, so that the statement
	// is tried again only when its cache entry expires.
	p.queryPlanCache.Add(planCacheKey, plan)
	return plan, true
}

func (p *postgreSQLScraper) recordExplainOutcome(ctx context.Context, outcome metadata.AttributeExplainOutcome) {
	p.telemetry.PostgresqlExplainFunctionCalls.Add(ctx, 1, metric.WithAttributes(attribute.String("explain_outcome", outcome.String())))
}
