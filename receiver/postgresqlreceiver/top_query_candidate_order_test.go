// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"bytes"
	"testing"
	"text/template"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/receiver"
)

func TestTopQueryCandidateOrderConfig(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	assert.Equal(t, TopQueryCandidateOrderCalls, cfg.TopQueryCollection.CandidateOrder, "calls is the default (upstream behavior)")

	cm := confmap.NewFromStringMap(map[string]any{
		"username": "otel",
		"password": "otel",
		"top_query_collection": map[string]any{
			"candidate_order": "total_time",
		},
	})
	require.NoError(t, cm.Unmarshal(cfg))
	assert.Equal(t, TopQueryCandidateOrderTotalTime, cfg.TopQueryCollection.CandidateOrder)
	require.NoError(t, confmap.Validate(cfg))

	// Only the whitelisted values reach the SQL; anything else is rejected.
	cfg.TopQueryCollection.CandidateOrder = "random()"
	err := confmap.Validate(cfg)
	require.Error(t, err)
	assert.ErrorContains(t, err, "top_query_collection.candidate_order")
}

func TestTopQueryCandidateOrderSQL(t *testing.T) {
	tmpl := template.Must(template.New("topQuery").Option("missingkey=error").Parse(topQueryTemplate))
	for _, tc := range []struct {
		name     string
		order    TopQueryCandidateOrder
		toplevel bool
		want     string
	}{
		{name: "default", order: TopQueryCandidateOrderCalls, want: "ORDER BY calls DESC"},
		{name: "default toplevel", order: TopQueryCandidateOrderCalls, toplevel: true, want: "ORDER BY calls DESC"},
		{name: "total_time", order: TopQueryCandidateOrderTotalTime, want: "ORDER BY total_exec_time DESC"},
		{name: "total_time toplevel", order: TopQueryCandidateOrderTotalTime, toplevel: true, want: "ORDER BY total_exec_time DESC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := bytes.Buffer{}
			require.NoError(t, tmpl.Execute(&buf, map[string]any{
				"limit":             int64(10),
				"excludedDatabases": "",
				"toplevel":          tc.toplevel,
				"statsSince":        false,
				"orderBy":           tc.order.column(),
			}))
			assert.Contains(t, buf.String(), tc.want+"\nLIMIT 10;")
		})
	}
}

// TestTopQueryCandidateOrderReachesTheQuery: the scraper's row query uses the
// configured order.
func TestTopQueryCandidateOrderReachesTheQuery(t *testing.T) {
	s := newTopQueryTestScraper(t, func(cfg *Config, _ *receiver.Settings) {
		cfg.TopQueryCollection.CandidateOrder = TopQueryCandidateOrderTotalTime
	})
	s.expectCapabilities(170002, "1.11")
	s.expectInfo(0, t1)
	s.mock.ExpectQuery(`toplevel::TEXT[\s\S]*ORDER BY total_exec_time DESC\s+LIMIT 3`).WillReturnRows(sqlmock.NewRows(topQueryColumns))
	s.scrape(t, 0)
}
