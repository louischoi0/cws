## Reporting to the {{.Project}} task loop

This project's work is dispatched from and reported back to a cws
task/result server at `{{.ServerURL}}`. When an intermediary agent is
working a task from that queue in this project:

1. **Claim the task, then fetch it.** `POST
   {{.ServerURL}}/tasks/{id}/claim/` with `{"agent": "<your identity>"}`
   takes an exclusive 30-minute lease. **A 409 means another agent holds
   it — work a different task, never one you did not claim.** Refresh a
   long-running lease by claiming again with the same agent id; give a
   task back early with `POST {{.ServerURL}}/tasks/{id}/release/`.

   `GET {{.ServerURL}}/tasks/{id}/` then returns
   `{id, version, title, content, type, raised_at, last_shipped_at,
   pending, derived_from, milestone_id, priority, claimed_by, claimed_at,
   claim_expired}` — `content` is the task body in markdown.
   `GET {{.ServerURL}}/tasks/?pending=true&claimable=true` lists what is
   outstanding and unheld; add `&milestone_id=<id>` to scope it.

2. **Do the actual work using this project's own rules.** Read and follow
   *this project's* `CLAUDE.md` and invoke *this project's* own
   subagents (e.g. anything under `.claude/agents/`) if it defines any.
   This template governs only how you report back — it has no opinion on
   how the work itself gets done, and never overrides this project's own
   working rules.

3. **Report a result when you finish (or stop).** `POST
   {{.ServerURL}}/tasks/{id}/results/` with JSON body:
   ```json
   {"status": "<short token>", "content": "<markdown report of what happened>"}
   ```
   - `status` is a short free-form token — `^[A-Za-z0-9._-]{1,128}$`, no
     fixed vocabulary (e.g. `done`, `failed`, `partial`, `blocked`).
   - `content` is capped at roughly **1.2 KB of raw text** today — KDS has
     no var-heap yet, and the server refuses anything over the limit with
     `413`. Keep the report to a summary; put anything longer in the
     project's own tree and reference it instead of pasting it in.
   - Reporting a result bumps `task.last_shipped_at` to now **and
     releases your claim**, both in the same transaction as the insert —
     the task stops showing as `pending`, and you should not also call
     release.

4. **Reporting is not closing.** A task can accumulate more than one
   result over its life (`task_id` is not 1:1) — this call records what
   *this* attempt produced, not a final verdict. Multiple sessions may
   pick up the same task across separate attempts.

5. **After reporting, hand off to the reporter callback.** If this
   project defines `.claude/agents/reporter-agent.md`, invoke it once per
   loop iteration — it syncs anything the iteration surfaced (new issues,
   milestone progress) back to `{{.ServerURL}}`, so the working agent
   itself never has to.

`GET {{.ServerURL}}/tasks/?pending=true` lists every task whose
`last_shipped_at` still equals its `raised_at` — i.e. never reported
against. A task may carry `derived_from` (another task's id) when it's a
subtask — not an enforced link, just informational.

**Milestones** group a project's work: `GET {{.ServerURL}}/milestones/`,
and `PATCH {{.ServerURL}}/milestones/{id}/` with `{"state": "..."}` to
record that one was reached (all fields optional). The full API reference
is `GET {{.ServerURL}}/help`.

**Issues** (separate from tasks — a lighter-weight per-project ticket,
not part of the work loop itself): `POST {{.ServerURL}}/issue/{{.Project}}/`
with `{"alias": "...", "title": "...", "content": "<markdown>"}` creates
one; `GET {{.ServerURL}}/issue/{{.Project}}/{alias}/` fetches one by its
alias, `GET {{.ServerURL}}/issue/{{.Project}}/` lists all of them. Check
an alias doesn't already exist before creating — nothing server-side
enforces uniqueness.

Note: `task`/`result` have no `project` column today (`issue` does) —
this document is generated per-project only for this text's own context
where `task`/`result` are concerned, not because the server filters
those by project. If more than one project shares one cws instance for
its task queue, that's an open gap, not a guarantee.
