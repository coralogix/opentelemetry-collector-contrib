// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/metric/metricdata/metricdatatest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadata"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadatatest"
)

// [DBM P8] explain_function tests (EC-04b). Synthetic statements only.
const (
	p8Statement  = "SELECT a FROM accounts WHERE id = $1"
	p8Statement2 = "SELECT b FROM orders WHERE id = $1"
	p8Plan       = `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"accounts","Alias":"accounts"}}]`
	// The exact SQL the receiver may send on the explain-function path.
	// The only SQL the receiver sends on this path: the queryid and the
	// optional schemas, never the statement text (requester, 2026-11-16).
	p8Call       = "/* otel-collector-ignore */ SELECT coralogix_dbm.explain_statement($1::bigint, $2::name[]) AS plan"
	p8MetricName = "otelcol_postgresql.explain_function.calls"
)

func p8SetTimeout(ms int64) string {
	return fmt.Sprintf("/* otel-collector-ignore */ SET LOCAL statement_timeout = %d", ms)
}

// p8PQError is an error raised by the explain function. Its message and
// detail quote a value, as a server message can.
func p8PQError(code string) error {
	return &pq.Error{
		Severity: "ERROR",
		Code:     pqerror.Code(code),
		Message:  "dbm_explain: " + p7Literal,
		Detail:   "sqlstate 22P02 " + p7Literal,
	}
}

func p8Client(t *testing.T) (*postgreSQLClient, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return &postgreSQLClient{client: db, closeFn: func() error { return nil }}, mock
}

func p8ObfuscatedPlan(t *testing.T) string {
	t.Helper()
	want, err := obfuscateSQLExecPlan(p8Plan)
	require.NoError(t, err)
	require.NotEmpty(t, want)
	return want
}

