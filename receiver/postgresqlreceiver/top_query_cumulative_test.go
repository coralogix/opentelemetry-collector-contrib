// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"database/sql/driver"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/receiver"
)

const (
	attrAggregationTemporality = dbAttributePrefix + "aggregation_temporality"
	attrStartTime              = dbAttributePrefix + "start_time"
	attrToplevel               = dbAttributePrefix + "toplevel"
	// With pg_stat_statements 1.11+ cumulative mode also selects stats_since.
	topQueryStatsSinceRowsPattern = `toplevel::TEXT,[\s\S]*stats_since[\s\S]*LIMIT 3`
)

func cumulative(cfg *Config, _ *receiver.Settings) {
	cfg.TopQueryCollection.Temporality = TopQueryTemporalityCumulative
}

// cumulativeRow is one pg_stat_statements row for the cumulative tests.
type cumulativeRow struct {
	queryID  string
	value    string
	toplevel string // "" = column not selected
	since    string // stats_since as epoch microseconds; "" = column not selected
	query    string // "" = topQueryRow's default
}

func (s *topQueryTestScraper) expectCumulativeRows(pattern string, rows ...cumulativeRow) {
	columns := append([]string{}, topQueryColumns...)
	if len(rows) > 0 && rows[0].toplevel != "" {
		columns = append(columns, "toplevel")
	}
	if len(rows) > 0 && rows[0].since != "" {
		columns = append(columns, "stats_since")
	}
	result := sqlmock.NewRows(columns)
	for _, r := range rows {
		row := topQueryRow(r.queryID, r.value)
		if r.query != "" {
			for i, c := range topQueryColumns {
				if c == "query" {
					row[i] = r.query
				}
			}
		}
		if r.toplevel != "" {
			row = append(row, driver.Value(r.toplevel))
		}
		if r.since != "" {
			row = append(row, driver.Value(r.since))
		}
		result.AddRow(row...)
	}
	s.mock.ExpectQuery(pattern).WillReturnRows(result)
}

func topQueryRecords(logs plog.Logs) []map[string]any {
	var out []map[string]any
	for _, lr := range recordsByEvent(logs, "db.server.top_query") {
		out = append(out, lr.Attributes().AsRaw())
	}
	return out
}

func TestTopQueryTemporalityConfig(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	assert.Equal(t, TopQueryTemporalityDelta, cfg.TopQueryCollection.Temporality, "delta is the default (upstream behavior)")

	cm := confmap.NewFromStringMap(map[string]any{
		"username": "otel",
		"password": "otel",
		"top_query_collection": map[string]any{
			"temporality": "cumulative",
		},
	})
	require.NoError(t, cm.Unmarshal(cfg))
	assert.Equal(t, TopQueryTemporalityCumulative, cfg.TopQueryCollection.Temporality)
	require.NoError(t, confmap.Validate(cfg))

	cfg.TopQueryCollection.Temporality = "monthly"
	err := confmap.Validate(cfg)
	require.Error(t, err)
	assert.ErrorContains(t, err, "top_query_collection.temporality")
}

// TestTopQueryDeltaModeUnchanged: with the flag off (default) the first scrape
// still seeds and nothing of the cumulative mode leaks into the records.
func TestTopQueryDeltaModeUnchanged(t *testing.T) {
	s := newTopQueryTestScraper(t)
	s.expectCapabilities(170002, "1.11")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"})
	assert.Empty(t, topQueryRecords(s.scrape(t, 0)), "delta mode seeds on the first scrape")

	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1010", "true"})
	records := topQueryRecords(s.scrape(t, 0))
	require.Len(t, records, 1)
	assert.Equal(t, int64(10), records[0][dbAttributePrefix+callsColumnName])
	for _, key := range []string{attrAggregationTemporality, attrStartTime, attrToplevel} {
		assert.NotContains(t, records[0], key)
	}
}

