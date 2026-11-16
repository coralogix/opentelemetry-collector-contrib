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
		{"M6-comment-prefixed-skipped-locally", "explain_function.go", "if strings.HasPrefix(s, \"/*\") || strings.HasPrefix(s, \"--\") {\n\t\treturn true", "if false {\n\t\treturn true", "TestExplainFunctionInput|TestTopQueryExplainFunctionGetsThePlan"},
		{"M8-only-DBM21-not-eligible", "explain_function.go", "case strings.HasPrefix(code, \"DBM2\"), code == \"DBM15\":", "case code == \"DBM21\", code == \"DBM15\":", "TestExplainFunctionOutcome"},
		{"M9-not-eligible-retried", "explain_function.go", "\t\tp.explainNotEligible[database] = now.Add(p.config.DBM.explainFunctionRecheckInterval())\n\t\tif !probe {", "\t\tif !probe {", "TestTopQueryExplainFunctionNotEligibleStopsCalls"},
		{"M10-no-rate-limit", "explain_function.go", "if l.full && now.Sub(l.calls[l.next]) < explainRateWindow {", "if false {", "TestExplainRateLimiter|TestExplainRateLimiterWindow|TestTopQueryExplainFunctionRateLimited"},
		{"M11-rate-limited-cached", "explain_function.go", "\t\tp.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeRateLimited)\n", "\t\tp.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeRateLimited)\n\t\tp.queryPlanCache.Add(planCacheKey, \"\")\n", "TestTopQueryExplainFunctionRateLimited"},
		{"M12-server-message-logged", "explain_function.go", "zap.Int(\"queryLength\", len(rawQuery))}, sqlErrorFields(err)...)", "zap.Int(\"queryLength\", len(rawQuery))}, zap.Error(err))", "TestTopQueryExplainFunctionFailureLogsSQLStateOnly"},
		{"M13-statement-text-logged", "explain_function.go", "zap.Int(\"queryLength\", len(rawQuery))}", "zap.String(\"queryLength\", rawQuery)}", "TestTopQueryExplainFunctionFailureLogsSQLStateOnly"},
		{"M14-any-function-name", "explain_function.go", "if cfg.ExplainFunction != explainFunctionName {", "if false {", "TestExplainFunctionConfigValidate"},
		{"M15-timeout-above-2s-allowed", "explain_function.go", "t > maxExplainFunctionTimeout", "t > 3*maxExplainFunctionTimeout", "TestExplainFunctionConfigValidate"},
		{"M16-no-default-timeout", "explain_function.go", "timeout = defaultExplainFunctionTimeout", "timeout = 0", "TestExplainFunctionOptionsDefaults|TestTopQueryExplainFunctionGetsThePlan"},
		{"M17-non-select-sent", "explain_function.go", "case \"select\", \"with\":\n\t\treturn true\n\t}\n\treturn false", "case \"select\", \"with\":\n\t\treturn true\n\t}\n\treturn s != \"\"", "TestExplainFunctionInput|TestTopQueryExplainFunctionSkipsNonSelect"},
		{"M18-no-warn-on-not-eligible", "explain_function.go", "logger.Warn(\"plans unavailable: database not eligible for the explain function\",", "logger.Debug(\"plans unavailable: database not eligible for the explain function\",", "TestTopQueryExplainFunctionNotEligibleStopsCalls"},
		{"M19-null-plan-error", "explain_function.go", "if !raw.Valid || raw.String == \"\" {\n\t\treturn \"\", nil", "if !raw.Valid || raw.String == \"\" {\n\t\treturn \"\", errors.New(\"null plan\")", "TestExplainViaFunctionNullPlan"},
		{"M20-schema-length-unchecked", "explain_function.go", "if schema == \"\" || len(schema) > maxSchemaNameBytes {", "if schema == \"\" {", "TestExplainFunctionConfigValidate"},
		{"M21-queryid-not-validated", "explain_function.go", "if err != nil || strconv.FormatInt(id, 10) != queryID {", "if false {", "TestExplainViaFunctionRejectsABadQueryID"},
		{"M22-wrong-queryid-sent", "explain_function.go", "explainFunctionCallSQL, id, pq.Array", "explainFunctionCallSQL, id+1, pq.Array", "TestExplainViaFunctionPassesTheQueryID|TestTopQueryExplainFunctionGetsThePlan"},
		{"M23-statement-text-sent", "explain_function.go", "plan, err := dbClient.explainViaFunction(ctx, queryID, ", "plan, err := dbClient.explainViaFunction(ctx, rawQuery, ", "TestTopQueryExplainFunctionGetsThePlan"},
		{"M24-refused-statement-retried", "explain_function.go", "\t\tp.explainNoPlan.Add(planCacheKey, now.Add(explainNoPlanRetry))\n", "", "TestTopQueryExplainFunctionStatementNotEligible"},
		{"M25-statement-refusal-blocks-database", "explain_function.go", "case code == \"DBM24\", code == \"DBM25\", code == \"DBM26\":", "case false:", "TestExplainFunctionOutcome|TestTopQueryExplainFunctionStatementNotEligible"},
		{"M26-DBM16-not-input-refused", "explain_function.go", "code == \"DBM14\", code == \"DBM16\":", "code == \"DBM14\":", "TestExplainFunctionOutcome"},
		{"R1-function-missing-per-statement", "explain_function.go", "case code == \"42883\", code == \"3F000\", code == \"42501\":", "case false:", "TestExplainFunctionOutcome|TestTopQueryExplainFunctionMissingIsDatabaseLevel"},
		{"R2-DBM15-per-request", "explain_function.go", "case strings.HasPrefix(code, \"DBM2\"), code == \"DBM15\":", "case strings.HasPrefix(code, \"DBM2\"):", "TestExplainFunctionOutcome"},
		{"R3-schemas-unbounded", "explain_function.go", "if len(cfg.ExplainFunctionSchemas) > maxExplainFunctionSchemas {", "if false {", "TestExplainFunctionConfigValidate"},
		{"R4-budget-counts-skips", "scraper.go", "if called {", "if called || true {", "TestTopQueryExplainFunctionBudgetCountsCallsOnly"},
		{"R5-limiter-window-halved", "explain_function.go", "now.Sub(l.calls[l.next]) < explainRateWindow {", "now.Sub(l.calls[l.next]) < explainRateWindow/2 {", "TestExplainRateLimiter|TestExplainRateLimiterWindow"},
		{"R6-no-recheck", "explain_function.go", "if probe && now.Before(until) {", "if probe && (true || now.Before(until)) {", "TestTopQueryExplainFunctionRecheck"},
		{"R7-warn-on-every-refusal", "explain_function.go", "if !probe {\n\t\t\tsqlState", "if true {\n\t\t\tsqlState", "TestTopQueryExplainFunctionRecheck"},
		{"R8-no-available-again-warn", "explain_function.go", "logger.Warn(\"plans available again from the explain function\"", "logger.Debug(\"plans available again from the explain function\"", "TestTopQueryExplainFunctionRecheck"},
		{"R9-no-hint", "explain_function.go", "return \"use a clean connection\"", "return \"\"", "TestTopQueryExplainFunctionRecheck"},
		{"R10-errorcategory-empty", "query_text_privacy.go", "category = \"dbm\"", "category = \"\"", "TestTopQueryExplainFunctionFailureLogsSQLStateOnly"},
		{"R11-skipped-local-not-counted", "explain_function.go", "\t\tp.recordExplainOutcome(ctx, metadata.AttributeExplainOutcomeSkippedLocal)\n", "", "TestTopQueryExplainFunctionSkipsNonSelect"},
		{"R12-non-canonical-queryid", "explain_function.go", "if err != nil || strconv.FormatInt(id, 10) != queryID {", "if err != nil {", "TestExplainViaFunctionRejectsABadQueryID"},
		{"R13-no-default-recheck", "explain_function.go", "\t\treturn defaultExplainFunctionRecheckInterval\n", "\t\treturn 0\n", "TestExplainFunctionOptionsDefaults"},
		{"R14-recheck-minimum-unchecked", "explain_function.go", "r != 0 && r < minExplainFunctionRecheckInterval", "r < 0", "TestExplainFunctionConfigValidate"},
		{"R15-empty-schemas-sent", "explain_function.go", "if len(cfg.ExplainFunctionSchemas) > 0 {\n\t\topts.schemas", "if true {\n\t\topts.schemas", "TestExplainFunctionOptionsDefaults"},
		{"R16-refused-statement-retried-at-once", "explain_function.go", "if now.Before(retryAt) {", "if now.Before(retryAt) && false {", "TestTopQueryExplainFunctionStatementNotEligible|TestTopQueryExplainFunctionStatementRetryAfter24h"},
		{"R17-refused-statement-never-retried", "explain_function.go", "explainNoPlanRetry = 24 * time.Hour", "explainNoPlanRetry = 1000 * time.Hour", "TestTopQueryExplainFunctionStatementRetryAfter24h"},
		{"R18-probe-clears-on-any-result", "explain_function.go", "if !explainProbeClears(outcome, err) {", "if false {", "TestTopQueryExplainFunctionProbeClearRule"},
		{"R19-DBM11-clears", "explain_function.go", "return ok && pqErr.Code != \"DBM11\"", "return ok", "TestTopQueryExplainFunctionProbeClearRule"},
		{"R20-failed-probe-not-rearmed", "explain_function.go", "\t\t\tp.explainNotEligible[database] = now.Add(p.config.DBM.explainFunctionRecheckInterval())\n\t\t} else {", "\t\t\t_ = now\n\t\t} else {", "TestTopQueryExplainFunctionProbeClearRule"},
	}
	control := exec.Command("go", "test", "-count=1", "-run", "^(TestExplain|TestTopQueryExplainFunction)", ".")
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