func TestExplainViaFunctionCallsTheFunction(t *testing.T) {
	client, mock := p8Client(t)
	mock.ExpectBegin()
	mock.ExpectExec(p8SetTimeout(1000)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(p8Call).WithArgs(int64(42), nil).WillReturnRows(sqlmock.NewRows([]string{"plan"}).AddRow(p8Plan))
	mock.ExpectRollback()

	plan, err := client.explainViaFunction(t.Context(), "42", explainFunctionOptions{timeoutMS: 1000}, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, p8ObfuscatedPlan(t), plan)
	require.NoError(t, mock.ExpectationsWereMet(), "only BEGIN, SET LOCAL statement_timeout, the function call and ROLLBACK are sent")
}

func TestExplainViaFunctionPassesSchemasAndTimeout(t *testing.T) {
	client, mock := p8Client(t)
	mock.ExpectBegin()
	mock.ExpectExec(p8SetTimeout(250)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(p8Call).WithArgs(int64(42), `{"app","billing"}`).
		WillReturnRows(sqlmock.NewRows([]string{"plan"}).AddRow(p8Plan))
	mock.ExpectRollback()

	plan, err := client.explainViaFunction(t.Context(), "42",
		explainFunctionOptions{timeoutMS: 250, schemas: []string{"app", "billing"}}, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, p8ObfuscatedPlan(t), plan)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestExplainViaFunctionNullPlan(t *testing.T) {
	client, mock := p8Client(t)
	mock.ExpectBegin()
	mock.ExpectExec(p8SetTimeout(1000)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(p8Call).WithArgs(int64(42), nil).WillReturnRows(sqlmock.NewRows([]string{"plan"}).AddRow(nil))
	mock.ExpectRollback()

	plan, err := client.explainViaFunction(t.Context(), "42", explainFunctionOptions{timeoutMS: 1000}, zap.NewNop())
	require.NoError(t, err)
	assert.Empty(t, plan)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestExplainViaFunctionErrorHasNoFallback: an error from the function ends
// the request; the receiver never runs PREPARE or EXPLAIN itself.
func TestExplainViaFunctionErrorHasNoFallback(t *testing.T) {
	for _, code := range []string{"DBM21", "DBM30", "42883", "42501"} {
		t.Run(code, func(t *testing.T) {
			client, mock := p8Client(t)
			mock.ExpectBegin()
			mock.ExpectExec(p8SetTimeout(1000)).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(p8Call).WithArgs(int64(42), nil).WillReturnError(p8PQError(code))
			mock.ExpectRollback()

			plan, err := client.explainViaFunction(t.Context(), "42", explainFunctionOptions{timeoutMS: 1000}, zap.NewNop())
			var pqErr *pq.Error
			require.ErrorAs(t, err, &pqErr)
			assert.Equal(t, code, string(pqErr.Code))
			assert.Empty(t, plan)
			require.NoError(t, mock.ExpectationsWereMet(), "nothing is sent after the failed call")
		})
	}
}

func TestExplainViaFunctionSetTimeoutFailure(t *testing.T) {
	client, mock := p8Client(t)
	mock.ExpectBegin()
	mock.ExpectExec(p8SetTimeout(1000)).WillReturnError(p8PQError("42501"))
	mock.ExpectRollback()

	_, err := client.explainViaFunction(t.Context(), "42", explainFunctionOptions{timeoutMS: 1000}, zap.NewNop())
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet(), "the function is not called without the timeout")
}

// TestExplainViaFunctionPassesTheQueryID: pg_stat_statements queryids are
// signed 64-bit; the full range is passed as a bigint.
func TestExplainViaFunctionPassesTheQueryID(t *testing.T) {
	for _, id := range []int64{-9223372036854775808, -1, 9223372036854775807} {
		client, mock := p8Client(t)
		mock.ExpectBegin()
		mock.ExpectExec(p8SetTimeout(1000)).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(p8Call).WithArgs(id, nil).WillReturnRows(sqlmock.NewRows([]string{"plan"}).AddRow(p8Plan))
		mock.ExpectRollback()

		_, err := client.explainViaFunction(t.Context(), fmt.Sprint(id), explainFunctionOptions{timeoutMS: 1000}, zap.NewNop())
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	}
}

// TestExplainViaFunctionRejectsABadQueryID: nothing is sent for a queryid
// that is not a bigint.
func TestExplainViaFunctionRejectsABadQueryID(t *testing.T) {
	for _, id := range []string{"", "abc", "4242; SELECT", "9223372036854775808"} {
		client, mock := p8Client(t)
		_, err := client.explainViaFunction(t.Context(), id, explainFunctionOptions{timeoutMS: 1000}, zap.NewNop())
		require.ErrorIs(t, err, errExplainFunctionQueryID, "queryid %q", id)
		require.NoError(t, mock.ExpectationsWereMet())
	}
}

func TestExplainFunctionTxIsReadOnly(t *testing.T) {
	assert.True(t, explainFunctionTxOptions().ReadOnly)
}

func TestStripLeadingComments(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"SELECT 1", "SELECT 1"},
		{"  \n\tSELECT 1", "SELECT 1"},
		{"/* app=checkout */ SELECT 1", "SELECT 1"},
		{"/**/SELECT 1", "SELECT 1"},
		{"-- note\nSELECT 1", "SELECT 1"},
		{"/* a */ -- b\n /* c */SELECT 1", "SELECT 1"},
		{"/* a /* nested */ still a comment */ SELECT 1", "SELECT 1"},
		{"/*/ x */ SELECT 1", "SELECT 1"},
		{"/* unterminated SELECT 1", ""},
		{"/* a /* nested */ unterminated SELECT 1", ""},
		{"-- only a comment", ""},
		{"", ""},
		{"SELECT /* kept */ 1", "SELECT /* kept */ 1"},
		{"SELECT 1 -- kept", "SELECT 1 -- kept"},
	} {
		assert.Equal(t, tc.want, stripLeadingComments(tc.in), "input %q", tc.in)
	}
}

func TestExplainFunctionInput(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"/* app */ SELECT a FROM t WHERE id = $1", "SELECT a FROM t WHERE id = $1", true},
		{"select a from t", "select a from t", true},
		{"-- x\n WITH x AS (SELECT a FROM t) SELECT * FROM x", "WITH x AS (SELECT a FROM t) SELECT * FROM x", true},
		{"(SELECT a FROM t)", "", false},
		{"UPDATE t SET a = $1", "", false},
		{"/* SELECT */ DELETE FROM t", "", false},
		{"SELECTED", "", false},
		{"/* unterminated", "", false},
		{"", "", false},
	} {
		got, ok := explainFunctionInput(tc.in)
		assert.Equal(t, tc.ok, ok, "input %q", tc.in)
		assert.Equal(t, tc.want, got, "input %q", tc.in)
	}
}

func TestExplainFunctionOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want metadata.AttributeExplainOutcome
	}{
		{"nil", nil, metadata.AttributeExplainOutcomeOk},
		// W0-07b grants/postgres-plans-definer.sql (EC-04b hand-off).
		{"DBM20 temporary objects", p8PQError("DBM20"), metadata.AttributeExplainOutcomeNotEligible},
		{"DBM21 untrusted role", p8PQError("DBM21"), metadata.AttributeExplainOutcomeNotEligible},
		{"DBM22 custom dotted setting", p8PQError("DBM22"), metadata.AttributeExplainOutcomeNotEligible},
		{"DBM23 pg_stat_statements missing", p8PQError("DBM23"), metadata.AttributeExplainOutcomeNotEligible},
		{"DBM29 unknown DBM2x is database level", p8PQError("DBM29"), metadata.AttributeExplainOutcomeNotEligible},
		{"wrapped DBM21", fmt.Errorf("explain: %w", p8PQError("DBM21")), metadata.AttributeExplainOutcomeNotEligible},
		{"DBM24 no application statement", p8PQError("DBM24"), metadata.AttributeExplainOutcomeStatementNotEligible},
		{"DBM25 several texts", p8PQError("DBM25"), metadata.AttributeExplainOutcomeStatementNotEligible},
		{"DBM26 $n before PG 16", p8PQError("DBM26"), metadata.AttributeExplainOutcomeStatementNotEligible},
		{"DBM10 timeout", p8PQError("DBM10"), metadata.AttributeExplainOutcomeTimeoutRefused},
		{"DBM11 null or size", p8PQError("DBM11"), metadata.AttributeExplainOutcomeInputRefused},
		{"DBM12 separator", p8PQError("DBM12"), metadata.AttributeExplainOutcomeInputRefused},
		{"DBM13 literal", p8PQError("DBM13"), metadata.AttributeExplainOutcomeInputRefused},
		{"DBM14 not SELECT", p8PQError("DBM14"), metadata.AttributeExplainOutcomeInputRefused},
		{"DBM15 schema not allowed", p8PQError("DBM15"), metadata.AttributeExplainOutcomeInputRefused},
		{"DBM16 forbidden function", p8PQError("DBM16"), metadata.AttributeExplainOutcomeInputRefused},
		{"DBM30 planning", p8PQError("DBM30"), metadata.AttributeExplainOutcomePlanningFailed},
		{"42883 function missing", p8PQError("42883"), metadata.AttributeExplainOutcomeError},
		{"42501 no privilege", p8PQError("42501"), metadata.AttributeExplainOutcomeError},
		{"57014 canceled by statement_timeout", p8PQError("57014"), metadata.AttributeExplainOutcomeError},
		{"DB021 not DBM2x", p8PQError("DB021"), metadata.AttributeExplainOutcomeError},
		{"not a database error", errors.New("boom"), metadata.AttributeExplainOutcomeError},
		{"deadline", context.DeadlineExceeded, metadata.AttributeExplainOutcomeError},
	} {
		assert.Equal(t, tc.want, explainFunctionOutcome(tc.err), tc.name)
	}
}

func TestExplainRateLimiter(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	l := newExplainRateLimiter(2)
	assert.True(t, l.allow(t0), "starts full")
	assert.True(t, l.allow(t0))
	assert.False(t, l.allow(t0), "burst is the per-minute cap")
	assert.False(t, l.allow(t0.Add(29*time.Second)))
	assert.True(t, l.allow(t0.Add(30*time.Second)), "one call per 30s refills at 2 per minute")
	assert.False(t, l.allow(t0.Add(30*time.Second)))
	later := t0.Add(10 * time.Minute)
	assert.True(t, l.allow(later))
	assert.True(t, l.allow(later))
	assert.False(t, l.allow(later), "never more than the cap after idle time")
	assert.False(t, l.allow(t0), "a clock step back adds nothing")
}

func TestExplainFunctionConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dbm     DBMConfig
		wantErr string
	}{
		{name: "unset", dbm: DBMConfig{}},
		{name: "function only", dbm: DBMConfig{ExplainFunction: explainFunctionName}},
		{name: "all options", dbm: DBMConfig{
			ExplainFunction: explainFunctionName, ExplainFunctionTimeout: 2 * time.Second,
			ExplainFunctionSchemas: []string{"app", "billing"}, ExplainFunctionMaxCallsPerMinute: 600,
		}},
		{name: "1ms", dbm: DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionTimeout: time.Millisecond, ExplainFunctionMaxCallsPerMinute: 1}},
		{name: "another function", dbm: DBMConfig{ExplainFunction: "public.explain_statement"}, wantErr: "'dbm.explain_function'"},
		{name: "quoted name", dbm: DBMConfig{ExplainFunction: `coralogix_dbm."explain_statement"`}, wantErr: "'dbm.explain_function'"},
		{name: "timeout above 2s", dbm: DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionTimeout: 2001 * time.Millisecond}, wantErr: "'dbm.explain_function_timeout'"},
		{name: "timeout below 1ms", dbm: DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionTimeout: 500 * time.Microsecond}, wantErr: "'dbm.explain_function_timeout'"},
		{name: "timeout not whole ms", dbm: DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionTimeout: 1500 * time.Microsecond}, wantErr: "'dbm.explain_function_timeout'"},
		{name: "negative timeout", dbm: DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionTimeout: -time.Second}, wantErr: "'dbm.explain_function_timeout'"},
		{name: "empty schema", dbm: DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionSchemas: []string{"app", ""}}, wantErr: "'dbm.explain_function_schemas'"},
		{name: "schema too long", dbm: DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionSchemas: []string{string(make([]byte, 64))}}, wantErr: "'dbm.explain_function_schemas'"},
		{name: "calls above 600", dbm: DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionMaxCallsPerMinute: 601}, wantErr: "'dbm.explain_function_max_calls_per_minute'"},
		{name: "negative calls", dbm: DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionMaxCallsPerMinute: -1}, wantErr: "'dbm.explain_function_max_calls_per_minute'"},
		{name: "schemas without function", dbm: DBMConfig{ExplainFunctionSchemas: []string{"app"}}, wantErr: "requires 'dbm.explain_function'"},
		{name: "timeout without function", dbm: DBMConfig{ExplainFunctionTimeout: time.Second}, wantErr: "requires 'dbm.explain_function'"},
		{name: "calls without function", dbm: DBMConfig{ExplainFunctionMaxCallsPerMinute: 10}, wantErr: "requires 'dbm.explain_function'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := createDefaultConfig().(*Config)
			cfg.Username = "otel"
			cfg.Password = "otel"
			cfg.DBM = tc.dbm
			err := cfg.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestExplainFunctionOptionsDefaults(t *testing.T) {
	assert.Equal(t, explainFunctionOptions{timeoutMS: 1000}, DBMConfig{ExplainFunction: explainFunctionName}.explainFunctionOptions())
	assert.Equal(t, explainFunctionOptions{timeoutMS: 250, schemas: []string{"app"}},
		DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionTimeout: 250 * time.Millisecond, ExplainFunctionSchemas: []string{"app"}}.explainFunctionOptions())
	assert.Equal(t, 30, DBMConfig{ExplainFunction: explainFunctionName}.explainFunctionMaxCallsPerMinute())
	assert.Equal(t, 5, DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionMaxCallsPerMinute: 5}.explainFunctionMaxCallsPerMinute())
}

