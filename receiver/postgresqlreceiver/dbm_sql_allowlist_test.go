// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

// [DBM BR-3, EC-08-S2] Allow-list test over every SQL text this receiver can
// send: the templates/*.tmpl files and every string literal (with constant
// concatenations folded) in the package's non-test Go files. It fails on DML,
// DDL, LOCK and other writing or session-changing statements, and on any read
// of histograms, most-common values or column statistics (pg_stats and its
// relatives). The deny lists are copied verbatim from the DBM sweep allow-list
// (coralogix-otel-collector pkg/dbmsweep/allowlist.go, forbiddenKeywords and
// forbiddenNames, at ecd3201); keep them in sync. It runs in the package's unit
// tests, so every fork rebase re-checks upstream's SQL (EC-08 drift guard).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forbiddenKeywords: copied from pkg/dbmsweep (BR-3). Statement words that
// write, change the schema, take locks, change settings or control
// transactions. SELECT ... INTO creates a table and FOR UPDATE/SHARE takes
// row locks, so INTO and FOR are forbidden.
var brForbiddenKeywords = map[string]bool{
	"insert": true, "update": true, "delete": true, "merge": true, "truncate": true,
	"create": true, "alter": true, "drop": true, "grant": true, "revoke": true,
	"lock": true, "copy": true, "call": true, "do": true, "execute": true,
	"prepare": true, "deallocate": true, "vacuum": true, "analyze": true,
	"reindex": true, "cluster": true, "refresh": true, "listen": true,
	"notify": true, "unlisten": true, "set": true, "reset": true, "discard": true,
	"comment": true, "security": true, "import": true, "into": true, "for": true,
	"table": true, "begin": true, "commit": true, "rollback": true,
	"savepoint": true, "returning": true, "load": true, "checkpoint": true,
}

// forbiddenNames: copied from pkg/dbmsweep (BR-3). Objects and columns that
// hold literal values of user tables on any engine. The sweep list also holds
// "query" (the statement text column); the receiver reads statement text by
// design (query samples, top queries), so "query" is the one entry left out
// here. Literals in that text are handled by the obfuscator (P7) and the edge
// processor, not by this test.
var brForbiddenNames = map[string]bool{
	// PostgreSQL
	"pg_statistic": true, "pg_statistic_ext": true, "pg_statistic_ext_data": true,
	"pg_stats": true, "pg_stats_ext": true, "pg_stats_ext_exprs": true,
	"most_common_vals": true, "most_common_freqs": true, "most_common_elems": true,
	"most_common_elem_freqs": true, "histogram_bounds": true, "elem_count_histogram": true,
	"most_common_base_freqs": true,
	// Oracle, Db2, MySQL (BR-3 list)
	"histogram": true, "low_value": true, "high_value": true, "high2key": true,
	"low2key": true, "coldist": true, "column_statistics": true,
	"dba_tab_histograms": true, "all_tab_histograms": true, "user_tab_histograms": true,
	"dba_histograms": true, "dba_tab_col_statistics": true, "all_tab_col_statistics": true,
	"user_tab_col_statistics": true,
}

// brStatementExceptions are the only statements that may use a forbidden
// keyword, each matched on its whole normalised text (comments removed,
// whitespace collapsed, lower case, Go format verbs kept). Only the listed
// keywords are exempt, and only in that exact statement. They are the
// receiver's EXPLAIN path (client.go explainQuery): a session-only setting
// and a prepared statement on a dedicated connection, explained without
// ANALYZE (the statement never runs) and deallocated.
var brStatementExceptions = map[string][]string{
	"set plan_cache_mode = force_generic_plan": {"set"},
	"prepare otel_%s as %s":                    {"prepare"},
	"explain(format json) execute otel_%s%s":   {"execute"},
	"deallocate prepare otel_%s":               {"deallocate", "prepare"},
}

// brStatementStarters are the first words that make a string literal a SQL
// statement for this test. A literal that starts with any other word (a log
// message, a fragment) is checked for forbidden names only.
var brStatementStarters = map[string]bool{
	"select": true, "with": true, "show": true, "explain": true, "values": true,
}

func init() {
	for k := range brForbiddenKeywords {
		brStatementStarters[k] = true
	}
}

// brSQLText is one SQL text found in the package.
type brSQLText struct {
	origin string // file:line
	text   string
}

// brViolation is one allow-list violation.
type brViolation struct {
	origin, statement, reason string
}

func (v brViolation) String() string {
	return fmt.Sprintf("%s: %s (statement: %q)", v.origin, v.reason, v.statement)
}

// collectReceiverSQL returns the templates and the folded string literals of
// the package's non-test Go files under dir.
func collectReceiverSQL(_ string) ([]brSQLText, []brSQLText, error) {
	return nil, nil, errors.New("not implemented")
}

