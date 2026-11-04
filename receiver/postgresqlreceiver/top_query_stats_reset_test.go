// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadata"
)

const statStatementsInfoPattern = "pg_stat_statements_info"

func newStatsResetTestScraper(t *testing.T) (*postgreSQLScraper, sqlmock.Sqlmock, *observer.ObservedLogs) {
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
	scraper, err := newTopQueryScraper(settings, cfg, mockSimpleClientFactory{db: db})
	require.NoError(t, err)
	return scraper, mock, observed
}

// scrapeTargetCalls runs one top_query scrape and returns the emitted calls deltas.
func scrapeTargetCalls(t *testing.T, scraper *postgreSQLScraper, mock sqlmock.Sqlmock, value string) []int64 {
	t.Helper()
	mock.ExpectQuery("LIMIT 3").WillReturnRows(sqlmock.NewRows(topQueryColumns).AddRow(topQueryRow("42", "10", value)...))
	logs, err := scraper.scrapeTopQuery(t.Context(), 3, 3, 0, 0)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	var calls []int64
	for _, rl := range logs.ResourceLogs().All() {
		for _, sl := range rl.ScopeLogs().All() {
			for _, lr := range sl.LogRecords().All() {
				calls = append(calls, lr.Attributes().AsRaw()[dbAttributePrefix+callsColumnName].(int64))
			}
		}
	}
	return calls
}

func expectStatStatementsInfo(mock sqlmock.Sqlmock, dealloc int64, statsReset time.Time) {
	mock.ExpectQuery(statStatementsInfoPattern).WillReturnRows(
		sqlmock.NewRows([]string{"dealloc", "stats_reset"}).AddRow(dealloc, statsReset),
	)
}

// TestTopQueryStatsResetReportsCountersThatGrewPastStaleCache covers a reset
// after which the counter climbs past the stale cached value within one
// interval: the monotonic heuristic alone would report only the difference.
func TestTopQueryStatsResetReportsCountersThatGrewPastStaleCache(t *testing.T) {
	scraper, mock, observed := newStatsResetTestScraper(t)
	t1 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	expectStatStatementsInfo(mock, 0, t1)
	assert.Empty(t, scrapeTargetCalls(t, scraper, mock, "1000"), "first scrape seeds")
	expectStatStatementsInfo(mock, 0, t1)
	assert.Equal(t, []int64{10}, scrapeTargetCalls(t, scraper, mock, "1010"))

	// pg_stat_statements_reset(), then 2000 calls before the next scrape: 2000 > 1010.
	expectStatStatementsInfo(mock, 0, t2)
	assert.Equal(t, []int64{2000}, scrapeTargetCalls(t, scraper, mock, "2000"),
		"a changed stats_reset must report current values, not current minus the stale cache")
	expectStatStatementsInfo(mock, 0, t2)
	assert.Equal(t, []int64{5}, scrapeTargetCalls(t, scraper, mock, "2005"), "the cache is re-seeded after the reset")

	// Deallocations are surfaced at debug level.
	expectStatStatementsInfo(mock, 3, t2)
	assert.Equal(t, []int64{5}, scrapeTargetCalls(t, scraper, mock, "2010"))
	deallocLogs := observed.FilterMessage("pg_stat_statements deallocated statements since the previous scrape").AllUntimed()
	require.Len(t, deallocLogs, 1)
	assert.Equal(t, int64(3), deallocLogs[0].ContextMap()["deallocated"])
}

// TestTopQueryStatsResetFallsBackWithoutStatStatementsInfo covers PostgreSQL < 14
// (no pg_stat_statements_info): the receiver stops querying the view and keeps
// the monotonic heuristic.
func TestTopQueryStatsResetFallsBackWithoutStatStatementsInfo(t *testing.T) {
	scraper, mock, _ := newStatsResetTestScraper(t)

	mock.ExpectQuery(statStatementsInfoPattern).WillReturnError(&pq.Error{Code: "42P01", Message: `relation "pg_stat_statements_info" does not exist`})
	assert.Empty(t, scrapeTargetCalls(t, scraper, mock, "1000"))
	assert.Equal(t, statStatementsInfoUnsupported, scraper.statStatementsInfo)

	// No further pg_stat_statements_info query is expected from here on.
	assert.Equal(t, []int64{10}, scrapeTargetCalls(t, scraper, mock, "1010"))
	assert.Equal(t, []int64{5}, scrapeTargetCalls(t, scraper, mock, "5"), "a decreased counter is still reported as a reset")
	assert.Equal(t, []int64{4}, scrapeTargetCalls(t, scraper, mock, "9"))
}

// TestTopQueryStatsResetTransientErrorKeepsTrying covers a transient failure
// reading pg_stat_statements_info: the scrape still succeeds with the heuristic
// and the view is queried again next time.
func TestTopQueryStatsResetTransientErrorKeepsTrying(t *testing.T) {
	scraper, mock, _ := newStatsResetTestScraper(t)
	t1 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(statStatementsInfoPattern).WillReturnError(errors.New("connection reset by peer"))
	assert.Empty(t, scrapeTargetCalls(t, scraper, mock, "1000"))
	assert.NotEqual(t, statStatementsInfoUnsupported, scraper.statStatementsInfo)

	expectStatStatementsInfo(mock, 0, t1)
	assert.Equal(t, []int64{10}, scrapeTargetCalls(t, scraper, mock, "1010"))
}
