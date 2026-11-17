package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/obs"
	"github.com/cplieger/plex-language-sync/internal/testsupport/alertrules"
)

const dashboardFile = "grafana-dashboard.json"

type dashQuery struct {
	Spec struct {
		Query struct {
			Spec struct {
				Expr string `json:"expr"`
			} `json:"spec"`
		} `json:"query"`
	} `json:"spec"`
}

type dashPanel struct {
	Spec struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Data        struct {
			Spec struct {
				Queries      []dashQuery `json:"queries"`
				QueryOptions struct {
					MaxDataPoints int `json:"maxDataPoints"`
				} `json:"queryOptions"`
			} `json:"spec"`
		} `json:"data"`
		VizConfig struct {
			Group string `json:"group"`
			Spec  struct {
				FieldConfig struct {
					Defaults struct {
						NoValue  string `json:"noValue"`
						Mappings []struct {
							Type    string          `json:"type"`
							Options json.RawMessage `json:"options"`
						} `json:"mappings"`
					} `json:"defaults"`
				} `json:"fieldConfig"`
			} `json:"spec"`
		} `json:"vizConfig"`
	} `json:"spec"`
}

type conditionRule struct {
	Kind string `json:"kind"`
	Spec struct {
		Value    any    `json:"value"`
		Variable string `json:"variable"`
		Operator string `json:"operator"`
	} `json:"spec"`
}

type conditionGroup struct {
	Spec struct {
		Visibility string          `json:"visibility"`
		Condition  string          `json:"condition"`
		Items      []conditionRule `json:"items"`
	} `json:"spec"`
}

type gridItem struct {
	Spec struct {
		ConditionalRendering *conditionGroup `json:"conditionalRendering"`
		Element              struct {
			Name string `json:"name"`
		} `json:"element"`
	} `json:"spec"`
}

type layoutNode struct {
	Spec struct {
		Layout               *layoutNode     `json:"layout"`
		ConditionalRendering *conditionGroup `json:"conditionalRendering"`
		Title                string          `json:"title"`
		Rows                 []layoutNode    `json:"rows"`
		Items                []gridItem      `json:"items"`
	} `json:"spec"`
}

type dashVariable struct {
	Kind string `json:"kind"`
	Spec struct {
		Name    string `json:"name"`
		Regex   string `json:"regex"`
		Refresh string `json:"refresh"`
		Sort    string `json:"sort"`
		Current struct {
			Value any `json:"value"`
		} `json:"current"`
		Query json.RawMessage `json:"query"`
	} `json:"spec"`
}

type dashboard struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Title     string               `json:"title"`
		Elements  map[string]dashPanel `json:"elements"`
		Layout    layoutNode           `json:"layout"`
		Variables []dashVariable       `json:"variables"`
		Preload   bool                 `json:"preload"`
	} `json:"spec"`
}

func loadDashboard(t *testing.T) dashboard {
	t.Helper()
	raw, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatalf("reading %s: %v", dashboardFile, err)
	}
	var d dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("decoding %s: %v", dashboardFile, err)
	}
	if len(d.Spec.Elements) == 0 {
		t.Fatalf("%s holds no panel", dashboardFile)
	}
	return d
}

func panelExprs(p *dashPanel) []string {
	var out []string
	for _, q := range p.Spec.Data.Spec.Queries {
		out = append(out, q.Spec.Query.Spec.Expr)
	}
	return out
}

func (d *dashboard) variables() map[string]dashVariable {
	out := map[string]dashVariable{}
	for _, v := range d.Spec.Variables {
		out[v.Spec.Name] = v
	}
	return out
}

// variableQuery is a query variable's PromQL, or "" for any other variable.
func variableQuery(v *dashVariable) string {
	var q struct {
		Spec struct {
			Query string `json:"query"`
		} `json:"spec"`
	}
	if v.Kind != "QueryVariable" || json.Unmarshal(v.Spec.Query, &q) != nil {
		return ""
	}
	return q.Spec.Query
}

