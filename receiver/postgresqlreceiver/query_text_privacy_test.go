// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
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

// [DBM P7] Synthetic statements only. p7Literal stands for a literal value
// that must never reach the collector's own log.
const (
	p7Literal = "p7-synthetic-literal"
	// p7Unterminated fails obfuscation: the string literal never ends, as in a
	// statement cut by track_activity_query_size.
	p7Unterminated = "SELECT * FROM accounts WHERE token = '" + p7Literal
	// p7Explainable obfuscates, but its text still holds the literal.
	p7Explainable = "SELECT * FROM accounts WHERE note = '" + p7Literal + "'"
)

// p7PQError is a PostgreSQL error whose message, detail and context quote a
// value of the statement, as real server errors do (22P02).
func p7PQError() error {
	return &pq.Error{
		Severity: "ERROR",
		Code:     "22P02",
		Message:  `invalid input syntax for type integer: "` + p7Literal + `"`,
		Detail:   "value " + p7Literal,
		Where:    "statement " + p7Explainable,
	}
}

// p7PQErrorFields are the only fields a database error may add to a log line.
var p7PQErrorFields = map[string]any{"sqlState": "22P02", "errorCategory": "invalid_text_representation"}

func withFields(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	maps.Copy(out, base)
	maps.Copy(out, extra)
	return out
}

// assertNoStatementText fails if any log entry, in its message or in any
// field, holds the literal.
func assertNoStatementText(t *testing.T, observed *observer.ObservedLogs) {
	t.Helper()
	entries := observed.AllUntimed()
	for i := range entries {
		entry := &entries[i]
		assert.NotContains(t, entry.Message, p7Literal, "log message holds statement text")
		for key, value := range entry.ContextMap() {
			assert.NotContains(t, fmt.Sprint(value), p7Literal, "log field %q of %q holds statement text", key, entry.Message)
		}
	}
}

// newUnorderedMock matches expectations in any order. In order, sqlmock
// rejects the scraper's probe queries (version, capabilities) with an error
// that dumps the next expectation's rows, statement text included, into the
// receiver's debug log, which would hide what these tests check.
func newUnorderedMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	mock.MatchExpectationsInOrder(false)
	t.Cleanup(func() { db.Close() })
	return db, mock
}

func plogWithEvents(events ...string) plog.Logs {
	logs := plog.NewLogs()
	lrs := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	for _, event := range events {
		lrs.AppendEmpty().SetEventName(event)
	}
	return logs
}

func querySampleRow(values map[string]any) []driver.Value {
	row := make([]driver.Value, len(querySampleColumns))
	for i, column := range querySampleColumns {
		if v, ok := values[column]; ok {
			row[i] = v
		} else {
			row[i] = ""
		}
	}
	return row
}

func p7TopQueryRow(columns []string, query string) []driver.Value {
	values := map[string]driver.Value{
		"datname":         "postgres",
		"query":           query,
		queryidColumnName: "4242",
		"userid":          "10",
		"rolname":         "otel",
	}
	row := make([]driver.Value, len(columns))
	for i, column := range columns {
		if v, ok := values[column]; ok {
			row[i] = v
		} else {
			row[i] = "10"
		}
	}
	return row
}

func TestQuerySampleObfuscationFailureLogsNoText(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	core, observed := observer.New(zapcore.DebugLevel)
	client := &postgreSQLClient{client: db, closeFn: func() error { return nil }}

	mock.ExpectQuery("FROM pg_stat_activity").WillReturnRows(sqlmock.NewRows(querySampleColumns).AddRow(querySampleRow(map[string]any{
		querySampleColumnPID:     "1001",
		querySampleColumnQueryID: "77",
		querySampleColumnState:   "active",
		querySampleColumnQuery:   p7Unterminated,
	})...))

	atts, _, err := client.getQuerySamples(t.Context(), 10, 0, nil, zap.New(core))
	require.NoError(t, err)
	require.Len(t, atts, 1)

	assertNoStatementText(t, observed)
	entries := observed.FilterMessage("failed to obfuscate query").AllUntimed()
	require.Len(t, entries, 1)
	assert.Equal(t, map[string]any{
		"event":       "query_sample",
		"reason":      "unterminated_string",
		"queryID":     "77",
		"queryLength": int64(len(p7Unterminated)),
	}, entries[0].ContextMap())
	assert.Empty(t, atts[0]["db.query.text"], "db.query.text stays empty on failure")
	assert.Equal(t, true, atts[0][obfuscationFailedKey])
}

