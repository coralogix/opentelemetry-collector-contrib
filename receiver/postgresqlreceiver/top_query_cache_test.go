// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"database/sql/driver"
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadata"
)

func TestTopQueryCacheRetainsFetchedStatements(t *testing.T) {
	columns := []string{
		callsColumnName, "datname", sharedBlksDirtiedColumnName, sharedBlksHitColumnName,
		sharedBlksReadColumnName, sharedBlksWrittenColumnName, tempBlksReadColumnName,
		tempBlksWrittenColumnName, "query", queryidColumnName, "userid", "rolname", rowsColumnName,
		totalExecTimeColumnName, totalPlanTimeColumnName,
	}
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true
	cfg.TopQueryCollection.MaxRowsPerQuery = 500
	cfg.TopQueryCollection.TopNQuery = 50
	cfg.TopQueryCollection.MaxExplainEachInterval = 0

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	scraper, err := newTopQueryScraper(receivertest.NewNopSettings(metadata.Type), cfg, mockSimpleClientFactory{db: db})
	require.NoError(t, err)

	ids := make([]int, 500)
	for i := range ids {
		ids[i] = i
	}
	reversed := slices.Clone(ids)
	slices.Reverse(reversed)
	changed := slices.Clone(ids)
	for i := range changed[:250] {
		changed[i] = 500 + i
	}

	for _, tt := range []struct {
		name          string
		ids           []int
		increment     bool
		expectedCount int
		// One entry is cached per statement; a cold statement is seeded (not
		// emitted), so the cache must retain every fetched statement for the next
		// interval's delta to be correct.
		expectedCacheLen int
	}{
		{name: "first scrape seeds without emitting", ids: ids, expectedCacheLen: 500},
		{name: "unchanged counters", ids: ids, expectedCacheLen: 500},
		{name: "reordered statements", ids: reversed, expectedCacheLen: 500},
		{name: "changed statement membership seeds new statements", ids: changed, expectedCacheLen: 750},
		{name: "unchanged after membership change", ids: changed, expectedCacheLen: 750},
		{name: "one active statement", ids: changed, increment: true, expectedCount: 1, expectedCacheLen: 750},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows := sqlmock.NewRows(columns)
			for _, id := range tt.ids {
				values := map[string]driver.Value{
					"datname":         "postgres",
					"query":           "SELECT 1",
					"rolname":         "otel",
					queryidColumnName: fmt.Sprint(id),
				}
				for _, column := range columns {
					if _, exists := values[column]; !exists {
						values[column] = "10000"
					}
				}
				if tt.increment && id == 499 {
					values[callsColumnName] = "10003"
					values[rowsColumnName] = "10007"
					values[sharedBlksHitColumnName] = "10009"
					values[totalExecTimeColumnName] = "12500"
					values[totalPlanTimeColumnName] = "10500"
				}
				row := make([]driver.Value, len(columns))
				for i, column := range columns {
					row[i] = values[column]
				}
				rows.AddRow(row...)
			}
			mock.ExpectQuery("LIMIT 500").WillReturnRows(rows)
			logs, scrapeErr := scraper.scrapeTopQuery(t.Context(), cfg.TopQueryCollection.MaxRowsPerQuery, cfg.TopQueryCollection.TopNQuery, cfg.TopQueryCollection.MaxExplainEachInterval, 0)
			require.NoError(t, scrapeErr)
			require.NoError(t, mock.ExpectationsWereMet())
			require.Equal(t, tt.expectedCount, logs.LogRecordCount())
			require.Equal(t, tt.expectedCacheLen, scraper.cache.len())
			if tt.increment {
				attrs := logs.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes().AsRaw()
				assert.Equal(t, "499", attrs[dbAttributePrefix+queryidColumnName])
				assert.Equal(t, int64(3), attrs[dbAttributePrefix+callsColumnName])
				assert.Equal(t, int64(7), attrs[dbAttributePrefix+rowsColumnName])
				assert.Equal(t, int64(9), attrs[dbAttributePrefix+sharedBlksHitColumnName])
				assert.Equal(t, 2.5, attrs[dbAttributePrefix+totalExecTimeColumnName])
				assert.Equal(t, 0.5, attrs[dbAttributePrefix+totalPlanTimeColumnName])
				assert.Equal(t, int64(0), attrs[dbAttributePrefix+sharedBlksReadColumnName])
			}
		})
	}
	mock.ExpectClose()
}

