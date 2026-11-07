// Package alertrules reads the shipped rule files under alerts/ for tests that
// bind a rule to the code it reads. Import from _test.go files only.
package alertrules

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

// Rule is one alerting rule of a rule file.
type Rule struct {
	Alert string
	Expr  string
}

// Load returns every rule of the rule file at path, failing the test when
// the file cannot be read or holds no rule.
func Load(t testing.TB, path string) []Rule {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Groups []struct {
			Rules []Rule
		}
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var rules []Rule
	for _, g := range file.Groups {
		rules = append(rules, g.Rules...)
	}
	if len(rules) == 0 {
		t.Fatalf("%s holds no rule", path)
	}
	return rules
}

// Expr returns the expression of the named alert in rules, failing the test
// when no rule has that name.
func Expr(t testing.TB, rules []Rule, alert string) string {
	t.Helper()
	for _, r := range rules {
		if r.Alert == alert {
			return r.Expr
		}
	}
	t.Fatalf("no rule named %s", alert)
	return ""
}

// LineFilter is one LogQL line filter: Contains for |=, otherwise !=.
type LineFilter struct {
	Text     string
	Contains bool
}

var lineFilter = regexp.MustCompile("(\\|=|!=|\\|~|!~)\\s*(`[^`]*`|\"(?:[^\"\\\\]|\\\\.)*\")")

// LineFilters returns the line filters of a LogQL expression in order. It
// fails the test on a regular-expression filter, which a substring match
// cannot apply.
func LineFilters(t testing.TB, expr string) []LineFilter {
	t.Helper()
	matches := lineFilter.FindAllStringSubmatch(expr, -1)
	out := make([]LineFilter, 0, len(matches))
	for _, m := range matches {
		op, lit := m[1], m[2]
		if op == "|~" || op == "!~" {
			t.Fatalf("regular-expression line filter %s %s is not supported", op, lit)
		}
		text := strings.Trim(lit, "`")
		if lit[0] == '"' {
			var err error
			if text, err = strconv.Unquote(lit); err != nil {
				t.Fatalf("line filter %s: %v", lit, err)
			}
		}
		out = append(out, LineFilter{Text: text, Contains: op == "|="})
	}
	if len(out) == 0 {
		t.Fatalf("no line filter in %q", expr)
	}
	return out
}

// Match reports whether line passes every filter, as Loki applies them.
func Match(line string, filters []LineFilter) bool {
	for _, f := range filters {
		if strings.Contains(line, f.Text) != f.Contains {
			return false
		}
	}
	return true
}
