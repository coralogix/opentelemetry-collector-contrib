//go:build linux

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

// [DBM P7c] MUTATION PROBE, CI ONLY. This file exists in one probe
// commit and is removed by the next commit. Each mutant is applied with
// "go test -overlay" (the source on disk is never changed) and the tests
// that must kill it are run in a child "go test". It always fails with a
// summary so the result is printed in the CI log.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var p7cProbeOnce sync.Once

func TestP7cMutationProbe(t *testing.T) {
	ran := false
	p7cProbeOnce.Do(func() { ran = true })
	if !ran {
		t.Skip("the probe runs once per test binary")
	}
	mutants := []struct{ id, file, from, to, run string }{
		{"M1-no-roundtrip", "query_id_guard.go", "return err == nil && strconv.FormatInt(id, 10) == s", "return err == nil && strconv.FormatInt(id, 10) != \"\"", "TestIsCanonicalQueryID"},
		{"M2-bitsize-32", "query_id_guard.go", "strconv.ParseInt(s, 10, 64)", "strconv.ParseInt(s, 10, 32)", "TestIsCanonicalQueryID"},
		{"M3-log-field-echoes", "query_id_guard.go", "if queryID == \"\" || isCanonicalQueryID(queryID) {\n\t\treturn zap.String", "if true {\n\t\treturn zap.String", "TestQueryIDField|TestTopQueryObfuscationFailureWithInvalidQueryID"},
		{"M4-obfuscation-log-raw-field", "query_text_privacy.go", "queryIDField(queryID), // [DBM P7c]", "zap.String(\"queryID\", queryID),", "TestTopQueryObfuscationFailureWithInvalidQueryID"},
		{"M5-sample-check-skipped", "client.go", "if queryID != \"\" && !isCanonicalQueryID(queryID) {", "if false {", "TestQuerySampleInvalidQueryIDDropped"},
		{"M6-sample-empty-invalid", "client.go", "if queryID != \"\" && !isCanonicalQueryID(queryID) {", "if !isCanonicalQueryID(queryID) {", "TestQuerySampleInvalidQueryIDDropped"},
		{"M7-sample-value-kept", "client.go", "\t\t\tcurrentAttributes[dbAttributePrefix+querySampleColumnQueryID] = \"\"\n", "", "TestQuerySampleInvalidQueryIDDropped|TestQuerySampleInvalidQueryIDCounted"},
		{"M8-sample-drop-log-echoes", "client.go", "zap.Int(\"queryIDLength\", len(queryID)))\n\t\t\tqueryID = \"\"", "zap.String(\"queryIDLength\", queryID))\n\t\t\tqueryID = \"\"", "TestQuerySampleInvalidQueryIDDropped"},
		{"M9-sample-obfuscation-log-raw", "client.go", "logObfuscationFailure(logger, metadata.AttributeEventQuerySample, queryID,", "logObfuscationFailure(logger, metadata.AttributeEventQuerySample, row[querySampleColumnQueryID],", "TestQuerySampleObfuscationFailureWithInvalidQueryID"},
		{"M10-sample-not-counted", "scraper.go", "\tp.recordInvalidQueryIDs(ctx, attributes, metadata.AttributeEventQuerySample) // [DBM P7c]\n", "", "TestQuerySampleInvalidQueryIDCounted"},
		{"M11-marker-not-stripped", "query_id_guard.go", "\t\tdelete(row, queryIDInvalidKey)\n", "", "TestRecordInvalidQueryIDsStripsMarker"},
		{"M12-top-rows-not-filtered", "scraper.go", "\trows = p.dropInvalidTopQueryIDs(ctx, rows, logger)\n", "", "TestTopQueryInvalidQueryIDRowDropped"},
		{"M13-top-row-kept", "query_id_guard.go", "\t\tn++\n\t}\n\tp.countInvalidQueryIDs(ctx, n, metadata.AttributeEventTopQuery)", "\t\tn++\n\t\tkept = append(kept, row)\n\t}\n\tp.countInvalidQueryIDs(ctx, n, metadata.AttributeEventTopQuery)", "TestTopQueryInvalidQueryIDRowDropped"},
		{"M14-top-drop-log-echoes", "query_id_guard.go", "zap.Int(\"queryIDLength\", len(queryID)))\n\t\tn++", "zap.String(\"queryIDLength\", queryID))\n\t\tn++", "TestTopQueryInvalidQueryIDRowDropped"},
		{"M15-top-not-counted", "query_id_guard.go", "\tp.countInvalidQueryIDs(ctx, n, metadata.AttributeEventTopQuery)\n", "", "TestTopQueryInvalidQueryIDRowDropped"},
		{"M16-non-string-kept", "query_id_guard.go", "\t\tif value == nil {\n", "\t\tif _, ok := value.(string); value == nil || !ok {\n", "TestTopQueryNonStringQueryIDRowDropped"},
		{"M17-explain-guard-removed", "client.go", "if !isCanonicalQueryID(queryID) {\n\t\tlogger.Error(\"refusing EXPLAIN", "if false {\n\t\tlogger.Error(\"refusing EXPLAIN", "TestExplainQueryRefusesInvalidQueryID"},
		{"M18-explain-refusal-log-echoes", "client.go", "zap.Bool(\"queryIDInvalid\", true), zap.Int(\"queryIDLength\", len(queryID)))", "zap.String(\"queryIDInvalid\", queryID), zap.Int(\"queryIDLength\", len(queryID)))", "TestExplainQueryRefusesInvalidQueryID"},
	}
	control := exec.Command("go", "test", "-count=1", "-run", "^(TestIsCanonicalQueryID|TestQueryIDField|TestQuerySample|TestTopQuery|TestExplainQuery|TestRecordInvalidQueryIDs)", ".")
	if out, err := control.CombinedOutput(); err != nil {
		t.Fatalf("P7C-PROBE control run failed: %v\n%s", err, out)
	}
	// The full package run, verbose, for the evidence test list (tests.md).
	all, allErr := exec.Command("go", "test", "-count=1", "-v", "-skip", "^TestP7cMutationProbe$", ".").CombinedOutput()
	var allLines []string
	for _, line := range strings.Split(string(all), "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "--- PASS: ") || strings.HasPrefix(l, "--- FAIL: ") || strings.HasPrefix(l, "--- SKIP: ") || strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "FAIL") {
			allLines = append(allLines, "P7C-ALLTESTS "+l)
		}
	}
	t.Logf("P7C-ALLTESTS exit error: %v\n%s", allErr, strings.Join(allLines, "\n"))
	results := make([]string, len(mutants))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, m := range mutants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			src, err := os.ReadFile(m.file)
			if err != nil {
				results[i] = m.id + ": ERROR " + err.Error()
				return
			}
			if n := strings.Count(string(src), m.from); n != 1 {
				results[i] = m.id + ": ERROR anchor found " + string(rune('0'+n)) + " times"
				return
			}
			dir := t.TempDir()
			mutated := filepath.Join(dir, m.file)
			if err = os.WriteFile(mutated, []byte(strings.Replace(string(src), m.from, m.to, 1)), 0o600); err != nil {
				results[i] = m.id + ": ERROR " + err.Error()
				return
			}
			abs, _ := filepath.Abs(m.file)
			ov, _ := json.Marshal(map[string]map[string]string{"Replace": {abs: mutated}})
			ovFile := filepath.Join(dir, "overlay.json")
			if err = os.WriteFile(ovFile, ov, 0o600); err != nil {
				results[i] = m.id + ": ERROR " + err.Error()
				return
			}
			out, err := exec.Command("go", "test", "-count=1", "-overlay", ovFile, "-run", "^("+m.run+")$", ".").CombinedOutput()
			switch {
			case err == nil:
				results[i] = m.id + ": SURVIVED (" + m.run + ")"
			case strings.Contains(string(out), "[build failed]") || strings.Contains(string(out), "[setup failed]"):
				results[i] = m.id + ": ERROR build failed: " + string(out)
			case strings.Contains(string(out), "--- FAIL: "):
				var killers []string
				for _, line := range strings.Split(string(out), "\n") {
					if f, ok := strings.CutPrefix(strings.TrimSpace(line), "--- FAIL: "); ok && !strings.Contains(f, "/") {
						killers = append(killers, strings.Fields(f)[0])
					}
				}
				results[i] = m.id + ": KILLED by " + strings.Join(killers, ",")
			default:
				results[i] = m.id + ": ERROR " + string(out)
			}
		}()
	}
	wg.Wait()
	killed := 0
	for _, r := range results {
		if strings.Contains(r, ": KILLED by ") {
			killed++
		}
	}
	t.Errorf("P7C-PROBE results (this test always fails, to print them):\nP7C-PROBE %s\nP7C-PROBE mutants: killed %d of %d",
		strings.Join(results, "\nP7C-PROBE "), killed, len(mutants))
}
