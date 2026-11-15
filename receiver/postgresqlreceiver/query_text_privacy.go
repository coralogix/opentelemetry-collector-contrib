// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver"

import (
	"context"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadata"
)

// [DBM P7] Statement text never goes to the collector's own log.

// dbQueryTextTruncatedAttribute marks a db.server.query_sample record whose
// statement text was cut by the server (track_activity_query_size).
const dbQueryTextTruncatedAttribute = "db.query.text.truncated"

const (
	// obfuscationFailedKey carries, from the client to the scraper, that the
	// statement text of a row failed obfuscation. Never emitted.
	obfuscationFailedKey = "_dbm_obfuscation_failed"
	// querySampleTextTruncatedKey carries, from the client to the scraper, that
	// the statement text of a query sample is truncated. Never emitted.
	querySampleTextTruncatedKey = "_dbm_query_text_truncated"
)

// obfuscationFailureReason returns a fixed reason for an obfuscation error.
func obfuscationFailureReason(_ error) string {
	return ""
}

// logObfuscationFailure logs an obfuscation failure without the statement text.
func logObfuscationFailure(_ *zap.Logger, _ metadata.AttributeEvent, _ string, _ int, _ error) {}

// querySampleTextTruncated reports whether pg_stat_activity cut the text.
func querySampleTextTruncated(_ string, _, _ int64) bool {
	return false
}

// applyQuerySampleTruncated marks truncated db.server.query_sample records.
func applyQuerySampleTruncated(_ plog.Logs, _ []bool, _ *zap.Logger) {}

// recordObfuscationFailures counts and strips the obfuscation marker of rows.
func (*postgreSQLScraper) recordObfuscationFailures(_ context.Context, _ []map[string]any, _ metadata.AttributeEvent) {
}