// p8TopQueryRow is a pg_stat_statements row for database postgres, userid
// 10, with every counter set to value.
func p8TopQueryRow(queryid, query, value string) []driver.Value {
	values := map[string]driver.Value{
		"datname":         "postgres",
		"query":           query,
		queryidColumnName: queryid,
		"userid":          "10",
		"rolname":         "otel",
	}
	row := make([]driver.Value, len(topQueryColumns))
	for i, column := range topQueryColumns {
		if v, ok := values[column]; ok {
			row[i] = v
		} else {
			row[i] = value
		}
	}
	return row
}

type p8Harness struct {
	scraper  *postgreSQLScraper
	mock     sqlmock.Sqlmock
	tel      *componenttest.Telemetry
	observed *observer.ObservedLogs
}

func newP8Harness(t *testing.T, dbm DBMConfig) *p8Harness {
	t.Helper()
	tel := componenttest.NewTelemetry()
	t.Cleanup(func() { require.NoError(t, tel.Shutdown(context.Background())) }) //nolint:usetesting // t.Context() is already canceled when Cleanup runs
	core, observed := observer.New(zapcore.DebugLevel)
	settings := receivertest.NewNopSettings(metadata.Type)
	settings.TelemetrySettings = tel.NewTelemetrySettings()
	settings.Logger = zap.New(core)
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true
	cfg.LogsBuilderConfig.Events.DbServerQueryPlan.Enabled = true
	cfg.DBM = dbm

	db, mock := newUnorderedMock(t)
	scraper, err := newTopQueryScraper(settings, cfg, mockSimpleClientFactory{db: db})
	require.NoError(t, err)
	return &p8Harness{scraper: scraper, mock: mock, tel: tel, observed: observed}
}

func (h *p8Harness) expectCall(timeoutMS, queryID int64) *sqlmock.ExpectedQuery {
	h.mock.ExpectBegin()
	h.mock.ExpectExec(regexp.QuoteMeta(p8SetTimeout(timeoutMS))).WillReturnResult(sqlmock.NewResult(0, 0))
	// The arguments are the queryid and the schemas: never the statement text.
	q := h.mock.ExpectQuery(regexp.QuoteMeta(p8Call)).WithArgs(queryID, nil)
	h.mock.ExpectRollback()
	return q
}

func (h *p8Harness) assertCalls(t *testing.T, want map[string]int64) {
	t.Helper()
	dps := make([]metricdata.DataPoint[int64], 0, len(want))
	for outcome, n := range want {
		dps = append(dps, metricdata.DataPoint[int64]{Attributes: attribute.NewSet(attribute.String("explain_outcome", outcome)), Value: n})
	}
	metadatatest.AssertEqualPostgresqlExplainFunctionCalls(t, h.tel, dps, metricdatatest.IgnoreTimestamp())
}

// assertNoOwnExplain fails if the receiver tried its own PREPARE / EXPLAIN.
func (h *p8Harness) assertNoOwnExplain(t *testing.T) {
	t.Helper()
	for _, msg := range []string{"failed to prepare statement for EXPLAIN", "failed to explain statement", "failed to explain query", "failed to obtain a dedicated connection for EXPLAIN"} {
		assert.Empty(t, h.observed.FilterMessage(msg).AllUntimed(), "the receiver must not run its own EXPLAIN (%s)", msg)
	}
}

func queryPlans(logs plog.Logs) map[string]string {
	out := map[string]string{}
	for _, lr := range recordsByEvent(logs, "db.server.query_plan") {
		id, _ := lr.Attributes().Get("postgresql.queryid")
		plan, _ := lr.Attributes().Get("postgresql.query_plan")
		out[id.AsString()] = plan.AsString()
	}
	return out
}

func TestTopQueryExplainFunctionGetsThePlan(t *testing.T) {
	h := newP8Harness(t, DBMConfig{ExplainFunction: explainFunctionName})
	primeTopQueryCache(h.scraper, "postgres", "10", "4242")
	h.mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).
		AddRow(p8TopQueryRow("4242", "/* app=checkout */ -- c\n"+p8Statement, "10")...))
	// The statement text (with its comments) is never sent: only the queryid.
	h.expectCall(1000, 4242).WillReturnRows(sqlmock.NewRows([]string{"plan"}).AddRow(p8Plan))

	logs, err := h.scraper.scrapeTopQuery(t.Context(), 10, 10, 10, 0)
	require.NoError(t, err)
	require.NoError(t, h.mock.ExpectationsWereMet())
	assert.Equal(t, map[string]string{"4242": p8ObfuscatedPlan(t)}, queryPlans(logs))
	h.assertNoOwnExplain(t)
	h.assertCalls(t, map[string]int64{"ok": 1})
}

