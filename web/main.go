// A read-only web dashboard for the cws task server: one page showing
// every milestone, its tasks, their results, and the issues alongside.
//
// This is a separate service from the API on purpose. It holds no
// database handle and knows no SQL — it reads `GET /overview` and renders
// it. That is what makes "read-only" a structural property rather than a
// promise: there is no code path here that could write, whatever a
// request asks for.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

//go:embed templates/layout.html
var layoutHTML string

//go:embed templates/list.html
var listHTML string

//go:embed templates/detail.html
var detailHTML string

// md renders the markdown bodies the API stores. Raw HTML is left
// **disabled** — goldmark escapes it unless html.WithUnsafe() is passed,
// and it is deliberately not passed here. Task and issue content is
// written by agents and by whoever POSTs to the API, so it is untrusted
// input; rendering it as live HTML would make this read-only page an
// injection surface. GFM is on for tables, strikethrough and autolinks,
// none of which reintroduce raw HTML.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

var templateFuncs = template.FuncMap{
	"shortTime": shortTime,
	"markdown":  renderMarkdown,
}

// Two template sets over one layout: each content file defines "content",
// so they cannot be parsed together — the layout is compiled once per
// page kind instead of duplicating its markup.
var (
	listTemplate   = template.Must(template.New("list").Funcs(templateFuncs).Parse(layoutHTML + listHTML))
	detailTemplate = template.Must(template.New("detail").Funcs(templateFuncs).Parse(layoutHTML + detailHTML))
)

// renderMarkdown converts stored markdown to HTML for the page. On a
// conversion failure it falls back to the escaped source text rather than
// dropping the content — a body that will not parse is still something
// the reader needs to see.
func renderMarkdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(buf.String())
}

// The shapes below mirror the API's JSON. They are deliberately a
// separate declaration rather than an import of the server package: the
// two services share a wire format, not a type, so the API can add a
// field without this one failing to build.
type milestone struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Directory     string `json:"directory"`
	State         string `json:"state"`
	Version       string `json:"version"`
	LastUpdatedAt string `json:"last_updated_at"`
}

type task struct {
	ID            string  `json:"id"`
	Version       string  `json:"version"`
	Title         string  `json:"title"`
	Content       string  `json:"content"`
	Type          string  `json:"type"`
	RaisedAt      string  `json:"raised_at"`
	LastShippedAt string  `json:"last_shipped_at"`
	Pending       bool    `json:"pending"`
	DerivedFrom   *string `json:"derived_from"`
	MilestoneID   *string `json:"milestone_id"`
	Priority      *int    `json:"priority"`
	ClaimedBy     *string `json:"claimed_by"`
	ClaimedAt     *string `json:"claimed_at"`
	ClaimExpired  bool    `json:"claim_expired"`
	State         string  `json:"state"`
	LastUpdatedAt string  `json:"last_updated_at"`
}

type result struct {
	ID          string `json:"id"`
	TaskID      string `json:"task_id"`
	Status      string `json:"status"`
	Content     string `json:"content"`
	CompletedAt string `json:"completed_at"`
}

type issue struct {
	ID            string `json:"id"`
	Project       string `json:"project"`
	Alias         string `json:"alias"`
	Title         string `json:"title"`
	Content       string `json:"content"`
	LastUpdatedAt string `json:"last_updated_at"`
}

type issueReport struct {
	ID            string `json:"id"`
	Project       string `json:"project"`
	Version       string `json:"version"`
	State         string `json:"state"`
	Category      string `json:"category"`
	Text          string `json:"text"`
	LastUpdatedAt string `json:"last_updated_at"`
}

type overview struct {
	Milestones   []milestone   `json:"milestones"`
	Tasks        []task        `json:"tasks"`
	Results      []result      `json:"results"`
	Issues       []issue       `json:"issues"`
	IssueReports []issueReport `json:"issue_reports"`
}

// taskView is one task with the results that belong to it, ready to
// render.
type taskView struct {
	task
	Results []result
}

// milestoneGroup is a milestone and its tasks. The zero milestone (ID
// "") is the bucket for tasks with no milestone_id — they are shown
// rather than dropped, because the API's own list endpoint cannot reach
// them and a dashboard that hid them too would make them invisible
// everywhere.
type milestoneGroup struct {
	Milestone  *milestone
	Unassigned bool
	Tasks      []taskView
	PendingCnt int
	ShippedCnt int
	ClaimedCnt int
}

// metaItem is one label/value pair in a detail page's header strip.
// Kind picks how it renders: "state" gets the workflow dot, "mono" a
// monospace chip, "plain" no chip at all, "" an ordinary chip.
type metaItem struct{ Key, Value, Kind string }

