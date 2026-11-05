// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCumulativeDelta(t *testing.T) {
	tests := []struct {
		name      string
		cached    float64
		seen      bool
		current   float64
		wantDelta float64
		wantCache float64
	}{
		{name: "cold cache seeds and emits zero (no restart spike)", cached: 0, seen: false, current: 987654, wantDelta: 0, wantCache: 987654},
		{name: "normal increase emits difference", cached: 100, seen: true, current: 130, wantDelta: 30, wantCache: 130},
		{name: "no change emits zero", cached: 100, seen: true, current: 100, wantDelta: 0, wantCache: 100},
		{name: "reset (counter decreased) emits current and re-seeds (no blackout)", cached: 5000, seen: true, current: 12, wantDelta: 12, wantCache: 12},
		{name: "reset to zero emits zero and re-seeds", cached: 5000, seen: true, current: 0, wantDelta: 0, wantCache: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDelta, gotCache := cumulativeDelta(tt.cached, tt.seen, tt.current)
			assert.Equal(t, tt.wantDelta, gotDelta)
			assert.Equal(t, tt.wantCache, gotCache)
		})
	}
}

func TestCumulativeDeltaRecoversAfterReset(t *testing.T) {
	cache, seen := 0.0, false
	var emitted []float64
	for _, cur := range []float64{1000, 1010, 5, 9, 20} { // reset between 1010 and 5
		d, c := cumulativeDelta(cache, seen, cur)
		emitted = append(emitted, d)
		cache, seen = c, true
	}
	assert.Equal(t, []float64{0, 10, 5, 4, 11}, emitted)
}

func TestStatementDelta(t *testing.T) {
	tests := []struct {
		name        string
		prev        statementCounters
		seen        bool
		statsReset  bool
		current     statementCounters
		wantDeltas  statementCounters
		wantOutcome deltaOutcome
	}{
		{
			name:        "unseen statement is seeded as a whole",
			current:     statementCounters{"calls": 1000, "rows": 50},
			wantDeltas:  statementCounters{"calls": 0, "rows": 0},
			wantOutcome: deltaSeeded,
		},
		{
			name:        "steady interval reports per-column differences",
			prev:        statementCounters{"calls": 1000, "rows": 50},
			seen:        true,
			current:     statementCounters{"calls": 1010, "rows": 55},
			wantDeltas:  statementCounters{"calls": 10, "rows": 5},
			wantOutcome: deltaSteady,
		},
		{
			name:        "any decreased column resets the whole statement",
			prev:        statementCounters{"calls": 1000, "rows": 50},
			seen:        true,
			current:     statementCounters{"calls": 1001, "rows": 2},
			wantDeltas:  statementCounters{"calls": 1001, "rows": 2},
			wantOutcome: deltaReset,
		},
		{
			name:        "cached entry missing a column re-seeds the whole statement (no partial deltas)",
			prev:        statementCounters{"calls": 1000},
			seen:        true,
			current:     statementCounters{"calls": 1010, "rows": 55},
			wantDeltas:  statementCounters{"calls": 0, "rows": 0},
			wantOutcome: deltaSeeded,
		},
		{
			name:        "stats_reset reports current values even when they grew past the stale cache",
			prev:        statementCounters{"calls": 1000, "rows": 50},
			seen:        true,
			statsReset:  true,
			current:     statementCounters{"calls": 2000, "rows": 60},
			wantDeltas:  statementCounters{"calls": 2000, "rows": 60},
			wantOutcome: deltaReset,
		},
		{
			name:        "stats_reset reports current values for statements not in the cache",
			statsReset:  true,
			current:     statementCounters{"calls": 7, "rows": 1},
			wantDeltas:  statementCounters{"calls": 7, "rows": 1},
			wantOutcome: deltaReset,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDeltas, gotOutcome := statementDelta(tt.prev, tt.seen, tt.statsReset, tt.current)
			assert.Equal(t, tt.wantDeltas, gotDeltas)
			assert.Equal(t, tt.wantOutcome, gotOutcome)
		})
	}
}
