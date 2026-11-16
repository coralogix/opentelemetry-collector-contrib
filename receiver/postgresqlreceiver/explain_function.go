// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver"

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	// maxSchemaNameBytes is NAMEDATALEN - 1.
	maxSchemaNameBytes = 63
)

// The statements of the explain-function path. The function itself checks
// the input, plans it as a generic plan and returns JSON; see
// grants/postgres-plans-definer.sql (W0-07b).
const (
	explainFunctionTimeoutSQL     = "/* otel-collector-ignore */ SET LOCAL statement_timeout = %d"
	explainFunctionCallSQL        = "/* otel-collector-ignore */ SELECT coralogix_dbm.explain_statement($1) AS plan"
	explainFunctionCallSchemasSQL = "/* otel-collector-ignore */ SELECT coralogix_dbm.explain_statement($1, $2::name[]) AS plan"
)

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
// function call. SET LOCAL ends with the transaction, so nothing stays set on
// a pooled connection. There is no fallback to PREPARE / EXPLAIN: any error
// is returned and the statement gets no plan. The caller logs the error by
// SQLSTATE only.
func (c *postgreSQLClient) explainViaFunction(ctx context.Context, query, _ string, opts explainFunctionOptions, _ *zap.Logger) (string, error) {
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
	if len(opts.schemas) == 0 {
		err = tx.QueryRowContext(ctx, explainFunctionCallSQL, query).Scan(&raw)
	} else {
		err = tx.QueryRowContext(ctx, explainFunctionCallSchemasSQL, query, pq.Array(opts.schemas)).Scan(&raw)
	}
	if err != nil {
		return "", err
	}
	if !raw.Valid || raw.String == "" {
		return "", nil
	}
	return obfuscateSQLExecPlan(raw.String)
}

// stripLeadingComments removes the whitespace and comments before the first
// token of a statement (C4: the function refuses any comment, so that the
// database side needs no lexer). Block comments nest, as in PostgreSQL. An
// unterminated leading comment, or text that is only comments, yields "".
// Comments after the first token are kept; the function refuses them.
func stripLeadingComments(query string) string {
	s := query
	for {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		switch {
		case strings.HasPrefix(s, "--"):
			end := strings.IndexByte(s, '\n')
			if end < 0 {
				return ""
			}
			s = s[end+1:]
		case strings.HasPrefix(s, "/*"):
			depth, i := 1, 2
			for depth > 0 {
				if i+1 >= len(s) {
					return ""
				}
				switch s[i : i+2] {
				case "/*":
					depth++
					i += 2
				case "*/":
					depth--
					i += 2
				default:
					i++
				}
			}
			s = s[i:]
		default:
			return s
		}
	}
}

// explainFunctionInput returns the text sent to the function and whether to
// send it: leading comments stripped, and only SELECT or WITH statements,
// the only ones the function accepts (SEC-02 C5).
func explainFunctionInput(query string) (string, bool) {
	s := stripLeadingComments(query)
	end := strings.IndexFunc(s, func(r rune) bool { return !isIdentRune(r) })
	if end < 0 {
		end = len(s)
	}
	switch strings.ToLower(s[:end]) {
	case "select", "with":
		return s, true
	}
	return "", false
}

func isIdentRune(r rune) bool {
	return r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// explainFunctionOutcome classifies a call result by the SQLSTATE the
// function raises (grants/postgres-plans-definer.sql, W0-07b): DBM10 caller
// timeout refused, DBM11-DBM14 input refused, DBM15 schema not allowed (a
// refused request too), DBM2x database not eligible (temporary objects,
// untrusted roles), DBM30 planning failed. Anything else is an error.
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
	case strings.HasPrefix(code, "DBM2"):
		return metadata.AttributeExplainOutcomeNotEligible
	case code == "DBM10":
		return metadata.AttributeExplainOutcomeTimeoutRefused
	case code == "DBM11", code == "DBM12", code == "DBM13", code == "DBM14", code == "DBM15":
		return metadata.AttributeExplainOutcomeInputRefused
	case code == "DBM30":
		return metadata.AttributeExplainOutcomePlanningFailed
	}
	return metadata.AttributeExplainOutcomeError
}

// explainRateLimiter is a token bucket: at most maxPerMinute calls in a
// burst, refilled at maxPerMinute per minute (C14). Credit is kept as a
// duration, so the arithmetic is exact: one call costs a minute divided by
// maxPerMinute.
type explainRateLimiter struct {
	cost     time.Duration
	capacity time.Duration
	credit   time.Duration
	last     time.Time
}

func newExplainRateLimiter(maxPerMinute int) *explainRateLimiter {
	n := time.Duration(max(maxPerMinute, 1))
	cost := time.Minute / n
	return &explainRateLimiter{cost: cost, capacity: cost * n, credit: cost * n}
}

// allow takes one call's credit if it is available at now.
func (l *explainRateLimiter) allow(now time.Time) bool {
	if l.last.IsZero() {
		l.last = now
	}
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.credit = min(l.capacity, l.credit+elapsed)
		l.last = now
	}
	if l.credit < l.cost {
		return false
	}
	l.credit -= l.cost
	return true
}

