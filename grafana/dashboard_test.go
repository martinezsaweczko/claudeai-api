// Package grafana_test guards the Grafana dashboard and the example alert rules
// against drifting away from what the service really exposes.
//
// There is no Grafana in CI, so rendering is not verified; what is verified is
// the structure of the dashboard and that every metric and label its queries use
// exists in a real Prometheus exposition of the app's instruments.
package grafana_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.yaml.in/yaml/v3"

	"github.com/martinezsaweczko/claudeai-api/o11"
)

// ---------------------------------------------------------------------------
// Dashboard model
// ---------------------------------------------------------------------------

type gridPos struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

type target struct {
	Expr         string          `json:"expr"`
	RefID        string          `json:"refId"`
	LegendFormat string          `json:"legendFormat"`
	Datasource   json.RawMessage `json:"datasource"`
}

type panel struct {
	ID         int             `json:"id"`
	Type       string          `json:"type"`
	Title      string          `json:"title"`
	GridPos    gridPos         `json:"gridPos"`
	Datasource json.RawMessage `json:"datasource"`
	Targets    []target        `json:"targets"`
	Panels     []panel         `json:"panels"`
}

type variable struct {
	Name  string          `json:"name"`
	Type  string          `json:"type"`
	Query json.RawMessage `json:"query"`
}

type dashboard struct {
	UID        string `json:"uid"`
	Panels     []panel
	Templating struct {
		List []variable `json:"list"`
	} `json:"templating"`
}

func loadDashboard(t *testing.T) dashboard {
	t.Helper()
	raw, err := os.ReadFile("dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var d dashboard
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("dashboard.json is not valid: %v", err)
	}
	return d
}

// flatten returns every panel including those nested in collapsed rows.
func flatten(ps []panel) []panel {
	var out []panel
	for _, p := range ps {
		out = append(out, p)
		out = append(out, flatten(p.Panels)...)
	}
	return out
}

func TestDashboardStructure(t *testing.T) {
	d := loadDashboard(t)
	if d.UID != "claudeai-api-ops" {
		t.Errorf("uid = %q", d.UID)
	}
	all := flatten(d.Panels)
	if len(all) == 0 {
		t.Fatal("no panels")
	}

	ids := map[int]string{}
	for _, p := range all {
		if p.ID <= 0 {
			t.Errorf("panel %q has id %d", p.Title, p.ID)
		}
		if prev, dup := ids[p.ID]; dup {
			t.Errorf("duplicate panel id %d (%q and %q)", p.ID, prev, p.Title)
		}
		ids[p.ID] = p.Title
		if p.Title == "" {
			t.Errorf("panel %d has no title", p.ID)
		}
		g := p.GridPos
		if g.X < 0 || g.Y < 0 || g.W < 1 || g.H < 1 || g.X+g.W > 24 {
			t.Errorf("panel %d %q has an invalid gridPos %+v", p.ID, p.Title, g)
		}
		if p.Type == "row" {
			continue
		}
		if len(p.Targets) == 0 {
			t.Errorf("panel %d %q has no targets", p.ID, p.Title)
		}
		for _, tg := range p.Targets {
			if strings.TrimSpace(tg.Expr) == "" {
				t.Errorf("panel %d %q has an empty expr", p.ID, p.Title)
			}
			if tg.RefID == "" {
				t.Errorf("panel %d %q has a target without refId", p.ID, p.Title)
			}
		}
		// A datasource on the panel or on every target.
		for _, tg := range p.Targets {
			if len(p.Datasource) == 0 && len(tg.Datasource) == 0 {
				t.Errorf("panel %d %q target %s has no datasource", p.ID, p.Title, tg.RefID)
			}
		}
	}

	for i := range all {
		for j := i + 1; j < len(all); j++ {
			a, b := all[i].GridPos, all[j].GridPos
			if a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H {
				t.Errorf("panels %d %q %+v and %d %q %+v overlap", all[i].ID, all[i].Title, a, all[j].ID, all[j].Title, b)
			}
		}
	}

	vars := map[string]bool{}
	for _, v := range d.Templating.List {
		vars[v.Name] = true
	}
	for _, want := range []string{"datasource", "job", "model"} {
		if !vars[want] {
			t.Errorf("templating variable %q missing", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Exposition contract
// ---------------------------------------------------------------------------

// exposition holds what a real scrape of the app's Claude and skills
// instruments contains: series names (with _bucket/_sum/_count) and the label
// names each series carries (otel_scope_* removed).
type exposition struct {
	labels map[string]map[string]bool
}

var (
	sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(.*)\})?\s`)
	labelName  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="`)
)

func scrapeExposition(t *testing.T) exposition {
	t.Helper()
	ctx := context.Background()
	reg := prom.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		t.Fatal(err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = mp.Shutdown(ctx) })

	cm, err := o11.NewClaudeMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	if err := cm.RegisterInFlight(func() int64 { return 1 }); err != nil {
		t.Fatal(err)
	}
	sm, err := o11.NewSkillsMetrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.RegisterState(func() o11.SkillsState {
		return o11.SkillsState{HaveSnapshot: true, Commit: "0123456789ab", Ref: "main", Loaded: 1, Skipped: 1, LastSuccess: time.Unix(1700000000, 0)}
	}); err != nil {
		t.Fatal(err)
	}

	model := attribute.String("model", "claude-haiku-5-5")
	cm.RunsTotal.Add(ctx, 1, metric.WithAttributes(model, attribute.String("outcome", "success")))
	cm.RunDuration.Record(ctx, 3, metric.WithAttributes(model))
	cm.TimeToFirstToken.Record(ctx, 1, metric.WithAttributes(model))
	cm.QueueWait.Record(ctx, 0, metric.WithAttributes(model))
	cm.TokensTotal.Add(ctx, 10, metric.WithAttributes(model, attribute.String("type", "output")))
	cm.CostUSDTotal.Add(ctx, 0.001, metric.WithAttributes(model))
	cm.SkillInvocationsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("skill", "summarize")))
	sm.SyncTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "success")))
	sm.SyncDuration.Record(ctx, 0.4)

	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	e := exposition{labels: map[string]map[string]bool{}}
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("unparsable exposition line %q", line)
		}
		set := e.labels[m[1]]
		if set == nil {
			set = map[string]bool{}
			e.labels[m[1]] = set
		}
		for _, l := range labelName.FindAllStringSubmatch(m[3], -1) {
			if !strings.HasPrefix(l[1], "otel_scope_") {
				set[l[1]] = true
			}
		}
	}
	return e
}