func TestTopQueryCumulativeEmitsRawCountersFromFirstScrape(t *testing.T) {
	s := newTopQueryTestScraper(t, cumulative)
	s.expectCapabilities(170002, "1.10")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"})
	records := topQueryRecords(s.scrape(t, 0))
	require.Len(t, records, 1, "cumulative mode does not seed: the ingester does")
	assert.Equal(t, int64(1000), records[0][dbAttributePrefix+callsColumnName])
	assert.Equal(t, int64(1000), records[0][dbAttributePrefix+rowsColumnName])
	assert.InDelta(t, 1.0, records[0][dbAttributePrefix+totalExecTimeColumnName], 1e-9, "1000 ms")
	assert.Equal(t, "cumulative", records[0][attrAggregationTemporality])

	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1010", "true"})
	records = topQueryRecords(s.scrape(t, 0))
	require.Len(t, records, 1)
	assert.Equal(t, int64(1010), records[0][dbAttributePrefix+callsColumnName], "cumulative, not the delta 10")

	// A reset is visible downstream as a decrease (and a new start_time).
	s.expectInfo(0, t2)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "7", "true"})
	records = topQueryRecords(s.scrape(t, 0))
	require.Len(t, records, 1)
	assert.Equal(t, int64(7), records[0][dbAttributePrefix+callsColumnName])
	assert.Equal(t, t2.Format(time.RFC3339Nano), records[0][attrStartTime])
}

// TestTopQueryCumulativeEmitsAllCandidates: S2 decision 3, every fetched
// candidate is emitted (the ingester does the top-K cut); records are still
// ordered by delta execution time and plans stay limited to top_n_query.
func TestTopQueryCumulativeEmitsAllCandidates(t *testing.T) {
	s := newTopQueryTestScraper(t, cumulative)
	for _, id := range []string{"1", "2", "3"} {
		s.queryPlanCache.Add("postgres\x0010\x00"+id, "plan-"+id)
	}
	s.expectCapabilities(170002, "1.10")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern,
		[]string{"1", "100", "true"}, []string{"2", "200", "true"}, []string{"3", "300", "true"})
	logs, err := s.scrapeTopQuery(t.Context(), 3, 1, 0, 0)
	require.NoError(t, err)
	require.Len(t, topQueryRecords(logs), 3, "first scrape: every candidate, though all deltas are 0")

	// Only statement 2 changed: it ranks first and is the only one with a plan.
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern,
		[]string{"1", "100", "true"}, []string{"2", "250", "true"}, []string{"3", "300", "true"})
	logs, err = s.scrapeTopQuery(t.Context(), 3, 1, 0, 0)
	require.NoError(t, err)
	require.NoError(t, s.mock.ExpectationsWereMet())
	records := topQueryRecords(logs)
	require.Len(t, records, 3, "unchanged candidates are emitted too")
	assert.Equal(t, "2", records[0][dbAttributePrefix+queryidColumnName])
	assert.Equal(t, "plan-2", records[0][dbAttributePrefix+"query_plan"])
	for _, r := range records[1:] {
		assert.Empty(t, r[dbAttributePrefix+"query_plan"], "plans only for top_n_query records")
	}
}

func TestTopQueryCumulativeEmitsToplevelUnmerged(t *testing.T) {
	s := newTopQueryTestScraper(t, cumulative)
	s.expectCapabilities(170002, "1.10")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"}, []string{"42", "50", "false"})
	records := topQueryRecords(s.scrape(t, 0))
	require.Len(t, records, 2, "top-level and nested entries are separate records")
	got := map[bool]int64{}
	for _, r := range records {
		got[r[attrToplevel].(bool)] = r[dbAttributePrefix+callsColumnName].(int64)
	}
	assert.Equal(t, map[bool]int64{true: 1000, false: 50}, got)

	// Without toplevel support (PG < 14) the SQL sums the entries: no attribute.
	s2 := newTopQueryTestScraper(t, cumulative)
	s2.expectCapabilities(130018, "1.8")
	s2.expectRows(topQueryRowsPattern, []string{"42", "1050"})
	records = topQueryRecords(s2.scrape(t, 0))
	require.Len(t, records, 1)
	assert.NotContains(t, records[0], attrToplevel)
	assert.NotContains(t, records[0], attrStartTime, "PG 13 has neither stats_since nor stats_reset")
	assert.Equal(t, "cumulative", records[0][attrAggregationTemporality], "the marker tells the ingester these are cumulatives")
	assert.Equal(t, int64(1050), records[0][dbAttributePrefix+callsColumnName])
}

