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

//go:embed templates/dashboard.html
var dashboardHTML string

// md renders the markdown bodies the API stores. Raw HTML is left
// **disabled** — goldmark escapes it unless html.WithUnsafe() is passed,
// and it is deliberately not passed here. Task and issue content is
// written by agents and by whoever POSTs to the API, so it is untrusted
// input; rendering it as live HTML would make this read-only page an
// injection surface. GFM is on for tables, strikethrough and autolinks,
// none of which reintroduce raw HTML.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

var dashboardTemplate = template.Must(
	template.New("dashboard").Funcs(template.FuncMap{
		"shortTime": shortTime,
		"markdown":  renderMarkdown,
	}).Parse(dashboardHTML))

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
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	State     string `json:"state"`
	Version   string `json:"version"`
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
}

type result struct {
	ID          string `json:"id"`
	TaskID      string `json:"task_id"`
	Status      string `json:"status"`
	Content     string `json:"content"`
	CompletedAt string `json:"completed_at"`
}

type issue struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Alias   string `json:"alias"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

type issueReport struct {
	ID       string `json:"id"`
	Project  string `json:"project"`
	Version  string `json:"version"`
	State    string `json:"state"`
	Category string `json:"category"`
	Text     string `json:"text"`
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

type pageData struct {
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

func handleDashboard(apiBase string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := pageData{
			APIBase:   apiBase,
			FetchedAt: time.Now().In(kst()).Format("2006-01-02 15:04:05 MST"),
		}

		ov, err := fetchOverview(apiBase)
		if err != nil {
			// Render the page with the error rather than a bare 502: the
			// operator wants to see *that* the API is unreachable, in the
			// place they were already looking.
			data.Err = err.Error()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			dashboardTemplate.Execute(w, data)
			return
		}

		data.Groups = group(ov)
		data.Milestones = ov.Milestones
		data.Issues = ov.Issues
		data.IssueReports = ov.IssueReports
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

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := dashboardTemplate.Execute(w, data); err != nil {
			log.Printf("render: %v", err)
		}
	}
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