// detailData is one record shown on its own page, where its markdown is
// rendered at reading size rather than folded into a table cell.
type detailData struct {
	Title     string
	Content   string
	Meta      []metaItem
	Results   []result
	BackView  string
	BackLabel string
}

type pageData struct {
	// View is which single table the list page shows. The nav selects a
	// table rather than scrolling to one, so exactly one is rendered.
	View   string
	Detail *detailData

	Groups       []milestoneGroup
	Milestones   []milestone
	Issues       []issue
	IssueReports []issueReport
	APIBase      string
	FetchedAt    string
	Err          string

	// Counts for the nav badges, so a reader can see what each section
	// holds before scrolling to it.
	TotalMilestones int
	TotalTasks      int
	TotalPending    int
	TotalClaimed    int
	TotalIssues     int
	TotalReports    int
}

func main() {
	apiBase := envOr("CWS_API", "http://127.0.0.1:8080")
	httpAddr := envOr("HTTP_ADDR", "0.0.0.0:8081")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleDashboard(apiBase))
	mux.HandleFunc("GET /task/{id}", handleDetail(apiBase, "task"))
	mux.HandleFunc("GET /issue/{id}", handleDetail(apiBase, "issue"))
	mux.HandleFunc("GET /report/{id}", handleDetail(apiBase, "report"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	log.Printf("listening on %s, reading %s/overview", httpAddr, apiBase)
	log.Fatal(http.ListenAndServe(httpAddr, mux))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// validViews is the closed set ?view= accepts. An unknown value falls
// back to the default rather than erroring — a mistyped tab should still
// show the page.
var validViews = map[string]bool{"milestones": true, "tasks": true, "issues": true, "reports": true}

const defaultView = "tasks"

func handleDashboard(apiBase string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		view := r.URL.Query().Get("view")
		if !validViews[view] {
			view = defaultView
		}

		data, ov, ok := basePage(w, apiBase, view, listTemplate)
		if !ok {
			return
		}
		data.Groups = group(ov)
		data.Milestones = ov.Milestones
		data.Issues = ov.Issues
		data.IssueReports = ov.IssueReports

		render(w, listTemplate, data)
	}
}

// basePage fetches the snapshot and fills everything the nav needs. On a
// fetch failure it renders the error through the given template and
// reports ok=false, so the operator sees the failure where they were
// already looking rather than getting a bare 502.
func basePage(w http.ResponseWriter, apiBase, view string, tpl *template.Template) (pageData, *overview, bool) {
	data := pageData{
		View:      view,
		APIBase:   apiBase,
		FetchedAt: time.Now().In(kst()).Format("2006-01-02 15:04:05 MST"),
	}
	ov, err := fetchOverview(apiBase)
	if err != nil {
		data.Err = err.Error()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		tpl.ExecuteTemplate(w, "layout", data)
		return data, nil, false
	}
	data.TotalMilestones = len(ov.Milestones)
	data.TotalTasks = len(ov.Tasks)
	data.TotalIssues = len(ov.Issues)
	data.TotalReports = len(ov.IssueReports)
	for _, t := range ov.Tasks {
		if t.Pending {
			data.TotalPending++
		}
		if t.ClaimedBy != nil && !t.ClaimExpired {
			data.TotalClaimed++
		}
	}
	return data, ov, true
}

func render(w http.ResponseWriter, tpl *template.Template, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render: %v", err)
	}
}

// handleDetail renders one record on its own page. It reads the same
// /overview snapshot the list does and picks the row out of it: the
// dataset is small, and one dependency is easier to reason about than
// three per-kind endpoints — the issue lookup in particular is keyed by
// project+alias in the API, not by the id the list links with.
func handleDetail(apiBase, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		view := map[string]string{"task": "tasks", "issue": "issues", "report": "reports"}[kind]
		data, ov, ok := basePage(w, apiBase, view, detailTemplate)
		if !ok {
			return
		}

		d := buildDetail(kind, id, ov)
		if d == nil {
			w.WriteHeader(http.StatusNotFound)
			data.Err = fmt.Sprintf("no %s with id %q", kind, id)
			render(w, detailTemplate, data)
			return
		}
		data.Detail = d
		render(w, detailTemplate, data)
	}
}

