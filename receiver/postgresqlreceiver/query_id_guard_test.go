// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
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

// [DBM P7c] A queryid is a bigint that PostgreSQL prints in canonical decimal
// form. Anything else is forged or corrupted and may carry text, so it is
// dropped and never echoed. p7cForged is such a value: it carries the
// synthetic literal.
const p7cForged = "4242 /* " + p7Literal + " */"

// assertNoForgedQueryID fails if any log entry holds the forged value or
// the literal it carries.
func assertNoForgedQueryID(t *testing.T, observed *observer.ObservedLogs) {
	t.Helper()
	assertNoStatementText(t, observed)
	entries := observed.AllUntimed()
	for i := range entries {
		for key, value := range entries[i].ContextMap() {
			assert.NotEqual(t, p7cForged, fmt.Sprint(value), "log field %q of %q echoes the forged queryid", key, entries[i].Message)
		}
	}
}

func TestIsCanonicalQueryID(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"0", true},
		{"42", true},
		{"-42", true},
		{"9223372036854775807", true},
		{"-9223372036854775808", true},
		{"", false},
		{"+42", false},
		{"042", false},
		{"00", false},
		{"-0", false},
		{"-042", false},
		{" 42", false},
		{"42 ", false},
		{"4 2", false},
		{"42\n", false},
		{"9223372036854775808", false},
		{"-9223372036854775809", false},
		{"0x2a", false},
		{"4e2", false},
		{"42.0", false},
		{"1_000", false},
		{"abc-def-123", false},
		{"-", false},
		// [P7c review] Unicode digits are not ASCII digits.
		{"\uff14\uff12", false},         // fullwidth "42"
		{"\u0664\u0662", false},         // Arabic-Indic "42"
		{"4\u0662", false},              // mixed
		{"\U0001D7DC\U0001D7DA", false}, // mathematical double-struck "42"
		// [P7c review] very long values.
		{strings.Repeat("9", 4096), false},
		{"1" + strings.Repeat("0", 4096), false},
		{"1" + strings.Repeat("0", 18), true},
		{p7cForged, false},
	} {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			assert.Equal(t, tc.want, isCanonicalQueryID(tc.in))
		})
	}
}

func TestQueryIDField(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want map[string]any
	}{
		{in: "4242", want: map[string]any{"queryID": "4242"}},
		{in: "-7", want: map[string]any{"queryID": "-7"}},
		{in: "", want: map[string]any{"queryID": ""}},
		{in: p7cForged, want: map[string]any{"queryIDInvalid": true}},
		{in: "042", want: map[string]any{"queryIDInvalid": true}},
	} {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			enc := zapcore.NewMapObjectEncoder()
			queryIDField(tc.in).AddTo(enc)
			assert.Equal(t, tc.want, enc.Fields)
		})
	}
}

// TestQuerySampleInvalidQueryIDDropped: a query sample keeps its record, but
// a non-canonical query_id is replaced by "" and marked for the counter; a
// canonical or empty query_id is kept as is.
func TestQuerySampleInvalidQueryIDDropped(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	core, observed := observer.New(zapcore.DebugLevel)
	client := &postgreSQLClient{client: db, closeFn: func() error { return nil }}

	mock.ExpectQuery("FROM pg_stat_activity").WillReturnRows(sqlmock.NewRows(querySampleColumns).
		AddRow(querySampleRow(map[string]any{querySampleColumnPID: "1001", querySampleColumnQueryID: p7cForged, querySampleColumnState: "active", querySampleColumnQuery: "SELECT 1"})...).
		AddRow(querySampleRow(map[string]any{querySampleColumnPID: "1002", querySampleColumnQueryID: "77", querySampleColumnState: "active", querySampleColumnQuery: "SELECT 1"})...).
		AddRow(querySampleRow(map[string]any{querySampleColumnPID: "1003", querySampleColumnQueryID: "", querySampleColumnState: "active", querySampleColumnQuery: "SELECT 1"})...))

	atts, _, err := client.getQuerySamples(t.Context(), 10, 0, nil, zap.New(core))
	require.NoError(t, err)
	require.Len(t, atts, 3, "the sample itself is kept")

	assert.Empty(t, atts[0][dbAttributePrefix+querySampleColumnQueryID], "a non-canonical query_id is dropped")
	assert.Equal(t, true, atts[0][queryIDInvalidKey])
	assert.Equal(t, "77", atts[1][dbAttributePrefix+querySampleColumnQueryID])
	assert.NotContains(t, atts[1], queryIDInvalidKey)
	assert.Empty(t, atts[2][dbAttributePrefix+querySampleColumnQueryID])
	assert.NotContains(t, atts[2], queryIDInvalidKey, "an empty query_id (compute_query_id off) is not invalid")

	assertNoForgedQueryID(t, observed)
	entries := observed.FilterMessage("query_id is not a canonical int64; dropped").AllUntimed()
	require.Len(t, entries, 1)
	assert.Equal(t, zapcore.WarnLevel, entries[0].Level)
	assert.Equal(t, map[string]any{"event": "query_sample", "queryIDLength": int64(len(p7cForged))}, entries[0].ContextMap())
}