func TestTopQueryCumulativeStartTime(t *testing.T) {
	t.Run("stats_since per entry on pg_stat_statements 1.11", func(t *testing.T) {
		s := newTopQueryTestScraper(t, cumulative)
		since := time.Date(2026, 11, 3, 8, 30, 15, 123456000, time.UTC)
		s.expectCapabilities(170002, "1.11")
		s.expectInfo(0, t1)
		s.expectCumulativeRows(topQueryStatsSinceRowsPattern,
			cumulativeRow{queryID: "42", value: "1000", toplevel: "true", since: strconv.FormatInt(since.UnixMicro(), 10)})
		records := topQueryRecords(s.scrape(t, 0))
		require.Len(t, records, 1)
		assert.Equal(t, since.Format(time.RFC3339Nano), records[0][attrStartTime], "per-entry stats_since beats the global stats_reset")
	})
	t.Run("stats_reset on pg_stat_statements 1.9 and 1.10", func(t *testing.T) {
		s := newTopQueryTestScraper(t, cumulative)
		s.expectCapabilities(160004, "1.10")
		s.expectInfo(0, t1)
		s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"})
		records := topQueryRecords(s.scrape(t, 0))
		require.Len(t, records, 1)
		assert.Equal(t, t1.Format(time.RFC3339Nano), records[0][attrStartTime])
	})
	t.Run("delta mode does not select stats_since", func(t *testing.T) {
		s := newTopQueryTestScraper(t)
		s.expectCapabilities(170002, "1.11")
		s.expectInfo(0, t1)
		s.mock.ExpectQuery(`toplevel::TEXT\s+FROM`).WillReturnRows(sqlmock.NewRows(topQueryColumns))
		s.scrape(t, 0)
	})
}

// TestTopQueryCumulativeDuplicateKeyEmitsOnce: two rows with one cache key in
// one scrape must not give the ingester two cumulatives for one key.
func TestTopQueryCumulativeDuplicateKeyEmitsOnce(t *testing.T) {
	s := newTopQueryTestScraper(t, cumulative)
	s.expectCapabilities(170002, "1.10")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"}, []string{"42", "3", "true"})
	records := topQueryRecords(s.scrape(t, 0))
	require.Len(t, records, 1)
	assert.Equal(t, int64(1000), records[0][dbAttributePrefix+callsColumnName])
}

// TestTopQueryCumulativeKeepsQueryTextObfuscated: the cumulative path emits
// the same obfuscated text as delta mode and never the raw statement (raw
// text is opt-in P5 / EC-04, behind the credential rule of BR-1). The PG BR-1
// credential forms are covered too: pg_stat_statements keeps utility
// statements with their literals.
func TestTopQueryCumulativeKeepsQueryTextObfuscated(t *testing.T) {
	const secret = "synthetic-literal-1"
	cases := map[string]string{
		"select literal":            "SELECT * FROM users WHERE token = '" + secret + "'",
		"alter role cleartext":      "ALTER ROLE app_user PASSWORD '" + secret + "'",
		"alter role md5":            "ALTER ROLE app_user PASSWORD 'md5" + secret + "'",
		"alter role scram":          "ALTER ROLE app_user PASSWORD 'SCRAM-SHA-256$4096:c2FsdA==$" + secret + ":c2VydmVy'",
		"create user with password": "CREATE USER app_user WITH PASSWORD '" + secret + "'",
		"user mapping":              "CREATE USER MAPPING FOR app_user SERVER remote OPTIONS (user 'app', password '" + secret + "')",
		"subscription connection":   "CREATE SUBSCRIPTION sub CONNECTION 'host=db.example dbname=app user=rep password=" + secret + "' PUBLICATION pub",
		"dollar quoted":             "ALTER ROLE app_user PASSWORD $$" + secret + "$$",
		"dollar quoted tag":         "ALTER ROLE app_user PASSWORD $pw$" + secret + "$pw$",
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			s := newTopQueryTestScraper(t, cumulative)
			s.expectCapabilities(170002, "1.10")
			s.expectInfo(0, t1)
			s.expectCumulativeRows(topQueryToplevelRowsPattern,
				cumulativeRow{queryID: "42", value: "1000", toplevel: "true", query: query})
			records := topQueryRecords(s.scrape(t, 0))
			require.Len(t, records, 1)
			assert.NotContains(t, records[0], dbAttributePrefix+"raw_query")
			for key, v := range records[0] {
				if str, ok := v.(string); ok {
					assert.NotContains(t, str, secret, "literal leaked in %s", key)
				}
			}
		})
	}
}

