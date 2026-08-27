# cws

A tiny Go HTTP server backed by its own vendored KDS (ckdbs) database
instance, built for a Claude-session-loop development workflow.

## Layout

- `ckdbs/` — vendored source of [ckdbs](https://github.com/louischoi0/ckdbs),
  built as `ckdbs/build-release/kds_server`.
- `kds.conf` — this project's KDS instance config; data lives under
  `kds-data/` (gitignored).
- `server/` — the API server (`go.mod` module `cws-issues-server`).
- `web/` — the read-only dashboard (`go.mod` module `cws-web`), a
  **separate service**: it holds no database handle and knows no SQL, it
  reads the API's `GET /overview` and renders one page. That is what
  makes read-only a structural property rather than a promise.
- `templates/` — copy-paste templates (symlinked from `server/templates/`,
  which is what's actually `go:embed`ded into the binary).
- `.claude/agents/intermediary-agent.md` — the subagent that drives the
  task loop described below; `.claude/agents/reporter-agent.md` — its
  per-iteration callback, syncing project state back to `issue`/`milestone`.
- `run.sh` — starts all three: `kds_server`, the API, the dashboard. It
  aborts if `kds_server` dies during startup rather than leaving the
  other two running against whatever else holds its port.

Run it: `./run.sh` from the repo root. That starts all three processes —
KDS on `127.0.0.1:19432`, the API on `0.0.0.0:8080`, and the dashboard on
`0.0.0.0:8081`. Open <http://localhost:8081/> to see everything at once.

The API reads (`server/`):

| Variable | Default | What it sets |
|---|---|---|
| `HTTP_ADDR` | `0.0.0.0:8080` | where the API listens |
| `KDS_ADDR` | `127.0.0.1:19432` | where it reaches `kds_server` — a project-specific port, deliberately not KDS's default 15432, which ckdbs' own tools and benchmarks use |
| `PUBLIC_BASE_URL` | the request's `Host` header | the base URL written into `POST /init/{project}/` output — set it when the server is reached through a proxy or a different hostname than it binds |

The dashboard reads (`web/`):

| Variable | Default | What it sets |
|---|---|---|
| `HTTP_ADDR` | `0.0.0.0:8081` | where the dashboard listens |
| `CWS_API` | `http://127.0.0.1:8080` | the API it reads `/overview` from |

## API

`GET /help` is the live reference — every route, its parameters, and what
it returns, as structured JSON. The list below is a summary; `/help` is
the source of truth and won't drift from the actual routes the way this
file could.

- `GET /ping` — round-trips KDS's own `PING`, confirming the whole stack
  is up.
- `GET /overview` — every row of every table in one read-only snapshot,
  which is what the dashboard consumes. It exists because the scoped
  endpoints below cannot answer "show me everything": `GET /tasks/`
  requires a `milestone_id`, `GET /issue/` needs a project name with no
  way to enumerate projects, and `GET /issues/` needs all four path
  segments. Unpaginated by design — this is a small operational store,
  and a partial snapshot presented as a whole one is the failure worth
  avoiding.
- `POST /issues/{project}/{version}/{state}/{category}/` and the matching
  `GET` — markdown status reports keyed by those four segments, stored in
  the `issues` table (plural — the original one).
- `POST /issue/{project}/`, `GET /issue/{project}/` (list),
  `GET /issue/{project}/{alias}/` — lightweight per-project tickets in
  the `issue` table (singular): `{alias, title, content}`. Named
  confusingly close to `issues` above only because of a naming collision
  discovered after the fact — they are unrelated tables.
- `POST /milestones/`, `GET /milestones/`, `GET /milestones/{id}/`,
  `PATCH /milestones/{id}/` — a `milestone` (`title`, `directory`,
  `state`, `version`) is the superset over one project's
  issues/tasks/results: the thing the loop is working toward. The PATCH
  is all-optional-fields, and is how the loop records that a milestone
  was reached.
- `POST /tasks/`, `GET /tasks/` (**`?milestone_id=<id>` is required** —
  the queue is always read in the context of one milestone; add
  `?pending=true` and/or `?claimable=true`), `GET /tasks/{id}/`,
  `PATCH /tasks/{id}/` (only the keys you send are written; an explicit
  `null` clears a nullable column, and `claimed_by`/`claimed_at`/
  `raised_at`/`last_shipped_at` are refused by name — leases move only
  through claim/release) — the task queue (see below). A task may
  carry `derived_from` (another task's id, nullable) when it's a subtask —
  **not an enforced foreign key**: KDS refuses a column referencing the
  table it's declared on, so this is validated app-side instead.
  `milestone_id` (nullable) *is* an enforced FK, since it references a
  different table. `priority` (nullable integer) has no fixed range or
  direction — a caller convention, not an engine one.
- `POST /tasks/{id}/claim/`, `POST /tasks/{id}/release/` — an exclusive
  lease on a task, so two concurrent sessions never work the same one.
  Claiming is atomic under concurrency (a compare-and-swap; see
  `CLAUDE.md`), a lease expires after 30 minutes and may then be stolen,
  and re-claiming your own task refreshes it.
  `GET /tasks/?milestone_id=<id>&claimable=true` lists what nothing
  currently holds.
- `POST /tasks/{id}/results/`, `GET /tasks/{id}/results/` — reporting and
  reading results against a task (one task can have many results).
  Reporting also releases the claim.
- `POST /init/{project}/` — renders `templates/agent-report-protocol.md`
  for one project name and returns it as plain markdown text, meant to be
  copy-pasted straight into that project's own `CLAUDE.md`.

All markdown bodies (`issues.body`, `issue.content`, `task.content`,
`result.content`) are stored base64-encoded and capped around 1.2 KB of
raw text — KDS has no var-heap yet, so every `varchar` column is a
fixed-width cell (`inline_cell_width` in `kds.conf`), and a value that
doesn't fit is refused rather than truncated. Short fields (`title`,
`alias`, `directory`, `state`, `version`, ...) are stored as-is, not
base64 — they're restricted to a charset that's already safe as a raw
KDS string literal (see `server/main.go`'s `identRe`/`freeTextRe`).

## The task/result loop

`task` and `result` are how per-project development work gets dispatched
and reported on across Claude Code sessions. A `task` describes one unit
of work — a `title` and markdown `content`, an open-ended `type`, a
`version`, optional `milestone_id`/`priority`/`derived_from`, and KST
timestamps; a `result` (`status`, markdown `content`, `completed_at`)
records what one attempt at it produced, and a task can accumulate many
results over its life rather than one. `CLAUDE.md` has every column with
its constraints.

The **intermediary agent** (`.claude/agents/intermediary-agent.md`) is the
worker on the other end: it polls
`GET /tasks/?milestone_id=<id>&pending=true&claimable=true`, **claims** one with
`POST /tasks/{id}/claim/` — a 409 there means another agent got it first,
so it moves on — and does the actual work *inside the target project*,
following *that project's own* `CLAUDE.md` and subagents rather than any
process of its own; this repo only owns the reporting contract, never how
the work itself gets done. Work outlasting the 30-minute lease is kept
alive by re-claiming. When it's done (or gives up), it reports back via
`POST /tasks/{id}/results/`, which marks the task shipped and releases the
claim in one transaction. It then
hands off to the **reporter agent** (`.claude/agents/reporter-agent.md`),
a callback run once per loop iteration: it syncs anything that iteration
surfaced — new issues, milestone progress — back into `issue`/`milestone`,
so the loop's own state changes never get pushed by the worker itself. To
wire a new project into this loop, run `POST /init/{project}/` against a
running server and paste the returned text into that project's
`CLAUDE.md`.

## The dashboard

`web/` is a second service whose only job is showing the above at a
glance: milestones with their tasks nested under them, each task's
results inline, claim holders and expired leases flagged, then issues and
status reports. Server-rendered HTML with no build step, no JavaScript
and no external assets — one binary and one embedded template.

A sticky top nav jumps to the three sections — Milestones, Tasks, Issues
— each with a live count. State is a **table column**, not a tag inside
the title: `pending`/`shipped` and the claim holder each get their own
column so a row can be scanned down rather than read across. Only
`pending`/`shipped` are colour-coded, because those are booleans the
server actually derives; a milestone's `state` is an open-ended string
nothing validates, so colouring it would assert a meaning no check
stands behind.

Markdown bodies are rendered rather than shown raw, via goldmark with
**raw HTML left disabled** — task and issue content is written by agents
and by anything that can POST to the API, so it is untrusted by the time
it reaches this page. goldmark's defaults drop raw HTML and strip
`javascript:`/`data:` URLs; `web/markdown_test.go` pins both, and fails
the moment someone adds `html.WithUnsafe()`.

It reads `GET /overview` on every request and holds nothing between
them, so reloading is refreshing and there is no cache to go stale.
Tasks with no `milestone_id` get their own group at the bottom rather
than being dropped: the API's list endpoint genuinely cannot reach them,
so a dashboard that hid them too would make them invisible everywhere.
When the API is unreachable the page renders the error in place rather
than returning a bare 502 — the operator is already looking here.