// panelQueries is every panel query and every query variable's PromQL, with
// its # comments removed so no check reads a comment as a query.
func panelQueries(d *dashboard) map[string]string {
	queries := map[string]string{}
	for name, v := range d.variables() {
		if q := variableQuery(&v); q != "" {
			queries["variable "+name] = withoutComments(q)
		}
	}
	for name, p := range d.Spec.Elements {
		for i, e := range panelExprs(&p) {
			queries[fmt.Sprintf("%s %s query %d", name, p.Spec.Title, i)] = withoutComments(e)
		}
	}
	return queries
}

// skipString returns the index just past the string literal opening at s[i].
func skipString(s string, i int) int {
	q := s[i]
	for i++; i < len(s); i++ {
		switch {
		case s[i] == '\\' && q != '`':
			i++
		case s[i] == q:
			return i + 1
		}
	}
	return len(s)
}

// withoutComments drops each # comment from expr, up to its newline. A # inside
// a string literal opens no comment, and a quote inside a comment opens no
// string.
func withoutComments(expr string) string {
	var b strings.Builder
	for i := 0; i < len(expr); {
		switch expr[i] {
		case '#':
			if j := strings.IndexByte(expr[i:], '\n'); j >= 0 {
				i += j
			} else {
				i = len(expr)
			}
		case '"', '\'', '`':
			j := skipString(expr, i)
			b.WriteString(expr[i:j])
			i = j
		default:
			b.WriteByte(expr[i])
			i++
		}
	}
	return b.String()
}

// promqlCode is expr with its string literals and # comments blanked, so a
// name inside one is not read as code.
func promqlCode(expr string) string {
	b := []byte(expr)
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '"', '\'', '`':
			end := skipString(expr, i)
			for j := i; j < end; j++ {
				b[j] = ' '
			}
			i = end - 1
		case '#':
			for ; i < len(b) && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

type selectorBlock struct {
	name, text string
}

// selectors returns each {...} block of expr with the series name written
// before it, empty when none, a keyword (x or {...}) or a variable ($v{...})
// is. Quoted strings are skipped, so a brace inside a label value ends no
// block.
func selectors(expr string) []selectorBlock {
	var out []selectorBlock
	start := -1
	for i := 0; i < len(expr); i++ {
		switch c := expr[i]; {
		case c == '"' || c == '\'' || c == '`':
			i = skipString(expr, i) - 1
		case c == '{':
			start = i
		case c == '}' && start >= 0:
			before := strings.TrimRight(expr[:start], " ")
			name := before[len(strings.TrimRightFunc(before, func(r rune) bool {
				return r == '_' || r == ':' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
			})):]
			if slices.Contains(promqlKeywords, name) || strings.HasSuffix(before[:len(before)-len(name)], "$") {
				name = ""
			}
			out = append(out, selectorBlock{name: name, text: name + expr[start:i+1]})
			start = -1
		}
	}
	return out
}

func row(t *testing.T, d *dashboard, title string) *layoutNode {
	t.Helper()
	for i := range d.Spec.Layout.Spec.Rows {
		if r := &d.Spec.Layout.Spec.Rows[i]; r.Spec.Title == title {
			return r
		}
	}
	t.Fatalf("no row titled %q", title)
	return nil
}

var sampleRe = regexp.MustCompile(`(?m)^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{([^}]*)\})? `)

// exposition is the metrics page a fresh registry serves.
func exposition(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	obs.New("test").Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// servedLabels is every series a fresh registry serves, with the label
// names its samples carry.
func servedLabels(t *testing.T) map[string][]string {
	t.Helper()
	served := map[string][]string{}
	for _, m := range sampleRe.FindAllStringSubmatch(exposition(t), -1) {
		labels := served[m[1]]
		for _, l := range matcherNameRe.FindAllStringSubmatch(m[2], -1) {
			if !slices.Contains(labels, l[1]) {
				labels = append(labels, l[1])
			}
		}
		served[m[1]] = labels
	}
	if _, ok := served["plex_language_sync_build_info"]; !ok {
		t.Fatalf("Setup: the registry serves no plex_language_sync_build_info: %v", served)
	}
	return served
}

var (
	quotedRe      = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	selectorRe    = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^}]*)\}`)
	matcherNameRe = regexp.MustCompile(`([a-zA-Z_]\w*)\s*(?:=~|!~|!=|=)`)
	matchersRe    = regexp.MustCompile(`\{[^}]*\}`)
	groupingRe    = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\(([^)]*)\)`)
	rangeRe       = regexp.MustCompile(`\[[^\]]*\]`)
	variableRe    = regexp.MustCompile(`\$\{?\w+(?::\w+)?\}?`)
	labelValuesRe = regexp.MustCompile(`label_values\(([^,]+),\s*\w+\)`)
	functionRe    = regexp.MustCompile(`\b[a-z_]+\s*\(`)
	identRe       = regexp.MustCompile(`\b[a-zA-Z_:][a-zA-Z0-9_:]*\b`)
	alertsRe      = regexp.MustCompile(`ALERTS\{alertname="(\w+)"`)
	stallTermRe   = regexp.MustCompile(`ALERTS\{alertname="PlexLanguageSyncResolutionStalled"[^}]*cause="([^"]*)"[^}]*\}(?:\s*\*\s*0\s*\+\s*(\d+))?`)
	stallCauseRe  = regexp.MustCompile(`(?m)^plex_language_sync_user_resolution_stalled\{cause="([^"]*)"\} `)
	// bracedVariableRe matches ${name}, whose braces are no selector.
	bracedVariableRe = regexp.MustCompile(`\$\{\w+(?::\w+)?\}`)
	// absentCallRe matches a call of absent() or absent_over_time(); PromQL
	// lets whitespace stand before a call's parenthesis.
	absentCallRe = regexp.MustCompile(`(?:^|[^\w:])absent(?:_over_time)?\s*\(`)
)