func buildDetail(kind, id string, ov *overview) *detailData {
	switch kind {
	case "task":
		for _, t := range ov.Tasks {
			if t.ID != id {
				continue
			}
			meta := []metaItem{
				{"id", t.ID, "mono"},
				{"state", t.State, "state"},
				{"type", t.Type, ""},
				{"version", t.Version, "mono"},
			}
			if t.Priority != nil {
				meta = append(meta, metaItem{"prio", fmt.Sprint(*t.Priority), ""})
			}
			if t.MilestoneID != nil {
				for _, m := range ov.Milestones {
					if m.ID == *t.MilestoneID {
						meta = append(meta, metaItem{"milestone", m.Title, ""})
					}
				}
			}
			if t.DerivedFrom != nil {
				meta = append(meta, metaItem{"subtask of", "#" + *t.DerivedFrom, ""})
			}
			if t.ClaimedBy != nil {
				label := *t.ClaimedBy
				if t.ClaimExpired {
					label += " (lease expired)"
				}
				meta = append(meta, metaItem{"claimed by", label, ""})
			}
			meta = append(meta,
				metaItem{"raised", t.RaisedAt, "plain"},
				metaItem{"updated", t.LastUpdatedAt, "plain"})

			var rs []result
			for _, res := range ov.Results {
				if res.TaskID == t.ID {
					rs = append(rs, res)
				}
			}
			return &detailData{
				Title: t.Title, Content: t.Content, Meta: meta, Results: rs,
				BackView: "tasks", BackLabel: "Tasks",
			}
		}
	case "issue":
		for _, is := range ov.Issues {
			if is.ID != id {
				continue
			}
			return &detailData{
				Title:   is.Title,
				Content: is.Content,
				Meta: []metaItem{
					{"id", is.ID, "mono"},
					{"project", is.Project, ""},
					{"alias", is.Alias, "mono"},
					{"updated", is.LastUpdatedAt, "plain"},
				},
				BackView: "issues", BackLabel: "Issues",
			}
		}
	case "report":
		for _, rp := range ov.IssueReports {
			if rp.ID != id {
				continue
			}
			return &detailData{
				Title:   fmt.Sprintf("%s · %s · %s", rp.Project, rp.Category, rp.State),
				Content: rp.Text,
				Meta: []metaItem{
					{"id", rp.ID, "mono"},
					{"project", rp.Project, ""},
					{"version", rp.Version, "mono"},
					{"state", rp.State, ""},
					{"category", rp.Category, ""},
					{"updated", rp.LastUpdatedAt, "plain"},
				},
				BackView: "reports", BackLabel: "Status reports",
			}
		}
	}
	return nil
}

func fetchOverview(apiBase string) (*overview, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(apiBase + "/overview")
	if err != nil {
		return nil, fmt.Errorf("cannot reach the cws API at %s: %w", apiBase, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s/overview returned %s", apiBase, resp.Status)
	}
	var ov overview
	if err := json.NewDecoder(resp.Body).Decode(&ov); err != nil {
		return nil, fmt.Errorf("decoding /overview: %w", err)
	}
	return &ov, nil
}

// group folds tasks and results into their milestones, in milestone id
// order, with the unassigned bucket last.
func group(ov *overview) []milestoneGroup {
	resultsByTask := map[string][]result{}
	for _, res := range ov.Results {
		resultsByTask[res.TaskID] = append(resultsByTask[res.TaskID], res)
	}

	tasksByMilestone := map[string][]taskView{}
	for _, t := range ov.Tasks {
		key := ""
		if t.MilestoneID != nil {
			key = *t.MilestoneID
		}
		tasksByMilestone[key] = append(tasksByMilestone[key], taskView{
			task:    t,
			Results: resultsByTask[t.ID],
		})
	}

	var groups []milestoneGroup
	for i := range ov.Milestones {
		m := ov.Milestones[i]
		groups = append(groups, newGroup(&m, false, tasksByMilestone[m.ID]))
	}
	if orphans := tasksByMilestone[""]; len(orphans) > 0 {
		groups = append(groups, newGroup(nil, true, orphans))
	}
	return groups
}

func newGroup(m *milestone, unassigned bool, tasks []taskView) milestoneGroup {
	// Pending first, then by priority (lower first; nil last), then by id
	// — the order someone picking up work would want to read.
	sort.SliceStable(tasks, func(i, j int) bool {
		a, b := tasks[i], tasks[j]
		if a.Pending != b.Pending {
			return a.Pending
		}
		if (a.Priority == nil) != (b.Priority == nil) {
			return a.Priority != nil
		}
		if a.Priority != nil && *a.Priority != *b.Priority {
			return *a.Priority < *b.Priority
		}
		return len(a.ID) < len(b.ID) || (len(a.ID) == len(b.ID) && a.ID < b.ID)
	})

	g := milestoneGroup{Milestone: m, Unassigned: unassigned, Tasks: tasks}
	for _, t := range tasks {
		if t.Pending {
			g.PendingCnt++
		} else {
			g.ShippedCnt++
		}
		if t.ClaimedBy != nil && !t.ClaimExpired {
			g.ClaimedCnt++
		}
	}
	return g
}

func kst() *time.Location { return time.FixedZone("KST", 9*3600) }

// shortTime trims an RFC3339 timestamp to what a dashboard reader
// actually scans. It returns the input unchanged if it does not parse,
// rather than hiding a value the API did send.
func shortTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.In(kst()).Format("01-02 15:04")
}
