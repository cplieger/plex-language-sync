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
						NoValue string `json:"noValue"`
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
	Kind string `json:"kind"`
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
			Text  any `json:"text"`
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

// servedLabels is every series a fresh registry serves, with the label
// names its samples carry.
func servedLabels(t *testing.T) map[string][]string {
	t.Helper()
	rec := httptest.NewRecorder()
	obs.New("test").Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	served := map[string][]string{}
	for _, m := range sampleRe.FindAllStringSubmatch(string(body), -1) {
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
)

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
		for _, loc := range selectorRe.FindAllStringSubmatchIndex(expr, -1) {
			if strings.Contains(expr[loc[4]:loc[5]], `instance="$instance"`) {
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