// producedElsewhere lists the metric families the dashboard and the alerts use
// that the Claude/skills instruments do not produce: template HTTP and external
// API metrics, the Go runtime, and Prometheus' own `up`. Keep it small; a
// claude_* or skills_* name must never be listed here.
var producedElsewhere = []string{"http_server_", "external_api_", "go_", "process_", "up"}

func allowlisted(name string) bool {
	for _, a := range producedElsewhere {
		if name == a || (strings.HasSuffix(a, "_") && strings.HasPrefix(name, a)) {
			return true
		}
	}
	return false
}

// Labels any series may be filtered or grouped by regardless of the metric:
// added by the scrape, not by the app.
var scrapeLabels = map[string]bool{"job": true, "instance": true}

// promQLFunctions are the functions and aggregations the queries may use. An
// identifier followed by "(" that is not here fails the test, so extending the
// dashboard with a new function is a conscious edit.
var promQLFunctions = map[string]bool{
	"sum": true, "min": true, "max": true, "avg": true, "count": true, "topk": true, "bottomk": true,
	"rate": true, "irate": true, "increase": true, "delta": true, "deriv": true,
	"histogram_quantile": true, "time": true, "vector": true, "scalar": true,
	"abs": true, "ceil": true, "floor": true, "round": true, "clamp_min": true, "clamp_max": true,
	"sum_over_time": true, "avg_over_time": true, "max_over_time": true, "min_over_time": true,
	"label_replace": true, "label_join": true, "absent": true, "sort": true, "sort_desc": true,
}