// servedCauses is every stall cause a fresh registry serves, sorted.
func servedCauses(t *testing.T) []string {
	t.Helper()
	var served []string
	for _, m := range stallCauseRe.FindAllStringSubmatch(exposition(t), -1) {
		served = append(served, m[1])
	}
	if len(served) == 0 {
		t.Fatal("Setup: the registry serves no plex_language_sync_user_resolution_stalled series")
	}
	slices.Sort(served)
	return served
}

var promqlKeywords = []string{"and", "or", "unless", "bool", "offset"}

// seriesIn returns the series names a panel or variable query reads;
// label_values names a label after its series, which is no series.
func seriesIn(expr string) []string {
	expr = labelValuesRe.ReplaceAllString(expr, "$1")
	for _, re := range []*regexp.Regexp{matchersRe, quotedRe, groupingRe, rangeRe, variableRe, functionRe} {
		expr = re.ReplaceAllString(expr, " ")
	}
	var names []string
	for _, id := range identRe.FindAllString(expr, -1) {
		if !slices.Contains(promqlKeywords, id) && !slices.Contains(names, id) {
			names = append(names, id)
		}
	}
	return names
}

func TestDashboardIsSchemaV2(t *testing.T) {
	d := loadDashboard(t)
	if d.APIVersion != "dashboard.grafana.app/v2" || d.Kind != "Dashboard" {
		t.Errorf("apiVersion %q kind %q, want a dashboard.grafana.app/v2 Dashboard", d.APIVersion, d.Kind)
	}
	if d.Metadata.Name != "plex-language-sync" {
		t.Errorf("metadata.name = %q, want plex-language-sync, the uid an import keeps", d.Metadata.Name)
	}
	if d.Spec.Title != "Plex Language Sync" {
		t.Errorf("title = %q, want Plex Language Sync", d.Spec.Title)
	}
}

