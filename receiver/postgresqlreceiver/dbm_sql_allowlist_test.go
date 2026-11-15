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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// brNonSQLLiterals are Go literals that start like a statement but are not
// SQL the receiver sends, matched on their whole normalised text: the keys of
// explainableStatements (client.go, the first words of statements that EXPLAIN
// accepts), the "Merge Cond" plan key (obfuscate.go) and the "reset" outcome
// (scraper.go). Any longer text is checked.
var brNonSQLLiterals = map[string]bool{
	"select": true, "table": true, "delete": true, "insert": true, "update": true,
	"with": true, "merge": true, "values": true, "merge cond": true, "reset": true,
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
func collectReceiverSQL(dir string) ([]brSQLText, []brSQLText, error) {
	var templates, literals []brSQLText
	tmpls, err := filepath.Glob(filepath.Join(dir, "templates", "*.tmpl"))
	if err != nil {
		return nil, nil, err
	}
	for _, path := range tmpls {
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, nil, readErr
		}
		templates = append(templates, brSQLText{origin: filepath.ToSlash(path) + ":1", text: string(b)})
	}
	goFiles, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	for _, path := range goFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.BinaryExpr:
				if s, ok := brFold(e); ok {
					literals = append(literals, brSQLText{origin: fset.Position(e.Pos()).String(), text: s})
					return false
				}
			case *ast.BasicLit:
				if s, ok := brFold(e); ok {
					literals = append(literals, brSQLText{origin: fset.Position(e.Pos()).String(), text: s})
				}
			}
			return true
		})
	}
	return templates, literals, nil
}

// brFold returns the value of a string literal or of a + concatenation of
// string literals.
func brFold(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return brFold(v.X)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok := brFold(v.X)
		if !ok {
			return "", false
		}
		r, ok := brFold(v.Y)
		if !ok {
			return "", false
		}
		return l + r, true
	}
	return "", false
}

var (
	brTemplateAction = regexp.MustCompile(`(?s)\{\{.*?\}\}`)
	brBlockComment   = regexp.MustCompile(`(?s)/\*.*?\*/`)
	brLineComment    = regexp.MustCompile(`--[^\n]*`)
	brSpaces         = regexp.MustCompile(`\s+`)
)

// brNormalize removes comments, collapses whitespace and lower-cases.
func brNormalize(s string) string {
	s = brBlockComment.ReplaceAllString(s, " ")
	s = brLineComment.ReplaceAllString(s, " ")
	s = brSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(strings.ToLower(s))
}

type brTok struct {
	kind byte // 'w' word, 'q' quoted identifier, 's' string, 'p' punctuation or number
	text string
}

// brTokenize splits normalised SQL into tokens. Unlike the sweep tokenizer it
// accepts semicolons, dollar signs and backslashes, which the receiver's SQL
// and Go format strings use; an unterminated string runs to the end.
func brTokenize(s string) []brTok {
	var out []brTok
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ':
			i++
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(s) {
				if s[j] == c {
					if j+1 < len(s) && s[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			kind := byte('s')
			if c == '"' {
				kind = 'q'
			}
			out = append(out, brTok{kind, s[i+1 : min(j, len(s))]})
			i = j + 1
		case c == '_' || c >= 'a' && c <= 'z':
			j := i
			for j < len(s) && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			out = append(out, brTok{'w', s[i:j]})
			i = j
		default:
			out = append(out, brTok{'p', string(c)})
			i++
		}
	}
	return out
}

// brIsStatement reports whether a Go literal starts like a SQL statement.
func brIsStatement(s string) bool {
	toks := brTokenize(brNormalize(s))
	for _, t := range toks {
		if t.kind == 'p' && t.text == "(" {
			continue
		}
		return t.kind == 'w' && brStatementStarters[t.text]
	}
	return false
}

func isNotIdent(r rune) bool {
	return r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9')
}

// checkSQL returns the violations of one SQL text. isTemplate marks a text
// that is SQL as a whole (a template); a Go literal is checked for keywords
// only when it starts like a statement.
func checkSQL(t brSQLText, isTemplate bool) []brViolation {
	text := t.text
	if isTemplate {
		text = brTemplateAction.ReplaceAllString(text, " ")
	}
	norm := brNormalize(text)
	var out []brViolation
	for _, tok := range brTokenize(norm) {
		switch tok.kind {
		case 'w', 'q':
			if brForbiddenNames[tok.text] {
				out = append(out, brViolation{t.origin, norm, "forbidden name " + tok.text})
			}
		case 's':
			for _, w := range strings.FieldsFunc(tok.text, isNotIdent) {
				if brForbiddenNames[w] {
					out = append(out, brViolation{t.origin, norm, "forbidden name " + w + " (in a string)"})
				}
			}
		}
	}
	if !isTemplate && (!brIsStatement(text) || brNonSQLLiterals[norm]) {
		return out
	}
	for stmt := range strings.SplitSeq(norm, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		exempt := map[string]bool{}
		allowed, isException := brStatementExceptions[stmt]
		for _, k := range allowed {
			exempt[k] = true
		}
		toks := brTokenize(stmt)
		first := ""
		for _, tok := range toks {
			if tok.kind == 'p' && tok.text == "(" {
				continue
			}
			if tok.kind == 'w' {
				first = tok.text
			}
			break
		}
		if !isException && first != "select" && first != "with" && first != "show" && first != "explain" && first != "values" {
			out = append(out, brViolation{t.origin, stmt, fmt.Sprintf("statement starts with %q, not SELECT, WITH, SHOW, EXPLAIN or VALUES", first)})
		}
		for i, tok := range toks {
			if tok.kind != 'w' || !brForbiddenKeywords[tok.text] || exempt[tok.text] {
				continue
			}
			// "relname AS table" is a column alias, not the TABLE command.
			if tok.text == "table" && i > 0 && toks[i-1].kind == 'w' && toks[i-1].text == "as" {
				continue
			}
			out = append(out, brViolation{t.origin, stmt, "forbidden keyword " + strings.ToUpper(tok.text)})
		}
	}
	return out
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
	t.Logf("BR-3 scan: %d templates, %d Go literals, %d of them SQL statements", len(templates), len(literals), statements)
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
		{name: "a one-word statement that is not exempt", text: "VACUUM"},
		{name: "an exempt word with more text", text: "DELETE FROM t"},
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
