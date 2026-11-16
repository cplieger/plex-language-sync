package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
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
		Refresh string `json:"refresh"`
		Sort    string `json:"sort"`
		Current struct {
			Value any `json:"value"`
		} `json:"current"`
		Multi      bool            `json:"multi"`
		IncludeAll bool            `json:"includeAll"`
		Query      json.RawMessage `json:"query"`
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

// panelQueries is every panel query and every query variable's PromQL but
// the instance variable's, which lists the instances the others filter on.
func panelQueries(d *dashboard) map[string]string {
	queries := map[string]string{}
	for name, v := range d.variables() {
		if q := variableQuery(&v); q != "" && name != "instance" {
			queries["variable "+name] = q
		}
	}
	for name, p := range d.Spec.Elements {
		for i, e := range panelExprs(&p) {
			queries[fmt.Sprintf("%s %s query %d", name, p.Spec.Title, i)] = e
		}
	}
	return queries
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
	alertsRe      = regexp.MustCompile(`ALERTS\{alertname="([A-Za-z]+)"([^}]*)\}`)
	stallTermRe   = regexp.MustCompile(`ALERTS\{alertname="PlexLanguageSyncResolutionStalled"[^}]*cause="([^"]*)"[^}]*\}(?:\s*\*\s*0\s*\+\s*(\d+))?`)
	stallCauseRe  = regexp.MustCompile(`(?m)^plex_language_sync_user_resolution_stalled\{cause="([^"]*)"\} `)
	counterCallRe = regexp.MustCompile(`\b(increase|rate|irate)\s*\(`)
	oneSlotRe     = regexp.MustCompile(`\[\s*\$(?:__interval|\{__interval\}|\{__interval_ms\}ms)\s*(?::[^\]]*)?\]`)
	// Grafana's variable grammar, variableRegex in
	// https://github.com/grafana/grafana/blob/v13.2.3/public/app/features/variables/utils.ts
	grafanaVarRe = regexp.MustCompile(`\$(\w+)|\[\[(\w+?)(?::\w+)?\]\]|\$\{(\w+)(?:\.[^:^}]+)?(?::[^}]+)?\}`)
	// ${name}'s braces are no selector.
	bracedVarRe = regexp.MustCompile(`\$\{\w+(?::\w+)?\}`)
	matcherRe   = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*|` + promqlString + `)\s*(=~|!~|!=|=)\s*(` + promqlString + `)$`)
	stringRe    = regexp.MustCompile(`^(?:` + promqlString + `)$`)
)

const promqlString = `"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|` + "`[^`]*`"

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
// fails the build instead of blanking a panel. ALERTS is the ruler's series;
// instance is the scrape's label.
func TestDashboardReadsOnlyExportedSeries(t *testing.T) {
	served := servedLabels(t)
	d := loadDashboard(t)
	queries := panelQueries(&d)
	inst := d.variables()["instance"]
	queries["variable instance"] = variableQuery(&inst)
	for where, expr := range queries {
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

// TestDashboardFiltersOnTheInstanceOnly fails when a series a query reads is
// not filtered on the selected instance, or a query matches on a label the
// user or their collector names (job, hostname, container), which would blank
// the panel for anyone who named theirs differently.
func TestDashboardFiltersOnTheInstanceOnly(t *testing.T) {
	served := servedLabels(t)
	d := loadDashboard(t)
	inst := d.variables()["instance"]
	if q := variableQuery(&inst); q != "label_values(plex_language_sync_build_info, instance)" {
		t.Errorf("instance variable query = %q, want label_values(plex_language_sync_build_info, instance)", q)
	}
	if inst.Spec.Sort != "alphabeticalAsc" || inst.Spec.Refresh != "onTimeRangeChanged" ||
		inst.Spec.Multi || inst.Spec.IncludeAll || inst.Spec.Current.Value != "" {
		t.Errorf("instance variable is not a single-select, alphabetical, range-refreshed list with an empty current value")
	}
	for where, expr := range panelQueries(&d) {
		filtered := map[int]bool{}
		pinned := strings.Contains(expr, `instance="$instance"`)
		for _, loc := range selectorRe.FindAllStringSubmatchIndex(expr, -1) {
			if sel := expr[loc[4]:loc[5]]; strings.Contains(sel, `instance="$instance"`) ||
				pinned && strings.Contains(sel, `instance=""`) {
				filtered[loc[0]] = true
			}
			for _, m := range matcherNameRe.FindAllStringSubmatch(quotedRe.ReplaceAllString(expr[loc[4]:loc[5]], `""`), -1) {
				if slices.Contains([]string{"job", "hostname", "container"}, m[1]) {
					t.Errorf("%s matches on %s, a label the user names", where, m[1])
				}
			}
		}
		// Blank quoted strings in place, keeping offsets, so a label value
		// is never read as a series name.
		bare := quotedRe.ReplaceAllStringFunc(expr, func(s string) string { return strings.Repeat(" ", len(s)) })
		for _, loc := range identRe.FindAllStringIndex(bare, -1) {
			name := bare[loc[0]:loc[1]]
			if _, ok := served[name]; (ok || name == "ALERTS") && !filtered[loc[0]] {
				t.Errorf("%s reads %s without instance=\"$instance\"", where, name)
			}
		}
	}
}

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

func skipComment(s string, i int) int {
	if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
		return i + j
	}
	return len(s)
}

// promqlUnquote decodes a string literal. PromQL escapes as Go does
// (https://prometheus.io/docs/prometheus/latest/querying/basics/#string-literals),
// so "inst\x61nce" names the instance label.
func promqlUnquote(s string) (string, error) {
	if s[0] == '\'' {
		s = `"` + strings.ReplaceAll(strings.ReplaceAll(s[1:len(s)-1], `\'`, `'`), `"`, `\"`) + `"`
	}
	return strconv.Unquote(s)
}

func selectors(expr string) [][]string {
	var out [][]string
	var parts []string
	start, part := -1, 0
	for i := 0; i < len(expr); i++ {
		switch c := expr[i]; {
		case c == '#':
			i = skipComment(expr, i)
		case c == '"' || c == '\'' || c == '`':
			i = skipString(expr, i) - 1
		case c == '{':
			start, part, parts = i, i+1, nil
		case (c == ',' || c == '}') && start >= 0:
			if p := strings.TrimSpace(expr[part:i]); p != "" {
				parts = append(parts, p)
			}
			part = i + 1
			if c == '}' {
				out = append(out, parts)
				start = -1
			}
		}
	}
	return out
}