func TestTopQueryExplainFunctionNotEligibleStopsCalls(t *testing.T) {
	h := newP8Harness(t, DBMConfig{ExplainFunction: explainFunctionName})
	primeTopQueryCache(h.scraper, "postgres", "10", "4242")
	h.mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).AddRow(p8TopQueryRow("4242", p8Statement, "10")...))
	h.expectCall(1000, 4242).WillReturnError(p8PQError("DBM21"))

	logs, err := h.scraper.scrapeTopQuery(t.Context(), 10, 10, 10, 0)
	require.NoError(t, err)
	require.NoError(t, h.mock.ExpectationsWereMet())
	assert.Empty(t, queryPlans(logs))

	// The next scrape does not call the function again for this database.
	h.mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).AddRow(p8TopQueryRow("4242", p8Statement, "20")...))
	logs, err = h.scraper.scrapeTopQuery(t.Context(), 10, 10, 10, 0)
	require.NoError(t, err)
	require.NoError(t, h.mock.ExpectationsWereMet())
	assert.Empty(t, queryPlans(logs))
	require.Len(t, recordsByEvent(logs, "db.server.top_query"), 1, "the statement is still reported, without a plan")

	h.assertCalls(t, map[string]int64{"not_eligible": 1, "skipped_not_eligible": 1})
	entries := h.observed.FilterMessage("plans unavailable: database not eligible for the explain function").AllUntimed()
	require.Len(t, entries, 1, "reported once per database")
	assert.Equal(t, zapcore.WarnLevel, entries[0].Level)
	assert.Equal(t, map[string]any{"database": "postgres", "sqlState": "DBM21"}, entries[0].ContextMap())
	assertNoStatementText(t, h.observed)
	h.assertNoOwnExplain(t)
}

// TestTopQueryExplainFunctionStatementNotEligible: DBM24-DBM26 refuse one
// statement, never retried; other statements of the database are still
// explained.
func TestTopQueryExplainFunctionStatementNotEligible(t *testing.T) {
	h := newP8Harness(t, DBMConfig{ExplainFunction: explainFunctionName})
	primeTopQueryCache(h.scraper, "postgres", "10", "4242")
	h.mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).AddRow(p8TopQueryRow("4242", p8Statement, "10")...))
	h.expectCall(1000, 4242).WillReturnError(p8PQError("DBM26"))
	_, err := h.scraper.scrapeTopQuery(t.Context(), 10, 10, 10, 0)
	require.NoError(t, err)
	require.NoError(t, h.mock.ExpectationsWereMet())

	// The plan cache forgets the statement (TTL); the refusal is still kept.
	h.scraper.queryPlanCache.Purge()
	primeTopQueryCache(h.scraper, "postgres", "10", "4343")
	h.mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).
		AddRow(p8TopQueryRow("4242", p8Statement, "20")...).
		AddRow(p8TopQueryRow("4343", p8Statement2, "10")...))
	h.expectCall(1000, 4343).WillReturnRows(sqlmock.NewRows([]string{"plan"}).AddRow(p8Plan))
	logs, err := h.scraper.scrapeTopQuery(t.Context(), 10, 10, 10, 0)
	require.NoError(t, err)
	require.NoError(t, h.mock.ExpectationsWereMet(), "4242 is not called again; 4343 is")
	assert.Equal(t, map[string]string{"4343": p8ObfuscatedPlan(t)}, queryPlans(logs))

	h.assertCalls(t, map[string]int64{"statement_not_eligible": 1, "ok": 1})
	assert.Empty(t, h.observed.FilterMessage("plans unavailable: database not eligible for the explain function").AllUntimed())
	assertNoStatementText(t, h.observed)
	h.assertNoOwnExplain(t)
}