// TestQuerySampleObfuscationFailureWithInvalidQueryID: the obfuscation
// failure log of a sample with a forged query_id carries the dropped ("")
// value, never the forged one.
func TestQuerySampleObfuscationFailureWithInvalidQueryID(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	core, observed := observer.New(zapcore.DebugLevel)
	client := &postgreSQLClient{client: db, closeFn: func() error { return nil }}

	mock.ExpectQuery("FROM pg_stat_activity").WillReturnRows(sqlmock.NewRows(querySampleColumns).AddRow(querySampleRow(map[string]any{
		querySampleColumnPID: "1001", querySampleColumnQueryID: p7cForged, querySampleColumnState: "active", querySampleColumnQuery: p7Unterminated,
	})...))

	_, _, err = client.getQuerySamples(t.Context(), 10, 0, nil, zap.New(core))
	require.NoError(t, err)

	assertNoForgedQueryID(t, observed)
	entries := observed.FilterMessage("failed to obfuscate query").AllUntimed()
	require.Len(t, entries, 1)
	assert.Equal(t, map[string]any{
		"event":       "query_sample",
		"reason":      "unterminated_string",
		"queryID":     "",
		"queryLength": int64(len(p7Unterminated)),
	}, entries[0].ContextMap())
}

// TestTopQueryObfuscationFailureWithInvalidQueryID: the client's obfuscation
// failure log runs before the scraper drops the row, so it must not echo a
// forged queryid either.
func TestTopQueryObfuscationFailureWithInvalidQueryID(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	core, observed := observer.New(zapcore.DebugLevel)
	client := &postgreSQLClient{client: db, closeFn: func() error { return nil }}

	row := p7TopQueryRow(topQueryColumns, p7Unterminated)
	setTopQueryColumn(row, queryidColumnName, p7cForged)
	mock.ExpectQuery("pg_stat_statements").WillReturnRows(sqlmock.NewRows(topQueryColumns).AddRow(row...))

	_, err = client.getTopQuery(t.Context(), 10, nil, false, false, TopQueryCandidateOrderTotalTime, zap.New(core))
	require.NoError(t, err)

	assertNoForgedQueryID(t, observed)
	entries := observed.FilterMessage("failed to obfuscate query").AllUntimed()
	require.Len(t, entries, 1)
	assert.Equal(t, map[string]any{
		"event":          "top_query",
		"reason":         "unterminated_string",
		"queryIDInvalid": true,
		"queryLength":    int64(len(p7Unterminated)),
	}, entries[0].ContextMap())
}

func setTopQueryColumn(row []driver.Value, column string, value any) {
	for i, c := range topQueryColumns {
		if c == column {
			row[i] = value
		}
	}
}

// TestTopQueryInvalidQueryIDRowDropped: a top query row whose queryid is not
// canonical is dropped before it touches the cache, EXPLAIN or the emitted
// records; the valid row is still emitted; both drops are counted.
func TestTopQueryInvalidQueryIDRowDropped(t *testing.T) {
	tel := componenttest.NewTelemetry()
	defer func() { require.NoError(t, tel.Shutdown(t.Context())) }()
	core, observed := observer.New(zapcore.DebugLevel)
	settings := receivertest.NewNopSettings(metadata.Type)
	settings.TelemetrySettings = tel.NewTelemetrySettings()
	settings.Logger = zap.New(core)
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true

	db, mock := newUnorderedMock(t)
	scraper, err := newTopQueryScraper(settings, cfg, mockSimpleClientFactory{db: db})
	require.NoError(t, err)
	primeTopQueryCache(scraper, "postgres", "10", "4242")
	primeTopQueryCache(scraper, "postgres", "10", p7cForged)
	primeTopQueryCache(scraper, "postgres", "10", "0042")

	forged := p7TopQueryRow(topQueryColumns, "SELECT 1")
	setTopQueryColumn(forged, queryidColumnName, p7cForged)
	padded := p7TopQueryRow(topQueryColumns, "SELECT 2")
	setTopQueryColumn(padded, queryidColumnName, "0042")
	valid := p7TopQueryRow(topQueryColumns, "SELECT 3")
	mock.ExpectQuery("LIMIT 10").WillReturnRows(sqlmock.NewRows(topQueryColumns).AddRow(forged...).AddRow(padded...).AddRow(valid...))

	logs, err := scraper.scrapeTopQuery(t.Context(), 10, 10, 0, 0)
	require.NoError(t, err)

	records := topQueryRecords(logs)
	require.Len(t, records, 1, "only the canonical row is emitted")
	assert.Equal(t, "4242", records[0][dbAttributePrefix+queryidColumnName])

	assertNoForgedQueryID(t, observed)
	entries := observed.FilterMessage("queryid is not a canonical int64; top query row dropped").AllUntimed()
	require.Len(t, entries, 2)
	assert.Equal(t, zapcore.WarnLevel, entries[0].Level, "a repeated forged queryid must not flood the Error level")
	assert.Equal(t, zapcore.WarnLevel, entries[1].Level)
	assert.Equal(t, map[string]any{"event": "top_query", "queryIDLength": int64(len(p7cForged))}, entries[0].ContextMap())
	assert.Equal(t, map[string]any{"event": "top_query", "queryIDLength": int64(4)}, entries[1].ContextMap())

	metadatatest.AssertEqualPostgresqlQueryIDInvalid(t, tel, []metricdata.DataPoint[int64]{
		{Attributes: attribute.NewSet(attribute.String("event", "top_query")), Value: 2},
	}, metricdatatest.IgnoreTimestamp())
}