func TestTopQueryObfuscationFailureLogsNoText(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	core, observed := observer.New(zapcore.DebugLevel)
	client := &postgreSQLClient{client: db, closeFn: func() error { return nil }}

	mock.ExpectQuery("pg_stat_statements").WillReturnRows(sqlmock.NewRows(topQueryColumns).AddRow(p7TopQueryRow(topQueryColumns, p7Unterminated)...))

	atts, err := client.getTopQuery(t.Context(), 10, nil, false, false, TopQueryCandidateOrderTotalTime, zap.New(core))
	require.NoError(t, err)
	require.Len(t, atts, 1)

	assertNoStatementText(t, observed)
	entries := observed.FilterMessage("failed to obfuscate query").AllUntimed()
	require.Len(t, entries, 1)
	assert.Equal(t, map[string]any{
		"event":       "top_query",
		"reason":      "unterminated_string",
		"queryID":     "4242",
		"queryLength": int64(len(p7Unterminated)),
	}, entries[0].ContextMap())
	assert.Empty(t, atts[0]["db.query.text"])
	assert.Equal(t, true, atts[0][obfuscationFailedKey])
}

func TestObfuscationFailureReason(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  string
	}{
		{query: p7Unterminated, want: "unterminated_string"},
		{query: "SELECT 1 /* " + p7Literal, want: "unterminated_comment"},
		{query: "SELECT $tag$" + p7Literal, want: "unterminated_dollar_quoted_string"},
	} {
		_, err := obfuscateSQL(tc.query)
		require.Error(t, err, tc.query)
		reason := obfuscationFailureReason(err)
		assert.Equal(t, tc.want, reason)
		assert.NotContains(t, reason, p7Literal)
	}
	assert.Equal(t, "unparsable_statement", obfuscationFailureReason(errors.New("at position 3: something "+p7Literal)),
		"an unknown error maps to a fixed reason, never to its text")
}

func TestObfuscationFailedCounter(t *testing.T) {
	tel := componenttest.NewTelemetry()
	defer func() { require.NoError(t, tel.Shutdown(t.Context())) }()
	core, observed := observer.New(zapcore.DebugLevel)
	settings := receivertest.NewNopSettings(metadata.Type)
	settings.TelemetrySettings = tel.NewTelemetrySettings()
	settings.Logger = zap.New(core)

	cfg := createDefaultConfig().(*Config)
	cfg.Databases = []string{}
	cfg.LogsBuilderConfig.Events.DbServerQuerySample.Enabled = true
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true

	db, mock := newUnorderedMock(t)
	scraper, err := newPostgreSQLScraper(settings, cfg, mockSimpleClientFactory{db: db}, newCache(10), newTTLCache[string](1, time.Second))
	require.NoError(t, err)

	mock.ExpectQuery("FROM pg_stat_activity").WillReturnRows(sqlmock.NewRows(querySampleColumns).
		AddRow(querySampleRow(map[string]any{querySampleColumnPID: "1001", querySampleColumnState: "active", querySampleColumnQuery: p7Unterminated})...).
		AddRow(querySampleRow(map[string]any{querySampleColumnPID: "1002", querySampleColumnState: "active", querySampleColumnQuery: "SELECT 1"})...))
	logs, err := scraper.scrapeQuerySamples(t.Context(), 10)
	require.NoError(t, err)
	for _, lr := range querySampleRecords(logs) {
		_, leaked := lr.Attributes().Get(obfuscationFailedKey)
		assert.False(t, leaked, "the internal marker is never emitted")
	}

	mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).AddRow(p7TopQueryRow(topQueryColumns, p7Unterminated)...))
	_, err = scraper.scrapeTopQuery(t.Context(), 10, 10, 0, 0)
	require.NoError(t, err)

	metadatatest.AssertEqualPostgresqlQueryObfuscationFailed(t, tel, []metricdata.DataPoint[int64]{
		{Attributes: attribute.NewSet(attribute.String("event", "query_sample")), Value: 1},
		{Attributes: attribute.NewSet(attribute.String("event", "top_query")), Value: 1},
	}, metricdatatest.IgnoreTimestamp())
	assertNoStatementText(t, observed)
}