func TestTopQueryExplainFunctionRateLimited(t *testing.T) {
	h := newP8Harness(t, DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionMaxCallsPerMinute: 1})
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	h.scraper.now = func() time.Time { return now }
	primeTopQueryCache(h.scraper, "postgres", "10", "4242")
	primeTopQueryCache(h.scraper, "postgres", "10", "4343")

	h.mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).
		AddRow(p8TopQueryRow("4242", p8Statement, "20")...).
		AddRow(p8TopQueryRow("4343", p8Statement2, "10")...))
	h.expectCall(1000, 4242).WillReturnRows(sqlmock.NewRows([]string{"plan"}).AddRow(p8Plan))
	logs, err := h.scraper.scrapeTopQuery(t.Context(), 10, 10, 10, 0)
	require.NoError(t, err)
	require.NoError(t, h.mock.ExpectationsWereMet())
	assert.Equal(t, map[string]string{"4242": p8ObfuscatedPlan(t)}, queryPlans(logs), "the second statement waits for the rate limit")
	h.assertCalls(t, map[string]int64{"ok": 1, "rate_limited": 1})

	// A minute later the rate-limited statement is explained; the first one
	// comes from the plan cache.
	now = now.Add(time.Minute)
	h.mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).
		AddRow(p8TopQueryRow("4242", p8Statement, "30")...).
		AddRow(p8TopQueryRow("4343", p8Statement2, "30")...))
	h.expectCall(1000, 4343).WillReturnRows(sqlmock.NewRows([]string{"plan"}).AddRow(p8Plan))
	logs, err = h.scraper.scrapeTopQuery(t.Context(), 10, 10, 10, 0)
	require.NoError(t, err)
	require.NoError(t, h.mock.ExpectationsWereMet())
	assert.Len(t, queryPlans(logs), 2)
	h.assertCalls(t, map[string]int64{"ok": 2, "rate_limited": 1})
	h.assertNoOwnExplain(t)
}

func TestTopQueryExplainFunctionFailureLogsSQLStateOnly(t *testing.T) {
	h := newP8Harness(t, DBMConfig{ExplainFunction: explainFunctionName, ExplainFunctionTimeout: 300 * time.Millisecond})
	primeTopQueryCache(h.scraper, "postgres", "10", "4242")
	h.mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).AddRow(p8TopQueryRow("4242", p7Explainable, "10")...))
	h.expectCall(300, 4242).WillReturnError(p8PQError("DBM30"))

	logs, err := h.scraper.scrapeTopQuery(t.Context(), 10, 10, 10, 0)
	require.NoError(t, err)
	require.NoError(t, h.mock.ExpectationsWereMet())
	assert.Empty(t, queryPlans(logs))

	assertNoStatementText(t, h.observed)
	entries := h.observed.FilterMessage("failed to get a plan from the explain function").AllUntimed()
	require.Len(t, entries, 1)
	assert.Equal(t, map[string]any{
		"queryID": "4242", "queryLength": int64(len(p7Explainable)),
		"sqlState": "DBM30", "errorCategory": "", "explainOutcome": "planning_failed",
	}, entries[0].ContextMap())
	h.assertCalls(t, map[string]int64{"planning_failed": 1})
	h.assertNoOwnExplain(t)
}

func TestTopQueryExplainFunctionSkipsNonSelect(t *testing.T) {
	h := newP8Harness(t, DBMConfig{ExplainFunction: explainFunctionName})
	primeTopQueryCache(h.scraper, "postgres", "10", "4242")
	h.mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).
		AddRow(p8TopQueryRow("4242", "UPDATE accounts SET a = $1 WHERE id = $2", "10")...))

	logs, err := h.scraper.scrapeTopQuery(t.Context(), 10, 10, 10, 0)
	require.NoError(t, err)
	require.NoError(t, h.mock.ExpectationsWereMet())
	assert.Empty(t, queryPlans(logs))
	_, err = h.tel.GetMetric(p8MetricName)
	require.Error(t, err, "the function is not called for a statement it would refuse")
	h.assertNoOwnExplain(t)
}
