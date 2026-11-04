// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"database/sql/driver"
	"errors"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/receiver"
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

const (
	statStatementsInfoPattern = "pg_stat_statements_info"
	statStatementsCapsPattern = "pg_extension"
	// Without toplevel the row query groups entries per statement server side.
	topQueryRowsPattern         = `SUM\(calls\) AS calls[\s\S]*GROUP BY[\s\S]*LIMIT 3`
	topQueryToplevelRowsPattern = `toplevel::TEXT[\s\S]*LIMIT 3`
)

var (
	t1 = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	t2 = t1.Add(time.Hour)
)

type topQueryTestScraper struct {
	*postgreSQLScraper
	mock     sqlmock.Sqlmock
	observed *observer.ObservedLogs
	clock    time.Time
}

// advance moves the scraper's injected clock forward.
func (s *topQueryTestScraper) advance(d time.Duration) {
	s.clock = s.clock.Add(d)
}

// probeFailures counts failed capability probes. sqlmock rejects an unexpected
// probe query with an error, so this also catches probes that should not happen.
func (s *topQueryTestScraper) probeFailures() int {
	return s.observed.FilterMessageSnippet("failed to probe pg_stat_statements capabilities").Len()
}

func newTopQueryTestScraper(t *testing.T, opts ...func(*Config, *receiver.Settings)) *topQueryTestScraper {
	t.Helper()
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true
	cfg.TopQueryCollection.MaxRowsPerQuery = 3
	cfg.TopQueryCollection.TopNQuery = 3
	cfg.TopQueryCollection.MaxExplainEachInterval = 0

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		mock.ExpectClose()
		assert.NoError(t, db.Close())
	})

	core, observed := observer.New(zapcore.DebugLevel)
	settings := receivertest.NewNopSettings(metadata.Type)
	settings.Logger = zap.New(core)
	for _, opt := range opts {
		opt(cfg, &settings)
	}
	scraper, err := newTopQueryScraper(settings, cfg, mockSimpleClientFactory{db: db})
	require.NoError(t, err)
	s := &topQueryTestScraper{postgreSQLScraper: scraper, mock: mock, observed: observed, clock: t1}
	scraper.now = func() time.Time { return s.clock }
	return s
}

func (s *topQueryTestScraper) expectCapabilities(serverVersionNum int64, extVersion string) {
	s.mock.ExpectQuery(statStatementsCapsPattern).WillReturnRows(
		sqlmock.NewRows([]string{"server_version_num", "extversion"}).AddRow(serverVersionNum, extVersion),
	)
}

func (s *topQueryTestScraper) expectInfo(dealloc int64, statsReset time.Time) {
	s.mock.ExpectQuery(statStatementsInfoPattern).WillReturnRows(
		sqlmock.NewRows([]string{"dealloc", "stats_reset"}).AddRow(dealloc, statsReset),
	)
}

// expectRows queues the top_query rows; each row is {queryid, counter value[, toplevel]}.
func (s *topQueryTestScraper) expectRows(pattern string, rows ...[]string) {
	columns := topQueryColumns
	if len(rows) > 0 && len(rows[0]) == 3 {
		columns = append(append([]string{}, topQueryColumns...), "toplevel")
	}
	result := sqlmock.NewRows(columns)
	for _, r := range rows {
		row := topQueryRow(r[0], r[1])
		if len(r) == 3 {
			row = append(row, driver.Value(r[2]))
		}
		result.AddRow(row...)
	}
	s.mock.ExpectQuery(pattern).WillReturnRows(result)
}

// scrape runs one top_query scrape and returns the logs.
func (s *topQueryTestScraper) scrape(t *testing.T, maxExplain int64) plog.Logs {
	t.Helper()
	logs, err := s.scrapeTopQuery(t.Context(), 3, 3, maxExplain, 0)
	require.NoError(t, err)
	require.NoError(t, s.mock.ExpectationsWereMet())
	return logs
}

// calls runs one top_query scrape and returns the sorted calls deltas of the
// emitted db.server.top_query records.
func (s *topQueryTestScraper) calls(t *testing.T) []int64 {
	t.Helper()
	var calls []int64
	for _, lr := range recordsByEvent(s.scrape(t, 0), "db.server.top_query") {
		calls = append(calls, lr.Attributes().AsRaw()[dbAttributePrefix+callsColumnName].(int64))
	}
	slices.Sort(calls)
	return calls
}

func recordsByEvent(logs plog.Logs, event string) []plog.LogRecord {
	var records []plog.LogRecord
	for _, rl := range logs.ResourceLogs().All() {
		for _, sl := range rl.ScopeLogs().All() {
			for _, lr := range sl.LogRecords().All() {
				if lr.EventName() == event {
					records = append(records, lr)
				}
			}
		}
	}
	return records
}

