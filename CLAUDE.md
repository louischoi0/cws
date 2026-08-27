# cws

Tiny Go HTTP server (`server/`) backed by its own vendored KDS/ckdbs
instance (`ckdbs/`, config `kds.conf`, runtime data `kds-data/`). See
`server/main.go` for the full API this repo owns and manages directly —
`issues`, `issue`, `milestone`, `task`, `result` all live in this repo's
own KDS instance; there is no external database or server involved.

## Data structures

These drive the Claude-session-loop workflow. See `README.md`'s "The
task/result loop" section for the operational picture,
`.claude/agents/intermediary-agent.md` for the worker that does the
actual development work, and `.claude/agents/reporter-agent.md` for its
per-iteration callback. `templates/agent-report-protocol.md` (rendered
per-project by `POST /init/{project}/`) is the copy-paste doc for wiring
a new project into this loop.

### `issues` (plural) — markdown status reports

The original table, keyed by `project`/`version`/`state`/`category` path
segments plus a `body` (markdown). Unrelated to `issue` below despite the
name; see README.md's API section for why they ended up this close.

### `issue` (singular) — per-project tickets

| Field | Type | Notes |
|---|---|---|
| `id` | integer, auto-increment | primary key |
| `project` | string | |
| `alias` | string | a short, caller-chosen, per-project lookup key — no uniqueness enforced by KDS, callers are expected to check before creating |
| `title` | string | free text (see `main.go`'s `freeTextRe`) |
| `content` | string (markdown) | base64-encoded on disk, ~1.2 KB raw text cap |

### `milestone` — the superset over one project's issue/task/result

| Field | Type | Notes |
|---|---|---|
| `id` | integer, auto-increment | primary key |
| `title` | string | free text |
| `directory` | string | the project's filesystem path — free text (paths can contain most punctuation), not validated as an actual path |
| `state` | string, open-ended | no fixed vocabulary, no "achieved" constant — a caller decides what counts |
| `version` | string | e.g. `"v2.5.0"` |

The loop (`intermediary-agent` + `reporter-agent`) keeps working a
project's issues/tasks until its milestone's `state` reads as achieved,
by whatever convention the caller uses — `PATCH /milestones/{id}/` is how
that transition gets recorded. `task.milestone_id` below links a task to
one; `issue` still has no such column — that half of the gap remains.

### `task`

| Field | Type | Notes |
|---|---|---|
| `id` | integer, auto-increment | primary key |
| `version` | string | e.g. `"v2.5.0"` |
| `title` | string | free text |
| `content` | string (markdown) | base64-encoded on disk, ~1.2 KB raw text cap |
| `type` | string, open-ended | e.g. `implement`, `experimental`, `hotfix`, `benchmarking`, `revising` — not a fixed enum |
| `raised_at` | timestamp, KST (UTC+9) | when the task was created |
| `last_shipped_at` | timestamp, KST (UTC+9) | last time any session reported a result against this task — **defaults to `raised_at`** at creation, so `raised_at == last_shipped_at` is this schema's "never shipped" convention, exposed as `pending` in the API |
| `derived_from` | integer, nullable | another task's id, if this one is a subtask. **Not an enforced FK** — KDS refuses a column that `REFERENCES` the table it's declared on ("references unknown relation", confirmed empirically), so existence is checked app-side in `handleCreateTask` instead |
| `milestone_id` | integer, nullable, `REFERENCES milestone` (enforced FK — a different table than the one it's declared on, so unlike `derived_from` this one *is* engine-checked) | which milestone this task belongs to; `GET /tasks/?milestone_id=<id>` filters on it (combinable with `?pending=true`) |
| `priority` | integer, nullable | no fixed range or direction (not documented as "lower is more urgent" or the reverse) — a caller convention, not an engine one |
| `claimed_by` | string, nullable | which agent currently holds an exclusive lease on this task (see below); NULL when nobody does |
| `claimed_at` | timestamp, KST (UTC+9), nullable | when that lease was taken or last refreshed |

**Claims are leases, and the atomicity is a compare-and-swap.** KDS
offers no `SELECT ... FOR UPDATE` and no `RETURNING`, so
`POST /tasks/{id}/claim/` relies on `UPDATE ... WHERE <precondition>`
reporting how many rows it changed: a CAS that loses gets `UPDATED 0`.
Because KDS's `WHERE` has no `OR` (AND-conjuncts only), "claim if free or
if expired" cannot be one statement, so `handleClaimTask` runs two
phases — claim-if-`IS NULL`, then, failing that, re-read and CAS against
the *exact* owner and timestamp just read. Verified under concurrency:
8 simultaneous claims on one task yield exactly one winner, and so do 8
simultaneous steals of one expired lease.

A lease older than `claimLeaseTTL` (30 min, a single server-wide
constant — a per-claim TTL would need another column and could not be
evaluated by anyone but the claimer) may be stolen. A live agent
refreshes by claiming again; that is the only heartbeat. Reporting a
result releases the claim unconditionally, in the same transaction as
the insert.

KDS *does* support real `NULL` storage (confirmed via `derived_from`
above) — `last_shipped_at`'s sentinel-equality convention predates that
being rechecked in this session and wasn't revisited, to avoid a second
migration. A future schema pass could make `last_shipped_at` genuinely
nullable instead; nothing today prevents it.

### `result`

| Field | Type | Notes |
|---|---|---|
| `id` | integer, auto-increment | primary key |
| `task_id` | integer, `REFERENCES task` (enforced FK — this one isn't self-referential, so the restriction above doesn't apply) | **not 1:1** — one task can have many results |
| `status` | string, open-ended | outcome of this attempt; exact vocabulary not fixed |
| `content` | string (markdown) | base64-encoded on disk, same ~1.2 KB cap |
| `completed_at` | timestamp, KST (UTC+9) | when this result was produced |

No `project` column on `task`/`result` today — if more than one project
ever shares this instance, that's an open gap (see
`templates/agent-report-protocol.md`'s closing note), not something
silently handled. Every schema here is a judgment call, expected to
change: extend it rather than working around its absence, and re-bisect
`inline_cell_width` (see `kds.conf`'s comment) empirically if a table
grows another `varchar` column — KDS has no `ALTER TABLE ADD COLUMN`
(confirmed empirically: `Unsupported`), so widening any table's schema
means `DROP TABLE` + `CREATE TABLE`, in FK-dependency order, losing
existing rows unless they're migrated by hand first.
