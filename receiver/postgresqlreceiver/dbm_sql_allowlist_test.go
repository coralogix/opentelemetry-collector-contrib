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
// forbiddenNames, at 9918271, coralogix/coralogix-otel-collector#236); keep
// them in sync. It runs in the package's unit
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

// forbiddenKeywords: copied from pkg/dbmsweep (BR-3, cdot 9918271). Statement words that
// write, change the schema, take locks, change settings or control
// transactions. SELECT ... INTO creates a table and FOR UPDATE/SHARE takes
// row locks, so INTO and FOR are forbidden.
var brForbiddenKeywords = map[string]bool{
	"insert": true, "update": true, "delete": true, "merge": true, "truncate": true,
	"create": true, "alter": true, "drop": true, "grant": true, "revoke": true,
	"lock": true, "copy": true, "call": true, "do": true, "execute": true,
	"prepare": true, "deallocate": true, "vacuum": true, "analyze": true, "analyse": true, //nolint:misspell // PostgreSQL also accepts this spelling
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
// receiver's EXPLAIN path (client.go explainQuery), on one connection taken
// from the pool: SET plan_cache_mode (it stays set on that pooled connection
// after it returns to the pool: a planner setting, no data change), a
// prepared statement explained without ANALYZE (the statement never runs),
// and DEALLOCATE.
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
// statement for this test: every PostgreSQL command word (review I1). A
// literal that starts with any other word (a log message, a fragment) is
// checked for forbidden names and denied functions only. Only SELECT, WITH,
// SHOW, EXPLAIN and VALUES may start a statement the receiver sends.
var brStatementStarters = map[string]bool{
	"abort": true, "alter": true, "analyse": true, "analyze": true, "begin": true, //nolint:misspell // PostgreSQL also accepts this spelling
	"call": true, "checkpoint": true, "close": true, "cluster": true, "comment": true,
	"commit": true, "copy": true, "create": true, "deallocate": true, "declare": true,
	"delete": true, "discard": true, "do": true, "drop": true, "end": true,
	"execute": true, "explain": true, "fetch": true, "grant": true, "import": true,
	"insert": true, "listen": true, "load": true, "lock": true, "merge": true,
	"move": true, "notify": true, "prepare": true, "reassign": true, "refresh": true,
	"reindex": true, "release": true, "reset": true, "revoke": true, "rollback": true,
	"savepoint": true, "security": true, "select": true, "set": true, "show": true,
	"start": true, "table": true, "truncate": true, "unlisten": true, "update": true,
	"vacuum": true, "values": true, "with": true,
}

// brAllowedStarts are the only first words of a statement the receiver sends.
var brAllowedStarts = map[string]bool{"select": true, "with": true, "show": true, "explain": true, "values": true}

// brDeniedFunctions are writing, session-changing and admin functions
// (review I2). They are rejected anywhere a name is followed by "(", also in
// fragments, before the allow-list is consulted.
var brDeniedFunctions = regexp.MustCompile(`^(.*_reset|set_config|setval|nextval|pg_(try_)?advisory_.*|pg_terminate_backend|pg_cancel_backend|pg_reload_conf|pg_rotate_logfile|lo_.*|pg_read_(binary_)?file|pg_ls_.*|pg_file_.*|dblink.*|.*replication_slot.*|pg_logical_.*|pg_replication_origin_.*|pg_switch_wal|pg_promote|pg_create_restore_point|pg_backup_.*|txid_current|pg_current_xact_id|pg_sleep.*)$`)

// brAllowedFunctions are the functions the receiver's SQL calls today
// (review I2): any other function in a statement or template is rejected.
// Extend it in a reviewed change.
var brAllowedFunctions = map[string]bool{
	"array_length": true, "array_to_string": true, "cardinality": true, "cast": true,
	"clock_timestamp": true, "coalesce": true, "count": true, "current_database": true,
	"current_setting": true, "extract": true, "lower": true, "max": true,
	"pg_backend_pid": true, "pg_blocking_pids": true, "pg_current_wal_lsn": true,
	"pg_database_size": true, "pg_relation_size": true, "pg_total_relation_size": true,
	"pg_wal_lsn_diff": true, "regexp_match": true, "replace": true, "sum": true,
	"to_char": true, "to_timestamp": true, "trim": true, "unnest": true,
	// fork patch P7 (#48): the query-sample template reads octet_length(sa.query)
	"octet_length": true,
}

// brKeywordsBeforeParen are SQL words that may precede "(" without being a
// function call.
var brKeywordsBeforeParen = map[string]bool{
	"filter": true, "over": true, "in": true, "exists": true, "as": true,
	"and": true, "or": true, "not": true, "where": true, "from": true,
	"select": true, "on": true, "when": true, "then": true, "else": true,
	"case": true, "values": true, "using": true, "lateral": true, "all": true,
	"any": true, "some": true, "join": true, "with": true, "distinct": true, "by": true,
	"having": true, "explain": true, "union": true, "except": true, "intersect": true,
	"is": true, "like": true, "ilike": true, "between": true, "array": true, "row": true,
	"limit": true, "offset": true, "nulls": true, "into": true, "returning": true,
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
	stmt := v.statement
	if len(stmt) > 160 {
		stmt = stmt[:160] + "..."
	}
	return fmt.Sprintf("%s: %s (statement: %q)", v.origin, v.reason, stmt)
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
	brSpaces         = regexp.MustCompile(`\s+`)
	brDollarTag      = regexp.MustCompile(`^\$[A-Za-z_]*\$`)
)

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// brStatements lexes SQL into its statements. Comments are removed outside
// strings only (review M5); E” strings take backslash escapes (review M6);
// E” and dollar-quoted bodies are written back as standard strings; text
// outside strings is lower-cased; whitespace is collapsed. Statements are
// split at semicolons outside strings. ok is false when a string, quoted
// identifier, dollar quote or comment does not end.
func brStatements(sql string) (stmts []string, ok bool) {
	var b strings.Builder
	flush := func() {
		if st := strings.TrimSpace(brSpaces.ReplaceAllString(b.String(), " ")); st != "" {
			stmts = append(stmts, st)
		}
		b.Reset()
	}
	quote := func(body string) string { return "'" + strings.ReplaceAll(body, "'", "''") + "'" }
	n := len(sql)
	for i := 0; i < n; {
		c := sql[i]
		switch {
		case (c == 'e' || c == 'E') && i+1 < n && sql[i+1] == '\'' && (i == 0 || !isIdentByte(sql[i-1])):
			var body strings.Builder
			j, closed := i+2, false
			for j < n && !closed {
				switch {
				case sql[j] == '\\' && j+1 < n:
					body.WriteByte(sql[j+1])
					j += 2
				case sql[j] == '\'' && j+1 < n && sql[j+1] == '\'':
					body.WriteByte('\'')
					j += 2
				case sql[j] == '\'':
					closed = true
					j++
				default:
					body.WriteByte(sql[j])
					j++
				}
			}
			if !closed {
				return stmts, false
			}
			b.WriteString(quote(body.String()))
			i = j
		case c == '\'' || c == '"':
			j, closed := i+1, false
			for j < n && !closed {
				switch {
				case sql[j] == c && j+1 < n && sql[j+1] == c:
					j += 2
				case sql[j] == c:
					closed = true
					j++
				default:
					j++
				}
			}
			if !closed {
				return stmts, false
			}
			b.WriteString(sql[i:j])
			i = j
		case c == '$' && brDollarTag.MatchString(sql[i:]):
			tag := brDollarTag.FindString(sql[i:])
			end := strings.Index(sql[i+len(tag):], tag)
			if end < 0 {
				return stmts, false
			}
			b.WriteString(quote(sql[i+len(tag) : i+len(tag)+end]))
			i += 2*len(tag) + end
		case strings.HasPrefix(sql[i:], "--"):
			for i < n && sql[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case strings.HasPrefix(sql[i:], "/*"):
			depth := 0
			for {
				if i >= n {
					return stmts, false
				}
				open, closeHere := strings.HasPrefix(sql[i:], "/*"), strings.HasPrefix(sql[i:], "*/")
				switch {
				case open:
					depth++
					i += 2
				case closeHere:
					depth--
					i += 2
				default:
					i++
				}
				if closeHere && depth == 0 {
					break
				}
			}
			b.WriteByte(' ')
		case c == ';':
			flush()
			i++
		default:
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			b.WriteByte(c)
			i++
		}
	}
	flush()
	return stmts, true
}

type brTok struct {
	kind byte // 'w' word, 'q' quoted identifier, 's' string, 'p' punctuation or number
	text string
}

// brTokenize splits lower-cased SQL into tokens. An unterminated string runs
// to the end (the lenient pass over any literal relies on this).
func brTokenize(s string) []brTok {
	var out []brTok
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
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

// brFirstWord returns the first word of a literal, after leading space,
// comments and parentheses.
func brFirstWord(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n(")
		switch {
		case strings.HasPrefix(s, "/*"):
			end := strings.Index(s, "*/")
			if end < 0 {
				return ""
			}
			s = s[end+2:]
		case strings.HasPrefix(s, "--"):
			end := strings.IndexByte(s, '\n')
			if end < 0 {
				return ""
			}
			s = s[end+1:]
		default:
			j := 0
			for j < len(s) && isIdentByte(s[j]) {
				j++
			}
			return strings.ToLower(s[:j])
		}
	}
}

// brNormalize returns the statements of s, normalised, joined by "; ".
func brNormalize(s string) string {
	stmts, _ := brStatements(s)
	return strings.Join(stmts, "; ")
}

// brIsStatement reports whether a Go literal starts like a SQL statement.
func brIsStatement(s string) bool { return brStatementStarters[brFirstWord(s)] }

func isNotIdent(r rune) bool {
	return r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9')
}

// brNameAndFunctionViolations checks tokens for forbidden names and for
// function calls: denied functions always, and, when allowList is set,
// functions outside brAllowedFunctions.
func brNameAndFunctionViolations(origin, stmt string, toks []brTok, allowList bool) []brViolation {
	var out []brViolation
	for i, tok := range toks {
		switch tok.kind {
		case 'w', 'q':
			if brForbiddenNames[tok.text] {
				out = append(out, brViolation{origin, stmt, "forbidden name " + tok.text})
			}
		case 's':
			for _, w := range strings.FieldsFunc(strings.ToLower(tok.text), isNotIdent) {
				if brForbiddenNames[w] {
					out = append(out, brViolation{origin, stmt, "forbidden name " + w + " (in a string)"})
				}
			}
		}
		if tok.kind != 'w' || i+1 >= len(toks) || toks[i+1].kind != 'p' || toks[i+1].text != "(" {
			continue
		}
		if brKeywordsBeforeParen[tok.text] || brStatementStarters[tok.text] {
			continue
		}
		// A type modifier (::numeric(10,2), AS varchar(10)) is not a call.
		if i > 0 && (toks[i-1].kind == 'p' && toks[i-1].text == ":" || toks[i-1].kind == 'w' && toks[i-1].text == "as") {
			continue
		}
		switch {
		case brDeniedFunctions.MatchString(tok.text):
			out = append(out, brViolation{origin, stmt, "denied function " + tok.text})
		case allowList && !brAllowedFunctions[tok.text]:
			out = append(out, brViolation{origin, stmt, "function " + tok.text + " is not in the allow-list"})
		}
	}
	return out
}

// checkSQL returns the violations of one SQL text. isTemplate marks a text
// that is SQL as a whole (a template); a Go literal is checked for keywords,
// the statement start and the function allow-list only when it starts like
// a statement. Forbidden names and denied functions are checked in every
// text, over a lenient tokenization of the whole raw text (comments
// included, so nothing hides in one).
func checkSQL(t brSQLText, isTemplate bool) []brViolation {
	text := t.text
	if isTemplate {
		text = brTemplateAction.ReplaceAllString(text, " ")
	}
	lenient := strings.TrimSpace(strings.ToLower(brSpaces.ReplaceAllString(text, " ")))
	out := brNameAndFunctionViolations(t.origin, lenient, brTokenize(lenient), false)
	if !isTemplate && (!brIsStatement(text) || brNonSQLLiterals[lenient]) {
		return out
	}
	stmts, ok := brStatements(text)
	if !ok {
		return append(out, brViolation{t.origin, lenient, "unterminated string, quoted identifier, dollar quote or comment"})
	}
	for _, stmt := range stmts {
		exempt := map[string]bool{}
		allowed, isException := brStatementExceptions[stmt]
		for _, k := range allowed {
			exempt[k] = true
		}
		toks := brTokenize(stmt)
		out = append(out, brNameAndFunctionViolations(t.origin, stmt, toks, true)...)
		first := brFirstWord(stmt)
		if !isException && !brAllowedStarts[first] {
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
	for _, v := range violations {
		t.Errorf("BR-3: receiver SQL breaks the allow-list: %s", v)
	}
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
		// review I1: every PostgreSQL command word starts a statement
		{name: "start transaction read write", text: "START TRANSACTION READ WRITE"},
		{name: "end", text: "END"},
		{name: "abort", text: "ABORT"},
		{name: "declare cursor", text: "DECLARE c CURSOR WITH HOLD FOR SELECT 1"},
		{name: "fetch", text: "FETCH 10 FROM c"},
		{name: "move", text: "MOVE NEXT IN c"},
		{name: "close", text: "CLOSE c"},
		{name: "release", text: "RELEASE SAVEPOINT s"},
		{name: "reassign owned", text: "REASSIGN OWNED BY a TO b"},
		// review I2: writing and admin functions, and any function outside the allow-list
		{name: "stats reset", text: "SELECT pg_stat_statements_reset()"},
		{name: "qualified stats reset", text: "SELECT pg_catalog.pg_stat_reset()"},
		{name: "set_config", text: "SELECT set_config('log_statement', 'none', false)"},
		{name: "setval", text: "SELECT setval('s', 1)"},
		{name: "nextval", text: "SELECT nextval('s')"},
		{name: "advisory lock", text: "SELECT pg_advisory_lock(1)"},
		{name: "terminate backend", text: "SELECT pg_terminate_backend(pid) FROM pg_stat_activity"},
		{name: "cancel backend", text: "SELECT pg_cancel_backend(1)"},
		{name: "reload conf", text: "SELECT pg_reload_conf()"},
		{name: "large object", text: "SELECT lo_import('/etc/passwd')"},
		{name: "read file", text: "SELECT pg_read_file('postgresql.conf')"},
		{name: "read binary file", text: "SELECT pg_read_binary_file('x')"},
		{name: "dblink", text: "SELECT * FROM dblink('host=x', 'DELETE FROM t') AS t(a int)"},
		{name: "replication slot", text: "SELECT pg_create_logical_replication_slot('s', 'pgoutput')"},
		{name: "denied function in a fragment", text: " AND pg_terminate_backend(pid)"},
		{name: "function outside the allow-list", text: "SELECT pg_sleep(10)"},
		// review I3: the British spelling
		{name: "british spelling of analyze", text: "ANALYSE t"},                      //nolint:misspell // the PostgreSQL keyword under test
		{name: "explain with the british spelling", text: "EXPLAIN ANALYSE SELECT 1"}, //nolint:misspell // the PostgreSQL keyword under test
		// review M5: comment markers inside a string never hide what follows
		{name: "dash-dash in a string", text: "SELECT '--', histogram_bounds FROM x"},
		{name: "comment opener in a string", text: "SELECT '/*' AS a; DELETE FROM t; SELECT '*/'"},
		// review M6: E'' strings take backslash escapes
		// E'\'' is one quote; a tokenizer without escapes would read the rest as one string.
		{name: "escaped quote in an E string", text: `SELECT E'\'' ; DELETE FROM t; --'`},
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
		"failed to update the cache",                                              // a log message, not a statement
		"SELECT E'it\\'s' AS a",                                                   // an escaped quote stays inside the string
		"SELECT pg_catalog.pg_database_size(datname) FROM pg_catalog.pg_database", // an allow-listed function
	} {
		assert.Empty(t, checkSQL(brSQLText{origin: "probe", text: ok}, false), "must be allowed: %q", ok)
	}
}

// TestReceiverSQLCallSites (review I4): every SQL argument of a database call
// in the package resolves to a literal, a constant, an embedded template or a
// listed exception, so no SQL text can bypass TestReceiverSQLAllowList.
func TestReceiverSQLCallSites(t *testing.T) {
	sites, violations, err := checkCallSites(".")
	require.NoError(t, err)
	t.Logf("BR-3 call sites: %d database calls checked", len(sites))
	assert.GreaterOrEqual(t, len(sites), 30, "the call-site pass found too few database calls")
	assert.Empty(t, violations, "BR-3: a database call takes SQL that the allow-list scan cannot see")
}

// TestCallSiteCheckRejects proves the call-site pass on synthetic code.
func TestCallSiteCheckRejects(t *testing.T) {
	const header = "package p\n\nimport (\n\t\"context\"\n\t\"fmt\"\n\t\"os\"\n\t\"strings\"\n)\n\n" +
		"//go:embed templates/q.tmpl\nvar qs string\n\nvar qt = template.Must(template.New(\"q\").Parse(qs))\n\n" +
		"type O string\n\nfunc (o O) column() string {\n\tswitch o {\n\tcase \"a\":\n\t\treturn \"calls\"\n\t}\n\treturn \"\"\n}\n\n"
	for _, tc := range []struct {
		name, src string
		bad       bool
	}{
		{name: "literal", src: `func f(c C, ctx context.Context) { c.db.QueryContext(ctx, "SELECT 1") }`},
		{name: "const", src: "const q = `SELECT 1`\nfunc f(c C, ctx context.Context) { c.db.QueryRowContext(ctx, q) }"},
		{name: "local literal", src: `func f(c C, ctx context.Context) { q := "SELECT 1"; c.db.QueryContext(ctx, q) }`},
		{name: "helper param fed by literals", src: "func h(c C, ctx context.Context, q string) { c.db.QueryContext(ctx, q) }\nfunc f(c C, ctx context.Context) { h(c, ctx, `SELECT 1`) }"},
		{name: "environment", src: `func f(c C, ctx context.Context) { c.db.QueryContext(ctx, os.Getenv("Q")) }`, bad: true},
		{name: "appended input", src: `func f(c C, ctx context.Context, s string) { q := "SELECT "; q += s; c.db.ExecContext(ctx, q) }`, bad: true},
		{name: "sprintf with an unlisted argument", src: `func f(c C, ctx context.Context, s string) { c.db.ExecContext(ctx, fmt.Sprintf("SELECT %s", s)) }`, bad: true},
		{name: "helper param fed by input", src: "func h(c C, ctx context.Context, q string) { c.db.QueryContext(ctx, q) }\nfunc f(c C, ctx context.Context, s string) { h(c, ctx, s) }", bad: true},
		{name: "computed text", src: `func f(c C, ctx context.Context) { c.db.QueryContext(ctx, strings.ToUpper("select 1")) }`, bad: true},
		{name: "non-template Execute", src: `func f(t T) { t.Execute(nil, nil) }`, bad: true},
		// review round 2: I1 template data
		{name: "template data: literal, numeric and bool params, quoted list, listed method", src: `func f(c C, ctx context.Context, limit int64, ts float64, top, since bool, dbs []string, o O) {
	order := o.column()
	buf := bytes.Buffer{}
	_ = qt.Execute(&buf, map[string]any{"a": "x", "limit": limit, "ts": ts, "top": top && since, "dbs": quoteDatabaseList(dbs), "orderBy": order, "n": 3})
	c.db.QueryContext(ctx, buf.String())
}`},
		{name: "template data from the environment", src: `func f(c C, ctx context.Context) {
	buf := bytes.Buffer{}
	_ = qt.Execute(&buf, map[string]any{"orderBy": os.Getenv("Q")})
	c.db.QueryContext(ctx, buf.String())
}`, bad: true},
		{name: "template data from a string parameter", src: `func f(c C, ctx context.Context, s string) {
	buf := bytes.Buffer{}
	_ = qt.Execute(&buf, map[string]any{"orderBy": s})
	c.db.QueryContext(ctx, buf.String())
}`, bad: true},
		{name: "template data map in a variable", src: `func f(c C, ctx context.Context, m map[string]any) {
	buf := bytes.Buffer{}
	_ = qt.Execute(&buf, m)
	c.db.QueryContext(ctx, buf.String())
}`, bad: true},
		{name: "ExecuteTemplate data from the environment", src: `func f(c C, ctx context.Context) {
	buf := bytes.Buffer{}
	_ = qt.ExecuteTemplate(&buf, "q", map[string]any{"orderBy": os.Getenv("Q")})
	c.db.QueryContext(ctx, buf.String())
}`, bad: true},
		{name: "template data from an unlisted method", src: `func f(c C, ctx context.Context, o O) {
	buf := bytes.Buffer{}
	_ = qt.Execute(&buf, map[string]any{"orderBy": o.String()})
	c.db.QueryContext(ctx, buf.String())
}`, bad: true},
		// review round 2: minors
		{name: "M2 write through a pointer to the local", src: `func f(c C, ctx context.Context) {
	q := "SELECT 1"
	p := &q
	*p = os.Getenv("Q")
	c.db.QueryContext(ctx, q)
}`, bad: true},
		{name: "M3 method value", src: `func f(c C, ctx context.Context) {
	run := c.db.QueryContext
	run(ctx, os.Getenv("Q"))
}`, bad: true},
		{name: "M4 package-level function literal", src: "var run = func(c C, ctx context.Context) { c.db.QueryContext(ctx, os.Getenv(\"Q\")) }", bad: true},
		{name: "M5 aliased NewDbClient", src: `func f(c C) { _ = sq.NewDbClient(nil, os.Getenv("Q"), nil, nil) }`, bad: true},
		{name: "M5 DbSQLClient literal", src: `func f() { _ = sqlquery.DbSQLClient{SQL: "SELECT 1"} }`, bad: true},
		{name: "M6 template buffer written elsewhere", src: `func f(c C, ctx context.Context) {
	buf := bytes.Buffer{}
	_ = qt.Execute(&buf, map[string]any{})
	buf.WriteString(os.Getenv("Q"))
	c.db.QueryContext(ctx, buf.String())
}`, bad: true},
		{name: "closure parameter shadows a literal local", src: `func f(c C, ctx context.Context) {
	query := "SELECT 1"
	run := func(query string) { c.db.QueryContext(ctx, query) }
	run(os.Getenv("Q"))
	_ = query
}`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "probe.go", header+tc.src, parser.ParseComments|parser.SkipObjectResolution)
			require.NoError(t, err)
			_, violations := checkCallSitesFiles(fset, []*ast.File{f})
			if tc.bad {
				assert.NotEmpty(t, violations, "must be rejected")
			} else {
				assert.Empty(t, violations, "must be accepted")
			}
		})
	}
}

// brCallSite is one database call found by the call-site pass.
type brCallSite struct{ origin, call string }

// brSprintfExceptions are the only non-literal fmt.Sprintf arguments allowed
// in SQL, keyed "<function>:<argument>", each with its reason (review I4).
var brSprintfExceptions = map[string]string{
	"explainQuery:normalizedQueryID": "the pg_stat_statements queryid ('-' replaced by '_') names the prepared statement",
	"explainQuery:nullsString":       `"(null, ...)" built from literal nulls, one per parameter`,
	"explainQuery:query":             "the monitored statement itself, prepared and explained without ANALYZE (EXPLAIN-path exception; P8 replaces this path)",
}

// brSQLHelpers return the SQL of their first argument plus quoted literals.
var brSQLHelpers = map[string]string{
	"filterQueryByDatabases": "appends WHERE/AND datname IN (...) with database names quoted as literals by quoteDatabaseList, and GROUP BY datname",
}

// brTemplateDataFuncs may produce template data (review round 2, I1).
var brTemplateDataFuncs = map[string]string{
	"quoteDatabaseList": "quotes every database name as a SQL literal (pq.QuoteLiteral)",
}

// brLiteralMethods may produce template data when every return of the
// method is a string literal; the pass checks that.
var brLiteralMethods = map[string]string{
	"column": "TopQueryCandidateOrder.column: a fixed column name per enum value, \"\" otherwise",
}

// brNumericTypes are parameter or variable types whose values cannot carry
// SQL text into a template.
var brNumericTypes = map[string]bool{
	"bool": true, "int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true,
}

// brDBMethods maps a database call to the index of its SQL argument.
var brDBMethods = map[string]int{
	"QueryContext": 1, "QueryRowContext": 1, "ExecContext": 1, "PrepareContext": 1,
	"Query": 0, "QueryRow": 0, "Exec": 0, "Prepare": 0, "NewDbClient": 1,
}

// brSelectorValueExceptions are selectors named like a database method that
// are not method values, keyed "<function>:<selector>" (the pass has no type
// information, so a field cannot be told from a method otherwise).
var brSelectorValueExceptions = map[string]string{
	"obfuscateSQL:obfuscatedQuery.Query": "the Query string field of the obfuscator's ObfuscatedQuery result",
}

// brTemplateMethods maps a template call to the index of its data argument.
var brTemplateMethods = map[string]int{"Execute": 1, "ExecuteTemplate": 2}

type brCall struct {
	call *ast.CallExpr
	fn   *ast.FuncDecl
}

type brPkg struct {
	fset       *token.FileSet
	consts     map[string]ast.Expr
	embeds     map[string]bool // vars filled by //go:embed templates/...
	templates  map[string]bool // vars that parse an embedded template
	funcs      map[string][]*ast.FuncDecl
	calls      []brCall
	violations []brViolation
}

// checkCallSites runs the call-site pass over the package's non-test files.
func checkCallSites(dir string) ([]brCallSite, []brViolation, error) {
	goFiles, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, path := range goFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if parseErr != nil {
			return nil, nil, parseErr
		}
		files = append(files, f)
	}
	sites, violations := checkCallSitesFiles(fset, files)
	return sites, violations, nil
}

// brWalk visits every node under root with its parent and grandparent.
func brWalk(root ast.Node, visit func(n, parent, grand ast.Node)) {
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		var parent, grand ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		if len(stack) > 1 {
			grand = stack[len(stack)-2]
		}
		visit(n, parent, grand)
		stack = append(stack, n)
		return true
	})
}

