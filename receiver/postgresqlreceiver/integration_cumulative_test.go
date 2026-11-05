// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package postgresqlreceiver

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegrationTopQueryCumulative runs top_query_collection.temporality =
// cumulative against a real pg_stat_statements: raw counters from the first
// scrape, toplevel emitted per entry, and the start time from stats_since
// (pg_stat_statements 1.11), stats_reset (1.10) or none (PostgreSQL 13).
func TestIntegrationTopQueryCumulative(t *testing.T) {
	t.Run("pg17_pgss_latest", func(t *testing.T) { testTopQueryCumulative(t, post17TestVersion, "") })
	t.Run("pg17_pgss_1_10", func(t *testing.T) { testTopQueryCumulative(t, post17TestVersion, "1.10") })
	t.Run("pg13", func(t *testing.T) { testTopQueryCumulative(t, pre17TestVersion, "") })
}

type cumulativeTarget struct {
	calls     int64
	toplevel  any // bool, or nil when not emitted
	startTime string
	marker    any
}

func testTopQueryCumulative(t *testing.T, pgVersion, extVersion string) {
	pg13 := pgVersion == pre17TestVersion
	var extraArgs []string
	if !pg13 {
		extraArgs = []string{"-c", "pg_stat_statements.track=all"}
	}
	h := startTopQueryHarness(t, pgVersion, extraArgs...)
	h.cfg.TopQueryCollection.Temporality = TopQueryTemporalityCumulative
	connectDB, err := sql.Open("postgres", fmt.Sprintf("postgres://root:otel@localhost:%s/postgres?sslmode=disable", h.port))
	require.NoError(t, err)
	t.Cleanup(func() { connectDB.Close() })
	if extVersion != "" {
		_, err = connectDB.ExecContext(t.Context(), "DROP EXTENSION pg_stat_statements")
		require.NoError(t, err)
		_, err = connectDB.ExecContext(t.Context(), fmt.Sprintf("CREATE EXTENSION pg_stat_statements VERSION '%s'", extVersion))
		require.NoError(t, err)
	}
	_, err = h.workload.ExecContext(t.Context(), `CREATE FUNCTION run_target() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM * FROM test2 WHERE id = 67;
END $$`)
	require.NoError(t, err)
	runNested := func(n int) {
		t.Helper()
		for range n {
			_, execErr := h.workload.ExecContext(t.Context(), "SELECT run_target()")
			require.NoError(t, execErr)
		}
	}
	scraper := h.newScraper()
	targets := func() []cumulativeTarget {
		t.Helper()
		logs, scrapeErr := scraper.scrapeTopQuery(t.Context(), h.cfg.TopQueryCollection.MaxRowsPerQuery, h.cfg.TopQueryCollection.TopNQuery, 0, 0)
		require.NoError(t, scrapeErr)
		var out []cumulativeTarget
		for _, lr := range recordsByEvent(logs, "db.server.top_query") {
			a := lr.Attributes().AsRaw()
			query, _ := a["db.query.text"].(string)
			if a["db.namespace"] != "otel2" || !strings.HasPrefix(strings.ToLower(query), "select * from test2 where") {
				continue
			}
			start, _ := a["postgresql.start_time"].(string)
			out = append(out, cumulativeTarget{
				calls: a["postgresql.calls"].(int64), toplevel: a["postgresql.toplevel"],
				startTime: start, marker: a["postgresql.aggregation_temporality"],
			})
		}
		return out
	}
	byToplevel := func(ts []cumulativeTarget) map[any]cumulativeTarget {
		m := map[any]cumulativeTarget{}
		for _, x := range ts {
			m[x.toplevel] = x
		}
		return m
	}

	h.runWorkload(100)
	runNested(40)
	first := targets()
	for _, x := range first {
		assert.Equal(t, "cumulative", x.marker)
	}
	if pg13 {
		require.Len(t, first, 1, "PostgreSQL 13: one record per statement")
		// track=top (default) on 13: only the top-level calls are counted.
		assert.Equal(t, int64(100), first[0].calls, "raw cumulative on the first scrape")
		assert.Nil(t, first[0].toplevel)
		assert.Empty(t, first[0].startTime, "no stats_since or stats_reset before PostgreSQL 14")
		return
	}
	require.Len(t, first, 2, "top-level and nested entries are separate records")
	got := byToplevel(first)
	assert.Equal(t, int64(100), got[true].calls)
	assert.Equal(t, int64(40), got[false].calls)
	start1, err := time.Parse(time.RFC3339Nano, got[true].startTime)
	require.NoError(t, err, "start_time must be set on PostgreSQL 14+")
	assert.WithinDuration(t, time.Now(), start1, 10*time.Minute)

	h.runWorkload(10)
	got = byToplevel(targets())
	assert.Equal(t, int64(110), got[true].calls, "cumulative, not the delta")
	assert.Equal(t, int64(40), got[false].calls, "unchanged entries are still emitted")
	assert.Equal(t, got[true].startTime, start1.Format(time.RFC3339Nano), "start_time is stable without a reset")

	time.Sleep(10 * time.Millisecond)
	_, err = connectDB.ExecContext(t.Context(), "SELECT pg_stat_statements_reset()")
	require.NoError(t, err)
	h.runWorkload(5)
	got = byToplevel(targets())
	assert.Equal(t, int64(5), got[true].calls)
	start2, err := time.Parse(time.RFC3339Nano, got[true].startTime)
	require.NoError(t, err)
	assert.True(t, start2.After(start1), "a reset moves start_time forward (%s -> %s)", start1, start2)
}
