// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver"

import (
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

// [DBM P5] Opt-in raw query text for Coralogix Database Monitoring.
//
// When dbm.allow_literal_capture is true (default false), each
// db.server.query_sample record also carries the un-obfuscated statement text
// in db.query.text.raw. db.query.text stays obfuscated. Top query records never
// carry it.
//
// The receiver applies no redaction to the raw text. The DBM edge processor
// (dbmprocessor) runs the credential rule first and then deletes the attribute
// unless a Full capture lease applies, so the raw text never leaves the
// collector without a lease. A pipeline that receives these records without
// that processor must keep the option off.
const dbQueryTextRawAttribute = "db.query.text.raw"

// querySampleRawTextKey carries the raw text from the client to the scraper.
// It is an internal map key and is never emitted under this name.
const querySampleRawTextKey = "_dbm_query_text_raw"

// applyQuerySampleRawText adds db.query.text.raw to the emitted
// db.server.query_sample records. The records are emitted in recording order,
// so they pair with rawText by position. It fails closed: when the counts
// differ, no record gets raw text.
func applyQuerySampleRawText(logs plog.Logs, rawText []string, logger *zap.Logger) {
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
	if len(records) != len(rawText) {
		logger.Error("query_sample raw text does not match the emitted records; not adding db.query.text.raw",
			zap.Int("records", len(records)), zap.Int("raw_texts", len(rawText)))
		return
	}
	for i, text := range rawText {
		if text == "" {
			continue
		}
		records[i].Attributes().PutStr(dbQueryTextRawAttribute, text)
	}
}
