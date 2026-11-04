// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver"

import "maps"

// cumulativeDelta converts a cumulative pg_stat_statements counter into a per-interval delta.
// It returns the delta to report and the value to store in the cache. The cache must always be
// updated with the returned value.
//   - unseen key (cold cache after start, reload or eviction): seed and report 0, never the lifetime total.
//   - counter decreased (pg_stat_statements_reset, server restart): treat as a reset and report the new value.
//   - otherwise: report current - cached.
func cumulativeDelta(cached float64, seen bool, current float64) (delta, cacheValue float64) {
	if !seen {
		return 0, current
	}
	if current < cached {
		return current, current
	}
	return current - cached, current
}

// statementCounters holds the cumulative pg_stat_statements counters of one
// statement, keyed by column name. The delta cache stores one entry per
// statement so that all of its columns are seeded, evicted and reset together.
type statementCounters map[string]float64

// deltaOutcome tells how statementDelta treated a statement.
type deltaOutcome int

const (
	// deltaSteady: the statement was cached and no counter decreased.
	deltaSteady deltaOutcome = iota
	// deltaSeeded: the statement was not cached (or its entry was incomplete);
	// every column reports 0 for this interval.
	deltaSeeded
	// deltaReset: the counters were reset (pg_stat_statements_info.stats_reset
	// changed, or a counter decreased); every column reports its current value.
	deltaReset
)

// statementDelta converts one statement's cumulative counters into per-interval
// deltas. The caller must always store current in the cache afterwards.
//   - statsReset (pg_stat_statements_info.stats_reset changed since the previous
//     scrape): every counter accumulated after the reset, so report current values,
//     even if they already grew past the stale cached values.
//   - not cached, or the cached entry lacks a column: seed the whole statement and
//     report 0 for every column, never lifetime totals or a partial mix.
//   - any column decreased (reset or deallocation without stats_reset information):
//     treat the whole statement as reset and report current values.
//   - otherwise: report current - cached per column.
func statementDelta(prev statementCounters, seen, statsReset bool, current statementCounters) (statementCounters, deltaOutcome) {
	deltas := make(statementCounters, len(current))
	if statsReset {
		maps.Copy(deltas, current)
		return deltas, deltaReset
	}
	complete := seen
	for column := range current {
		if _, ok := prev[column]; !ok {
			complete = false
			break
		}
	}
	if !complete {
		for column := range current {
			deltas[column] = 0
		}
		return deltas, deltaSeeded
	}
	for column, value := range current {
		if value < prev[column] {
			maps.Copy(deltas, current)
			return deltas, deltaReset
		}
	}
	for column, value := range current {
		deltas[column], _ = cumulativeDelta(prev[column], true, value)
	}
	return deltas, deltaSteady
}
