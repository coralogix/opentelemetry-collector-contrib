//go:build linux

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

// [DBM P8, EC-04b] MUTATION PROBE, CI ONLY. This file exists in one probe
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

var p8ProbeOnce sync.Once

func TestP8MutationProbe(t *testing.T) {
	ran := false
	p8ProbeOnce.Do(func() { ran = true })
	if !ran {
		t.Skip("the probe runs once per test binary")
	}
	mutants := []struct{ id, file, from, to, run string }{
		{"M1-tx-not-read-only", "explain_function.go", "return &sql.TxOptions{ReadOnly: true}", "return &sql.TxOptions{ReadOnly: false}", "TestExplainFunctionTxIsReadOnly"},
		{"M2-own-explain-instead-of-function", "scraper.go", "explained < maxExplainEachInterval && p.config.DBM.ExplainFunction != \"\" {", "explained < maxExplainEachInterval && false {", "TestTopQueryExplainFunctionGetsThePlan"},
		{"M3-no-statement-timeout", "explain_function.go", "if _, err = tx.ExecContext(ctx, fmt.Sprintf(explainFunctionTimeoutSQL, timeoutMS)); err != nil {", "if _ = timeoutMS; false {", "TestExplainViaFunctionCallsTheFunction"},
		{"M4-timeout-not-from-config", "explain_function.go", "timeoutMS := opts.timeoutMS", "timeoutMS := int64(1000)", "TestExplainViaFunctionPassesSchemasAndTimeout"},
		{"M5-schemas-ignored", "explain_function.go", "id, pq.Array(opts.schemas)", "id, pq.Array([]string(nil))", "TestExplainViaFunctionPassesSchemasAndTimeout"},
		{"M6-comments-not-stripped", "explain_function.go", "\ts := query\n\tfor {", "\ts := query\n\treturn s\n\tfor {", "TestStripLeadingComments|TestTopQueryExplainFunctionGetsThePlan"},
		{"M7-nested-comments-ignored", "explain_function.go", "\t\t\t\t\tdepth++\n", "\t\t\t\t\t_ = depth\n", "TestStripLeadingComments"},
		{"M8-only-DBM21-not-eligible", "explain_function.go", "case strings.HasPrefix(code, \"DBM2\"):", "case code == \"DBM21\":", "TestExplainFunctionOutcome"},
		{"M9-not-eligible-retried", "explain_function.go", "\t\tp.explainNotEligible[database] = struct{}{}\n", "", "TestTopQueryExplainFunctionNotEligibleStopsCalls"},
		{"M10-no-rate-limit", "explain_function.go", "if l.credit < l.cost {", "if false {", "TestExplainRateLimiter|TestTopQueryExplainFunctionRateLimited"},
		{"M11-rate-limited-cached", "explain_function.go", "\t\tp.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeRateLimited)\n", "\t\tp.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeRateLimited)\n\t\tp.queryPlanCache.Add(planCacheKey, \"\")\n", "TestTopQueryExplainFunctionRateLimited"},
		{"M12-server-message-logged", "explain_function.go", "zap.Int(\"queryLength\", len(rawQuery))}, sqlErrorFields(err)...)", "zap.Int(\"queryLength\", len(rawQuery))}, zap.Error(err))", "TestTopQueryExplainFunctionFailureLogsSQLStateOnly"},
		{"M13-statement-text-logged", "explain_function.go", "zap.Int(\"queryLength\", len(rawQuery))}", "zap.String(\"queryLength\", rawQuery)}", "TestTopQueryExplainFunctionFailureLogsSQLStateOnly"},
		{"M14-any-function-name", "explain_function.go", "if cfg.ExplainFunction != explainFunctionName {", "if false {", "TestExplainFunctionConfigValidate"},
		{"M15-timeout-above-2s-allowed", "explain_function.go", "t > maxExplainFunctionTimeout", "t > 3*maxExplainFunctionTimeout", "TestExplainFunctionConfigValidate"},
		{"M16-no-default-timeout", "explain_function.go", "timeout = defaultExplainFunctionTimeout", "timeout = 0", "TestExplainFunctionOptionsDefaults|TestTopQueryExplainFunctionGetsThePlan"},
		{"M17-non-select-sent", "explain_function.go", "\tcase \"select\", \"with\":\n\t\treturn s, true\n\t}\n\treturn \"\", false", "\tcase \"select\", \"with\":\n\t\treturn s, true\n\t}\n\treturn s, s != \"\"", "TestExplainFunctionInput|TestTopQueryExplainFunctionSkipsNonSelect"},
		{"M18-no-warn-on-not-eligible", "explain_function.go", "\t\tlogger.Warn(\"plans unavailable: database not eligible for the explain function\",", "\t\tlogger.Debug(\"plans unavailable: database not eligible for the explain function\",", "TestTopQueryExplainFunctionNotEligibleStopsCalls"},
		{"M19-null-plan-error", "explain_function.go", "if !raw.Valid || raw.String == \"\" {\n\t\treturn \"\", nil", "if !raw.Valid || raw.String == \"\" {\n\t\treturn \"\", errors.New(\"null plan\")", "TestExplainViaFunctionNullPlan"},
		{"M20-schema-length-unchecked", "explain_function.go", "if schema == \"\" || len(schema) > maxSchemaNameBytes {", "if schema == \"\" {", "TestExplainFunctionConfigValidate"},
		{"M21-queryid-not-validated", "explain_function.go", "if err != nil {\n\t\treturn \"\", errExplainFunctionQueryID", "if false {\n\t\treturn \"\", errExplainFunctionQueryID", "TestExplainViaFunctionRejectsABadQueryID"},
		{"M22-wrong-queryid-sent", "explain_function.go", "explainFunctionCallSQL, id, pq.Array", "explainFunctionCallSQL, id+1, pq.Array", "TestExplainViaFunctionPassesTheQueryID|TestTopQueryExplainFunctionGetsThePlan"},
		{"M23-statement-text-sent", "explain_function.go", "plan, err := dbClient.explainViaFunction(ctx, queryID, ", "plan, err := dbClient.explainViaFunction(ctx, rawQuery, ", "TestTopQueryExplainFunctionGetsThePlan"},
		{"M24-refused-statement-retried", "explain_function.go", "\t\tp.explainNoPlan.Add(planCacheKey, struct{}{})\n", "", "TestTopQueryExplainFunctionStatementNotEligible"},
		{"M25-statement-refusal-blocks-database", "explain_function.go", "case code == \"DBM24\", code == \"DBM25\", code == \"DBM26\":", "case false:", "TestExplainFunctionOutcome|TestTopQueryExplainFunctionStatementNotEligible"},
		{"M26-DBM16-not-input-refused", "explain_function.go", "code == \"DBM15\", code == \"DBM16\":", "code == \"DBM15\":", "TestExplainFunctionOutcome"},
	}
	control := exec.Command("go", "test", "-count=1", "-run", "^(TestExplain|TestStripLeadingComments|TestTopQueryExplainFunction)", ".")
	if out, err := control.CombinedOutput(); err != nil {
		t.Fatalf("P8-PROBE control run failed: %v\n%s", err, out)
	}
	// The full package run, verbose, for the evidence test list (tests.md).
	all, allErr := exec.Command("go", "test", "-count=1", "-v", "-skip", "^TestP8MutationProbe$", ".").CombinedOutput()
	var allLines []string
	for _, line := range strings.Split(string(all), "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "--- PASS: ") || strings.HasPrefix(l, "--- FAIL: ") || strings.HasPrefix(l, "--- SKIP: ") || strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "FAIL") {
			allLines = append(allLines, "P8-ALLTESTS "+l)
		}
	}
	t.Logf("P8-ALLTESTS exit error: %v\n%s", allErr, strings.Join(allLines, "\n"))
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
	t.Errorf("P8-PROBE results (this test always fails, to print them):\nP8-PROBE %s\nP8-PROBE mutants: killed %d of %d",
		strings.Join(results, "\nP8-PROBE "), killed, len(mutants))
}