// checkCallSitesFiles runs the call-site pass over parsed files: every SQL
// argument of a database call or NewDbClient, and every template Execute
// (its receiver and its data), must resolve to text the allow-list scan
// sees; method values and DbSQLClient literals are rejected outright.
func checkCallSitesFiles(fset *token.FileSet, files []*ast.File) ([]brCallSite, []brViolation) {
	p := &brPkg{fset: fset, consts: map[string]ast.Expr{}, embeds: map[string]bool{}, templates: map[string]bool{}, funcs: map[string][]*ast.FuncDecl{}}
	vars := map[string]ast.Expr{}
	var bodies []*ast.FuncDecl
	var others []ast.Node // package-level initializers that are not function literals
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					embedded := false
					for _, cg := range []*ast.CommentGroup{d.Doc, vs.Doc} {
						if cg == nil {
							continue
						}
						for _, c := range cg.List {
							embedded = embedded || strings.HasPrefix(c.Text, "//go:embed templates/")
						}
					}
					for i, name := range vs.Names {
						switch {
						case d.Tok == token.CONST && i < len(vs.Values):
							p.consts[name.Name] = vs.Values[i]
						case d.Tok == token.VAR && embedded:
							p.embeds[name.Name] = true
						case d.Tok == token.VAR && i < len(vs.Values):
							vars[name.Name] = vs.Values[i]
							// review round 2, M4: a package-level function literal is
							// checked like a function named after its variable.
							if lit, ok := vs.Values[i].(*ast.FuncLit); ok {
								bodies = append(bodies, &ast.FuncDecl{Name: ast.NewIdent(name.Name), Type: lit.Type, Body: lit.Body})
							} else {
								others = append(others, vs.Values[i])
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Body != nil {
					bodies = append(bodies, d)
					p.funcs[d.Name.Name] = append(p.funcs[d.Name.Name], d)
				}
			}
		}
	}
	for name, init := range vars {
		ast.Inspect(init, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Parse" && len(c.Args) == 1 {
					if id, ok := c.Args[0].(*ast.Ident); ok && p.embeds[id.Name] {
						p.templates[name] = true
					}
				}
			}
			return true
		})
	}
	scan := func(root ast.Node, fn *ast.FuncDecl) {
		brWalk(root, func(n, parent, _ ast.Node) {
			switch v := n.(type) {
			case *ast.CallExpr:
				p.calls = append(p.calls, brCall{v, fn})
			case *ast.SelectorExpr:
				_, db := brDBMethods[v.Sel.Name]
				_, tm := brTemplateMethods[v.Sel.Name]
				fnName := ""
				if fn != nil {
					fnName = fn.Name.Name
				}
				if _, listed := brSelectorValueExceptions[fnName+":"+brExprString(v)]; listed {
					return
				}
				if c, ok := parent.(*ast.CallExpr); (db || tm) && (!ok || c.Fun != v) {
					// review round 2, M3: a method value hides the call site.
					p.violations = append(p.violations, brViolation{fset.Position(v.Pos()).String(), v.Sel.Name, "method value of " + v.Sel.Name + " (not in call position)"})
				}
			case *ast.CompositeLit:
				if name := brExprString(v.Type); name == "DbSQLClient" || strings.HasSuffix(name, ".DbSQLClient") {
					// review round 2, M5: build it with NewDbClient, which is checked.
					p.violations = append(p.violations, brViolation{fset.Position(v.Pos()).String(), name, "DbSQLClient literal (use NewDbClient)"})
				}
			}
		})
	}
	for _, fn := range bodies {
		scan(fn.Body, fn)
	}
	for _, init := range others {
		scan(init, nil)
	}

	var sites []brCallSite
	for _, c := range p.calls {
		sel, ok := c.call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		origin := fset.Position(c.call.Pos()).String()
		if dataIdx, isTmpl := brTemplateMethods[sel.Sel.Name]; isTmpl {
			sites = append(sites, brCallSite{origin, sel.Sel.Name})
			if x, ok := sel.X.(*ast.Ident); !ok || !p.templates[x.Name] {
				p.violations = append(p.violations, brViolation{origin, sel.Sel.Name, sel.Sel.Name + " on a value that is not an embedded SQL template"})
				continue
			}
			if why := p.resolveTemplateData(c.call, dataIdx, c.fn); why != "" {
				p.violations = append(p.violations, brViolation{origin, sel.Sel.Name, "template data: " + why})
			}
			continue
		}
		// review round 2, M5: NewDbClient is matched by name, whatever the
		// package is imported as.
		idx, isDB := brDBMethods[sel.Sel.Name]
		if !isDB {
			continue
		}
		sites = append(sites, brCallSite{origin, sel.Sel.Name})
		if idx >= len(c.call.Args) {
			p.violations = append(p.violations, brViolation{origin, sel.Sel.Name, "no SQL argument"})
			continue
		}
		if why := p.resolve(c.call.Args[idx], c.fn, 0); why != "" {
			p.violations = append(p.violations, brViolation{origin, sel.Sel.Name, why})
		}
	}
	return sites, p.violations
}

func brExprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return brExprString(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return brExprString(v.Fun) + "(...)"
	case *ast.BasicLit:
		return v.Value
	case nil:
		return ""
	}
	return fmt.Sprintf("%T", e)
}

// resolve returns "" when e is SQL the allow-list scan sees, else why not.
func (p *brPkg) resolve(e ast.Expr, fn *ast.FuncDecl, depth int) string {
	if depth > 8 {
		return "SQL argument resolves too deep"
	}
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			return ""
		}
		return "SQL argument is not a string"
	case *ast.ParenExpr:
		return p.resolve(v.X, fn, depth+1)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "SQL argument uses operator " + v.Op.String()
		}
		if why := p.resolve(v.X, fn, depth+1); why != "" {
			return why
		}
		return p.resolve(v.Y, fn, depth+1)
	case *ast.Ident:
		return p.resolveIdent(v.Name, fn, depth)
	case *ast.CallExpr:
		return p.resolveCall(v, fn, depth)
	}
	return fmt.Sprintf("SQL argument %s is not a literal, constant, template or listed exception", brExprString(e))
}

// brScopeProblem fails closed on what brLocals cannot follow (review round
// 2, M2 and closure scoping): the address of name is taken (a write through
// the pointer is invisible), or a function literal or range clause inside
// fn declares its own name.
func brScopeProblem(fn *ast.FuncDecl, name string) string {
	if fn == nil {
		return ""
	}
	why := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.UnaryExpr:
			if id, ok := v.X.(*ast.Ident); ok && v.Op == token.AND && id.Name == name {
				why = "the address of " + name + " is taken (writes through the pointer cannot be followed)"
			}
		case *ast.FuncLit:
			for _, field := range v.Type.Params.List {
				for _, id := range field.Names {
					if id.Name == name {
						why = "a function literal parameter shadows " + name
					}
				}
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{v.Key, v.Value} {
				if id, ok := e.(*ast.Ident); ok && id.Name == name {
					why = "a range variable shadows " + name
				}
			}
		}
		return why == ""
	})
	return why
}

