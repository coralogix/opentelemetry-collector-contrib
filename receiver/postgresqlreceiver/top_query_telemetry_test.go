// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"database/sql/driver"
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

// topQueryRow builds one pg_stat_statements row (userid 10) where every counter column holds value.
func topQueryRow(queryID, value string) []driver.Value {
	values := map[string]driver.Value{
		"datname":         "postgres",
		"query":           "SELECT 1",
		queryidColumnName: queryID,
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

// TestTopQueryCacheTelemetryCountsSeedsAndEvictions forces evictions with a
// one-statement cache and checks that every seed and eviction is counted, that
// no record is emitted from a partially cached statement, and that each scrape
// logs its seed/eviction counts at debug level.
func TestTopQueryCacheTelemetryCountsSeedsAndEvictions(t *testing.T) {
	tel := componenttest.NewTelemetry()
	defer func() { require.NoError(t, tel.Shutdown(t.Context())) }()

	core, observed := observer.New(zapcore.DebugLevel)
	settings := receivertest.NewNopSettings(metadata.Type)
	settings.TelemetrySettings = tel.NewTelemetrySettings()
	settings.Logger = zap.New(core)

	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true
	cfg.TopQueryCollection.MaxRowsPerQuery = 2
	cfg.TopQueryCollection.TopNQuery = 2
	cfg.TopQueryCollection.MaxExplainEachInterval = 0

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	// Room for a single statement: two statements per scrape evict each other.
	scraper, err := newPostgreSQLScraper(settings, cfg, mockSimpleClientFactory{db: db}, newCache(1), newTTLCache[string](1, time.Second))
	require.NoError(t, err)

	for scrape, value := range []string{"100", "200"} {
		mock.ExpectQuery("LIMIT 2").WillReturnRows(sqlmock.NewRows(topQueryColumns).
			AddRow(topQueryRow("1", value)...).
			AddRow(topQueryRow("2", value)...))
		logs, scrapeErr := scraper.scrapeTopQuery(t.Context(), 2, 2, 0, 0)
		require.NoError(t, scrapeErr)
		require.NoError(t, mock.ExpectationsWereMet())
		assert.Equal(t, 0, logs.LogRecordCount(), "scrape %d: evicted statements must be re-seeded as a whole, never reported", scrape)
	}

	// Scrape 1: seed 1, seed 2 (evicts 1). Scrape 2: seed 1 (evicts 2), seed 2 (evicts 1).
	metadatatest.AssertEqualPostgresqlTopQueryCacheEvents(t, tel, []metricdata.DataPoint[int64]{
		{Attributes: attribute.NewSet(attribute.String("outcome", "seeded")), Value: 4},
		{Attributes: attribute.NewSet(attribute.String("outcome", "evicted")), Value: 3},
	}, metricdatatest.IgnoreTimestamp())

	entries := observed.FilterMessage("top_query delta cache").AllUntimed()
	require.Len(t, entries, 2, "one debug log per scrape")
	for i, want := range []struct{ seeded, evicted int64 }{{2, 1}, {2, 2}} {
		fields := entries[i].ContextMap()
		assert.Equal(t, zapcore.DebugLevel, entries[i].Level)
		assert.Equal(t, want.seeded, fields["seeded"], "scrape %d seeded", i)
		assert.Equal(t, want.evicted, fields["evicted"], "scrape %d evicted", i)
	}
	mock.ExpectClose()
}
