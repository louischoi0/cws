// A tiny HTTP server that accepts markdown issue reports and drives an
// issue/task/result/milestone queue for the Claude-session-loop workflow,
// all stored in its own KDS (ckdbs) database.
package main

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const (
	// Must match kds.conf's inline_cell_width. Every varchar column gets
	// exactly this many bytes, tag byte and u16 length included
	// (docs/spec/types.md), and no var-heap exists yet to spill into —
	// 1600 was chosen with margin below the 1621 ceiling the widest table
	// here (5 varchar columns) imposes (see kds.conf's comment).
	cellWidth    = 1600
	cellOverhead = 3
	maxTextBytes = cellWidth - cellOverhead
)

// identRe covers short, token-like fields (path segments, version, type,
// status, alias, state): they land in raw single-quoted KDS string
// literals with no quote-escaping available (manual/sql/sql.md), so the
// charset itself is what keeps statement construction safe.
var identRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// freeTextRe covers human-readable free text (titles, a milestone's
// filesystem directory): wider than identRe, but still excludes the
// single quote (breaks out of the KDS literal), comma (this server's own
// reply parser splits rows on it — KDS's reply format has no CSV
// quoting), backslash and raw CR/LF (breaks the one-line-per-command wire
// protocol).
var freeTextRe = regexp.MustCompile(`^[^',\\\r\n]{1,256}$`)

// idRe validates a numeric path id (or a numeric field like
// task.derived_from) before it's interpolated into a statement — KDS ids
// are engine-issued positive integers.
var idRe = regexp.MustCompile(`^[1-9][0-9]*$`)

// kdsTimestampLayout is the literal/render format for KDS's `timestamp`
// type (docs/spec/types.md: int64 microseconds since the epoch, UTC).
// Confirmed empirically: INSERT with '2026-08-27 06:30:00' round-trips
// through SELECT unchanged, no offset suffix.
const kdsTimestampLayout = "2006-01-02 15:04:05"

// kst is a fixed +9:00 offset rather than an IANA zone lookup — Korea has
// no DST, so this is exact and doesn't depend on the host having tzdata.
var kst = time.FixedZone("KST", 9*3600)

// claimLeaseTTL is how long a task claim stays valid before another agent
// may steal it. A single server-wide constant rather than a per-claim
// value: storing a per-claim TTL would need another column, and a claim
// whose TTL only the claimer knows can't be evaluated by anyone else.
// A live agent refreshes its own lease by claiming again (see
// handleClaimTask) — that is the heartbeat.
const claimLeaseTTL = 30 * time.Minute

var updatedCountRe = regexp.MustCompile(`^UPDATED (\d+)$`)

// taskStateNames maps a task's stored state code to the name the API
// speaks. The column is an int64 and not a varchar for a hard reason,
// not a stylistic one: `task` already carries five varchar columns, and
// at `inline_cell_width = 1600` a sixth pushes the row to 9657 bytes
// against the 8115 a heap page holds. Lowering the width to fit one
// would cap every body at ~995 raw bytes, and five rows already stored
// exceed that — so a varchar state cannot be added without losing
// content that exists. (KDS's `char` is exactly one byte and takes no
// length argument, so `char(10)` is not available either.) An int64
// costs 8 bytes and fits with room to spare.
//
// A code's meaning is its position, so **append only** — renaming or
// reordering an entry silently relabels every row already stored.
var taskStateNames = []string{
	"init",       // 0 — what every task is created as
	"pending",    // 1
	"inprogress", // 2
	"done",       // 3
	"blocked",    // 4
	"cancelled",  // 5
}

// taskStateInit is the code every task starts at.
const taskStateInit = 0

func taskStateName(code int) string {
	if code < 0 || code >= len(taskStateNames) {
		// A code this build has no name for is shown as itself rather
		// than as a guess — an older row, or a newer writer.
		return fmt.Sprintf("state(%d)", code)
	}
	return taskStateNames[code]
}

func taskStateCode(name string) (int, bool) {
	for i, n := range taskStateNames {
		if n == name {
			return i, true
		}
	}
	return 0, false
}

//go:embed templates/agent-report-protocol.md
var agentReportTemplateSrc string

var agentReportTemplate = template.Must(template.New("agent-report-protocol").Parse(agentReportTemplateSrc))

var insertedIDRe = regexp.MustCompile(`\bid=(\d+)\b`)

type insertRequest struct {
	Text string `json:"text"`
}

// issueReport is a row of `issues`: a markdown status report keyed by
// project/version/state/category. Distinct from `issue` below — the two
// were named before the collision was noticed; see README.md.
type issueReport struct {
	ID            string `json:"id"`
	Project       string `json:"project"`
	Version       string `json:"version"`
	State         string `json:"state"`
	Category      string `json:"category"`
	Text          string `json:"text"`
	LastUpdatedAt string `json:"last_updated_at"`
}

// issue is a row of `issue` (singular): a lightweight per-project ticket
// keyed by a human-chosen alias.
type issue struct {
	ID            string `json:"id"`
	Project       string `json:"project"`
	Alias         string `json:"alias"`
	Title         string `json:"title"`
	Content       string `json:"content"`
	LastUpdatedAt string `json:"last_updated_at"`
}