// TestTopQueryNonStringQueryIDRowDropped: a queryid of another type is
// dropped like a non-canonical one, never type-asserted into a panic.
func TestTopQueryNonStringQueryIDRowDropped(t *testing.T) {
	core, observed := observer.New(zapcore.DebugLevel)
	settings := receivertest.NewNopSettings(metadata.Type)
	settings.Logger = zap.New(core)
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true

	scraper, err := newTopQueryScraper(settings, cfg, mockSimpleClientFactory{})
	require.NoError(t, err)

	rows := []map[string]any{{
		"db.namespace":                        "postgres",
		dbAttributePrefix + "userid":          "10",
		dbAttributePrefix + queryidColumnName: int64(4242),
	}}
	var kept []map[string]any
	assert.NotPanics(t, func() {
		kept = scraper.dropInvalidTopQueryIDs(t.Context(), rows, zap.New(core))
	})
	assert.Empty(t, kept)
	entries := observed.FilterMessage("queryid is not a canonical int64; top query row dropped").AllUntimed()
	require.Len(t, entries, 1)
	assert.Equal(t, map[string]any{"event": "top_query", "queryIDLength": int64(0), "queryIDType": "int64"}, entries[0].ContextMap())
}

// TestQuerySampleInvalidQueryIDCounted: the scrape counts the dropped
// query_id, emits the sample with an empty postgresql.query_id and never
// emits the internal marker.
func TestQuerySampleInvalidQueryIDCounted(t *testing.T) {
	tel := componenttest.NewTelemetry()
	defer func() { require.NoError(t, tel.Shutdown(t.Context())) }()
	core, observed := observer.New(zapcore.DebugLevel)
	settings := receivertest.NewNopSettings(metadata.Type)
	settings.TelemetrySettings = tel.NewTelemetrySettings()
	settings.Logger = zap.New(core)
	cfg := createDefaultConfig().(*Config)
	cfg.Databases = []string{}
	cfg.LogsBuilderConfig.Events.DbServerQuerySample.Enabled = true

	db, mock := newUnorderedMock(t)
	scraper, err := newPostgreSQLScraper(settings, cfg, mockSimpleClientFactory{db: db}, newCache(10), newTTLCache[string](1, time.Second))
	require.NoError(t, err)

	mock.ExpectQuery("FROM pg_stat_activity").WillReturnRows(sqlmock.NewRows(querySampleColumns).
		AddRow(querySampleRow(map[string]any{querySampleColumnPID: "1001", querySampleColumnQueryID: p7cForged, querySampleColumnState: "active", querySampleColumnQuery: "SELECT 1"})...).
		AddRow(querySampleRow(map[string]any{querySampleColumnPID: "1002", querySampleColumnQueryID: "77", querySampleColumnState: "active", querySampleColumnQuery: "SELECT 1"})...))
	logs, err := scraper.scrapeQuerySamples(t.Context(), 10)
	require.NoError(t, err)

	records := querySampleRecords(logs)
	require.Len(t, records, 2)
	for _, lr := range records {
		pid, ok := lr.Attributes().Get("postgresql.pid")
		require.True(t, ok)
		id, ok := lr.Attributes().Get("postgresql.query_id")
		require.True(t, ok)
		if pid.Int() == 1001 {
			assert.Empty(t, id.Str())
		} else {
			assert.Equal(t, "77", id.Str())
		}
		_, leaked := lr.Attributes().Get(queryIDInvalidKey)
		assert.False(t, leaked, "the internal marker is never emitted")
	}
	metadatatest.AssertEqualPostgresqlQueryIDInvalid(t, tel, []metricdata.DataPoint[int64]{
		{Attributes: attribute.NewSet(attribute.String("event", "query_sample")), Value: 1},
	}, metricdatatest.IgnoreTimestamp())
	assertNoForgedQueryID(t, observed)
}