func (p *brPkg) resolveIdent(name string, fn *ast.FuncDecl, depth int) string {
	if why := brScopeProblem(fn, name); why != "" {
		return why
	}
	locals, isParam, paramIdx := brLocals(fn, name)
	for _, rhs := range locals {
		if why := p.resolve(rhs, fn, depth+1); why != "" {
			return why
		}
	}
	if isParam {
		callers := p.callersOf(fn)
		if len(callers) == 0 {
			return fmt.Sprintf("parameter %s of %s has no caller in the package", name, fn.Name.Name)
		}
		for _, c := range callers {
			if paramIdx >= len(c.call.Args) {
				return fmt.Sprintf("call of %s lacks argument %d", fn.Name.Name, paramIdx)
			}
			if why := p.resolve(c.call.Args[paramIdx], c.fn, depth+1); why != "" {
				return fmt.Sprintf("parameter %s of %s: %s", name, fn.Name.Name, why)
			}
		}
	}
	if len(locals) > 0 || isParam {
		return ""
	}
	if v, ok := p.consts[name]; ok {
		return p.resolve(v, nil, depth+1)
	}
	if p.embeds[name] {
		return ""
	}
	return fmt.Sprintf("identifier %s is not a literal, constant, template or listed exception", name)
}

// brLocals returns every value assigned to name inside fn, whether name is a
// parameter of fn (and its index among the call arguments), and its
// declared parameter type.
func brLocals(fn *ast.FuncDecl, name string) ([]ast.Expr, bool, int) {
	locals, isParam, idx, _ := brLocalsTyped(fn, name)
	return locals, isParam, idx
}