// TestDashboardReadsOnlyExportedSeries fails when a panel or a variable reads
// a series or a label plex-language-sync does not serve, so a renamed series
// fails the build instead of blanking a panel. It also fails a selector with
// no series name before its braces, which this test and the ALERTS pin cannot
// read. ALERTS is the ruler's series; instance is the scrape's label.
func TestDashboardReadsOnlyExportedSeries(t *testing.T) {
	served := servedLabels(t)
	d := loadDashboard(t)
	for where, expr := range panelQueries(&d) {
		for _, sel := range selectors(bracedVariableRe.ReplaceAllString(expr, "$$v")) {
			if sel.name == "" {
				t.Errorf("%s has a selector with no series name before its braces, which this check cannot read: %s", where, sel.text)
			}
		}
		var labels []string
		for _, name := range seriesIn(expr) {
			switch have, ok := served[name]; {
			case name == "ALERTS":
			case !ok:
				t.Errorf("%s reads %s, which plex-language-sync does not serve", where, name)
			default:
				labels = append(labels, have...)
			}
		}
		for _, sel := range selectorRe.FindAllStringSubmatch(quotedRe.ReplaceAllString(expr, `""`), -1) {
			have, ok := served[sel[1]]
			if !ok {
				continue
			}
			for _, m := range matcherNameRe.FindAllStringSubmatch(sel[2], -1) {
				if !slices.Contains(have, m[1]) && m[1] != "instance" {
					t.Errorf("%s matches %s on %s, which carries no such label", where, m[1], sel[1])
				}
			}
		}
		for _, g := range groupingRe.FindAllStringSubmatch(expr, -1) {
			for l := range strings.SplitSeq(g[2], ",") {
				l = strings.TrimSpace(l)
				// A label the query names as a string is one label_replace makes.
				computed := strings.Contains(expr, `"`+l+`"`)
				if l != "" && !computed && !slices.Contains(labels, l) && l != "instance" {
					t.Errorf("%s groups by %s, which no series it reads carries", where, l)
				}
			}
		}
	}
}

// TestDashboardInstanceVariableListsServedInstances fails when the instance
// variable stops listing every plex-language-sync instance that reports a
// build as one alphabetical list that refreshes with the range and starts
// empty.
func TestDashboardInstanceVariableListsServedInstances(t *testing.T) {
	d := loadDashboard(t)
	inst := d.variables()["instance"]
	if q := variableQuery(&inst); q != "label_values(plex_language_sync_build_info, instance)" {
		t.Errorf("instance variable query = %q, want label_values(plex_language_sync_build_info, instance)", q)
	}
	if inst.Spec.Regex != "" {
		t.Errorf("instance variable has regex %q, which hides instances its query lists", inst.Spec.Regex)
	}
	if inst.Spec.Sort != "alphabeticalAsc" || inst.Spec.Refresh != "onTimeRangeChanged" || inst.Spec.Current.Value != "" {
		t.Errorf("instance variable is not an alphabetical, range-refreshed list with an empty current value")
	}
}