type createIssueRequest struct {
	Alias   string `json:"alias"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// milestone is a row of `milestone`: the superset over issue/task/result
// for one project directory — the intermediary agent's loop keeps working
// that directory's issues and tasks until the milestone's state reads as
// achieved (whatever token the caller uses for that; not enforced here).
type milestone struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Directory     string `json:"directory"`
	State         string `json:"state"`
	Version       string `json:"version"`
	LastUpdatedAt string `json:"last_updated_at"`
}

type createMilestoneRequest struct {
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
	// State is the explicit, caller-driven workflow state — distinct
	// from Pending above, which is derived (nothing was ever reported
	// against this task). They answer different questions and can
	// legitimately disagree.
	State string `json:"state"`
	// LastUpdatedAt moves on every write to the row — creation included,
	// and claim/release too, since those are writes like any other.
	LastUpdatedAt string `json:"last_updated_at"`
	// ClaimExpired reports whether a held claim is past claimLeaseTTL and
	// so may be stolen. False when nothing holds the task.
	ClaimExpired bool `json:"claim_expired"`

	// claimedAtRaw is the timestamp exactly as KDS stores and renders it
	// (UTC, no offset). The claim CAS compares against this literal, so
	// it must not be the KST-formatted ClaimedAt above.
	claimedAtRaw string
}

type createTaskRequest struct {
	Version     string  `json:"version"`
	Title       string  `json:"title"`
	Content     string  `json:"content"`
	Type        string  `json:"type"`
	DerivedFrom *string `json:"derived_from"`
	MilestoneID *string `json:"milestone_id"`
	Priority    *int    `json:"priority"`
}

type result struct {
	ID            string `json:"id"`
	TaskID        string `json:"task_id"`
	Status        string `json:"status"`
	Content       string `json:"content"`
	CompletedAt   string `json:"completed_at"`
	LastUpdatedAt string `json:"last_updated_at"`
}

type createResultRequest struct {
	Status  string `json:"status"`
	Content string `json:"content"`
}

type claimRequest struct {
	Agent string `json:"agent"`
}

type setStateRequest struct {
	State string `json:"state"`
}

// taskPatchable lists the columns PATCH /tasks/{id}/ may write, in a fixed
// order so the generated SET clause is deterministic rather than following
// Go's randomized map iteration.
//
// Deliberately excluded, and refused by name rather than ignored: `id`
// (identity — invariant 11 of the engine makes it unupdatable anyway),
// `raised_at`/`last_shipped_at` (lifecycle, written at creation and by
// result reporting), and `claimed_by`/`claimed_at` (owned by
// claim/release, whose compare-and-swap is the only safe way to move
// them — a plain PATCH would defeat the mutual exclusion entirely).
var taskPatchable = []string{
	"version", "title", "content", "type",
	"milestone_id", "priority", "derived_from",
}

// updateMilestoneRequest is all-optional: a nil field is left untouched,
// so a caller can PATCH just `state` without restating the rest.
type updateMilestoneRequest struct {
	Title     *string `json:"title"`
	Directory *string `json:"directory"`
	State     *string `json:"state"`
	Version   *string `json:"version"`
}

func main() {
	kdsAddr := envOr("KDS_ADDR", "127.0.0.1:19432")
	httpAddr := envOr("HTTP_ADDR", "0.0.0.0:8080")

	db := NewKDSClient(kdsAddr)
	if err := db.Connect(10, 500*time.Millisecond); err != nil {
		log.Fatalf("cannot reach kds at %s (start it first: ckdbs/build-release/kds_server --config kds.conf): %v", kdsAddr, err)
	}
	if err := ensureTables(db); err != nil {
		log.Fatalf("ensure tables: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /help", handleHelp)
	mux.HandleFunc("GET /ping", handlePing(db))
	mux.HandleFunc("GET /overview", handleOverview(db))
	mux.HandleFunc("POST /init/{project}/{$}", handleInit)
	mux.HandleFunc("POST /issues/{project}/{version}/{state}/{category}/{$}", handleCreateIssueReport(db))
	mux.HandleFunc("GET /issues/{project}/{version}/{state}/{category}/{$}", handleListIssueReports(db))
	mux.HandleFunc("POST /issue/{project}/{$}", handleCreateIssue(db))
	mux.HandleFunc("GET /issue/{project}/{$}", handleListIssues(db))
	mux.HandleFunc("GET /issue/{project}/{alias}/{$}", handleGetIssue(db))
	mux.HandleFunc("POST /milestones/{$}", handleCreateMilestone(db))
	mux.HandleFunc("GET /milestones/{$}", handleListMilestones(db))
	mux.HandleFunc("GET /milestones/{id}/{$}", handleGetMilestone(db))
	mux.HandleFunc("PATCH /milestones/{id}/{$}", handleUpdateMilestone(db))
	mux.HandleFunc("POST /tasks/{$}", handleCreateTask(db))
	mux.HandleFunc("GET /tasks/{$}", handleListTasks(db))
	mux.HandleFunc("GET /tasks/{id}/{$}", handleGetTask(db))
	mux.HandleFunc("PATCH /tasks/{id}/{$}", handleUpdateTask(db))
	mux.HandleFunc("POST /tasks/{id}/state/{$}", handleSetTaskState(db))
	mux.HandleFunc("POST /tasks/{id}/claim/{$}", handleClaimTask(db))
	mux.HandleFunc("POST /tasks/{id}/release/{$}", handleReleaseTask(db))
	mux.HandleFunc("POST /tasks/{id}/results/{$}", handleCreateResult(db))
	mux.HandleFunc("GET /tasks/{id}/results/{$}", handleListResults(db))

	log.Printf("listening on %s, kds at %s", httpAddr, kdsAddr)
	log.Fatal(http.ListenAndServe(httpAddr, mux))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func ensureTables(db *KDSClient) error {
	reply, err := db.Exec("SHOW TABLES")
	if err != nil {
		return err
	}
	existing := make(map[string]bool)
	for _, t := range strings.Fields(reply) {
		existing[t] = true
	}

	// Declaration order matters: result.task_id REFERENCES task, so task
	// must exist first. ensureTables only creates missing tables — it
	// never migrates an existing one's columns (KDS's ALTER TABLE can
	// rename but not add/drop columns; see README.md).
	tables := []struct{ name, stmt string }{
		{"issues", "CREATE TABLE issues (id int64, project varchar, version varchar, state varchar, category varchar, body varchar, last_updated_at timestamp) BTREE"},
		{"issue", "CREATE TABLE issue (id int64, project varchar, alias varchar, title varchar, content varchar, last_updated_at timestamp) BTREE"},
		{"milestone", "CREATE TABLE milestone (id int64, title varchar, directory varchar, state varchar, version varchar, last_updated_at timestamp) BTREE"},
		{"task", "CREATE TABLE task (id int64, version varchar, title varchar, content varchar, type varchar, raised_at timestamp, last_shipped_at timestamp, derived_from int64 NULL, milestone_id int64 NULL REFERENCES milestone, priority int64 NULL, claimed_by varchar NULL, claimed_at timestamp NULL, state int64, last_updated_at timestamp) BTREE"},
		{"result", "CREATE TABLE result (id int64, task_id int64 REFERENCES task, status varchar, content varchar, completed_at timestamp, last_updated_at timestamp) BTREE"},
	}
	for _, t := range tables {
		if existing[t.name] {
			continue
		}
		if _, err := db.Exec(t.stmt); err != nil {
			return fmt.Errorf("create %s: %w", t.name, err)
		}
	}
	return nil
}

// helpParam documents one path segment, query parameter, or JSON body
// field for handleHelp's output.
type helpParam struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// helpEndpoint documents one route for handleHelp's output. This is
// maintained by hand alongside the mux.HandleFunc calls in main() — there
// is no reflection-based derivation, so a new route needs an entry here
// too, or it won't show up in GET /help.
type helpEndpoint struct {
	Method      string      `json:"method"`
	Path        string      `json:"path"`
	Description string      `json:"description"`
	PathParams  []helpParam `json:"path_params,omitempty"`
	QueryParams []helpParam `json:"query_params,omitempty"`
	Body        []helpParam `json:"body,omitempty"`
	Response    string      `json:"response"`
}

var helpEndpoints = []helpEndpoint{
	{
		Method:      "GET",
		Path:        "/help",
		Description: "This document: every route, its parameters, and what it returns.",
		Response:    "JSON: {name, description, endpoints: [...]}.",
	},
	{
		Method:      "GET",
		Path:        "/ping",
		Description: "Round-trips KDS's own PING, so a caller can tell the whole stack is up, not just this HTTP process.",
		Response:    `{"kds": "PONG"}, or 502 if KDS is unreachable.`,
	},
	{
		Method:      "GET",
		Path:        "/overview",
		Description: "Every row of every table in one read-only snapshot, for a dashboard. Every row carries last_updated_at, stamped on creation and on every write to it. Exists because the scoped endpoints cannot answer \"show me everything\": GET /tasks/ requires a milestone_id, GET /issue/ needs a project name with no way to enumerate projects, and GET /issues/ needs all four path segments. Unpaginated by design.",
		Response:    "{milestones, tasks, results, issues, issue_reports}, each a JSON array in id order.",
	},
	{
		Method:      "POST",
		Path:        "/init/{project}/",
		Description: "Renders templates/agent-report-protocol.md for one project name — plain markdown text meant to be copy-pasted into that project's own CLAUDE.md, not JSON.",
		PathParams:  []helpParam{{"project", identRe.String()}},
		Response:    "text/markdown body (not JSON).",
	},
	{
		Method:      "POST",
		Path:        "/issues/{project}/{version}/{state}/{category}/",
		Description: "Creates a markdown status report row in the issues (plural) table. Unrelated to the issue (singular) table below despite the similar name.",
		PathParams: []helpParam{
			{"project", identRe.String()}, {"version", identRe.String()},
			{"state", identRe.String()}, {"category", identRe.String()},
		},
		Body:     []helpParam{{"text", "markdown, non-empty, ~1.2 KB raw text cap (stored base64-encoded)"}},
		Response: "201 with {project, version, state, category, kds: <raw KDS reply>}.",
	},
	{
		Method:      "GET",
		Path:        "/issues/{project}/{version}/{state}/{category}/",
		Description: "Exact-match lookup against the issues (plural) table on all four path segments.",
		PathParams: []helpParam{
			{"project", identRe.String()}, {"version", identRe.String()},
			{"state", identRe.String()}, {"category", identRe.String()},
		},
		Response: "JSON array (possibly empty) of {id, project, version, state, category, text}.",
	},
	{
		Method:      "POST",
		Path:        "/issue/{project}/",
		Description: "Creates a lightweight per-project ticket in the issue (singular) table.",
		PathParams:  []helpParam{{"project", identRe.String()}},
		Body: []helpParam{
			{"alias", identRe.String() + " — caller-chosen; uniqueness NOT enforced by KDS, check first with GET .../{alias}/"},
			{"title", freeTextRe.String()},
			{"content", "markdown, non-empty, ~1.2 KB raw text cap (stored base64-encoded)"},
		},
		Response: "201 with {id, project, alias, title, content}.",
	},
	{
		Method:      "GET",
		Path:        "/issue/{project}/",
		Description: "Lists every issue for one project.",
		PathParams:  []helpParam{{"project", identRe.String()}},
		Response:    "JSON array of {id, project, alias, title, content}.",
	},
	{
		Method:      "GET",
		Path:        "/issue/{project}/{alias}/",
		Description: "Fetches one issue by its project + alias.",
		PathParams:  []helpParam{{"project", identRe.String()}, {"alias", identRe.String()}},
		Response:    "{id, project, alias, title, content}, or 404.",
	},
	{
		Method:      "POST",
		Path:        "/milestones/",
		Description: "Creates a milestone: the superset over one project's issue/task/result, the thing the loop is working toward. No update endpoint exists — state is set once, at creation.",
		Body: []helpParam{
			{"title", freeTextRe.String()},
			{"directory", freeTextRe.String() + " — the project's filesystem path; not validated as an actual path"},
			{"state", identRe.String() + " — open-ended, no fixed \"achieved\" value"},
			{"version", identRe.String()},
		},
		Response: "201 with {id, title, directory, state, version}.",
	},
	{
		Method:      "GET",
		Path:        "/milestones/",
		Description: "Lists every milestone.",
		Response:    "JSON array of {id, title, directory, state, version}.",
	},
	{
		Method:      "GET",
		Path:        "/milestones/{id}/",
		Description: "Fetches one milestone by id.",
		PathParams:  []helpParam{{"id", "positive integer"}},
		Response:    "{id, title, directory, state, version}, or 404.",
	},
	{
		Method:      "PATCH",
		Path:        "/milestones/{id}/",
		Description: "Updates a milestone in place. Every field is optional — only those present are written, so advancing state alone needs no restating of the rest. This is how the loop records that a milestone was reached.",
		PathParams:  []helpParam{{"id", "positive integer"}},
		Body: []helpParam{
			{"title", "optional, " + freeTextRe.String()},
			{"directory", "optional, " + freeTextRe.String()},
			{"state", "optional, " + identRe.String()},
			{"version", "optional, " + identRe.String()},
		},
		Response: "the full updated milestone, or 404. 400 if no field was supplied.",
	},
	{
		Method:      "POST",
		Path:        "/tasks/",
		Description: "Creates a task — one unit of development work in the loop.",
		Body: []helpParam{
			{"version", identRe.String()},
			{"title", freeTextRe.String()},
			{"content", "markdown, non-empty, ~1.2 KB raw text cap (stored base64-encoded)"},
			{"type", identRe.String() + " — open-ended, e.g. implement/experimental/hotfix/benchmarking/revising"},
			{"derived_from", "optional: another task's id (string), if this is a subtask. Checked app-side, not an engine FK (self-reference isn't supported by KDS)"},
			{"milestone_id", "optional: a milestone's id (string). Engine-enforced FK — 400 if it doesn't exist"},
			{"priority", "optional: integer, no fixed range or direction"},
		},
		Response: "201 with the created task. raised_at/last_shipped_at start equal — that equality is this schema's \"never shipped\" convention, and `pending` is derived from it. `state` always starts at `init`; it is not settable here, only through POST /tasks/{id}/state/.",
	},
	{
		Method:      "GET",
		Path:        "/tasks/",
		Description: "Lists the tasks of one milestone. The queue is always read in the context of a milestone, so milestone_id is required — an unscoped listing would hand an agent work belonging to another goal.",
		QueryParams: []helpParam{
			{"milestone_id", "REQUIRED, positive integer. 400 without it. GET /milestones/ lists them"},
			{"pending", "true to list only tasks whose last_shipped_at still equals raised_at (never reported against)"},
			{"claimable", "true to list only tasks nothing currently holds (unclaimed, or claim expired). Advisory — another agent can claim one between this read and your POST; the claim call is what decides"},
			{"state", "exact workflow state; one of: " + strings.Join(taskStateNames, ", ")},
		},
		Response: "JSON array of tasks, or 400 if milestone_id is missing or malformed.",
	},
	{
		Method:      "PATCH",
		Path:        "/tasks/{id}/",
		Description: "Updates a task in place. Only keys present in the body are written; an explicit null clears a nullable column (milestone_id, priority, derived_from). An unknown or non-patchable key is refused by name rather than ignored.",
		PathParams:  []helpParam{{"id", "positive integer"}},
		Body: []helpParam{
			{"version", "optional, " + identRe.String()},
			{"title", "optional, " + freeTextRe.String()},
			{"content", "optional, markdown, non-empty, ~1.2 KB raw text cap"},
			{"type", "optional, " + identRe.String()},
			{"milestone_id", "optional, positive integer or null. Engine-enforced FK — 400 if the milestone doesn't exist"},
			{"priority", "optional, integer or null"},
			{"derived_from", "optional, positive integer or null. Checked app-side; cannot point at the task itself"},
		},
		Response: "the full updated task, 404 if no such task, 400 if no field was supplied or a non-patchable one was (raised_at, last_shipped_at, claimed_by and claimed_at are refused — leases move only through claim/release).",
	},
	{
		Method:      "GET",
		Path:        "/tasks/{id}/",
		Description: "Fetches one task by id.",
		PathParams:  []helpParam{{"id", "positive integer"}},
		Response:    "the task, or 404.",
	},
	{
		Method:      "POST",
		Path:        "/tasks/{id}/state/",
		Description: "Moves a task through its workflow. State is stored as an int64 code and spoken as a name; every task is created as `init` and can only change here — PATCH refuses the field, because a transition is not a field edit. No transition table is enforced: done -> inprogress is legal, since work reopens.",
		PathParams:  []helpParam{{"id", "positive integer"}},
		Body:        []helpParam{{"state", "one of: " + strings.Join(taskStateNames, ", ")}},
		Response:    "the full updated task, 404 if no such task, 400 if the state name is unknown (the message lists them).",
	},
	{
		Method:      "POST",
		Path:        "/tasks/{id}/claim/",
		Description: "Takes an exclusive lease on a task so two sessions can't work it at once. Claiming a task this agent already holds refreshes the lease — that is the heartbeat. A lease older than " + claimLeaseTTL.String() + " may be stolen by another agent. Atomic: concurrent claimers race a compare-and-swap and exactly one wins.",
		PathParams:  []helpParam{{"id", "positive integer"}},
		Body:        []helpParam{{"agent", identRe.String() + " — whoever is claiming; also the identity release checks"}},
		Response:    "the task with claimed_by/claimed_at set, 404 if no such task, or 409 if another agent holds a live lease (body names the holder).",
	},
	{
		Method:      "POST",
		Path:        "/tasks/{id}/release/",
		Description: "Drops a claim. Only the holding agent may release, so a stale worker cannot clear a lease that has since been stolen and is being worked by someone else. Reporting a result releases the claim on its own — this is for giving a task back without reporting.",
		PathParams:  []helpParam{{"id", "positive integer"}},
		Body:        []helpParam{{"agent", identRe.String() + " — must match the current holder"}},
		Response:    "the released task, 404 if no such task, or 409 if this agent does not hold it.",
	},
	{
		Method:      "POST",
		Path:        "/tasks/{id}/results/",
		Description: "Reports the outcome of one attempt at a task — the \"work is done\" call. Also bumps last_shipped_at and drops any claim, atomically with the insert. A task can accumulate many results over its life; this is never a close/final-verdict call.",
		PathParams:  []helpParam{{"id", "positive integer — 404 if the task doesn't exist (KDS FK_VIOLATION mapped to 404 here, not 400, since the bad id is in the URL)"}},
		Body: []helpParam{
			{"status", identRe.String() + " — open-ended, e.g. done/failed/partial/blocked"},
			{"content", "markdown, non-empty, ~1.2 KB raw text cap (stored base64-encoded)"},
		},
		Response: "201 with {id, task_id, status, content, completed_at}.",
	},
	{
		Method:      "GET",
		Path:        "/tasks/{id}/results/",
		Description: "Lists every result reported against one task.",
		PathParams:  []helpParam{{"id", "positive integer"}},
		Response:    "JSON array of results.",
	},
}

// handleHelp is self-describing: helpEndpoints below is the same list a
// human reads in README.md, just structured for a caller (or an agent) to
// consume without reading source.
func handleHelp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"name":        "cws",
		"description": "Issue/milestone/task/result API for the Claude-session-loop workflow, backed by this server's own KDS instance.",
		"endpoints":   helpEndpoints,
	})
}

// handleOverview returns every row of every table in one read-only
// snapshot. It exists because the scoped endpoints cannot answer "show me
// everything": GET /tasks/ requires a milestone_id (so a NULL-milestone
// task is unreachable there), GET /issue/ needs a project name with no way
// to enumerate projects, and GET /issues/ needs all four path segments.
// A dashboard needs the whole picture, and assembling it client-side is
// impossible rather than merely tedious.
//
// Deliberately unpaginated: this is a small operational store, and a
// partial snapshot presented as a complete one is the failure worth
// avoiding here. If it ever grows past one response, that is a real
// design decision, not a limit to raise quietly.
func handleOverview(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		milestonesReply, err := db.Exec("SELECT * FROM milestone ORDER BY id")
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		milestones, err := parseMilestoneRows(milestonesReply)
		if err != nil {
			http.Error(w, "kds reply (milestone): "+err.Error(), http.StatusBadGateway)
			return
		}

		tasksReply, err := db.Exec("SELECT * FROM task ORDER BY id")
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		tasks, err := parseTaskRows(tasksReply)
		if err != nil {
			http.Error(w, "kds reply (task): "+err.Error(), http.StatusBadGateway)
			return
		}

		resultsReply, err := db.Exec("SELECT * FROM result ORDER BY id")
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		results, err := parseResultRows(resultsReply)
		if err != nil {
			http.Error(w, "kds reply (result): "+err.Error(), http.StatusBadGateway)
			return
		}

		issuesReply, err := db.Exec("SELECT * FROM issue ORDER BY id")
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		issues, err := parseIssueRows(issuesReply)
		if err != nil {
			http.Error(w, "kds reply (issue): "+err.Error(), http.StatusBadGateway)
			return
		}

		reportsReply, err := db.Exec("SELECT * FROM issues ORDER BY id")
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		reports, err := parseIssueReportRows(reportsReply)
		if err != nil {
			http.Error(w, "kds reply (issues): "+err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"milestones":    milestones,
			"tasks":         tasks,
			"results":       results,
			"issues":        issues,
			"issue_reports": reports,
		})
	}
}

// handlePing round-trips KDS's own PING, so a caller can tell the whole
// stack is up rather than just this HTTP process.
func handlePing(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reply, err := db.Exec("PING")
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"kds": reply})
	}
}

// handleInit renders templates/agent-report-protocol.md for one project
// name — plain text meant to be copy-pasted into that project's own
// CLAUDE.md, not JSON (a JSON wrapper would just have to be unescaped by
// hand before pasting).
func handleInit(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !identRe.MatchString(project) {
		http.Error(w, fmt.Sprintf("invalid project %q: must match %s", project, identRe.String()), http.StatusBadRequest)
		return
	}

	serverURL := envOr("PUBLIC_BASE_URL", "http://"+r.Host)

	var buf bytes.Buffer
	if err := agentReportTemplate.Execute(&buf, map[string]string{
		"Project":   project,
		"ServerURL": serverURL,
	}); err != nil {
		http.Error(w, "render template: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write(buf.Bytes())
}

// issueReportPathValues reads and validates the /issues/ route's four path
// segments.
func issueReportPathValues(w http.ResponseWriter, r *http.Request) (map[string]string, bool) {
	values := make(map[string]string, 4)
	for _, name := range []string{"project", "version", "state", "category"} {
		v := r.PathValue(name)
		if !identRe.MatchString(v) {
			http.Error(w, fmt.Sprintf("invalid %s %q: must match %s", name, v, identRe.String()), http.StatusBadRequest)
			return nil, false
		}
		values[name] = v
	}
	return values, true
}

// pathID reads and validates the {id} route segment shared by the task and
// milestone routes.
func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		http.Error(w, fmt.Sprintf("invalid id %q: must be a positive integer", id), http.StatusBadRequest)
		return "", false
	}
	return id, true
}

func handleCreateIssueReport(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path, ok := issueReportPathValues(w, r)
		if !ok {
			return
		}

		var req insertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Text == "" {
			http.Error(w, "text must not be empty", http.StatusBadRequest)
			return
		}

		// Stored base64: the wire protocol is one line per command with no
		// raw-newline or quote-escaping (manual/client/client.md §2), and
		// markdown routinely contains both.
		body := base64.StdEncoding.EncodeToString([]byte(req.Text))
		if len(body) > maxTextBytes {
			http.Error(w, fmt.Sprintf("text too large: %d bytes base64-encoded, cell capacity is %d", len(body), maxTextBytes), http.StatusRequestEntityTooLarge)
			return
		}

		stmt := fmt.Sprintf("INSERT INTO issues VALUES ('%s', '%s', '%s', '%s', '%s', '%s')",
			path["project"], path["version"], path["state"], path["category"], body,
			kdsTimestampLiteral(time.Now()))
		reply, err := db.Exec(stmt)
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"project":  path["project"],
			"version":  path["version"],
			"state":    path["state"],
			"category": path["category"],
			"kds":      reply,
		})
	}
}

func handleListIssueReports(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path, ok := issueReportPathValues(w, r)
		if !ok {
			return
		}

		stmt := fmt.Sprintf(
			"SELECT * FROM issues WHERE project = '%s' AND version = '%s' AND state = '%s' AND category = '%s'",
			path["project"], path["version"], path["state"], path["category"],
		)
		reply, err := db.Exec(stmt)
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}

		reports, err := parseIssueReportRows(reply)
		if err != nil {
			http.Error(w, "kds reply: "+err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reports)
	}
}

func handleCreateIssue(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		if !identRe.MatchString(project) {
			http.Error(w, fmt.Sprintf("invalid project %q: must match %s", project, identRe.String()), http.StatusBadRequest)
			return
		}

		var req createIssueRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !identRe.MatchString(req.Alias) {
			http.Error(w, fmt.Sprintf("invalid alias %q: must match %s", req.Alias, identRe.String()), http.StatusBadRequest)
			return
		}
		if !freeTextRe.MatchString(req.Title) {
			http.Error(w, fmt.Sprintf("invalid title %q: must match %s", req.Title, freeTextRe.String()), http.StatusBadRequest)
			return
		}
		if req.Content == "" {
			http.Error(w, "content must not be empty", http.StatusBadRequest)
			return
		}
		content := base64.StdEncoding.EncodeToString([]byte(req.Content))
		if len(content) > maxTextBytes {
			http.Error(w, fmt.Sprintf("content too large: %d bytes base64-encoded, cell capacity is %d", len(content), maxTextBytes), http.StatusRequestEntityTooLarge)
			return
		}

		now := kdsTimestampLiteral(time.Now())
		stmt := fmt.Sprintf("INSERT INTO issue VALUES ('%s', '%s', '%s', '%s', '%s')",
			project, req.Alias, req.Title, content, now)
		reply, err := db.Exec(stmt)
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		id, _ := parseInsertedID(reply)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(issue{ID: id, Project: project, Alias: req.Alias,
			Title: req.Title, Content: req.Content, LastUpdatedAt: formatKST(now)})
	}
}

func handleListIssues(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		if !identRe.MatchString(project) {
			http.Error(w, fmt.Sprintf("invalid project %q: must match %s", project, identRe.String()), http.StatusBadRequest)
			return
		}

		reply, err := db.Exec(fmt.Sprintf("SELECT * FROM issue WHERE project = '%s' ORDER BY id", project))
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		issues, err := parseIssueRows(reply)
		if err != nil {
			http.Error(w, "kds reply: "+err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(issues)
	}
}

func handleGetIssue(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		alias := r.PathValue("alias")
		if !identRe.MatchString(project) {
			http.Error(w, fmt.Sprintf("invalid project %q: must match %s", project, identRe.String()), http.StatusBadRequest)
			return
		}
		if !identRe.MatchString(alias) {
			http.Error(w, fmt.Sprintf("invalid alias %q: must match %s", alias, identRe.String()), http.StatusBadRequest)
			return
		}

		reply, err := db.Exec(fmt.Sprintf("SELECT * FROM issue WHERE project = '%s' AND alias = '%s'", project, alias))
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		issues, err := parseIssueRows(reply)
		if err != nil {
			http.Error(w, "kds reply: "+err.Error(), http.StatusBadGateway)
			return
		}
		if len(issues) == 0 {
			http.Error(w, "issue not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(issues[0])
	}
}

func handleCreateMilestone(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createMilestoneRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !freeTextRe.MatchString(req.Title) {
			http.Error(w, fmt.Sprintf("invalid title %q: must match %s", req.Title, freeTextRe.String()), http.StatusBadRequest)
			return
		}
		if !freeTextRe.MatchString(req.Directory) {
			http.Error(w, fmt.Sprintf("invalid directory %q: must match %s", req.Directory, freeTextRe.String()), http.StatusBadRequest)
			return
		}
		if !identRe.MatchString(req.State) {
			http.Error(w, fmt.Sprintf("invalid state %q: must match %s", req.State, identRe.String()), http.StatusBadRequest)
			return
		}
		if !identRe.MatchString(req.Version) {
			http.Error(w, fmt.Sprintf("invalid version %q: must match %s", req.Version, identRe.String()), http.StatusBadRequest)
			return
		}

		now := kdsTimestampLiteral(time.Now())
		stmt := fmt.Sprintf("INSERT INTO milestone VALUES ('%s', '%s', '%s', '%s', '%s')",
			req.Title, req.Directory, req.State, req.Version, now)
		reply, err := db.Exec(stmt)
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		id, _ := parseInsertedID(reply)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(milestone{ID: id, Title: req.Title, Directory: req.Directory,
			State: req.State, Version: req.Version, LastUpdatedAt: formatKST(now)})
	}
}

func handleListMilestones(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reply, err := db.Exec("SELECT * FROM milestone ORDER BY id")
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		milestones, err := parseMilestoneRows(reply)
		if err != nil {
			http.Error(w, "kds reply: "+err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(milestones)
	}
}

func handleGetMilestone(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}

		reply, err := db.Exec(fmt.Sprintf("SELECT * FROM milestone WHERE id = %s", id))
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		milestones, err := parseMilestoneRows(reply)
		if err != nil {
			http.Error(w, "kds reply: "+err.Error(), http.StatusBadGateway)
			return
		}
		if len(milestones) == 0 {
			http.Error(w, "milestone not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(milestones[0])
	}
}

// handleUpdateMilestone patches one milestone in place. Every field is
// optional; only those present in the body are written, so advancing
// `state` alone doesn't require restating the rest. This is what lets the
// loop record that a milestone was reached — without it, a milestone's
// state was fixed at creation and the loop had no termination signal.
func handleUpdateMilestone(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}

		var req updateMilestoneRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}

		var sets []string
		add := func(field, value string, re *regexp.Regexp) bool {
			if !re.MatchString(value) {
				http.Error(w, fmt.Sprintf("invalid %s %q: must match %s", field, value, re.String()), http.StatusBadRequest)
				return false
			}
			sets = append(sets, fmt.Sprintf("%s = '%s'", field, value))
			return true
		}
		if req.Title != nil && !add("title", *req.Title, freeTextRe) {
			return
		}
		if req.Directory != nil && !add("directory", *req.Directory, freeTextRe) {
			return
		}
		if req.State != nil && !add("state", *req.State, identRe) {
			return
		}
		if req.Version != nil && !add("version", *req.Version, identRe) {
			return
		}
		if len(sets) == 0 {
			http.Error(w, "no fields to update: pass at least one of title, directory, state, version", http.StatusBadRequest)
			return
		}

		sets = append(sets, fmt.Sprintf("last_updated_at = '%s'", kdsTimestampLiteral(time.Now())))
		reply, err := db.Exec(fmt.Sprintf("UPDATE milestone SET %s WHERE id = %s", strings.Join(sets, ", "), id))
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		if updatedCount(reply) != 1 {
			http.Error(w, "milestone not found", http.StatusNotFound)
			return
		}

		// Re-read so the reply reflects what is actually stored, including
		// the fields this request left alone.
		fresh, err := db.Exec(fmt.Sprintf("SELECT * FROM milestone WHERE id = %s", id))
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		milestones, err := parseMilestoneRows(fresh)
		if err != nil {
			http.Error(w, "kds reply: "+err.Error(), http.StatusBadGateway)
			return
		}
		if len(milestones) == 0 {
			http.Error(w, "milestone not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(milestones[0])
	}
}

func handleCreateTask(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !identRe.MatchString(req.Version) {
			http.Error(w, fmt.Sprintf("invalid version %q: must match %s", req.Version, identRe.String()), http.StatusBadRequest)
			return
		}
		if !identRe.MatchString(req.Type) {
			http.Error(w, fmt.Sprintf("invalid type %q: must match %s", req.Type, identRe.String()), http.StatusBadRequest)
			return
		}
		if !freeTextRe.MatchString(req.Title) {
			http.Error(w, fmt.Sprintf("invalid title %q: must match %s", req.Title, freeTextRe.String()), http.StatusBadRequest)
			return
		}
		if req.Content == "" {
			http.Error(w, "content must not be empty", http.StatusBadRequest)
			return
		}
		content := base64.StdEncoding.EncodeToString([]byte(req.Content))
		if len(content) > maxTextBytes {
			http.Error(w, fmt.Sprintf("content too large: %d bytes base64-encoded, cell capacity is %d", len(content), maxTextBytes), http.StatusRequestEntityTooLarge)
			return
		}

		// derived_from has no engine-enforced FK: KDS refuses a column
		// REFERENCES-ing the table it's declared on ("references unknown
		// relation", confirmed empirically — the parent doesn't exist yet
		// at that point in its own CREATE TABLE). So existence is checked
		// here instead, app-side.
		derivedFromLiteral := "NULL"
		if req.DerivedFrom != nil {
			if !idRe.MatchString(*req.DerivedFrom) {
				http.Error(w, fmt.Sprintf("invalid derived_from %q: must be a positive integer", *req.DerivedFrom), http.StatusBadRequest)
				return
			}
			parentReply, err := db.Exec(fmt.Sprintf("SELECT id FROM task WHERE id = %s", *req.DerivedFrom))
			if err != nil {
				http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
				return
			}
			if len(splitSelectRows(parentReply)) == 0 {
				http.Error(w, fmt.Sprintf("derived_from task %s not found", *req.DerivedFrom), http.StatusBadRequest)
				return
			}
			derivedFromLiteral = *req.DerivedFrom
		}

		// milestone_id, unlike derived_from, references a *different*
		// table that already exists by the time task is created, so it
		// is a real engine-enforced FK — no app-side existence check
		// needed, just an FK_VIOLATION to catch below.
		milestoneIDLiteral := "NULL"
		if req.MilestoneID != nil {
			if !idRe.MatchString(*req.MilestoneID) {
				http.Error(w, fmt.Sprintf("invalid milestone_id %q: must be a positive integer", *req.MilestoneID), http.StatusBadRequest)
				return
			}
			milestoneIDLiteral = *req.MilestoneID
		}

		priorityLiteral := "NULL"
		if req.Priority != nil {
			priorityLiteral = fmt.Sprintf("%d", *req.Priority)
		}

		// last_shipped_at starts equal to raised_at — the convention
		// carries over here for consistency: "never shipped" is
		// raised_at == last_shipped_at, not an absent value.
		// handleListTasks derives Pending from the same equality.
		now := kdsTimestampLiteral(time.Now())
		// state always starts at init; it moves only through
		// POST /tasks/{id}/state/, never at creation.
		stmt := fmt.Sprintf("INSERT INTO task VALUES ('%s', '%s', '%s', '%s', '%s', '%s', %s, %s, %s, NULL, NULL, %d, '%s')",
			req.Version, req.Title, content, req.Type, now, now,
			derivedFromLiteral, milestoneIDLiteral, priorityLiteral, taskStateInit, now)
		reply, err := db.Exec(stmt)
		if err != nil {
			if strings.Contains(err.Error(), "FK_VIOLATION") {
				http.Error(w, fmt.Sprintf("milestone %s not found", *req.MilestoneID), http.StatusBadRequest)
				return
			}
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		id, _ := parseInsertedID(reply)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(task{
			ID:            id,
			Version:       req.Version,
			Title:         req.Title,
			Content:       req.Content,
			Type:          req.Type,
			RaisedAt:      formatKST(now),
			LastShippedAt: formatKST(now),
			Pending:       true,
			DerivedFrom:   req.DerivedFrom,
			MilestoneID:   req.MilestoneID,
			Priority:      req.Priority,
			State:         taskStateName(taskStateInit),
			LastUpdatedAt: formatKST(now),
		})
	}
}

func handleListTasks(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// milestone_id is mandatory: the queue is always read in the
		// context of the milestone being worked toward, so an unscoped
		// listing would hand an agent tasks belonging to some other goal.
		// Refused rather than defaulted — there is no sensible default
		// milestone, and picking one silently would be a wrong answer
		// shaped like a right one.
		mid := r.URL.Query().Get("milestone_id")
		if mid == "" {
			http.Error(w, "milestone_id is required: pass ?milestone_id=<id> (GET /milestones/ lists them)", http.StatusBadRequest)
			return
		}
		if !idRe.MatchString(mid) {
			http.Error(w, fmt.Sprintf("invalid milestone_id %q: must be a positive integer", mid), http.StatusBadRequest)
			return
		}

		reply, err := db.Exec(fmt.Sprintf("SELECT * FROM task WHERE milestone_id = %s ORDER BY id", mid))
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		tasks, err := parseTaskRows(reply)
		if err != nil {
			http.Error(w, "kds reply: "+err.Error(), http.StatusBadGateway)
			return
		}

		if r.URL.Query().Get("pending") == "true" {
			filtered := tasks[:0]
			for _, t := range tasks {
				if t.Pending {
					filtered = append(filtered, t)
				}
			}
			tasks = filtered
		}

		if want := r.URL.Query().Get("state"); want != "" {
			if _, ok := taskStateCode(want); !ok {
				http.Error(w, fmt.Sprintf("unknown state %q: one of %s",
					want, strings.Join(taskStateNames, ", ")), http.StatusBadRequest)
				return
			}
			filtered := tasks[:0]
			for _, t := range tasks {
				if t.State == want {
					filtered = append(filtered, t)
				}
			}
			tasks = filtered
		}

		// claimable = nothing holds it, or what holds it has expired. This
		// is advisory only: the list is a snapshot, and another agent can
		// claim any of these between this read and the caller's POST — the
		// claim CAS is what actually decides, not this filter.
		if r.URL.Query().Get("claimable") == "true" {
			filtered := tasks[:0]
			for _, t := range tasks {
				if t.ClaimedBy == nil || t.ClaimExpired {
					filtered = append(filtered, t)
				}
			}
			tasks = filtered
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tasks)
	}
}

func handleGetTask(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}

		t, err := fetchTask(db, id)
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		if t == nil {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(t)
	}
}

// handleUpdateTask patches one task in place. Every field is optional;
// only keys actually present in the body are written, and an explicit
// JSON `null` clears a nullable column (which is why the body is decoded
// as raw messages — with plain pointers, "absent" and "null" arrive
// identically and clearing would be unexpressible).
//
// An unknown or non-patchable key is refused by name rather than
// ignored: silently dropping `claimed_by` from a PATCH would read as
// having moved a lease that in fact never moved.
func handleUpdateTask(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}

		var fields map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}

		patchable := make(map[string]bool, len(taskPatchable))
		for _, k := range taskPatchable {
			patchable[k] = true
		}
		var unknown []string
		for k := range fields {
			if !patchable[k] {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			msg := fmt.Sprintf("cannot patch %s; patchable fields are %s",
				strings.Join(unknown, ", "), strings.Join(taskPatchable, ", "))
			for _, k := range unknown {
				switch k {
				case "state":
					msg += ". Use POST /tasks/{id}/state/ to move a task's state"
				case "claimed_by", "claimed_at":
					msg += ". Use POST /tasks/{id}/claim/ and .../release/ for the lease"
				}
			}
			http.Error(w, msg, http.StatusBadRequest)
			return
		}

		// isNull distinguishes an explicit JSON null from a value.
		isNull := func(raw json.RawMessage) bool {
			return string(bytes.TrimSpace(raw)) == "null"
		}
		badRequest := func(msg string) {
			http.Error(w, msg, http.StatusBadRequest)
		}

		var sets []string
		for _, key := range taskPatchable {
			raw, present := fields[key]
			if !present {
				continue
			}

			switch key {
			case "version", "type":
				var v string
				if err := json.Unmarshal(raw, &v); err != nil {
					badRequest(fmt.Sprintf("%s must be a string", key))
					return
				}
				if !identRe.MatchString(v) {
					badRequest(fmt.Sprintf("invalid %s %q: must match %s", key, v, identRe.String()))
					return
				}
				sets = append(sets, fmt.Sprintf("%s = '%s'", key, v))

			case "title":
				var v string
				if err := json.Unmarshal(raw, &v); err != nil {
					badRequest("title must be a string")
					return
				}
				if !freeTextRe.MatchString(v) {
					badRequest(fmt.Sprintf("invalid title %q: must match %s", v, freeTextRe.String()))
					return
				}
				sets = append(sets, fmt.Sprintf("title = '%s'", v))

			case "content":
				var v string
				if err := json.Unmarshal(raw, &v); err != nil {
					badRequest("content must be a string")
					return
				}
				if v == "" {
					badRequest("content must not be empty")
					return
				}
				encoded := base64.StdEncoding.EncodeToString([]byte(v))
				if len(encoded) > maxTextBytes {
					http.Error(w, fmt.Sprintf("content too large: %d bytes base64-encoded, cell capacity is %d", len(encoded), maxTextBytes), http.StatusRequestEntityTooLarge)
					return
				}
				sets = append(sets, fmt.Sprintf("content = '%s'", encoded))

			case "priority":
				if isNull(raw) {
					sets = append(sets, "priority = NULL")
					break
				}
				var v int
				if err := json.Unmarshal(raw, &v); err != nil {
					badRequest("priority must be an integer or null")
					return
				}
				sets = append(sets, fmt.Sprintf("priority = %d", v))

			case "milestone_id":
				if isNull(raw) {
					sets = append(sets, "milestone_id = NULL")
					break
				}
				var v string
				if err := json.Unmarshal(raw, &v); err != nil {
					badRequest("milestone_id must be a string or null")
					return
				}
				if !idRe.MatchString(v) {
					badRequest(fmt.Sprintf("invalid milestone_id %q: must be a positive integer", v))
					return
				}
				// Existence is enforced by the engine FK; a bad id surfaces
				// as FK_VIOLATION on the UPDATE below.
				sets = append(sets, fmt.Sprintf("milestone_id = %s", v))

			case "derived_from":
				if isNull(raw) {
					sets = append(sets, "derived_from = NULL")
					break
				}
				var v string
				if err := json.Unmarshal(raw, &v); err != nil {
					badRequest("derived_from must be a string or null")
					return
				}
				if !idRe.MatchString(v) {
					badRequest(fmt.Sprintf("invalid derived_from %q: must be a positive integer", v))
					return
				}
				if v == id {
					badRequest("derived_from cannot point at the task itself")
					return
				}
				// derived_from has no engine FK (KDS refuses a self-
				// referential REFERENCES), so the parent is checked here.
				parent, err := fetchTask(db, v)
				if err != nil {
					http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
					return
				}
				if parent == nil {
					badRequest(fmt.Sprintf("derived_from task %s not found", v))
					return
				}
				sets = append(sets, fmt.Sprintf("derived_from = %s", v))
			}
		}

		if len(sets) == 0 {
			badRequest("no fields to update: pass at least one of " + strings.Join(taskPatchable, ", "))
			return
		}

		sets = append(sets, fmt.Sprintf("last_updated_at = '%s'", kdsTimestampLiteral(time.Now())))
		reply, err := db.Exec(fmt.Sprintf("UPDATE task SET %s WHERE id = %s", strings.Join(sets, ", "), id))
		if err != nil {
			if strings.Contains(err.Error(), "FK_VIOLATION") {
				badRequest("milestone not found")
				return
			}
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		if updatedCount(reply) != 1 {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}

		respondClaimed(w, db, id)
	}
}

// handleSetTaskState moves a task through its workflow. State lives
// behind its own endpoint rather than in PATCH for the same reason a
// claim does: it is a transition, not a field edit, and keeping one path
// per concern means there is exactly one place a state can change.
func handleSetTaskState(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}

		var req setStateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		code, known := taskStateCode(req.State)
		if !known {
			http.Error(w, fmt.Sprintf("unknown state %q: one of %s",
				req.State, strings.Join(taskStateNames, ", ")), http.StatusBadRequest)
			return
		}

		// Any state may follow any other: no transition table is enforced,
		// because nothing here knows enough to rule one out — a task can
		// legitimately go done -> inprogress when work reopens.
		reply, err := db.Exec(fmt.Sprintf("UPDATE task SET state = %d, last_updated_at = '%s' WHERE id = %s",
			code, kdsTimestampLiteral(time.Now()), id))
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		if updatedCount(reply) != 1 {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
		respondClaimed(w, db, id)
	}
}

// handleClaimTask takes an exclusive lease on a task so two concurrent
// sessions can't work the same one. The whole mechanism is a
// compare-and-swap on `claimed_by`, because KDS gives no other atomicity
// primitive here: `UPDATE ... WHERE <precondition>` reports how many rows
// it changed, so a CAS that loses reports `UPDATED 0`.
//
// It runs in two phases rather than one because KDS's WHERE has no `OR`
// (manual/sql/sql.md: AND-combined conjuncts only), so "claim it if free
// OR if the lease expired" cannot be a single statement:
//
//  1. Claim if free: `WHERE id = N AND claimed_by IS NULL`.
//  2. If that changed nothing, read the row and decide. A claim held by
//     this same agent is refreshed (that is the heartbeat), and one whose
//     lease has expired is stolen — both via a CAS against the *exact*
//     owner and timestamp just read, so a third agent racing the same
//     steal loses its own CAS instead of double-claiming.
func handleClaimTask(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}

		var req claimRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !identRe.MatchString(req.Agent) {
			http.Error(w, fmt.Sprintf("invalid agent %q: must match %s", req.Agent, identRe.String()), http.StatusBadRequest)
			return
		}

		now := kdsTimestampLiteral(time.Now())

		// Phase 1 — the task is unclaimed.
		reply, err := db.Exec(fmt.Sprintf(
			"UPDATE task SET claimed_by = '%s', claimed_at = '%s', last_updated_at = '%s' WHERE id = %s AND claimed_by IS NULL",
			req.Agent, now, now, id))
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		if updatedCount(reply) == 1 {
			respondClaimed(w, db, id)
			return
		}

		// Phase 2 — something holds it, or there is no such task.
		t, err := fetchTask(db, id)
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		if t == nil {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
		if t.ClaimedBy == nil {
			// Released between phase 1 and here. Report the conflict
			// rather than retrying — the caller can simply claim again,
			// and a retry loop here could spin against a busy queue.
			http.Error(w, "claim raced with a concurrent release; try again", http.StatusConflict)
			return
		}

		mine := *t.ClaimedBy == req.Agent
		if !mine && !t.ClaimExpired {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{
				"error":      "task is claimed by another agent",
				"claimed_by": *t.ClaimedBy,
				"claimed_at": t.ClaimedAt,
				"expires_in": fmt.Sprintf("%s", claimLeaseTTL),
			})
			return
		}

		// Refresh (same agent) or steal (expired), both conditional on the
		// row still holding exactly what was just read.
		reply, err = db.Exec(fmt.Sprintf(
			"UPDATE task SET claimed_by = '%s', claimed_at = '%s', last_updated_at = '%s' WHERE id = %s AND claimed_by = '%s' AND claimed_at = '%s'",
			req.Agent, now, now, id, *t.ClaimedBy, t.claimedAtRaw))
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		if updatedCount(reply) != 1 {
			http.Error(w, "claim raced with another agent; try again", http.StatusConflict)
			return
		}
		respondClaimed(w, db, id)
	}
}

// respondClaimed re-reads the task so the reply shows what was actually
// committed rather than what this process believes it wrote.
func respondClaimed(w http.ResponseWriter, db *KDSClient, id string) {
	t, err := fetchTask(db, id)
	if err != nil {
		http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
		return
	}
	if t == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(t)
}

// handleReleaseTask drops a claim. Only the holding agent may release, so
// a stale worker coming back from a pause cannot clear a lease that has
// since been stolen and is being worked by someone else.
func handleReleaseTask(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}

		var req claimRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !identRe.MatchString(req.Agent) {
			http.Error(w, fmt.Sprintf("invalid agent %q: must match %s", req.Agent, identRe.String()), http.StatusBadRequest)
			return
		}

		reply, err := db.Exec(fmt.Sprintf(
			"UPDATE task SET claimed_by = NULL, claimed_at = NULL, last_updated_at = '%s' WHERE id = %s AND claimed_by = '%s'",
			kdsTimestampLiteral(time.Now()), id, req.Agent))
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		if updatedCount(reply) != 1 {
			t, err := fetchTask(db, id)
			if err != nil {
				http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
				return
			}
			if t == nil {
				http.Error(w, "task not found", http.StatusNotFound)
				return
			}
			http.Error(w, fmt.Sprintf("task is not claimed by %q", req.Agent), http.StatusConflict)
			return
		}
		respondClaimed(w, db, id)
	}
}

// handleCreateResult is the "communicate work is done" endpoint: an agent
// that finished (or gave up on) a task reports what happened here. This
// also bumps the task's last_shipped_at, atomically with the insert — both
// statements run in one KDS transaction (KDSClient.ExecTxn) so a crash or
// error between them can't record a result against a task that still reads
// as never-shipped, or vice versa.
func handleCreateResult(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}

		var req createResultRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !identRe.MatchString(req.Status) {
			http.Error(w, fmt.Sprintf("invalid status %q: must match %s", req.Status, identRe.String()), http.StatusBadRequest)
			return
		}
		if req.Content == "" {
			http.Error(w, "content must not be empty", http.StatusBadRequest)
			return
		}
		content := base64.StdEncoding.EncodeToString([]byte(req.Content))
		if len(content) > maxTextBytes {
			http.Error(w, fmt.Sprintf("content too large: %d bytes base64-encoded, cell capacity is %d", len(content), maxTextBytes), http.StatusRequestEntityTooLarge)
			return
		}

		// Reporting a result ends the attempt, so it also drops any claim
		// — unconditionally, not CAS'd on the reporter's identity: the
		// work is done regardless of whose lease it was, and leaving a
		// stale lease behind would block the next attempt for a full TTL.
		now := kdsTimestampLiteral(time.Now())
		insertStmt := fmt.Sprintf("INSERT INTO result VALUES (%s, '%s', '%s', '%s', '%s')", id, req.Status, content, now, now)
		updateStmt := fmt.Sprintf("UPDATE task SET last_shipped_at = '%s', claimed_by = NULL, claimed_at = NULL, last_updated_at = '%s' WHERE id = %s", now, now, id)

		replies, err := db.ExecTxn([]string{insertStmt, updateStmt})
		if err != nil {
			if strings.Contains(err.Error(), "FK_VIOLATION") {
				http.Error(w, "task not found", http.StatusNotFound)
				return
			}
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		resultID, _ := parseInsertedID(replies[0])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(result{
			ID:            resultID,
			TaskID:        id,
			Status:        req.Status,
			Content:       req.Content,
			CompletedAt:   formatKST(now),
			LastUpdatedAt: formatKST(now),
		})
	}
}

func handleListResults(db *KDSClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}

		reply, err := db.Exec(fmt.Sprintf("SELECT * FROM result WHERE task_id = %s ORDER BY id", id))
		if err != nil {
			http.Error(w, "kds: "+err.Error(), http.StatusBadGateway)
			return
		}
		results, err := parseResultRows(reply)
		if err != nil {
			http.Error(w, "kds reply: "+err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	}
}

// splitSelectRows decodes a SELECT reply's shape: one header line then zero
// or more data rows, all joined by the literal two-byte sequence `\n`
// rather than a real newline (manual/client/client.md §2), each row
// comma-separated with no CSV quoting.
func splitSelectRows(reply string) [][]string {
	sections := strings.Split(reply, `\n`)
	rows := make([][]string, 0, len(sections))
	for _, row := range sections[1:] {
		if row == "" {
			continue
		}
		rows = append(rows, strings.Split(row, ","))
	}
	return rows
}

// parseIssueReportRows decodes `issues`' column order: id, project,
// version, state, category, body.
func parseIssueReportRows(reply string) ([]issueReport, error) {
	rows := splitSelectRows(reply)
	reports := make([]issueReport, 0, len(rows))
	for _, cols := range rows {
		if len(cols) != 7 {
			return nil, fmt.Errorf("unexpected row shape: %v", cols)
		}
		raw, err := base64.StdEncoding.DecodeString(cols[5])
		if err != nil {
			return nil, fmt.Errorf("decode body: %w", err)
		}
		reports = append(reports, issueReport{
			ID:            cols[0],
			Project:       cols[1],
			Version:       cols[2],
			State:         cols[3],
			Category:      cols[4],
			Text:          string(raw),
			LastUpdatedAt: formatKST(cols[6]),
		})
	}
	return reports, nil
}

// parseIssueRows decodes `issue`'s column order: id, project, alias,
// title, content.
func parseIssueRows(reply string) ([]issue, error) {
	rows := splitSelectRows(reply)
	issues := make([]issue, 0, len(rows))
	for _, cols := range rows {
		if len(cols) != 6 {
			return nil, fmt.Errorf("unexpected row shape: %v", cols)
		}
		raw, err := base64.StdEncoding.DecodeString(cols[4])
		if err != nil {
			return nil, fmt.Errorf("decode content: %w", err)
		}
		issues = append(issues, issue{
			ID:            cols[0],
			Project:       cols[1],
			Alias:         cols[2],
			Title:         cols[3],
			Content:       string(raw),
			LastUpdatedAt: formatKST(cols[5]),
		})
	}
	return issues, nil
}

// parseMilestoneRows decodes `milestone`'s column order: id, title,
// directory, state, version. No column here is base64-encoded — all four
// are already restricted to freeTextRe/identRe-safe charsets.
func parseMilestoneRows(reply string) ([]milestone, error) {
	rows := splitSelectRows(reply)
	milestones := make([]milestone, 0, len(rows))
	for _, cols := range rows {
		if len(cols) != 6 {
			return nil, fmt.Errorf("unexpected row shape: %v", cols)
		}
		milestones = append(milestones, milestone{
			ID:            cols[0],
			Title:         cols[1],
			Directory:     cols[2],
			State:         cols[3],
			Version:       cols[4],
			LastUpdatedAt: formatKST(cols[5]),
		})
	}
	return milestones, nil
}

// parseTaskRows decodes `task`'s column order: id, version, title, content,
// type, raised_at, last_shipped_at, derived_from, milestone_id, priority,
// claimed_by, claimed_at, state, last_updated_at.
func parseTaskRows(reply string) ([]task, error) {
	rows := splitSelectRows(reply)
	tasks := make([]task, 0, len(rows))
	for _, cols := range rows {
		if len(cols) != 14 {
			return nil, fmt.Errorf("unexpected row shape: %v", cols)
		}
		raw, err := base64.StdEncoding.DecodeString(cols[3])
		if err != nil {
			return nil, fmt.Errorf("decode content: %w", err)
		}
		var derivedFrom, milestoneID *string
		if cols[7] != "NULL" {
			v := cols[7]
			derivedFrom = &v
		}
		if cols[8] != "NULL" {
			v := cols[8]
			milestoneID = &v
		}
		var priority *int
		if cols[9] != "NULL" {
			p, err := strconv.Atoi(cols[9])
			if err != nil {
				return nil, fmt.Errorf("decode priority: %w", err)
			}
			priority = &p
		}
		var claimedBy, claimedAt *string
		var claimedAtRaw string
		claimExpired := false
		if cols[10] != "NULL" {
			v := cols[10]
			claimedBy = &v
		}
		if cols[11] != "NULL" {
			claimedAtRaw = cols[11]
			v := formatKST(claimedAtRaw)
			claimedAt = &v
		}
		if claimedBy != nil && claimedAtRaw != "" {
			if t, err := parseKDSTimestamp(claimedAtRaw); err == nil {
				claimExpired = time.Since(t) > claimLeaseTTL
			}
		}
		stateCode, err := strconv.Atoi(cols[12])
		if err != nil {
			return nil, fmt.Errorf("decode state: %w", err)
		}
		tasks = append(tasks, task{
			ID:            cols[0],
			Version:       cols[1],
			Title:         cols[2],
			Content:       string(raw),
			Type:          cols[4],
			RaisedAt:      formatKST(cols[5]),
			LastShippedAt: formatKST(cols[6]),
			Pending:       cols[5] == cols[6],
			DerivedFrom:   derivedFrom,
			MilestoneID:   milestoneID,
			Priority:      priority,
			ClaimedBy:     claimedBy,
			ClaimedAt:     claimedAt,
			ClaimExpired:  claimExpired,
			State:         taskStateName(stateCode),
			LastUpdatedAt: formatKST(cols[13]),
			claimedAtRaw:  claimedAtRaw,
		})
	}
	return tasks, nil
}

// updatedCount reads the row count out of an "UPDATED <n>" reply. It is
// how every compare-and-swap here learns whether it won: KDS has no
// RETURNING clause, so the count is the entire signal.
func updatedCount(reply string) int {
	m := updatedCountRe.FindStringSubmatch(strings.TrimSpace(reply))
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// fetchTask reads one task by id, or (nil, nil) when there is no such row.
func fetchTask(db *KDSClient, id string) (*task, error) {
	reply, err := db.Exec(fmt.Sprintf("SELECT * FROM task WHERE id = %s", id))
	if err != nil {
		return nil, err
	}
	tasks, err := parseTaskRows(reply)
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	return &tasks[0], nil
}

// parseResultRows decodes `result`'s column order: id, task_id, status,
// content, completed_at.
func parseResultRows(reply string) ([]result, error) {
	rows := splitSelectRows(reply)
	results := make([]result, 0, len(rows))
	for _, cols := range rows {
		if len(cols) != 6 {
			return nil, fmt.Errorf("unexpected row shape: %v", cols)
		}
		raw, err := base64.StdEncoding.DecodeString(cols[3])
		if err != nil {
			return nil, fmt.Errorf("decode content: %w", err)
		}
		results = append(results, result{
			ID:            cols[0],
			TaskID:        cols[1],
			Status:        cols[2],
			Content:       string(raw),
			CompletedAt:   formatKST(cols[4]),
			LastUpdatedAt: formatKST(cols[5]),
		})
	}
	return results, nil
}

// parseInsertedID pulls the assigned Keystone id out of an
// "INSERTED oid=<o> id=<n> ..." reply.
func parseInsertedID(reply string) (string, bool) {
	m := insertedIDRe.FindStringSubmatch(reply)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func kdsTimestampLiteral(t time.Time) string {
	return t.UTC().Format(kdsTimestampLayout)
}

func parseKDSTimestamp(s string) (time.Time, error) {
	return time.ParseInLocation(kdsTimestampLayout, s, time.UTC)
}

// formatKST renders a stored (UTC) KDS timestamp string in KST as
// RFC3339. On a parse failure it returns the raw value rather than
// masking the failure — a caller that only ever sees values this server
// itself wrote should never hit that path.
func formatKST(s string) string {
	t, err := parseKDSTimestamp(s)
	if err != nil {
		return s
	}
	return t.In(kst).Format(time.RFC3339)
}