// TestDashboardMatchesTheInstanceExactly fails when a query matches instance
// by regex or negation. instance is single-select, and Grafana regex-escapes a
// variable only when it is multi-value or include-all
// (https://github.com/grafana/grafana-prometheus-datasource/blob/v13.2.3/packages/grafana-prometheus/src/escaping.ts),
// so an IPv6 target's brackets parse as a character class and a host name's
// dots match its neighbours. instance="" is allowed only beside the pinned
// matcher, for an alert that carries no instance label.
func TestDashboardMatchesTheInstanceExactly(t *testing.T) {
	d := loadDashboard(t)
	for where, expr := range panelQueries(&d) {
		var matchers [][]string
		for _, parts := range selectors(bracedVarRe.ReplaceAllString(expr, "$$v")) {
			instances := 0
			for _, part := range parts {
				m := matcherRe.FindStringSubmatch(part)
				if m == nil {
					if !stringRe.MatchString(part) {
						t.Errorf("%s holds %q in a selector, which is no label matcher", where, part)
					}
					continue
				}
				label, lerr := m[1], error(nil)
				if stringRe.MatchString(label) {
					label, lerr = promqlUnquote(label)
				}
				value, verr := promqlUnquote(m[3])
				if lerr != nil || verr != nil {
					t.Errorf("%s quotes %q in a form the check cannot read", where, part)
					continue
				}
				if label == "instance" {
					instances++
					matchers = append(matchers, []string{m[2], value})
				}
			}
			if instances > 1 {
				t.Errorf("%s matches instance twice in one selector", where)
			}
		}
		pinned := slices.ContainsFunc(matchers, func(m []string) bool { return m[0] == "=" && m[1] == "$instance" })
		for _, m := range matchers {
			if m[0] != "=" || m[1] != "$instance" && (m[1] != "" || !pinned) {
				t.Errorf("%s matches instance%s%q, want instance=\"$instance\", or instance=\"\" beside it", where, m[0], m[1])
			}
		}
	}
}

func callArgs(expr string, open int) string {
	depth := 0
	for i := open; i < len(expr); i++ {
		switch expr[i] {
		case '#':
			i = skipComment(expr, i)
		case '"', '\'', '`':
			i = skipString(expr, i) - 1
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return expr[open+1 : i]
			}
		}
	}
	return expr[open+1:]
}

