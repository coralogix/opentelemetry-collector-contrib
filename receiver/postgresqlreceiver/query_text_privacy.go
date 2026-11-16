// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver"

import (
	"context"
	"errors"
	"strings"

	"github.com/lib/pq"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadata"
)

// [DBM P7] Statement text never goes to the collector's own log: a log line
// about a statement carries its query id, its length and a fixed reason.

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

// maxEncodingCharBytes is the longest character of any PostgreSQL server
// encoding (UTF-8, MULE_INTERNAL): when the server clips a cut statement back
// to a whole character it removes at most maxEncodingCharBytes-1 bytes.
const maxEncodingCharBytes = 4

// obfuscationFailureReasons maps the obfuscator's error texts to fixed
// reasons. The error text itself is never logged: some of its forms quote a
// character of the statement.
var obfuscationFailureReasons = []struct{ match, reason string }{
	{"unexpected EOF in dollar-quoted string", "unterminated_dollar_quoted_string"},
	{"unexpected EOF in string", "unterminated_string"},
	{"unexpected EOF in comment", "unterminated_comment"},
	{"unexpected EOF in escape sequence", "unterminated_escape_sequence"},
	{"invalid UTF-8", "invalid_utf8"},
	{"result is empty", "empty_result"},
}

// obfuscationFailureReason returns a fixed reason for an obfuscation error.
func obfuscationFailureReason(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, r := range obfuscationFailureReasons {
		if strings.Contains(msg, r.match) {
			return r.reason
		}
	}
	return "unparsable_statement"
}

// logObfuscationFailure logs an obfuscation failure without the statement text.
func logObfuscationFailure(logger *zap.Logger, event metadata.AttributeEvent, queryID string, textLength int, err error) {
	fields := []zap.Field{
		zap.String("event", event.String()),
		zap.String("reason", obfuscationFailureReason(err)),
		zap.String("queryID", queryID),
		zap.Int("queryLength", textLength),
	}
	if event == metadata.AttributeEventTopQuery {
		logger.Error("failed to obfuscate query", fields...)
		return
	}
	logger.Warn("failed to obfuscate query", fields...)
}

// querySampleTextTruncated reports whether pg_stat_activity cut the text.
// The server keeps at most track_activity_query_size-1 bytes and then clips a
// partial multibyte character. ASCII text cannot be clipped, so it is cut
// exactly when it fills the buffer; other text is marked when it is within
// maxEncodingCharBytes-1 bytes of it. octetLength is the server-side
// octet_length (in the server encoding). An unknown size or length (0) is
// treated as not truncated.
func querySampleTextTruncated(text string, octetLength, trackActivityQuerySize int64) bool {
	if trackActivityQuerySize <= 1 || octetLength <= 0 {
		return false
	}
	limit := trackActivityQuerySize - 1
	if !isASCII(text) {
		limit -= maxEncodingCharBytes - 1
	}
	return octetLength >= limit
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// applyQuerySampleTruncated marks truncated db.server.query_sample records.
// truncated holds one flag per record, in recording order.
func applyQuerySampleTruncated(logs plog.Logs, truncated []bool, logger *zap.Logger) {
	var records []plog.LogRecord
	for _, rl := range logs.ResourceLogs().All() {
		for _, sl := range rl.ScopeLogs().All() {
			for _, lr := range sl.LogRecords().All() {
				if lr.EventName() == "db.server.query_sample" {
					records = append(records, lr)
				}
			}
		}
	}
	if len(records) != len(truncated) {
		logger.Error("query_sample truncation flags do not match the emitted records; not adding db.query.text.truncated",
			zap.Int("records", len(records)), zap.Int("flags", len(truncated)))
		return
	}
	for i, cut := range truncated {
		if cut {
			records[i].Attributes().PutBool(dbQueryTextTruncatedAttribute, true)
		}
	}
}

// recordObfuscationFailures counts and strips the obfuscation marker of rows.
// It runs on the rows as read, so the count includes rows the scrape later
// drops (seeded top queries, a discarded sample batch).
func (p *postgreSQLScraper) recordObfuscationFailures(ctx context.Context, rows []map[string]any, event metadata.AttributeEvent) {
	var n int64
	for _, row := range rows {
		if failed, _ := row[obfuscationFailedKey].(bool); failed {
			n++
		}
		delete(row, obfuscationFailedKey)
	}
	if n > 0 {
		p.telemetry.PostgresqlQueryObfuscationFailed.Add(ctx, n, metric.WithAttributes(attribute.String("event", event.String())))
	}
}

// sqlErrorFields returns the log fields for a database error: the SQLSTATE
// and its fixed condition name, never the message. [DBM P7b] A PostgreSQL
// message, detail or context can quote a value of the statement (for
// example 22P02 "invalid input syntax for type integer: ..."), so the error
// text is never logged on the EXPLAIN path.
func sqlErrorFields(err error) []zap.Field {
	if pqErr, ok := errors.AsType[*pq.Error](err); ok {
		return []zap.Field{
			zap.String("sqlState", string(pqErr.Code)),
			zap.String("errorCategory", pqErr.Code.Name()),
		}
	}
	category := "other"
	switch {
	case errors.Is(err, context.Canceled):
		category = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		category = "deadline_exceeded"
	}
	return []zap.Field{zap.String("errorCategory", category)}
}