// TestDashboardCounterSlotsAreTheTestedQueries fails when a per-interval count
// differs from the query alerts/promql_test.yaml runs under its anchor, with
// $instance and a 5m interval filled in, or 15s for a _15s anchor.
func TestDashboardCounterSlotsAreTheTestedQueries(t *testing.T) {
	raw, err := os.ReadFile("alerts/promql_test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	testedAs := func(anchor, want string) {
		t.Helper()
		m := regexp.MustCompile(`(?m)expr: &` + anchor + ` '([^']*)'$`).FindSubmatch(raw)
		if m == nil {
			t.Errorf("alerts/promql_test.yaml has no expr anchored as %s", anchor)
		} else if string(m[1]) != want {
			t.Errorf("alerts/promql_test.yaml tests %s as\n%s\nthe dashboard reads, with its variables filled in,\n%s", anchor, m[1], want)
		}
	}
	at5m := strings.NewReplacer(`$instance`, "a", `${__interval_ms}`, "300000", `$__interval`, "5m")
	at15s := strings.NewReplacer(`$instance`, "a", `${__interval_ms}`, "15000", `$__interval`, "15s")
	d := loadDashboard(t)
	titles := map[string][]string{}
	for _, p := range d.Spec.Elements {
		titles[p.Spec.Title] = panelExprs(&p)
	}
	for title, anchors := range map[string][]string{
		"Track selections written": {"track_selections_set", "track_selections_failed"},
		"Plays handled":            {"plays_handled"},
		"Deep scans finished":      {"deep_scans_finished"},
		"Reconnect attempts":       {"reconnect_attempts"},
	} {
		exprs := titles[title]
		if len(exprs) != len(anchors) {
			t.Errorf("%q has %d queries, want %d", title, len(exprs), len(anchors))
			continue
		}
		for i, anchor := range anchors {
			testedAs(anchor, at5m.Replace(exprs[i]))
		}
	}
	if exprs := titles["Reconnect attempts"]; len(exprs) == 1 {
		bar := at15s.Replace(exprs[0])
		testedAs("reconnect_attempts_15s", bar)
		testedAs("reconnect_attempts_15s_total", "sum_over_time(("+bar+")[3m:15s])")
	}
}

type causeRead struct {
	cause string
	value int
}

// causeTiles holds each tile that tells its rule's causes apart: the
// aggregation and the number each cause's read adds, which the panel's value
// mappings put into words.
var causeTiles = map[string]struct {
	agg   string
	reads []causeRead
}{
	"PlexLanguageSyncResolutionStalled": {"max", []causeRead{{"sessions_unreadable", 1}, {"all_clients_absent", 2}}},
}

// attentionReads is every ALERTS selector a query may read the rule name's
// firing alerts with, for the rule expression expr, in its tile's order.
// absent() and absent_over_time() label their alert with only the selector's
// equality matchers, and a shipped rule's selector names no instance, so only
// such a rule's alert is also read with instance="":
// https://prometheus.io/docs/prometheus/latest/querying/functions/#absent
func attentionReads(name, expr string) []string {
	read := `ALERTS{alertname="` + name + `", alertstate="firing", instance=`
	if ct, ok := causeTiles[name]; ok {
		var reads []string
		for _, c := range ct.reads {
			reads = append(reads, read+`"$instance", cause="`+c.cause+`"}`)
		}
		return reads
	}
	if absentCallRe.MatchString(promqlCode(expr)) {
		return []string{read + `"$instance"}`, read + `""}`}
	}
	return []string{read + `"$instance"}`}
}

// attentionTile is the query of the Needs attention tile for the rule name
// with expression expr, which answers while any of its reads does.
func attentionTile(name, expr string) string {
	reads := attentionReads(name, expr)
	if ct, ok := causeTiles[name]; ok {
		parts := make([]string, len(reads))
		for i, r := range reads {
			parts[i] = fmt.Sprintf("%s * 0 + %d", r, ct.reads[i].value)
		}
		return ct.agg + "(" + strings.Join(parts, " or ") + ")"
	}
	if len(reads) == 2 {
		return `0 * max((` + reads[0] + ` or ` + reads[1] + `))`
	}
	return `0 * max(` + reads[0] + `)`
}