func TestTopQueryCacheSeparatesDatabaseAndRole(t *testing.T) {
	columns := []string{
		callsColumnName, "datname", sharedBlksDirtiedColumnName, sharedBlksHitColumnName,
		sharedBlksReadColumnName, sharedBlksWrittenColumnName, tempBlksReadColumnName,
		tempBlksWrittenColumnName, "query", queryidColumnName, "userid", "rolname", rowsColumnName,
		totalExecTimeColumnName, totalPlanTimeColumnName,
	}
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true
	cfg.TopQueryCollection.MaxRowsPerQuery = 3
	cfg.TopQueryCollection.TopNQuery = 3
	cfg.TopQueryCollection.MaxExplainEachInterval = 0

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	scraper, err := newTopQueryScraper(receivertest.NewNopSettings(metadata.Type), cfg, mockSimpleClientFactory{db: db})
	require.NoError(t, err)

	identities := []struct {
		database string
		role     string
		userid   string
	}{
		// app is the same role cluster-wide, so it carries the same userid in both
		// databases: database, not userid, is what disambiguates these two rows.
		{database: "db_a", role: "app", userid: "101"},
		{database: "db_b", role: "app", userid: "101"},
		{database: "db_a", role: "reporting", userid: "102"},
	}
	buildRows := func(increments []int) *sqlmock.Rows {
		rows := sqlmock.NewRows(columns)
		for i, identity := range identities {
			values := map[string]driver.Value{
				"datname":         identity.database,
				"query":           "SELECT count(*) FROM pg_class",
				queryidColumnName: "42",
				"userid":          identity.userid,
				"rolname":         identity.role,
			}
			for _, column := range columns {
				if _, exists := values[column]; !exists {
					values[column] = fmt.Sprint(10000 + increments[i])
				}
			}
			row := make([]driver.Value, len(columns))
			for j, column := range columns {
				row[j] = values[column]
			}
			rows.AddRow(row...)
		}
		return rows
	}

	// First scrape seeds every identity's counters without emitting.
	mock.ExpectQuery("LIMIT 3").WillReturnRows(buildRows([]int{0, 0, 0}))
	logs, err := scraper.scrapeTopQuery(t.Context(), cfg.TopQueryCollection.MaxRowsPerQuery, cfg.TopQueryCollection.TopNQuery, cfg.TopQueryCollection.MaxExplainEachInterval, 0)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, 0, logs.LogRecordCount())

	// Second scrape: each identity advances by a different amount, so a shared
	// cache key would produce a wrong delta for at least one of them.
	mock.ExpectQuery("LIMIT 3").WillReturnRows(buildRows([]int{1, 2, 3}))
	logs, err = scraper.scrapeTopQuery(t.Context(), cfg.TopQueryCollection.MaxRowsPerQuery, cfg.TopQueryCollection.TopNQuery, cfg.TopQueryCollection.MaxExplainEachInterval, 0)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, 3, logs.LogRecordCount())
	gotCalls := make(map[[2]string]int64, len(identities))
	for _, lr := range logs.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().All() {
		attrs := lr.Attributes().AsRaw()
		gotCalls[[2]string{attrs["db.namespace"].(string), attrs[dbAttributePrefix+"rolname"].(string)}] = attrs[dbAttributePrefix+callsColumnName].(int64)
	}
	assert.Equal(t, map[[2]string]int64{{"db_a", "app"}: 1, {"db_b", "app"}: 2, {"db_a", "reporting"}: 3}, gotCalls)
	mock.ExpectClose()
}