// TestTopQueryStatsResetReportsCountersThatGrewPastStaleCache covers a reset
// after which the counter climbs past the stale cached value within one
// interval: the monotonic heuristic alone would report only the difference.
func TestTopQueryStatsResetReportsCountersThatGrewPastStaleCache(t *testing.T) {
	s := newTopQueryTestScraper(t)

	s.expectCapabilities(170002, "1.11")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000"})
	assert.Empty(t, s.calls(t), "first scrape seeds")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1010"})
	assert.Equal(t, []int64{10}, s.calls(t))

	// pg_stat_statements_reset(), then 2000 calls before the next scrape: 2000 > 1010.
	s.expectInfo(0, t2)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "2000"})
	assert.Equal(t, []int64{2000}, s.calls(t),
		"a changed stats_reset must report current values, not current minus the stale cache")
	s.expectInfo(0, t2)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "2005"})
	assert.Equal(t, []int64{5}, s.calls(t), "the cache is re-seeded after the reset")

	// Deallocations are surfaced at debug level.
	s.expectInfo(3, t2)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "2010"})
	assert.Equal(t, []int64{5}, s.calls(t))
	deallocLogs := s.observed.FilterMessage("pg_stat_statements deallocated statements since the previous scrape").AllUntimed()
	require.Len(t, deallocLogs, 1)
	assert.Equal(t, int64(3), deallocLogs[0].ContextMap()["deallocated"])
}

// TestTopQueryStatsResetSurvivesRowsFailure: a reset detected in a scrape whose
// row query then fails must still be treated as a reset by the next scrape.
func TestTopQueryStatsResetSurvivesRowsFailure(t *testing.T) {
	s := newTopQueryTestScraper(t)

	s.expectCapabilities(170002, "1.11")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000"})
	assert.Empty(t, s.calls(t))

	s.expectInfo(0, t2)
	s.mock.ExpectQuery(topQueryToplevelRowsPattern).WillReturnError(errors.New("canceling statement due to statement timeout"))
	_, err := s.scrapeTopQuery(t.Context(), 3, 3, 0, 0)
	require.NoError(t, err) // row errors are partial errors logged by the scraper
	require.NoError(t, s.mock.ExpectationsWereMet())

	s.expectInfo(0, t2)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "2000"})
	assert.Equal(t, []int64{2000}, s.calls(t), "the reset seen by the failed scrape must not be lost")
}

// TestTopQueryWithoutToplevelSupportReprobes covers PostgreSQL < 14 (or
// pg_stat_statements < 1.9): no toplevel, no pg_stat_statements_info, rows
// grouped per statement in SQL, the decreasing-counter heuristic, and a
// capability re-probe at most every 10 minutes so an upgrade is picked up
// without a collector restart.
func TestTopQueryWithoutToplevelSupportReprobes(t *testing.T) {
	s := newTopQueryTestScraper(t)

	s.expectCapabilities(130018, "1.8")
	s.expectRows(topQueryRowsPattern, []string{"42", "1000"})
	assert.Empty(t, s.calls(t))
	assert.False(t, s.toplevelSupported)

	s.advance(time.Minute)
	s.expectRows(topQueryRowsPattern, []string{"42", "1010"})
	assert.Equal(t, []int64{10}, s.calls(t), "no re-probe within 10 minutes of a negative result")
	s.advance(10 * time.Minute)
	s.expectCapabilities(130018, "1.8")
	s.expectRows(topQueryRowsPattern, []string{"42", "5"})
	assert.Equal(t, []int64{5}, s.calls(t), "a decreased counter is still reported as a reset")
	assert.Zero(t, s.probeFailures(), "no unexpected capability probe")
}

// TestTopQueryWithoutToplevelAggregatesInSQL covers PostgreSQL 14+ with
// pg_stat_statements < 1.9 (common after pg_upgrade) and track = 'all': the
// top-level and nested entries cannot be told apart, so the row query sums them
// per statement before ORDER BY/LIMIT. Row order or LIMIT can then no longer
// make one entry be compared against the other's cached counters.
func TestTopQueryWithoutToplevelAggregatesInSQL(t *testing.T) {
	s := newTopQueryTestScraper(t)

	s.expectCapabilities(170002, "1.8")
	s.expectRows(topQueryRowsPattern, []string{"42", "1050"})
	assert.Empty(t, s.calls(t))
	assert.False(t, s.toplevelSupported)
	s.expectRows(topQueryRowsPattern, []string{"42", "1065"})
	assert.Equal(t, []int64{15}, s.calls(t))
	for _, entry := range s.observed.FilterMessage("top_query delta cache").AllUntimed() {
		assert.Equal(t, int64(0), entry.ContextMap()["reset"])
	}
	assert.Zero(t, s.probeFailures())
}