func canonicalIntervals(expr string) string {
	return grafanaVarRe.ReplaceAllStringFunc(expr, func(ref string) string {
		m := grafanaVarRe.FindStringSubmatch(ref)
		switch m[1] + m[2] + m[3] {
		case "__interval":
			return "$__interval"
		case "__interval_ms":
			return "${__interval_ms}"
		}
		return ref
	})
}

// TestDashboardNeverTakesACounterOverOneInterval fails when a query takes
// increase, rate or irate over one $__interval, in any of Grafana's spellings,
// plain or as a subquery. At short ranges the interval falls to the scrape
// interval, the window holds one sample and the panel goes blank while the
// user zooms in on an incident.
func TestDashboardNeverTakesACounterOverOneInterval(t *testing.T) {
	d := loadDashboard(t)
	for where, expr := range panelQueries(&d) {
		expr = canonicalIntervals(expr)
		for _, loc := range counterCallRe.FindAllStringSubmatchIndex(expr, -1) {
			if r := oneSlotRe.FindString(callArgs(expr, loc[1]-1)); r != "" {
				t.Errorf("%s takes %s over %s, want [$__rate_interval], or the offset slot for an exact count", where, expr[loc[2]:loc[3]], r)
			}
		}
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

// TestDashboardAttentionTilesMirrorEveryShippedRule fails when a rule in
// alerts/promql.yaml has no Needs attention tile, or a tile names a rule the
// file does not hold. Each tile reads its rule's firing ALERTS series and
// names the rule, and starts hidden until its query answers.
func TestDashboardAttentionTilesMirrorEveryShippedRule(t *testing.T) {
	d := loadDashboard(t)
	var rules []string
	for _, r := range alertrules.Load(t, "alerts/promql.yaml") {
		rules = append(rules, r.Alert)
	}
	attention := row(t, &d, "Needs attention")
	gate := attention.Spec.ConditionalRendering
	if gate == nil || gate.Spec.Visibility != "show" || len(gate.Spec.Items) != 1 ||
		gate.Spec.Items[0].Kind != "ConditionalRenderingVariable" ||
		gate.Spec.Items[0].Spec.Operator != "matches" || gate.Spec.Items[0].Spec.Value != "^[1-9]" {
		t.Fatal("Needs attention is not shown by a variable matching ^[1-9]")
	}
	gateVar := d.variables()[gate.Spec.Items[0].Spec.Variable]
	counted := variableQuery(&gateVar)
	var mirrored []string
	for _, it := range attention.Spec.Layout.Spec.Items {
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
		for _, e := range panelExprs(&p) {
			if !strings.Contains(counted, "count("+e+")") {
				t.Errorf("%q query is not counted by the row's variable", p.Spec.Title)
			}
			for _, m := range alertsRe.FindAllStringSubmatch(e, -1) {
				if !strings.Contains(m[2], `alertstate="firing"`) {
					t.Errorf("%q reads %s alerts in any state, want only firing ones", p.Spec.Title, m[1])
				}
				if !strings.Contains(p.Spec.Description, m[1]) {
					t.Errorf("%q reads %s, which its description does not name", p.Spec.Title, m[1])
				}
				if !slices.Contains(mirrored, m[1]) {
					mirrored = append(mirrored, m[1])
				}
			}
		}
	}
	for _, name := range rules {
		if !slices.Contains(mirrored, name) {
			t.Errorf("%s has no Needs attention tile reading its firing alerts", name)
		}
	}
	for _, name := range mirrored {
		if !slices.Contains(rules, name) {
			t.Errorf("a tile mirrors %s, which alerts/promql.yaml does not hold", name)
		}
	}
}

// TestDashboardUserAttributionNamesEveryStallCause fails when a query reading
// PlexLanguageSyncResolutionStalled misses a cause the app serves or names one
// it does not, or a panel shows a cause's value with no text for it. A cause
// no query names fires the alert with its tile and the Needs attention row
// hidden.
func TestDashboardUserAttributionNamesEveryStallCause(t *testing.T) {
	var served []string
	for _, m := range stallCauseRe.FindAllStringSubmatch(exposition(t), -1) {
		served = append(served, m[1])
	}
	if len(served) == 0 {
		t.Fatal("Setup: the registry serves no plex_language_sync_user_resolution_stalled series")
	}
	slices.Sort(served)
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