// TestTopQueryCacheSeparatesDroppedRoles guards the counter-cache key against
// two dropped roles (empty rolname) colliding and sharing one delta.
func TestTopQueryCacheSeparatesDroppedRoles(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true
	cfg.TopQueryCollection.MaxRowsPerQuery = 3
	cfg.TopQueryCollection.TopNQuery = 3
	cfg.TopQueryCollection.MaxExplainEachInterval = 0

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	scraper, err := newTopQueryScraper(receivertest.NewNopSettings(metadata.Type), cfg, mockSimpleClientFactory{db: db})
	require.NoError(t, err)

	buildRows := func(callsA, callsB, totalExecTimeA, totalExecTimeB string) *sqlmock.Rows {
		rows := sqlmock.NewRows(topQueryColumns)
		for _, r := range []struct {
			userid        string
			calls         string
			totalExecTime string
		}{
			{userid: "16415", calls: callsA, totalExecTime: totalExecTimeA},
			{userid: "16416", calls: callsB, totalExecTime: totalExecTimeB},
		} {
			values := map[string]driver.Value{
				"datname":               "postgres",
				"query":                 "SELECT count(*) FROM pg_class",
				queryidColumnName:       "42",
				"userid":                r.userid,
				"rolname":               "",
				callsColumnName:         r.calls,
				totalExecTimeColumnName: r.totalExecTime,
			}
			for _, column := range topQueryColumns {
				if _, exists := values[column]; !exists {
					values[column] = "0"
				}
			}
			row := make([]driver.Value, len(topQueryColumns))
			for i, column := range topQueryColumns {
				row[i] = values[column]
			}
			rows.AddRow(row...)
		}
		return rows
	}

	mock.ExpectQuery("LIMIT 3").WillReturnRows(buildRows("100", "5000", "1000", "2000"))
	_, err = scraper.scrapeTopQuery(t.Context(), cfg.TopQueryCollection.MaxRowsPerQuery, cfg.TopQueryCollection.TopNQuery, cfg.TopQueryCollection.MaxExplainEachInterval, 0)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// Second scrape: each role's cumulative calls advances by 10.
	mock.ExpectQuery("LIMIT 3").WillReturnRows(buildRows("110", "5010", "1100", "2200"))
	logs, err := scraper.scrapeTopQuery(t.Context(), cfg.TopQueryCollection.MaxRowsPerQuery, cfg.TopQueryCollection.TopNQuery, cfg.TopQueryCollection.MaxExplainEachInterval, 0)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	gotCalls := make(map[string]int64, 2)
	for _, rl := range logs.ResourceLogs().All() {
		for _, sl := range rl.ScopeLogs().All() {
			for _, lr := range sl.LogRecords().All() {
				attrs := lr.Attributes().AsRaw()
				userid := attrs[dbAttributePrefix+"userid"].(string)
				gotCalls[userid] = attrs[dbAttributePrefix+callsColumnName].(int64)
			}
		}
	}
	assert.Equal(t, map[string]int64{"16415": 10, "16416": 10}, gotCalls,
		"each dropped role's calls delta must be computed against its own cumulative value")
	mock.ExpectClose()
}