func TestTopQueryToplevelCapabilityIsNotLatched(t *testing.T) {
	t.Run("42P01 then success enables toplevel", func(t *testing.T) {
		s := newTopQueryTestScraper(t)
		s.expectCapabilities(170002, "1.11")
		s.mock.ExpectQuery(statStatementsInfoPattern).WillReturnError(&pq.Error{Code: "42P01"})
		s.expectRows(topQueryRowsPattern, []string{"42", "1000"})
		s.calls(t)
		assert.False(t, s.toplevelSupported)

		s.expectCapabilities(170002, "1.11")
		s.expectInfo(0, t1)
		s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1010", "true"})
		s.calls(t)
		assert.True(t, s.toplevelSupported)
	})

	t.Run("success then 42P01 clears toplevel and re-probes", func(t *testing.T) {
		s := newTopQueryTestScraper(t)
		s.expectCapabilities(170002, "1.11")
		s.expectInfo(0, t1)
		s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"})
		s.calls(t)
		assert.True(t, s.toplevelSupported)

		s.mock.ExpectQuery(statStatementsInfoPattern).WillReturnError(&pq.Error{Code: "42P01"})
		s.expectRows(topQueryRowsPattern, []string{"42", "1010"})
		s.calls(t)
		assert.False(t, s.toplevelSupported)

		s.expectCapabilities(170002, "1.11")
		s.expectInfo(0, t1)
		s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1020", "true"})
		s.calls(t)
		assert.True(t, s.toplevelSupported)
	})

	t.Run("extension updated after start enables toplevel", func(t *testing.T) {
		s := newTopQueryTestScraper(t)
		s.expectCapabilities(170002, "1.8")
		s.expectRows(topQueryRowsPattern, []string{"42", "1000"})
		s.calls(t)
		assert.False(t, s.toplevelSupported)

		s.advance(5 * time.Minute)
		s.expectRows(topQueryRowsPattern, []string{"42", "1010"})
		s.calls(t)
		assert.False(t, s.toplevelSupported, "a negative result is re-probed at most every 10 minutes")

		s.advance(5 * time.Minute)
		s.expectCapabilities(170002, "1.11")
		s.expectInfo(0, t1)
		s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1010", "true"})
		s.calls(t)
		assert.True(t, s.toplevelSupported)
	})

	t.Run("42703 on the row query clears toplevel and re-probes", func(t *testing.T) {
		s := newTopQueryTestScraper(t)
		s.expectCapabilities(170002, "1.11")
		s.expectInfo(0, t1)
		s.mock.ExpectQuery(topQueryToplevelRowsPattern).WillReturnError(&pq.Error{Code: "42703", Message: `column "toplevel" does not exist`})
		_, err := s.scrapeTopQuery(t.Context(), 3, 3, 0, 0)
		require.NoError(t, err)
		require.NoError(t, s.mock.ExpectationsWereMet())
		assert.False(t, s.toplevelSupported)

		s.expectCapabilities(170002, "1.11")
		s.expectInfo(0, t1)
		s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"})
		s.calls(t)
		assert.True(t, s.toplevelSupported)
	})

	t.Run("transient probe failure on cold start is retried after 10 minutes", func(t *testing.T) {
		s := newTopQueryTestScraper(t)
		s.mock.ExpectQuery(statStatementsCapsPattern).WillReturnError(errors.New("connection reset by peer"))
		s.expectRows(topQueryRowsPattern, []string{"42", "1000"})
		s.calls(t)
		assert.False(t, s.toplevelSupported)
		assert.Equal(t, 1, s.probeFailures())

		s.advance(time.Minute)
		s.expectRows(topQueryRowsPattern, []string{"42", "1000"})
		s.calls(t)
		assert.Equal(t, 1, s.probeFailures(), "a failed probe is retried at most every 10 minutes")

		s.advance(10 * time.Minute)
		s.expectCapabilities(170002, "1.11")
		s.expectInfo(0, t1)
		s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"})
		s.calls(t)
		assert.True(t, s.toplevelSupported)
	})

	t.Run("transient info failure keeps the cached capability", func(t *testing.T) {
		s := newTopQueryTestScraper(t)
		s.expectCapabilities(170002, "1.11")
		s.mock.ExpectQuery(statStatementsInfoPattern).WillReturnError(errors.New("connection reset by peer"))
		s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"})
		s.calls(t)
		assert.True(t, s.toplevelSupported, "toplevel support comes from server_version_num and extversion, not from the info read")

		s.expectInfo(0, t1)
		s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1010", "true"})
		assert.Equal(t, []int64{10}, s.calls(t))
	})
}

