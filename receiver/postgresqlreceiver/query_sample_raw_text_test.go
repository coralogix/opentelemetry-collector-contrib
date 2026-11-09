// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"database/sql/driver"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadata"
)

// Synthetic statements only. Each carries a literal that the obfuscator masks.
const (
	rawSampleText1 = "SELECT * FROM orders WHERE status = 'pending' AND id = 4242"
	rawSampleText2 = "UPDATE users SET nickname = 'synthetic-user' WHERE id = 42"
)

func TestDBMAllowLiteralCaptureDefaultsOff(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	assert.False(t, cfg.DBM.AllowLiteralCapture, "raw query text must be off by default")
}

func TestDBMAllowLiteralCaptureUnmarshal(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	require.NoError(t, confmap.NewFromStringMap(map[string]any{
		"dbm": map[string]any{"allow_literal_capture": true},
	}).Unmarshal(cfg))
	assert.True(t, cfg.DBM.AllowLiteralCapture)
}

func scrapeRawTextSamples(t *testing.T, allow bool) plog.Logs {
	t.Helper()
	cfg := createDefaultConfig().(*Config)
	cfg.Databases = []string{}
	cfg.LogsBuilderConfig.Events.DbServerQuerySample.Enabled = true
	cfg.DBM.AllowLiteralCapture = allow

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	settings := receivertest.NewNopSettings(metadata.Type)
	settings.Logger = zap.NewNop()
	scraper, err := newPostgreSQLScraper(settings, cfg, mockSimpleClientFactory{db: db}, newCache(1), newTTLCache[string](1, time.Second))
	require.NoError(t, err)
	scraper.newestQueryTimestamp = 123440.111

	rows := sqlmock.NewRows(querySampleColumns)
	for i, text := range []string{rawSampleText1, rawSampleText2} {
		data := map[string]any{
			querySampleColumnDatname:              "postgres",
			querySampleColumnUsename:              "synthetic",
			querySampleColumnClientPort:           "5432",
			querySampleColumnPID:                  []string{"1001", "1002"}[i],
			querySampleColumnQueryStartTimestamp:  "123445.123",
			querySampleColumnState:                "active",
			querySampleColumnQuery:                text,
			querySampleColumnDurationMilliseconds: "5.3",
		}
		values := make([]driver.Value, len(querySampleColumns))
		for j, col := range querySampleColumns {
			if v, ok := data[col]; ok {
				values[j] = v
				continue
			}
			values[j] = ""
		}
		rows.AddRow(values...)
	}
	mock.ExpectQuery(expectedScrapeSampleQuery).WillReturnRows(rows)

	logs, err := scraper.scrapeQuerySamples(t.Context(), 30)
	require.NoError(t, err)
	return logs
}

func querySampleRecords(logs plog.Logs) []plog.LogRecord {
	var out []plog.LogRecord
	for _, rl := range logs.ResourceLogs().All() {
		for _, sl := range rl.ScopeLogs().All() {
			for _, lr := range sl.LogRecords().All() {
				if lr.EventName() == "db.server.query_sample" {
					out = append(out, lr)
				}
			}
		}
	}
	return out
}

func TestQuerySampleRawTextOffByDefault(t *testing.T) {
	records := querySampleRecords(scrapeRawTextSamples(t, false))
	require.Len(t, records, 2)
	for _, lr := range records {
		_, ok := lr.Attributes().Get(dbQueryTextRawAttribute)
		assert.False(t, ok, "db.query.text.raw must not be emitted unless dbm.allow_literal_capture is true")
		text, ok := lr.Attributes().Get("db.query.text")
		require.True(t, ok)
		assert.NotContains(t, text.Str(), "pending")
		assert.NotContains(t, text.Str(), "synthetic-user")
	}
}

func TestQuerySampleRawTextOptIn(t *testing.T) {
	records := querySampleRecords(scrapeRawTextSamples(t, true))
	require.Len(t, records, 2)
	want := map[int64]string{1001: rawSampleText1, 1002: rawSampleText2}
	for _, lr := range records {
		pid, ok := lr.Attributes().Get("postgresql.pid")
		require.True(t, ok)
		raw, ok := lr.Attributes().Get(dbQueryTextRawAttribute)
		require.True(t, ok, "db.query.text.raw must be emitted when dbm.allow_literal_capture is true")
		assert.Equal(t, want[pid.Int()], raw.Str(), "raw text must pair with its own sample")
		// The obfuscated text is unchanged by the opt-in.
		text, ok := lr.Attributes().Get("db.query.text")
		require.True(t, ok)
		assert.NotContains(t, text.Str(), "pending")
		assert.NotContains(t, text.Str(), "synthetic-user")
	}
}

func TestQuerySampleRawTextOnlyOnQuerySamples(t *testing.T) {
	logs := plog.NewLogs()
	lrs := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	top := lrs.AppendEmpty()
	top.SetEventName("db.server.top_query")
	sample := lrs.AppendEmpty()
	sample.SetEventName("db.server.query_sample")

	applyQuerySampleRawText(logs, []string{rawSampleText1}, zap.NewNop())

	_, ok := top.Attributes().Get(dbQueryTextRawAttribute)
	assert.False(t, ok, "top_query records never carry raw text (deferred)")
	raw, ok := sample.Attributes().Get(dbQueryTextRawAttribute)
	require.True(t, ok)
	assert.Equal(t, rawSampleText1, raw.Str())
}

func TestQuerySampleRawTextFailsClosedOnMismatch(t *testing.T) {
	logs := plog.NewLogs()
	lrs := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	for range 2 {
		lrs.AppendEmpty().SetEventName("db.server.query_sample")
	}

	applyQuerySampleRawText(logs, []string{rawSampleText1}, zap.NewNop())

	for _, lr := range lrs.All() {
		_, ok := lr.Attributes().Get(dbQueryTextRawAttribute)
		assert.False(t, ok, "raw text is never attached when it cannot be paired with its record")
	}
}

func TestQuerySampleRawTextSkipsEmpty(t *testing.T) {
	logs := plog.NewLogs()
	lr := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetEventName("db.server.query_sample")

	applyQuerySampleRawText(logs, []string{""}, zap.NewNop())

	_, ok := lr.Attributes().Get(dbQueryTextRawAttribute)
	assert.False(t, ok)
}