// TestRecordInvalidQueryIDsStripsMarker: the marker is removed from every
// row, counted or not, and other keys are kept.
func TestRecordInvalidQueryIDsStripsMarker(t *testing.T) {
	tel := componenttest.NewTelemetry()
	defer func() { require.NoError(t, tel.Shutdown(t.Context())) }()
	settings := receivertest.NewNopSettings(metadata.Type)
	settings.TelemetrySettings = tel.NewTelemetrySettings()
	scraper, err := newPostgreSQLScraper(settings, createDefaultConfig().(*Config), mockSimpleClientFactory{}, newCache(1), newTTLCache[string](1, time.Second))
	require.NoError(t, err)

	rows := []map[string]any{
		{queryIDInvalidKey: true, "a": 1},
		{"a": 2},
		{queryIDInvalidKey: false, "a": 3},
	}
	scraper.recordInvalidQueryIDs(t.Context(), rows, metadata.AttributeEventQuerySample)
	for i, row := range rows {
		_, present := row[queryIDInvalidKey]
		assert.False(t, present, "row %d keeps the internal marker", i)
		assert.Len(t, row, 1, "row %d lost another key", i)
	}
	metadatatest.AssertEqualPostgresqlQueryIDInvalid(t, tel, []metricdata.DataPoint[int64]{
		{Attributes: attribute.NewSet(attribute.String("event", "query_sample")), Value: 1},
	}, metricdatatest.IgnoreTimestamp())
}

// TestExplainQueryRefusesInvalidQueryID: the queryid becomes part of the
// PREPARE / EXECUTE / DEALLOCATE identifier otel_<id>, so a non-canonical one
// is refused before any SQL runs and before a connection is taken: the
// database is closed first, so any Conn or statement would fail with
// "sql: database is closed" instead of errInvalidQueryID.
func TestExplainQueryRefusesInvalidQueryID(t *testing.T) {
	for _, id := range []string{p7cForged, "abc-def-123", "1; SELECT 2", "042", ""} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			require.NoError(t, err)
			t.Cleanup(func() { db.Close() })
			core, observed := observer.New(zapcore.DebugLevel)
			client := &postgreSQLClient{client: db, closeFn: func() error { return nil }}
			mock.ExpectClose()
			require.NoError(t, db.Close())

			plan, err := client.explainQuery(t.Context(), p7Explainable, id, zap.New(core))
			require.ErrorIs(t, err, errInvalidQueryID)
			assert.Empty(t, plan)
			require.NoError(t, mock.ExpectationsWereMet(), "no SQL runs")

			assertNoForgedQueryID(t, observed)
			entries := observed.FilterMessage("refusing EXPLAIN: queryid is not a canonical int64").AllUntimed()
			require.Len(t, entries, 1)
			assert.Equal(t, zapcore.WarnLevel, entries[0].Level, "a repeated forged queryid must not flood the Error level")
			assert.Equal(t, map[string]any{"queryIDInvalid": true, "queryIDLength": int64(len(id))}, entries[0].ContextMap())
			assert.NotContains(t, err.Error(), p7Literal)
		})
	}
}

// TestExplainQueryAcceptsNegativeQueryID: a negative queryid is canonical;
// its sign becomes "_" in the identifier, as before.
func TestExplainQueryAcceptsNegativeQueryID(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	client := &postgreSQLClient{client: db, closeFn: func() error { return nil }}

	mock.ExpectQuery("/* otel-collector-ignore */ SET plan_cache_mode = force_generic_plan;PREPARE otel__42 AS SELECT 1;").
		WillReturnError(errors.New("stop here"))
	mock.ExpectExec("/* otel-collector-ignore */ DEALLOCATE PREPARE otel__42").WillReturnResult(sqlmock.NewResult(0, 0))

	_, err = client.explainQuery(t.Context(), "SELECT 1", "-42", zap.NewNop())
	require.Error(t, err)
	require.NotErrorIs(t, err, errInvalidQueryID)
	require.NoError(t, mock.ExpectationsWereMet())
}