// TestTopQueryMergesToplevelAndNestedEntries covers pg_stat_statements.track =
// 'all' on PostgreSQL 14+: the top-level and nested entries of one statement
// keep independent cache entries (no reset of one by the other) and are
// emitted as one record whose deltas are their sum.
func TestTopQueryMergesToplevelAndNestedEntries(t *testing.T) {
	s := newTopQueryTestScraper(t)

	s.expectCapabilities(170002, "1.11")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"}, []string{"42", "50", "false"})
	assert.Empty(t, s.calls(t), "both entries are seeded on the first scrape")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1010", "true"}, []string{"42", "55", "false"})
	assert.Equal(t, []int64{15}, s.calls(t), "one record per statement, summing top-level and nested deltas")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1012", "true"}, []string{"42", "56", "false"})
	assert.Equal(t, []int64{3}, s.calls(t))

	for _, entry := range s.observed.FilterMessage("top_query delta cache").AllUntimed() {
		assert.Equal(t, int64(0), entry.ContextMap()["reset"], "top-level and nested entries must not reset each other")
	}
}

// TestTopQueryMergedStatementIsExplainedOnce: merging must also keep the
// statement to one EXPLAIN and one db.server.query_plan record.
func TestTopQueryMergedStatementIsExplainedOnce(t *testing.T) {
	s := newTopQueryTestScraper(t, func(cfg *Config, _ *receiver.Settings) {
		cfg.LogsBuilderConfig.Events.DbServerQueryPlan.Enabled = true
	})

	s.expectCapabilities(170002, "1.11")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"}, []string{"42", "50", "false"})
	s.scrape(t, 3)

	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1010", "true"}, []string{"42", "55", "false"})
	s.mock.ExpectQuery(regexp.QuoteMeta("/* otel-collector-ignore */ SET plan_cache_mode = force_generic_plan;PREPARE otel_42 AS SELECT 1;")).
		WillReturnRows(sqlmock.NewRows([]string{"result"}))
	s.mock.ExpectQuery(regexp.QuoteMeta("/* otel-collector-ignore */ SELECT COALESCE(array_length(parameter_types, 1), 0) AS param_count FROM pg_prepared_statements WHERE name = 'otel_42';")).
		WillReturnRows(sqlmock.NewRows([]string{"param_count"}).AddRow("0"))
	s.mock.ExpectQuery(regexp.QuoteMeta("EXPLAIN(FORMAT JSON) EXECUTE otel_42;")).
		WillReturnRows(sqlmock.NewRows([]string{"QUERY PLAN"}).AddRow(`[{"Plan":{"Node Type":"Result"}}]`))
	s.mock.ExpectExec(regexp.QuoteMeta("/* otel-collector-ignore */ DEALLOCATE PREPARE otel_42")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	logs := s.scrape(t, 3)

	assert.Len(t, recordsByEvent(logs, "db.server.top_query"), 1)
	assert.Len(t, recordsByEvent(logs, "db.server.query_plan"), 1)
}

// TestTopQueryDuplicateCacheKeyIsNotCompared guards against two rows of one
// scrape sharing a cache key (e.g. top-level and nested entries while toplevel
// support is unknown): the duplicate reports 0 and is counted, instead of being
// compared against the other row and looking like a reset.
func TestTopQueryDuplicateCacheKeyIsNotCompared(t *testing.T) {
	tel := componenttest.NewTelemetry()
	defer func() { require.NoError(t, tel.Shutdown(t.Context())) }()
	s := newTopQueryTestScraper(t, func(_ *Config, settings *receiver.Settings) {
		logger := settings.Logger
		settings.TelemetrySettings = tel.NewTelemetrySettings()
		settings.Logger = logger
	})

	s.expectCapabilities(130018, "1.8")
	s.expectRows(topQueryRowsPattern, []string{"42", "1000"}, []string{"42", "50"})
	assert.Empty(t, s.calls(t))
	s.expectRows(topQueryRowsPattern, []string{"42", "1010"}, []string{"42", "55"})
	assert.Equal(t, []int64{10}, s.calls(t), "only the first row of a duplicated key is compared against the cache")

	metadatatest.AssertEqualPostgresqlTopQueryCacheEvents(t, tel, []metricdata.DataPoint[int64]{
		{Attributes: attribute.NewSet(attribute.String("outcome", "seeded")), Value: 1},
		{Attributes: attribute.NewSet(attribute.String("outcome", "duplicated")), Value: 2},
	}, metricdatatest.IgnoreTimestamp())
	for _, entry := range s.observed.FilterMessage("top_query delta cache").AllUntimed() {
		assert.Equal(t, int64(1), entry.ContextMap()["duplicated"])
		assert.Equal(t, int64(0), entry.ContextMap()["reset"])
	}
}