// checkSQL returns the violations of one SQL text. isTemplate marks a text
// that is SQL as a whole (a template); a Go literal is checked for keywords
// only when it starts like a statement.
func checkSQL(_ brSQLText, _ bool) []brViolation {
	return nil
}

func TestReceiverSQLAllowList(t *testing.T) {
	templates, literals, err := collectReceiverSQL(".")
	require.NoError(t, err)

	// The scan must see the receiver's SQL, or a refactor could make it pass
	// by scanning nothing.
	names := make([]string, 0, len(templates))
	for _, tpl := range templates {
		names = append(names, filepath.Base(strings.SplitN(tpl.origin, ":", 2)[0]))
	}
	sort.Strings(names)
	assert.Subset(t, names, []string{"querySampleTemplate.tmpl", "topQueryTemplate.tmpl"})
	var statements int
	var joined strings.Builder
	for _, lit := range literals {
		if brIsStatement(lit.text) {
			statements++
			joined.WriteString(brNormalize(lit.text))
			joined.WriteString("\n")
		}
	}
	assert.GreaterOrEqual(t, statements, 15, "the scan found too few SQL statements in the Go files")
	for _, want := range []string{
		"select dealloc, stats_reset from pg_stat_statements_info",
		"show server_version",
		"from pg_stat_database_conflicts",
		"from pg_statio_user_tables",
	} {
		assert.Contains(t, joined.String(), want, "the scan missed a known statement")
	}

	var violations []string
	for _, tpl := range templates {
		for _, v := range checkSQL(tpl, true) {
			violations = append(violations, v.String())
		}
	}
	for _, lit := range literals {
		for _, v := range checkSQL(lit, false) {
			violations = append(violations, v.String())
		}
	}
	assert.Empty(t, violations, "BR-3: receiver SQL breaks the allow-list")
}

// TestReceiverSQLAllowListRejects proves that checkSQL catches each class of
// forbidden SQL, so a passing TestReceiverSQLAllowList means something.
func TestReceiverSQLAllowListRejects(t *testing.T) {
	for _, tc := range []struct {
		name       string
		text       string
		isTemplate bool
	}{
		{name: "DML in a literal", text: "UPDATE pg_settings SET setting = '1'"},
		{name: "DML after a comment", text: "/* otel-collector-ignore */ DELETE FROM t"},
		{name: "DDL", text: "CREATE TABLE t (a int)"},
		{name: "LOCK", text: "LOCK TABLE t IN ACCESS EXCLUSIVE MODE"},
		{name: "row lock", text: "SELECT a FROM t FOR UPDATE"},
		{name: "SELECT INTO", text: "SELECT a INTO t2 FROM t"},
		{name: "second statement", text: "SELECT 1; DROP TABLE t"},
		{name: "another session setting", text: "SET search_path = public"},
		{name: "histogram select", text: "SELECT histogram_bounds FROM pg_catalog.pg_stats"},
		{name: "most common values", text: "SELECT most_common_vals FROM pg_stats WHERE tablename = 't'"},
		{name: "pg_statistic", text: "SELECT stavalues1 FROM pg_statistic"},
		{name: "quoted name", text: `SELECT "histogram_bounds" FROM x`},
		{name: "name in a fragment", text: " JOIN pg_stats s ON true"},
		{name: "DML in a template", text: "SELECT 1;\n{{ if .x }}INSERT INTO t VALUES (1){{ end }}", isTemplate: true},
		{name: "template fragment", text: "{{ .a }} UPDATE t SET a = 1", isTemplate: true},
		{name: "oracle histogram", text: "SELECT low_value, high_value FROM dba_tab_col_statistics"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotEmpty(t, checkSQL(brSQLText{origin: "probe", text: tc.text}, tc.isTemplate), "must be rejected: %q", tc.text)
		})
	}
	for _, ok := range []string{
		"SELECT schemaname as schema, relname AS table, heap_blks_read FROM pg_statio_user_tables;",
		"/* otel-collector-ignore */ SET plan_cache_mode = force_generic_plan;",
		"PREPARE otel_%s AS %s;",
		"/* otel-collector-ignore */ DEALLOCATE PREPARE otel_%s",
		"failed to update the cache", // a log message, not a statement
	} {
		assert.Empty(t, checkSQL(brSQLText{origin: "probe", text: ok}, false), "must be allowed: %q", ok)
	}
}

// brIsStatement reports whether a Go literal starts like a SQL statement.
func brIsStatement(string) bool { return false }

// brNormalize removes comments, collapses whitespace and lower-cases.
func brNormalize(s string) string { return s }

var _ = os.ReadFile