func TestTopQueryPlanCacheSeparatesDatabaseAndRole(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true
	cfg.TopQueryCollection.MaxRowsPerQuery = 3
	cfg.TopQueryCollection.TopNQuery = 3
	cfg.TopQueryCollection.MaxExplainEachInterval = 3

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	factory := &recordingClientFactory{mockSimpleClientFactory: mockSimpleClientFactory{db: db}}
	scraper, err := newTopQueryScraper(receivertest.NewNopSettings(metadata.Type), cfg, factory)
	require.NoError(t, err)

	// Distinct mocked plans let us verify attribution independently of how the
	// database would plan the query under the receiver's connection credentials.
	statements := []struct {
		database string
		role     string
		userid   string
		plan     string
	}{
		// app is the same role cluster-wide, so it carries the same userid in both
		// databases: database, not userid, is what disambiguates these two rows.
		{database: "db_a", role: "app", userid: "101", plan: `[{"Plan":{"Node Type":"Seq Scan"}}]`},
		{database: "db_b", role: "app", userid: "101", plan: `[{"Plan":{"Node Type":"Index Scan"}}]`},
		{database: "db_a", role: "reporting", userid: "102", plan: `[{"Plan":{"Node Type":"Index Only Scan"}}]`},
	}
	expectedPlans := make(map[[2]string]string, len(statements))
	for _, statement := range statements {
		expectedPlans[[2]string{statement.database, statement.role}] = statement.plan
	}

	// Prime the counter cache so the first scrape below is a steady-state interval.
	for _, statement := range statements {
		primeTopQueryCache(scraper, statement.database, statement.userid, "42")
	}

	for scrape, name := range []string{"first scrape", "reordered active statements reuse cached plans"} {
		t.Run(name, func(t *testing.T) {
			factory.requestedDatabases = nil
			order := []int{0, 1, 2}
			if scrape > 0 {
				slices.Reverse(order)
			}
			rows := sqlmock.NewRows(topQueryColumns)
			for _, i := range order {
				statement := statements[i]
				values := map[string]driver.Value{
					"datname":               statement.database,
					"rolname":               statement.role,
					"userid":                statement.userid,
					"query":                 "SELECT count(*) FROM pg_class",
					queryidColumnName:       "42",
					totalExecTimeColumnName: fmt.Sprint((3-i)*10000 + scrape*(i+1)*1000),
				}
				row := make([]driver.Value, len(topQueryColumns))
				for j, column := range topQueryColumns {
					if value, exists := values[column]; exists {
						row[j] = value
					} else {
						row[j] = fmt.Sprint(10000 + scrape*(i+1))
					}
				}
				rows.AddRow(row...)
			}
			mock.ExpectQuery("LIMIT 3").WillReturnRows(rows)

			expectedDatabases := []string{defaultPostgreSQLDatabase}
			if scrape == 0 {
				// Distinct execution times make the first scrape's EXPLAIN order
				// deterministic. The second scrape reverses that priority as well.
				for _, statement := range statements {
					expectedDatabases = append(expectedDatabases, statement.database)
					mock.ExpectQuery(regexp.QuoteMeta("/* otel-collector-ignore */ SET plan_cache_mode = force_generic_plan;PREPARE otel_42 AS SELECT count(*) FROM pg_class;")).
						WillReturnRows(sqlmock.NewRows([]string{"result"}))
					mock.ExpectQuery(regexp.QuoteMeta("/* otel-collector-ignore */ SELECT COALESCE(array_length(parameter_types, 1), 0) AS param_count FROM pg_prepared_statements WHERE name = 'otel_42';")).
						WillReturnRows(sqlmock.NewRows([]string{"param_count"}).AddRow("0"))
					mock.ExpectQuery(regexp.QuoteMeta("EXPLAIN(FORMAT JSON) EXECUTE otel_42;")).
						WillReturnRows(sqlmock.NewRows([]string{"QUERY PLAN"}).AddRow(statement.plan))
					mock.ExpectExec(regexp.QuoteMeta("/* otel-collector-ignore */ DEALLOCATE PREPARE otel_42")).
						WillReturnResult(sqlmock.NewResult(0, 0))
				}
			}

			logs, scrapeErr := scraper.scrapeTopQuery(t.Context(), cfg.TopQueryCollection.MaxRowsPerQuery, cfg.TopQueryCollection.TopNQuery, cfg.TopQueryCollection.MaxExplainEachInterval, 0)
			require.NoError(t, scrapeErr)
			assert.Equal(t, expectedDatabases, factory.requestedDatabases)
			require.Equal(t, len(statements), logs.LogRecordCount())
			assert.Equal(t, len(statements), scraper.queryPlanCache.Len())

			seen := make(map[[2]string]bool, len(statements))
			for _, resourceLogs := range logs.ResourceLogs().All() {
				for _, scopeLogs := range resourceLogs.ScopeLogs().All() {
					for _, record := range scopeLogs.LogRecords().All() {
						attrs := record.Attributes().AsRaw()
						identity := [2]string{attrs["db.namespace"].(string), attrs[dbAttributePrefix+"rolname"].(string)}
						expectedPlan, exists := expectedPlans[identity]
						require.True(t, exists, "unexpected statement identity: %v", identity)
						assert.NotContains(t, seen, identity)
						seen[identity] = true
						assert.Equal(t, "42", attrs[dbAttributePrefix+queryidColumnName])
						assert.JSONEq(t, expectedPlan, attrs[dbAttributePrefix+"query_plan"].(string))
					}
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
	mock.ExpectClose()
}

// TestTopQueryPlanCacheSeparatesDroppedRoles guards the plan cache key against
// two dropped roles (empty rolname) colliding and sharing one cached plan.
func TestTopQueryPlanCacheSeparatesDroppedRoles(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true
	cfg.TopQueryCollection.MaxRowsPerQuery = 3
	cfg.TopQueryCollection.TopNQuery = 3
	cfg.TopQueryCollection.MaxExplainEachInterval = 3

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	scraper, err := newTopQueryScraper(receivertest.NewNopSettings(metadata.Type), cfg, mockSimpleClientFactory{db: db})
	require.NoError(t, err)

	statements := []struct {
		userid string
		plan   string
	}{
		{userid: "16415", plan: `[{"Plan":{"Node Type":"Seq Scan"}}]`},
		{userid: "16416", plan: `[{"Plan":{"Node Type":"Index Scan"}}]`},
	}
	expectedPlans := make(map[string]string, len(statements))
	for _, statement := range statements {
		expectedPlans[statement.userid] = statement.plan
	}

	for _, statement := range statements {
		primeTopQueryCache(scraper, "postgres", statement.userid, "42")
	}

	rows := sqlmock.NewRows(topQueryColumns)
	for i, statement := range statements {
		values := map[string]driver.Value{
			"datname":               "postgres",
			"userid":                statement.userid,
			"rolname":               "", // both roles are dropped: rolname alone cannot tell them apart
			"query":                 "SELECT count(*) FROM pg_class",
			queryidColumnName:       "42",
			totalExecTimeColumnName: fmt.Sprint((2 - i) * 10000),
		}
		row := make([]driver.Value, len(topQueryColumns))
		for j, column := range topQueryColumns {
			if value, exists := values[column]; exists {
				row[j] = value
			} else {
				row[j] = "10000"
			}
		}
		rows.AddRow(row...)
	}
	mock.ExpectQuery("LIMIT 3").WillReturnRows(rows)

	for _, statement := range statements {
		mock.ExpectQuery(regexp.QuoteMeta("/* otel-collector-ignore */ SET plan_cache_mode = force_generic_plan;PREPARE otel_42 AS SELECT count(*) FROM pg_class;")).
			WillReturnRows(sqlmock.NewRows([]string{"result"}))
		mock.ExpectQuery(regexp.QuoteMeta("/* otel-collector-ignore */ SELECT COALESCE(array_length(parameter_types, 1), 0) AS param_count FROM pg_prepared_statements WHERE name = 'otel_42';")).
			WillReturnRows(sqlmock.NewRows([]string{"param_count"}).AddRow("0"))
		mock.ExpectQuery(regexp.QuoteMeta("EXPLAIN(FORMAT JSON) EXECUTE otel_42;")).
			WillReturnRows(sqlmock.NewRows([]string{"QUERY PLAN"}).AddRow(statement.plan))
		mock.ExpectExec(regexp.QuoteMeta("/* otel-collector-ignore */ DEALLOCATE PREPARE otel_42")).
			WillReturnResult(sqlmock.NewResult(0, 0))
	}

	logs, err := scraper.scrapeTopQuery(t.Context(), cfg.TopQueryCollection.MaxRowsPerQuery, cfg.TopQueryCollection.TopNQuery, cfg.TopQueryCollection.MaxExplainEachInterval, 0)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, len(statements), logs.LogRecordCount())
	assert.Equal(t, len(statements), scraper.queryPlanCache.Len(),
		"each dropped role must get its own plan cache entry, not share one keyed on empty rolname")

	seen := make(map[string]bool, len(statements))
	for _, resourceLogs := range logs.ResourceLogs().All() {
		for _, scopeLogs := range resourceLogs.ScopeLogs().All() {
			for _, record := range scopeLogs.LogRecords().All() {
				attrs := record.Attributes().AsRaw()
				userid := attrs[dbAttributePrefix+"userid"].(string)
				expectedPlan, exists := expectedPlans[userid]
				require.True(t, exists, "unexpected userid: %v", userid)
				assert.NotContains(t, seen, userid)
				seen[userid] = true
				assert.JSONEq(t, expectedPlan, attrs[dbAttributePrefix+"query_plan"].(string))
			}
		}
	}
	mock.ExpectClose()
}

// TestTopQueryDeltaColdStartAndReset guards the top_query delta semantics:
// a cold cache must not emit lifetime totals (restart spike), and a counter
// that decreases after pg_stat_statements_reset() must be treated as a reset
// rather than reporting 0 until it climbs past the stale cached value (blackout).
func TestTopQueryDeltaColdStartAndReset(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled = true
	cfg.TopQueryCollection.MaxRowsPerQuery = 3
	cfg.TopQueryCollection.TopNQuery = 3
	cfg.TopQueryCollection.MaxExplainEachInterval = 0

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	newScraper := func() *postgreSQLScraper {
		s, scraperErr := newTopQueryScraper(receivertest.NewNopSettings(metadata.Type), cfg, mockSimpleClientFactory{db: db})
		require.NoError(t, scraperErr)
		return s
	}

	buildRows := func(calls, totalExecTime string) *sqlmock.Rows {
		values := map[string]driver.Value{
			"datname":               "postgres",
			"query":                 "SELECT count(*) FROM pg_class",
			queryidColumnName:       "42",
			"userid":                "10",
			"rolname":               "otel",
			callsColumnName:         calls,
			totalExecTimeColumnName: totalExecTime,
		}
		row := make([]driver.Value, len(topQueryColumns))
		for i, column := range topQueryColumns {
			if value, exists := values[column]; exists {
				row[i] = value
			} else {
				row[i] = "0"
			}
		}
		return sqlmock.NewRows(topQueryColumns).AddRow(row...)
	}

	scrape := func(s *postgreSQLScraper, calls, totalExecTime string) []int64 {
		mock.ExpectQuery("LIMIT 3").WillReturnRows(buildRows(calls, totalExecTime))
		logs, scrapeErr := s.scrapeTopQuery(t.Context(), cfg.TopQueryCollection.MaxRowsPerQuery, cfg.TopQueryCollection.TopNQuery, cfg.TopQueryCollection.MaxExplainEachInterval, 0)
		require.NoError(t, scrapeErr)
		require.NoError(t, mock.ExpectationsWereMet())
		var got []int64
		for _, rl := range logs.ResourceLogs().All() {
			for _, sl := range rl.ScopeLogs().All() {
				for _, lr := range sl.LogRecords().All() {
					got = append(got, lr.Attributes().AsRaw()[dbAttributePrefix+callsColumnName].(int64))
				}
			}
		}
		return got
	}

	scraper := newScraper()
	assert.Empty(t, scrape(scraper, "1000", "1000"), "cold cache must seed, not emit the lifetime total")
	assert.Equal(t, []int64{10}, scrape(scraper, "1010", "1100"))
	// pg_stat_statements_reset(): counters drop below the cached values.
	assert.Equal(t, []int64{5}, scrape(scraper, "5", "50"), "a decreased counter must be reported as a reset, not 0")
	assert.Equal(t, []int64{4}, scrape(scraper, "9", "90"), "the cache must be re-seeded after a reset (no blackout)")

	// A collector restart (new scraper, empty cache) must not emit the lifetime total.
	restarted := newScraper()
	assert.Empty(t, scrape(restarted, "5000", "9000"), "restart must not spike with lifetime totals")
	mock.ExpectClose()
}