// TestDashboardAttentionTilesMirrorEveryShippedRule fails when a rule in
// alerts/promql.yaml has no Needs attention tile, a tile's query is not
// exactly attentionTile's for a shipped rule its description names, a cause
// tile misses or adds a cause its rule fires for, the row's variable does not
// count exactly those tiles in layout order, a tile is not hidden until its
// query answers, or any query reads ALERTS other than through attentionReads'
// selectors, instance="" only beside instance="$instance".
func TestDashboardAttentionTilesMirrorEveryShippedRule(t *testing.T) {
	d := loadDashboard(t)
	rules := map[string]string{}
	for _, r := range alertrules.Load(t, "alerts/promql.yaml") {
		rules[r.Alert] = r.Expr
	}
	for where, expr := range panelQueries(&d) {
		sels := selectors(bracedVariableRe.ReplaceAllString(expr, "$$v"))
		var texts []string
		alertSels := 0
		for _, sel := range sels {
			texts = append(texts, sel.text)
			if sel.name == "ALERTS" {
				alertSels++
			}
		}
		n := 0
		for _, id := range identRe.FindAllString(promqlCode(expr), -1) {
			if id == "ALERTS" {
				n++
			}
		}
		if n != alertSels {
			t.Errorf("%s names ALERTS %d times but reads it through %d selectors: every ALERTS read is one of attentionReads' selectors", where, n, alertSels)
		}
		for _, sel := range sels {
			if sel.name != "ALERTS" {
				continue
			}
			name := ""
			if m := alertsRe.FindStringSubmatch(sel.text); m != nil {
				name = m[1]
			}
			rule, ok := rules[name]
			if !ok {
				t.Errorf("%s reads %s, whose alertname is not first or names no rule in alerts/promql.yaml", where, sel.text)
				continue
			}
			reads := attentionReads(name, rule)
			switch {
			case !slices.Contains(reads, sel.text):
				t.Errorf("%s reads %s; a read of %s must be exactly one of these, since a missing or looser instance match shows other instances' alerts, a second instance matcher reads nothing, and instance=\"\" reads only the alert of an absent() rule, which carries no instance:\n%s", where, sel.text, name, strings.Join(reads, "\n"))
			case strings.HasSuffix(sel.text, `instance=""}`) && !slices.Contains(texts, reads[0]):
				t.Errorf("%s reads %s without %s in the same query: a read of only instance=\"\" misses the rule's alerts for the selected instance", where, sel.text, reads[0])
			}
		}
	}
	served := servedCauses(t)
	for name, ct := range causeTiles {
		expr, ok := rules[name]
		if !ok {
			t.Errorf("causeTiles holds %s, which alerts/promql.yaml does not hold", name)
			continue
		}
		var got []string
		for _, c := range ct.reads {
			got = append(got, c.cause)
		}
		slices.Sort(got)
		// A rule whose selectors carry no matcher fires for every served cause.
		if !slices.Equal(got, served) || len(selectors(withoutComments(expr))) > 0 || absentCallRe.MatchString(promqlCode(expr)) {
			t.Errorf("%s's tile reads causes %q, want the served %q, from a rule that reads its series with no matcher and no absent() arm", name, got, served)
		}
	}
	attention := row(t, &d, "Needs attention")
	gate := attention.Spec.ConditionalRendering
	if gate == nil || gate.Spec.Visibility != "show" || len(gate.Spec.Items) != 1 ||
		gate.Spec.Items[0].Kind != "ConditionalRenderingVariable" ||
		gate.Spec.Items[0].Spec.Operator != "matches" || gate.Spec.Items[0].Spec.Value != "^[1-9]" {
		t.Fatal("Needs attention is not shown by a variable matching ^[1-9]")
	}
	var mirrored, counts []string
	for i, it := range attention.Spec.Layout.Spec.Items {
		p := d.Spec.Elements[it.Spec.Element.Name]
		cr := it.Spec.ConditionalRendering
		var conds []string
		if cr != nil {
			for _, c := range cr.Spec.Items {
				conds = append(conds, c.Kind)
			}
		}
		slices.Sort(conds)
		if cr == nil || cr.Spec.Visibility != "show" || cr.Spec.Condition != "or" ||
			!slices.Equal(conds, []string{"ConditionalRenderingData", "ConditionalRenderingTimeRangeSize"}) ||
			p.Spec.Data.Spec.QueryOptions.MaxDataPoints <= 0 {
			t.Errorf("%q does not show exactly while its query answers: want a shown group or'ing a has-data rule with a 0s range rule, and a maxDataPoints pin", p.Spec.Title)
		}
		exprs := panelExprs(&p)
		if len(exprs) != 1 {
			t.Errorf("%q holds %d queries, want 1", p.Spec.Title, len(exprs))
			continue
		}
		counts = append(counts, fmt.Sprintf(`label_replace(count(%s), "tile", "%d", "", "")`, exprs[0], i+1))
		m := alertsRe.FindStringSubmatch(exprs[0])
		if m == nil {
			t.Errorf("%q reads no ALERTS series by alertname: %s", p.Spec.Title, exprs[0])
			continue
		}
		name := m[1]
		mirrored = append(mirrored, name)
		if !strings.Contains(p.Spec.Description, name) {
			t.Errorf("%q reads %s, which its description does not name", p.Spec.Title, name)
		}
		expr, ok := rules[name]
		if !ok {
			t.Errorf("a tile mirrors %s, which alerts/promql.yaml does not hold", name)
			continue
		}
		if want := attentionTile(name, expr); exprs[0] != want {
			t.Errorf("%q must read exactly\n%s\nso it shows only the selected instance's firing %s alerts; it reads\n%s", p.Spec.Title, want, name, exprs[0])
		}
	}
	gateVar := d.variables()[gate.Spec.Items[0].Spec.Variable]
	if got, want := variableQuery(&gateVar), "query_result(count("+strings.Join(counts, " or ")+") or vector(0))"; got != want {
		t.Errorf("the row's variable must count exactly its tiles in layout order:\n%s\nit reads\n%s", want, got)
	}
	for _, name := range slices.Sorted(maps.Keys(rules)) {
		if !slices.Contains(mirrored, name) {
			t.Errorf("%s has no Needs attention tile reading its firing alerts", name)
		}
	}
}