func TestQuerySampleTextTruncated(t *testing.T) {
	ascii := strings.Repeat("a", 1023)
	multibyte := strings.Repeat("a", 1016) + "é"
	for _, tc := range []struct {
		name        string
		text        string
		octetLength int64
		size        int64
		want        bool
	}{
		{name: "ascii at the limit is cut", text: ascii, octetLength: 1023, size: 1024, want: true},
		{name: "ascii one byte under the limit is complete", text: ascii[:1022], octetLength: 1022, size: 1024, want: false},
		{name: "multibyte text can be clipped up to 3 bytes under the limit", text: multibyte, octetLength: 1020, size: 1024, want: true},
		{name: "multibyte text more than 3 bytes under the limit is complete", text: multibyte, octetLength: 1019, size: 1024, want: false},
		{name: "a short statement is complete", text: "SELECT 1", octetLength: 8, size: 1024, want: false},
		{name: "unknown size", text: ascii, octetLength: 1023, size: 0, want: false},
		{name: "unknown length", text: ascii, octetLength: 0, size: 1024, want: false},
		{name: "a larger setting", text: ascii, octetLength: 1023, size: 4096, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, querySampleTextTruncated(tc.text, tc.octetLength, tc.size))
		})
	}
}

func TestQuerySampleTruncatedTextIsMarked(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.Databases = []string{}
	cfg.LogsBuilderConfig.Events.DbServerQuerySample.Enabled = true
	db, mock := newUnorderedMock(t)
	scraper, err := newPostgreSQLScraper(receivertest.NewNopSettings(metadata.Type), cfg, mockSimpleClientFactory{db: db}, newCache(1), newTTLCache[string](1, time.Second))
	require.NoError(t, err)

	cut := ("SELECT * FROM accounts WHERE id IN (" + strings.Repeat("1, ", 400))[:1023]
	mock.ExpectQuery("FROM pg_stat_activity").WillReturnRows(sqlmock.NewRows(querySampleColumns).
		AddRow(querySampleRow(map[string]any{
			querySampleColumnPID: "1001", querySampleColumnState: "active", querySampleColumnQuery: cut,
			querySampleColumnQueryOctetLength: "1023", querySampleColumnTrackActivityQuerySize: "1024",
		})...).
		AddRow(querySampleRow(map[string]any{
			querySampleColumnPID: "1002", querySampleColumnState: "active", querySampleColumnQuery: "SELECT 1",
			querySampleColumnQueryOctetLength: "8", querySampleColumnTrackActivityQuerySize: "1024",
		})...))
	logs, err := scraper.scrapeQuerySamples(t.Context(), 10)
	require.NoError(t, err)

	records := querySampleRecords(logs)
	require.Len(t, records, 2)
	for _, lr := range records {
		pid, ok := lr.Attributes().Get("postgresql.pid")
		require.True(t, ok)
		truncated, ok := lr.Attributes().Get(dbQueryTextTruncatedAttribute)
		if pid.Int() == 1001 {
			require.True(t, ok, "a cut statement carries db.query.text.truncated")
			assert.True(t, truncated.Bool())
		} else {
			assert.False(t, ok, "a complete statement carries no truncation mark")
		}
		_, leaked := lr.Attributes().Get(querySampleTextTruncatedKey)
		assert.False(t, leaked, "the internal marker is never emitted")
	}
}

func TestApplyQuerySampleTruncatedOnlyOnQuerySamples(t *testing.T) {
	logs := plogWithEvents("db.server.top_query", "db.server.query_sample")
	applyQuerySampleTruncated(logs, []bool{true}, zap.NewNop())
	lrs := logs.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords()
	_, ok := lrs.At(0).Attributes().Get(dbQueryTextTruncatedAttribute)
	assert.False(t, ok, "top_query records are never marked")
	v, ok := lrs.At(1).Attributes().Get(dbQueryTextTruncatedAttribute)
	require.True(t, ok)
	assert.True(t, v.Bool())
}

func TestExplainQueryPrepareFailureLogsNoText(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	core, observed := observer.New(zapcore.DebugLevel)
	client := &postgreSQLClient{client: db, closeFn: func() error { return nil }}

	mock.ExpectQuery("/* otel-collector-ignore */ SET plan_cache_mode = force_generic_plan;PREPARE otel_42 AS " + p7Explainable + ";").
		WillReturnError(p7PQError())
	mock.ExpectExec("/* otel-collector-ignore */ DEALLOCATE PREPARE otel_42").WillReturnResult(sqlmock.NewResult(0, 0))

	_, err = client.explainQuery(t.Context(), p7Explainable, "42", zap.New(core))
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	assertNoStatementText(t, observed)
	entries := observed.FilterMessage("failed to prepare statement for EXPLAIN").AllUntimed()
	require.Len(t, entries, 1)
	assert.Equal(t, withFields(map[string]any{"queryID": "42", "queryLength": int64(len(p7Explainable))}, p7PQErrorFields), entries[0].ContextMap())
}