func brLocalsTyped(fn *ast.FuncDecl, name string) ([]ast.Expr, bool, int, ast.Expr) {
	if fn == nil {
		return nil, false, 0, nil
	}
	var out []ast.Expr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != name {
					continue
				}
				if len(s.Rhs) == len(s.Lhs) {
					out = append(out, s.Rhs[i])
				} else {
					out = append(out, s.Rhs[0])
				}
			}
		case *ast.ValueSpec:
			for i, id := range s.Names {
				if id.Name != name {
					continue
				}
				if i < len(s.Values) {
					out = append(out, s.Values[i])
				} else if t, ok := s.Type.(*ast.Ident); ok && brNumericTypes[t.Name] {
					out = append(out, &ast.BasicLit{Kind: token.INT, Value: "0"})
				} else {
					out = append(out, s.Type)
				}
			}
		}
		return true
	})
	idx := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			idx++
			continue
		}
		for _, id := range field.Names {
			if id.Name == name {
				return out, true, idx, field.Type
			}
			idx++
		}
	}
	return out, false, 0, nil
}

func (p *brPkg) callersOf(fn *ast.FuncDecl) []brCall {
	var out []brCall
	for _, c := range p.calls {
		switch f := c.call.Fun.(type) {
		case *ast.Ident:
			if fn.Recv == nil && f.Name == fn.Name.Name {
				out = append(out, c)
			}
		case *ast.SelectorExpr:
			if fn.Recv != nil && f.Sel.Name == fn.Name.Name {
				out = append(out, c)
			}
		}
	}
	return out
}