// TestTopQueryCumulativeMarkerSurvivesExtrasMismatch: if the per-record
// attributes cannot be paired with the emitted records, every record still
// carries the cumulative marker (raw counters are never emitted unmarked);
// only start_time and toplevel are skipped.
func TestTopQueryCumulativeMarkerSurvivesExtrasMismatch(t *testing.T) {
	s := newTopQueryTestScraper(t, cumulative)
	s.expectCapabilities(170002, "1.10")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"1", "100", "true"}, []string{"2", "200", "true"})
	// A stale entry forces a records/extras count mismatch.
	s.cumulativeExtras = []cumulativeExtra{{startTime: "stale", toplevel: true, hasToplevel: true}}
	records := topQueryRecords(s.scrape(t, 0))
	require.Len(t, records, 2)
	for _, r := range records {
		assert.Equal(t, "cumulative", r[attrAggregationTemporality])
		assert.NotContains(t, r, attrStartTime)
		assert.NotContains(t, r, attrToplevel)
	}
	assert.Equal(t, 1, s.observed.FilterMessageSnippet("do not match the emitted records").Len())
}

// TestTopQueryCumulativeReprobesStatsSince: with toplevel cached as supported
// but stats_since not (pg_stat_statements < 1.11), cumulative mode re-probes
// at most every 10 minutes, so ALTER EXTENSION ... UPDATE is picked up
// without a collector restart.
func TestTopQueryCumulativeReprobesStatsSince(t *testing.T) {
	s := newTopQueryTestScraper(t, cumulative)
	s.expectCapabilities(170002, "1.10")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"})
	s.scrape(t, 0)
	assert.False(t, s.statsSinceSupported)

	s.advance(time.Minute)
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1010", "true"})
	s.scrape(t, 0) // no probe within 10 minutes (sqlmock rejects an unexpected one)

	s.advance(10 * time.Minute)
	since := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	s.expectCapabilities(170002, "1.11")
	s.expectInfo(0, t1)
	s.expectCumulativeRows(topQueryStatsSinceRowsPattern,
		cumulativeRow{queryID: "42", value: "1020", toplevel: "true", since: strconv.FormatInt(since.UnixMicro(), 10)})
	records := topQueryRecords(s.scrape(t, 0))
	require.Len(t, records, 1)
	assert.True(t, s.statsSinceSupported)
	assert.True(t, s.toplevelSupported)
	assert.Equal(t, since.Format(time.RFC3339Nano), records[0][attrStartTime])
	assert.Zero(t, s.probeFailures())
}

// TestTopQueryDeltaModeDoesNotReprobeStatsSince: stats_since is only used in
// cumulative mode, so delta mode keeps the cached positive result.
func TestTopQueryDeltaModeDoesNotReprobeStatsSince(t *testing.T) {
	s := newTopQueryTestScraper(t)
	s.expectCapabilities(170002, "1.10")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"})
	s.scrape(t, 0)
	s.advance(11 * time.Minute)
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1010", "true"})
	s.scrape(t, 0)
	assert.Zero(t, s.probeFailures())
}

func TestTopQueryTemporalityEmptyIsDelta(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.Username, cfg.Password = "otel", "otel"
	cfg.TopQueryCollection.Temporality = ""
	require.NoError(t, confmap.Validate(cfg), "an unset temporality means delta")
}

// TestTopQueryCumulativeFailedReprobeKeepsToplevel: a failed stats_since
// re-probe keeps the cached toplevel result; start_time still comes from
// stats_reset.
func TestTopQueryCumulativeFailedReprobeKeepsToplevel(t *testing.T) {
	s := newTopQueryTestScraper(t, cumulative)
	s.expectCapabilities(170002, "1.10")
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1000", "true"})
	s.scrape(t, 0)
	require.True(t, s.toplevelSupported)

	s.advance(10 * time.Minute)
	s.mock.ExpectQuery(statStatementsCapsPattern).WillReturnError(errors.New("canceling statement due to statement timeout"))
	s.expectInfo(0, t1)
	s.expectRows(topQueryToplevelRowsPattern, []string{"42", "1010", "true"})
	records := topQueryRecords(s.scrape(t, 0))
	require.Len(t, records, 1)
	assert.True(t, s.toplevelSupported, "a failed re-probe keeps the cached toplevel result")
	assert.False(t, s.statsSinceSupported)
	assert.Equal(t, true, records[0][attrToplevel])
	assert.Equal(t, t1.Format(time.RFC3339Nano), records[0][attrStartTime], "start_time still comes from stats_reset")
	assert.Equal(t, 1, s.probeFailures())
}