// explainFunctionOptions returns the per-call settings, with defaults.
func (cfg DBMConfig) explainFunctionOptions() explainFunctionOptions {
	timeout := cfg.ExplainFunctionTimeout
	if timeout == 0 {
		timeout = defaultExplainFunctionTimeout
	}
	return explainFunctionOptions{timeoutMS: timeout.Milliseconds(), schemas: cfg.ExplainFunctionSchemas}
}

// explainFunctionMaxCallsPerMinute returns the rate limit, with its default.
func (cfg DBMConfig) explainFunctionMaxCallsPerMinute() int {
	if cfg.ExplainFunctionMaxCallsPerMinute == 0 {
		return defaultExplainFunctionMaxCallsPerMinute
	}
	return cfg.ExplainFunctionMaxCallsPerMinute
}

// validateExplainFunction checks the dbm.explain_function options.
func (cfg DBMConfig) validateExplainFunction() error {
	var errs []error
	if cfg.ExplainFunction == "" {
		if cfg.ExplainFunctionTimeout != 0 || len(cfg.ExplainFunctionSchemas) > 0 || cfg.ExplainFunctionMaxCallsPerMinute != 0 {
			errs = append(errs, errors.New("invalid config: each of 'dbm.explain_function_timeout', 'dbm.explain_function_schemas' and 'dbm.explain_function_max_calls_per_minute' requires 'dbm.explain_function'"))
		}
		return errors.Join(errs...)
	}
	if cfg.ExplainFunction != explainFunctionName {
		errs = append(errs, fmt.Errorf("invalid config: 'dbm.explain_function' must be %q", explainFunctionName))
	}
	if t := cfg.ExplainFunctionTimeout; t != 0 && (t < time.Millisecond || t > maxExplainFunctionTimeout || t%time.Millisecond != 0) {
		errs = append(errs, fmt.Errorf("invalid config: 'dbm.explain_function_timeout' must be a whole number of milliseconds from 1ms to %s, got %s", maxExplainFunctionTimeout, t))
	}
	for _, schema := range cfg.ExplainFunctionSchemas {
		if schema == "" || len(schema) > maxSchemaNameBytes {
			errs = append(errs, fmt.Errorf("invalid config: 'dbm.explain_function_schemas' entries must be 1 to %d bytes", maxSchemaNameBytes))
			break
		}
	}
	if n := cfg.ExplainFunctionMaxCallsPerMinute; n < 0 || n > maxExplainFunctionMaxCallsPerMinute {
		errs = append(errs, fmt.Errorf("invalid config: 'dbm.explain_function_max_calls_per_minute' must be from 1 to %d, got %d", maxExplainFunctionMaxCallsPerMinute, n))
	}
	return errors.Join(errs...)
}

// planFromFunction returns the plan of one top-query statement from the
// explain function, or "" when there is none. It records the outcome and
// adds the result to the plan cache unless the statement should be tried
// again on a later scrape (rate limited) or the database is not eligible.
func (p *postgreSQLScraper) planFromFunction(ctx context.Context, clientFactory postgreSQLClientFactory, database, rawQuery, queryID, planCacheKey string, logger *zap.Logger) string {
	if _, notEligible := p.explainNotEligible[database]; notEligible {
		p.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeSkippedNotEligible)
		return ""
	}
	input, ok := explainFunctionInput(rawQuery)
	if !ok {
		p.queryPlanCache.Add(planCacheKey, "")
		return ""
	}
	if !p.explainLimiter.allow(p.now()) {
		p.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeRateLimited)
		return ""
	}
	dbClient, err := clientFactory.getClient(ctx, database)
	if err != nil {
		p.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeError)
		logger.Error("failed to get a client for the explain function", append([]zap.Field{zap.String("queryID", queryID)}, sqlErrorFields(err)...)...)
		return ""
	}
	defer func() {
		if closeErr := dbClient.Close(); closeErr != nil {
			logger.Error("failed to close", sqlErrorFields(closeErr)...)
		}
	}()

	plan, err := dbClient.explainViaFunction(ctx, input, queryID, p.config.DBM.explainFunctionOptions(), logger)
	outcome := explainFunctionOutcome(err)
	p.recordExplainOutcome(ctx, outcome)
	if outcome == metadata.AttributeExplainOutcomeNotEligible {
		// C14: no retry; the collection status shows why plans are missing.
		p.explainNotEligible[database] = struct{}{}
		sqlState := ""
		if pqErr, isPQ := errors.AsType[*pq.Error](err); isPQ {
			sqlState = string(pqErr.Code)
		}
		logger.Warn("plans unavailable: database not eligible for the explain function",
			zap.String("database", database), zap.String("sqlState", sqlState))
		return ""
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
	return plan
}

func (p *postgreSQLScraper) recordExplainOutcome(ctx context.Context, outcome metadata.AttributeExplainOutcome) {
	p.telemetry.PostgresqlExplainFunctionCalls.Add(ctx, 1, metric.WithAttributes(attribute.String("explain_outcome", outcome.String())))
}