func (p *brPkg) resolveCall(c *ast.CallExpr, fn *ast.FuncDecl, depth int) string {
	name := brExprString(c.Fun)
	switch {
	case name == "fmt.Sprintf" && len(c.Args) > 0:
		if why := p.resolve(c.Args[0], fn, depth+1); why != "" {
			return why
		}
		for _, arg := range c.Args[1:] {
			if id, ok := arg.(*ast.Ident); ok && fn != nil {
				if _, listed := brSprintfExceptions[fn.Name.Name+":"+id.Name]; listed {
					continue
				}
			}
			if why := p.resolve(arg, fn, depth+1); why != "" {
				return "fmt.Sprintf argument: " + why
			}
		}
		return ""
	case brSQLHelpers[name] != "" && len(c.Args) > 0:
		return p.resolve(c.Args[0], fn, depth+1)
	}
	// buf.String() of a buffer that only an embedded template wrote into.
	if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "String" && fn != nil {
		if buf, ok := sel.X.(*ast.Ident); ok {
			if why := p.templateBuffer(fn, buf.Name); why != "" {
				return why
			}
			return ""
		}
	}
	return fmt.Sprintf("SQL argument %s is not a literal, constant, template or listed exception", name)
}

// templateBuffer returns "" when buf is written only by an embedded
// template's Execute (review round 2, M6): every use of buf in fn is its
// declaration, &buf as the first Execute/ExecuteTemplate argument of an
// embedded template, or buf.String().
func (p *brPkg) templateBuffer(fn *ast.FuncDecl, buf string) string {
	execs, why := 0, ""
	brWalk(fn.Body, func(n, parent, grand ast.Node) {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != buf || why != "" {
			return
		}
		switch par := parent.(type) {
		case *ast.SelectorExpr:
			if par.Sel == id {
				return // a field or method named like the buffer
			}
			if c, ok := grand.(*ast.CallExpr); ok && c.Fun == par && par.Sel.Name == "String" && len(c.Args) == 0 {
				return
			}
		case *ast.AssignStmt:
			if par.Tok == token.DEFINE && len(par.Lhs) == 1 && par.Lhs[0] == id {
				return
			}
		case *ast.ValueSpec:
			return
		case *ast.UnaryExpr:
			if c, ok := grand.(*ast.CallExpr); ok && par.Op == token.AND && len(c.Args) > 0 && c.Args[0] == par {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
					if _, tm := brTemplateMethods[sel.Sel.Name]; tm {
						if x, ok := sel.X.(*ast.Ident); ok && p.templates[x.Name] {
							execs++
							return
						}
					}
				}
			}
		}
		why = fmt.Sprintf("buffer %s is used other than by an embedded template's Execute and String", buf)
	})
	if why != "" {
		return why
	}
	if execs == 0 {
		return fmt.Sprintf("buffer %s is not written by an embedded template", buf)
	}
	return ""
}