// explainUpToExecute mocks a PREPARE and parameter count that succeed for
// otel_42 with no parameters.
func explainUpToExecute(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("/* otel-collector-ignore */ SET plan_cache_mode = force_generic_plan;PREPARE otel_42 AS " + p7Explainable + ";").
		WillReturnRows(sqlmock.NewRows([]string{"result"}))
	mock.ExpectQuery("/* otel-collector-ignore */ SELECT COALESCE(array_length(parameter_types, 1), 0) AS param_count FROM pg_prepared_statements WHERE name = 'otel_42';").
		WillReturnRows(sqlmock.NewRows([]string{"param_count"}).AddRow("0"))
}

func TestExplainStatementFailureLogsNoText(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	core, observed := observer.New(zapcore.DebugLevel)
	client := &postgreSQLClient{client: db, closeFn: func() error { return nil }}

	explainUpToExecute(mock)
	mock.ExpectQuery("EXPLAIN(FORMAT JSON) EXECUTE otel_42;").WillReturnError(p7PQError())
	mock.ExpectExec("/* otel-collector-ignore */ DEALLOCATE PREPARE otel_42").WillReturnResult(sqlmock.NewResult(0, 0))

	_, err = client.explainQuery(t.Context(), p7Explainable, "42", zap.New(core))
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	assertNoStatementText(t, observed)
	entries := observed.FilterMessage("failed to explain statement").AllUntimed()
	require.Len(t, entries, 1)
	assert.Equal(t, withFields(map[string]any{"queryID": "42"}, p7PQErrorFields), entries[0].ContextMap())
}

func TestExplainPlanObfuscationFailureLogsNoText(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	core, observed := observer.New(zapcore.DebugLevel)
	client := &postgreSQLClient{client: db, closeFn: func() error { return nil }}

	explainUpToExecute(mock)
	mock.ExpectQuery("EXPLAIN(FORMAT JSON) EXECUTE otel_42;").
		WillReturnRows(sqlmock.NewRows([]string{"QUERY PLAN"}).AddRow(`[{"Plan":{"Filter":"` + p7Literal))
	mock.ExpectExec("/* otel-collector-ignore */ DEALLOCATE PREPARE otel_42").WillReturnResult(sqlmock.NewResult(0, 0))

	_, err = client.explainQuery(t.Context(), p7Explainable, "42", zap.New(core))
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	assertNoStatementText(t, observed)
	entries := observed.FilterMessage("failed to obfuscate explain plan").AllUntimed()
	require.Len(t, entries, 1)
	assert.Equal(t, map[string]any{"queryID": "42", "reason": "unparsable_statement"}, entries[0].ContextMap())
}

func TestSQLErrorFieldsNeverCarryTheMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want map[string]any
	}{
		{name: "pq error", err: p7PQError(), want: p7PQErrorFields},
		{name: "wrapped pq error", err: fmt.Errorf("explain: %w", p7PQError()), want: p7PQErrorFields},
		{name: "deadline", err: fmt.Errorf("q: %w", context.DeadlineExceeded), want: map[string]any{"errorCategory": "deadline_exceeded"}},
		{name: "canceled", err: context.Canceled, want: map[string]any{"errorCategory": "canceled"}},
		{name: "other", err: errors.New("driver said " + p7Literal), want: map[string]any{"errorCategory": "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enc := zapcore.NewMapObjectEncoder()
			for _, f := range sqlErrorFields(tc.err) {
				f.AddTo(enc)
			}
			assert.Equal(t, tc.want, enc.Fields)
		})
	}
}