// groupings take a label list instead of an expression.
var groupings = map[string]bool{"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true}

// keywords may appear without parentheses.
var keywords = map[string]bool{"and": true, "or": true, "unless": true, "bool": true, "offset": true, "by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true}

type selector struct {
	name     string
	matchers []string // label names used in {...}
}

type query struct {
	selectors []selector
	grouping  []string // labels in by/without/on/ignoring lists
	functions []string
}

var (
	reString  = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	reMatcher = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)`)
)

// parseQuery tokenizes a PromQL expression just enough to list the metric
// selectors, their matcher labels, the grouping labels and the functions. It
// understands Grafana variables ($job, $__rate_interval, ${x}) and durations.
func parseQuery(t *testing.T, expr string) query {
	t.Helper()
	s := reString.ReplaceAllString(expr, `""`)
	var q query
	isIdentStart := func(c byte) bool { return c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
	isIdent := func(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }
	skipSpace := func(i int) int {
		for i < len(s) && (s[i] == ' ' || s[i] == '\n' || s[i] == '\t') {
			i++
		}
		return i
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '[': // range selector
			j := strings.IndexByte(s[i:], ']')
			if j < 0 {
				t.Fatalf("unterminated range in %q", expr)
			}
			i += j + 1
		case c == '$': // Grafana variable
			i++
			if i < len(s) && s[i] == '{' {
				j := strings.IndexByte(s[i:], '}')
				if j < 0 {
					t.Fatalf("unterminated variable in %q", expr)
				}
				i += j + 1
			} else {
				for i < len(s) && isIdent(s[i]) {
					i++
				}
			}
		case c >= '0' && c <= '9' || c == '.': // number or duration
			for i < len(s) && (isIdent(s[i]) || s[i] == '.') {
				i++
			}
		case isIdentStart(c):
			j := i
			for j < len(s) && isIdent(s[j]) {
				j++
			}
			word := s[i:j]
			k := skipSpace(j)
			i = j
			switch {
			case k < len(s) && s[k] == '(' && groupings[word]:
				end := strings.IndexByte(s[k:], ')')
				if end < 0 {
					t.Fatalf("unterminated %s list in %q", word, expr)
				}
				for _, l := range strings.Split(s[k+1:k+end], ",") {
					if l = strings.TrimSpace(l); l != "" {
						q.grouping = append(q.grouping, l)
					}
				}
				i = k + end + 1
			case k < len(s) && s[k] == '(':
				if !promQLFunctions[word] {
					t.Errorf("unknown PromQL function %q in %q (add it to promQLFunctions if it is intended)", word, expr)
				}
				q.functions = append(q.functions, word)
			case promQLFunctions[word] && !(k < len(s) && s[k] == '{'):
				// Aggregation followed by a by/without clause: "sum by (x) (...)".
				q.functions = append(q.functions, word)
			case keywords[word]:
			default:
				sel := selector{name: word}
				if k < len(s) && s[k] == '{' {
					end := strings.IndexByte(s[k:], '}')
					if end < 0 {
						t.Fatalf("unterminated matcher block in %q", expr)
					}
					for _, m := range reMatcher.FindAllStringSubmatch(s[k+1:k+end], -1) {
						sel.matchers = append(sel.matchers, m[1])
					}
					i = k + end + 1
				}
				q.selectors = append(q.selectors, sel)
			}
		case c == '{':
			t.Fatalf("selector without a metric name in %q", expr)
		default:
			i++
		}
	}
	return q
}

// checkQuery verifies one expression against the exposition and returns every
// problem found (unknown metric, matcher label or grouping label).
func checkQuery(t *testing.T, expr string, e exposition) []string {
	t.Helper()
	q := parseQuery(t, expr)
	var problems []string
	if len(q.selectors) == 0 {
		problems = append(problems, "no metric found")
	}
	union := map[string]bool{}
	for _, sel := range q.selectors {
		if allowlisted(sel.name) {
			if strings.HasPrefix(sel.name, "claude_") || strings.HasPrefix(sel.name, "skills_") {
				problems = append(problems, sel.name+" must not be allowlisted")
			}
			continue
		}
		labels, ok := e.labels[sel.name]
		if !ok {
			problems = append(problems, fmt.Sprintf("metric %q is not exposed by the app", sel.name))
			continue
		}
		for l := range labels {
			union[l] = true
		}
		for _, l := range sel.matchers {
			if !labels[l] && !scrapeLabels[l] {
				problems = append(problems, fmt.Sprintf("matcher label %q is not a label of %s (has %v)", l, sel.name, sortedKeys(labels)))
			}
		}
	}
	for _, l := range q.grouping {
		if !union[l] && !scrapeLabels[l] && len(union) > 0 {
			problems = append(problems, fmt.Sprintf("grouping label %q is not a label of the Claude/skills metrics used (known %v)", l, sortedKeys(union)))
		}
	}
	return problems
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var reLabelValues = regexp.MustCompile(`^label_values\(\s*([a-zA-Z_:][a-zA-Z0-9_:]*)\s*,\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)$`)

func TestDashboardMetricsExist(t *testing.T) {
	d := loadDashboard(t)
	e := scrapeExposition(t)

	// Sanity: the scrape really contains the contract.
	for _, n := range []string{"claude_runs_total", "claude_run_duration_seconds_bucket", "claude_runs_in_flight", "skills_info", "skills_sync_total"} {
		if _, ok := e.labels[n]; !ok {
			t.Fatalf("scrape lacks %s; got %v", n, mapKeys(e.labels))
		}
	}

	claudeOrSkills := 0
	for _, p := range flatten(d.Panels) {
		for _, tg := range p.Targets {
			for _, pr := range checkQuery(t, tg.Expr, e) {
				t.Errorf("panel %q: %s in %q", p.Title, pr, tg.Expr)
			}
			if strings.Contains(tg.Expr, "claude_") || strings.Contains(tg.Expr, "skills_") {
				claudeOrSkills++
			}
		}
	}
	if claudeOrSkills < 15 {
		t.Errorf("only %d targets use claude_/skills_ metrics; the dashboard lost its Claude rows", claudeOrSkills)
	}

	for _, v := range d.Templating.List {
		if v.Type != "query" {
			continue
		}
		var text string
		var obj struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(v.Query, &obj) == nil && obj.Query != "" {
			text = obj.Query
		} else if err := json.Unmarshal(v.Query, &text); err != nil {
			t.Fatalf("variable %s: unsupported query %s", v.Name, v.Query)
		}
		m := reLabelValues.FindStringSubmatch(strings.TrimSpace(text))
		if m == nil {
			t.Errorf("variable %s: query %q is not label_values(metric, label)", v.Name, text)
			continue
		}
		if labels, ok := e.labels[m[1]]; !ok || !labels[m[2]] {
			t.Errorf("variable %s: %s does not expose label %s", v.Name, m[1], m[2])
		}
	}
}

func mapKeys(m map[string]map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Alert rules
// ---------------------------------------------------------------------------

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func TestAlertRules(t *testing.T) {
	raw, err := os.ReadFile("alerts.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	var f ruleFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("alerts.example.yml is not valid YAML: %v", err)
	}
	if len(f.Groups) != 1 || f.Groups[0].Name != "claudeai-api" {
		t.Fatalf("want one group claudeai-api, got %+v", f.Groups)
	}
	e := scrapeExposition(t)

	want := map[string]bool{
		"ClaudeAuthErrors": false, "ClaudeRateLimited": false, "ClaudeErrorRatioHigh": false,
		"ClaudeTimeToFirstTokenHigh": false, "ClaudeBusyRejections": false,
		"SkillsSyncFailing": false, "SkillsStale": false, "GatewayDown": false,
	}
	durRe := regexp.MustCompile(`^[0-9]+(ms|s|m|h|d|w|y)$`)
	for _, r := range f.Groups[0].Rules {
		if _, ok := want[r.Alert]; !ok {
			t.Errorf("unexpected alert %q", r.Alert)
		}
		want[r.Alert] = true
		if strings.TrimSpace(r.Expr) == "" {
			t.Errorf("%s: empty expr", r.Alert)
		}
		if !durRe.MatchString(r.For) {
			t.Errorf("%s: for = %q, want a Prometheus duration", r.Alert, r.For)
		}
		if r.Labels["severity"] == "" {
			t.Errorf("%s: no severity label", r.Alert)
		}
		for _, k := range []string{"summary", "description"} {
			if strings.TrimSpace(r.Annotations[k]) == "" {
				t.Errorf("%s: no %s annotation", r.Alert, k)
			}
		}
		for _, pr := range checkQuery(t, r.Expr, e) {
			t.Errorf("alert %s: %s in %q", r.Alert, pr, r.Expr)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("alert %s is missing", name)
		}
	}
}

// ---------------------------------------------------------------------------
// The checker itself
// ---------------------------------------------------------------------------

func TestParseQuery(t *testing.T) {
	e := exposition{labels: map[string]map[string]bool{
		"claude_runs_total": {"model": true, "outcome": true},
	}}
	q := parseQuery(t, `sum by (outcome) (rate(claude_runs_total{model=~"$model",outcome!="success"}[$__rate_interval])) * 3600 / on() group_left up{job=~"$job"}`)
	if len(q.selectors) != 2 || q.selectors[0].name != "claude_runs_total" || q.selectors[1].name != "up" {
		t.Fatalf("selectors = %+v", q.selectors)
	}
	if got := strings.Join(q.selectors[0].matchers, ","); got != "model,outcome" {
		t.Errorf("matchers = %s", got)
	}
	if got := strings.Join(q.grouping, ","); got != "outcome" {
		t.Errorf("grouping = %s", got)
	}

	// An unknown metric, matcher label or grouping label is reported.
	for _, tc := range []struct {
		expr string
		ok   bool
	}{
		{`sum(rate(claude_runs_total{model="x"}[5m]))`, true},
		{`sum by (outcome) (claude_runs_total)`, true},
		{`sum(claude_run_total)`, false},
		{`sum(claude_runs_total{modle="x"})`, false},
		{`sum by (type) (claude_runs_total)`, false},
		{`sum(skills_info)`, false},
	} {
		if got := len(checkQuery(t, tc.expr, e)) == 0; got != tc.ok {
			t.Errorf("checkQuery(%q) ok = %v, want %v", tc.expr, got, tc.ok)
		}
	}
}