// resolveTemplateData returns "" when the data argument of a template call
// is a map literal whose every value is a literal or constant, a numeric or
// boolean value, quoteDatabaseList(...) or a listed literal-returning method
// (review round 2, I1).
func (p *brPkg) resolveTemplateData(c *ast.CallExpr, idx int, fn *ast.FuncDecl) string {
	if idx >= len(c.Args) {
		return "no data argument"
	}
	lit, ok := c.Args[idx].(*ast.CompositeLit)
	if !ok {
		return "data is not a map literal: " + brExprString(c.Args[idx])
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return "data entry is not key: value"
		}
		if why := p.resolveData(kv.Value, fn, 0); why != "" {
			return fmt.Sprintf("value of %s: %s", brExprString(kv.Key), why)
		}
	}
	return ""
}

func (p *brPkg) resolveData(e ast.Expr, fn *ast.FuncDecl, depth int) string {
	if depth > 8 {
		return "data resolves too deep"
	}
	switch v := e.(type) {
	case *ast.BasicLit:
		return ""
	case *ast.ParenExpr:
		return p.resolveData(v.X, fn, depth+1)
	case *ast.UnaryExpr:
		if v.Op == token.NOT || v.Op == token.SUB {
			return p.resolveData(v.X, fn, depth+1)
		}
	case *ast.BinaryExpr:
		if why := p.resolveData(v.X, fn, depth+1); why != "" {
			return why
		}
		return p.resolveData(v.Y, fn, depth+1)
	case *ast.Ident:
		if v.Name == "true" || v.Name == "false" {
			return ""
		}
		if why := brScopeProblem(fn, v.Name); why != "" {
			return why
		}
		locals, isParam, _, typ := brLocalsTyped(fn, v.Name)
		if isParam {
			// A reassigned numeric parameter still holds only a number.
			if t, ok := typ.(*ast.Ident); ok && brNumericTypes[t.Name] {
				return ""
			}
			return "parameter " + v.Name + " is not numeric or boolean"
		}
		if len(locals) > 0 {
			for _, rhs := range locals {
				if why := p.resolveData(rhs, fn, depth+1); why != "" {
					return why
				}
			}
			return ""
		}
		if c, ok := p.consts[v.Name]; ok {
			return p.resolveData(c, nil, depth+1)
		}
		return "identifier " + v.Name + " is not a literal, constant or numeric value"
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && brTemplateDataFuncs[id.Name] != "" {
			return ""
		}
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok && brLiteralMethods[sel.Sel.Name] != "" && p.returnsOnlyLiterals(sel.Sel.Name) {
			return ""
		}
		return "call " + brExprString(v.Fun) + " is not a listed data function or literal-returning method"
	}
	return fmt.Sprintf("%s is not a literal, constant or numeric value", brExprString(e))
}

// returnsOnlyLiterals reports whether every method named name in the
// package returns only string literals.
func (p *brPkg) returnsOnlyLiterals(name string) bool {
	decls := p.funcs[name]
	if len(decls) == 0 {
		return false
	}
	for _, d := range decls {
		ok := true
		ast.Inspect(d.Body, func(n ast.Node) bool {
			if r, isRet := n.(*ast.ReturnStmt); isRet {
				if len(r.Results) != 1 {
					ok = false
				} else if lit, isLit := r.Results[0].(*ast.BasicLit); !isLit || lit.Kind != token.STRING {
					ok = false
				}
			}
			return true
		})
		if !ok {
			return false
		}
	}
	return true
}