// TestRecordObfuscationFailuresStripsMarker: the internal marker is removed
// from every row, counted or not, so it can never be emitted (M3).
func TestRecordObfuscationFailuresStripsMarker(t *testing.T) {
	tel := componenttest.NewTelemetry()
	defer func() { require.NoError(t, tel.Shutdown(t.Context())) }()
	settings := receivertest.NewNopSettings(metadata.Type)
	settings.TelemetrySettings = tel.NewTelemetrySettings()
	scraper, err := newPostgreSQLScraper(settings, createDefaultConfig().(*Config), mockSimpleClientFactory{}, newCache(1), newTTLCache[string](1, time.Second))
	require.NoError(t, err)

	rows := []map[string]any{
		{obfuscationFailedKey: true, "a": 1},
		{"a": 2},
		{obfuscationFailedKey: false, "a": 3},
	}
	scraper.recordObfuscationFailures(t.Context(), rows, metadata.AttributeEventTopQuery)
	for i, row := range rows {
		_, present := row[obfuscationFailedKey]
		assert.False(t, present, "row %d keeps the internal marker", i)
		assert.Len(t, row, 1, "row %d lost another key", i)
	}
	metadatatest.AssertEqualPostgresqlQueryObfuscationFailed(t, tel, []metricdata.DataPoint[int64]{
		{Attributes: attribute.NewSet(attribute.String("event", "top_query")), Value: 1},
	}, metricdatatest.IgnoreTimestamp())
}

// TestApplyQuerySampleTruncatedCountMismatch: when the flags do not line up
// with the emitted records, nothing is marked and the mismatch is logged with
// both counts (M4).
func TestApplyQuerySampleTruncatedCountMismatch(t *testing.T) {
	core, observed := observer.New(zapcore.DebugLevel)
	logs := plogWithEvents("db.server.query_sample")
	applyQuerySampleTruncated(logs, []bool{true, true}, zap.New(core))

	_, ok := logs.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes().Get(dbQueryTextTruncatedAttribute)
	assert.False(t, ok, "no record is marked when the counts differ")
	entries := observed.FilterMessage("query_sample truncation flags do not match the emitted records; not adding db.query.text.truncated").AllUntimed()
	require.Len(t, entries, 1)
	assert.Equal(t, zapcore.ErrorLevel, entries[0].Level)
	assert.Equal(t, map[string]any{"records": int64(1), "flags": int64(2)}, entries[0].ContextMap())
}

func TestTopQueryExplainFailureLogsNoText(t *testing.T) {
	core, observed := observer.New(zapcore.DebugLevel)
	settings := receivertest.NewNopSettings(metadata.Type)
	settings.Logger = zap.New(core)
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true

	db, mock := newUnorderedMock(t)
	scraper, err := newTopQueryScraper(settings, cfg, mockSimpleClientFactory{db: db})
	require.NoError(t, err)
	primeTopQueryCache(scraper, "postgres", "10", "4242")

	mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).AddRow(p7TopQueryRow(topQueryColumns, p7Explainable)...))
	mock.ExpectQuery(regexp.QuoteMeta("PREPARE otel_4242 AS " + p7Explainable + ";")).WillReturnError(p7PQError())
	mock.ExpectExec(regexp.QuoteMeta("DEALLOCATE PREPARE otel_4242")).WillReturnResult(sqlmock.NewResult(0, 0))

	_, err = scraper.scrapeTopQuery(t.Context(), 10, 10, 1, 0)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	assertNoStatementText(t, observed)
	want := withFields(map[string]any{"queryID": "4242", "queryLength": int64(len(p7Explainable))}, p7PQErrorFields)
	for _, msg := range []string{"failed to prepare statement for EXPLAIN", "failed to explain query"} {
		entries := observed.FilterMessage(msg).AllUntimed()
		require.Len(t, entries, 1, msg)
		assert.Equal(t, want, entries[0].ContextMap(), msg)
	}
}

func TestTopQueryNilQueryIDLogsNoText(t *testing.T) {
	core, observed := observer.New(zapcore.DebugLevel)
	settings := receivertest.NewNopSettings(metadata.Type)
	settings.Logger = zap.New(core)
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true

	db, mock := newUnorderedMock(t)
	scraper, err := newTopQueryScraper(settings, cfg, mockSimpleClientFactory{db: db})
	require.NoError(t, err)

	columns := make([]string, 0, len(topQueryColumns))
	for _, column := range topQueryColumns {
		if column != queryidColumnName {
			columns = append(columns, column)
		}
	}
	mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(columns).AddRow(p7TopQueryRow(columns, p7Explainable)...))
	_, _ = scraper.scrapeTopQuery(t.Context(), 10, 10, 0, 0)

	entries := observed.FilterMessage("queryid is nil").AllUntimed()
	require.Len(t, entries, 1)
	assert.Empty(t, entries[0].ContextMap(), "the nil-queryid log carries no row fields")
	assertNoStatementText(t, observed)
}
