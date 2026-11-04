// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver"

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