// TestDashboardUserAttributionNamesEveryStallCause fails when a query reading
// PlexLanguageSyncResolutionStalled misses a cause the app serves or names one
// it does not, or a panel shows a cause's value with no text for it. A cause
// no query names fires the alert with its tile and the Needs attention row
// hidden.
func TestDashboardUserAttributionNamesEveryStallCause(t *testing.T) {
	served := servedCauses(t)
	d := loadDashboard(t)
	for where, expr := range panelQueries(&d) {
		var causes []string
		for _, m := range stallTermRe.FindAllStringSubmatch(expr, -1) {
			causes = append(causes, m[1])
		}
		slices.Sort(causes)
		if len(causes) > 0 && !slices.Equal(causes, served) {
			t.Errorf("%s names stall causes %v, want the served %v", where, causes, served)
		}
	}
	for name, p := range d.Spec.Elements {
		mapped := map[string]bool{}
		for _, mp := range p.Spec.VizConfig.Spec.FieldConfig.Defaults.Mappings {
			var opts map[string]struct {
				Text string `json:"text"`
			}
			if mp.Type == "value" && json.Unmarshal(mp.Options, &opts) == nil {
				for v, o := range opts {
					mapped[v] = o.Text != ""
				}
			}
		}
		for _, e := range panelExprs(&p) {
			for _, m := range stallTermRe.FindAllStringSubmatch(e, -1) {
				if !mapped[m[2]] {
					t.Errorf("%s %q shows cause %s as %q, which no value mapping names", name, p.Spec.Title, m[1], m[2])
				}
			}
		}
	}
}

func TestDashboardPanelsExplainThemselves(t *testing.T) {
	d := loadDashboard(t)
	if !d.Spec.Preload {
		t.Error("spec.preload = false, want true so a hidden tile below the fold still queries")
	}
	for name, p := range d.Spec.Elements {
		if strings.TrimSpace(p.Spec.Description) == "" {
			t.Errorf("%s %q has no description", name, p.Spec.Title)
		}
		switch p.Spec.VizConfig.Group {
		case "stat", "gauge", "bargauge", "table":
			if strings.TrimSpace(p.Spec.VizConfig.Spec.FieldConfig.Defaults.NoValue) == "" {
				t.Errorf("%s %q has no no-value text", name, p.Spec.Title)
			}
		}
	}
}
