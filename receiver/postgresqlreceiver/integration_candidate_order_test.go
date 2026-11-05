// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package postgresqlreceiver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegrationTopQueryCandidateOrder: frequent fast statements fill
// max_rows_per_query when candidates are ordered by calls, so a rare slow
// statement is never fetched; ordered by total_time it is.
func TestIntegrationTopQueryCandidateOrder(t *testing.T) {
	// PostgreSQL 13 uses the GROUP BY template (ordering by the SUM alias);
	// PostgreSQL 17 the per-entry template with toplevel.
	for _, v := range []string{pre17TestVersion, post17TestVersion} {
		t.Run(v+"/calls", func(t *testing.T) { testTopQueryCandidateOrder(t, v, TopQueryCandidateOrderCalls, false) })
		t.Run(v+"/total_time", func(t *testing.T) { testTopQueryCandidateOrder(t, v, TopQueryCandidateOrderTotalTime, true) })
	}
}

func testTopQueryCandidateOrder(t *testing.T, pgVersion string, order TopQueryCandidateOrder, wantSlow bool) {
	h := startTopQueryHarness(t, pgVersion)
	h.cfg.TopQueryCollection.CandidateOrder = order
	h.cfg.TopQueryCollection.MaxRowsPerQuery = 3
	h.cfg.TopQueryCollection.TopNQuery = 3
	exec := func(query string, n int) {
		t.Helper()
		for range n {
			_, err := h.workload.ExecContext(t.Context(), query)
			require.NoError(t, err)
		}
	}
	workload := func() {
		for _, q := range []string{
			"SELECT count(*) FROM test2",
			"SELECT max(id) FROM test2",
			"SELECT min(id) FROM test2",
			"SELECT id FROM test2 WHERE id = 67",
		} {
			exec(q, 50)
		}
		exec("SELECT pg_sleep(0.3)", 1)
	}
	scraper := h.newScraper()
	sawSlow := func() bool {
		t.Helper()
		logs, err := scraper.scrapeTopQuery(t.Context(), 3, 3, 0, 0)
		require.NoError(t, err)
		for _, lr := range recordsByEvent(logs, "db.server.top_query") {
			if q, _ := lr.Attributes().AsRaw()["db.query.text"].(string); strings.Contains(strings.ToLower(q), "pg_sleep") {
				return true
			}
		}
		return false
	}
	workload()
	sawSlow() // seeds the delta cache
	workload()
	assert.Equal(t, wantSlow, sawSlow(), "candidate_order=%s", order)
}
