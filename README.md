# pm-council

Pepgent PM council: a Go program that keeps the agenda queue of the PM repository and convenes a fresh headless omp panel per agenda issue, plus its TypeScript omp plugin (`plugin/`). One binary, two subcommands:

- `pm-council app` is the council GitHub App endpoint. It verifies each webhook delivery (`X-Hub-Signature-256`) and, for `issues`, `issue_comment`, `sub_issues` and `issue_dependencies` events of the PM repository, wakes the daemon with `POST /v1/events {"issue":"owner/repo#n","event","action"}`. It keeps no state and needs no `GH_TOKEN`.
- `pm-council daemon` rebuilds the queue from GitHub: at startup and every `daemon.rebuildIntervalSeconds` from every PM-repository issue in the Project, and on each event from that one issue. Issue fields come from the Project, prerequisites from the issue's "Blocked by" relations, panel progress from the `pm:` markers in its comments. It admits each human or agenda issue without an `Agenda` number (next `<prefix><n>`, `Status=Todo`), serves the queue, and starts, interrupts and resumes panels (below). Issue state lives only on GitHub; the daemon's copy is replaced by every rebuild. The live panels are the daemon's own processes.

## Run locally

Requires Go, `yq` (mikefarah v4), omp, `gh` logged in (the daemon reads `GH_TOKEN`; locally its value is the output of `gh auth token`) and a running pm-guardians (`guardiansURL`) whose `bases.yaml` has a base for every model named in `council/proxy-readers.yaml`; that is a deployment check, the council does not verify it.

```sh
mkdir -p /Users/mouriya/Ext/work/pm-council-local
cp -R config.example /Users/mouriya/Ext/work/pm-council-local/config   # then edit paths if needed
scripts/prepare-omp-dir.sh /Users/mouriya/Ext/work/pm-council-local/config
openssl rand -hex 32 > /Users/mouriya/Ext/work/pm-council-local/webhook-secret
chmod 600 /Users/mouriya/Ext/work/pm-council-local/webhook-secret
export GH_TOKEN=<output of gh auth token>
go run ./cmd/pm-council daemon --config-root /Users/mouriya/Ext/work/pm-council-local/config
go run ./cmd/pm-council app --config-root /Users/mouriya/Ext/work/pm-council-local/config     # second terminal
```

`scripts/prepare-omp-dir.sh <config-root>` builds the omp agent dir `omp.agentDir`: it copies `council/omp/config.yml` to `<agentDir>/config.yml` and links the credential store (`agent.db*`) and `models.yml` of `~/.omp/agent/`.

```sh
curl -s localhost:7421/v1/health
curl -s localhost:7421/v1/queue
curl -s 'localhost:7421/v1/sessions?limit=20'
curl -s localhost:7421/v1/sessions/<session id>/log
gh webhook forward --repo <pm repo> --events issues,issue_comment,sub_issues,issue_dependencies \
  --url http://localhost:7420/webhook --secret <contents of app.webhookSecretFile>
```

Tests: `go vet ./... && go test ./...`.

## Panels

After every queue publication the daemon applies, in order:

1. While a human issue is not decided, every live panel of a non-human issue is interrupted: its omp sessions are killed and it posts `pm:panel interrupted` listing them (`sessions[]` with seat, kind, session id and file); Project fields stay unchanged. The daemon waits for those posts.
2. Candidates, in queue order and never an issue with a live panel: `ready` issues are convened; `interrupted` ones are resumed; `failed` ones and `deliberating` ones without a live panel (left by an earlier daemon process) are resumed only on a full rebuild (startup or the rebuild interval).
3. Up to `maxConcurrentPanels` minus the live panels are started.

A panel keeps no progress of its own. Before every step it re-reads the issue and derives the step from its markers: the round is the latest `pm:panel` round; without a `pm:draft` of that round it runs the draft session of the seat with the `draft` right; with draft version `v` it runs, concurrently, a vote session for every seat with the `vote` right and no `pm:vote` on `v`; when every voting seat has voted it ends and the issue waits for the tally.

- **Convene:** `Status` set to the in-progress option, then `pm:panel started` round 1.
- **Draft session:** ends with `pm_draft`, posted as `pm:draft` with the next version, or `pm_block` (agenda issues only): the issue gets "Blocked by" the undecided human issue, `Status` the todo option, then `pm:panel blocked` with `prereq`, and the panel ends.
- **Vote session:** each `pm_vote` goes to the tally component, which posts `pm:vote` for the seat and version.
- **Failure:** a session that ends without a goal tool kills the issue's other sessions, and the panel posts `pm:panel failed` with omp's error text and `sessions: []` (only `interrupted` lists sessions); the daemon log names the sessions that were running.
- **Resume:** `pm:panel resumed` (same round), posted only when a draft or vote step is open. With `resume: fork`, each session listed in the latest `pm:panel interrupted` whose step is still open restarts as `omp --fork <its file>` with the kind's `resume` prompt; other steps, and `resume: fresh`, start new sessions. Right before posting, the issue is re-read: a draft for a round that already has one, or a vote of a seat that already voted on that version, is dropped.

Seat sessions: `omp --mode rpc` with the seat's `model`, `prompt` file as system prompt and `tools` plus the kind's plugin tools; session files under `sessionsDir/<issue number>/`; working directory `checkoutDir/<pm owner>/<pm name>`, cloned on first use and reset to the remote default branch before each panel; `PI_CODING_AGENT_DIR` is `omp.agentDir`. The plugin appends a `pm-meta` entry `{kind, seat, model, ref, round, version}` and keeps a session going (`session_stop` continuation with the kind's `continue` text) until one of the kind's goal tools succeeds; the program sends the same text after each turn, at most `maxContinuePrompts` times, then fails the session. Model fallback on hard errors is omp's `retry.fallbackChains` in `council/omp/config.yml`; the program does not retry.

## Plugin

`plugin/index.ts`, loaded with `--trusted-extension`. The program sets `PM_KIND`, `PM_GOAL_TOOLS`, `PM_CONTINUE`, `PM_META`, `PM_GUARDIANS_URL` and `PM_CONTEXT`, a JSON file written before each session under `sessionsDir/<n>/context/` with the issue ref, the seat, its rights, whether the issue is human, the PM repository, the code/design/omp repositories of `shared/repos.yaml`, the seat's `proxy-readers.yaml` entry, every human issue of the queue with whether it is decided, the draft (vote sessions) and the issue body's `pm:escalation` marker. `GH_TOKEN` is inherited. The plugin only reads; the program takes goal tool arguments from the RPC event stream and writes GitHub. An invalid call throws: the tool result is an error, the goal does not count and the session continues.

| Tool | Kinds | Checks |
|---|---|---|
| `pm_read_issue {ref}` | all | `ref` is an issue of the PM repository; returns title, body and comments |
| `pm_proxy_read {repo, question, model?}` | all | `model` (default the seat's `default`) is the seat's `default` or in `others`; `repo` belongs to one of the seat's `content` categories. Then `POST {guardiansURL}/v1/proxy-reads {seat, model, repo, question, agenda}`; the facts are the result; a non-200 answer is the error, verbatim |
| `pm_draft {rows[], supersedes[], settle, withdrawn}` | draft | row ids non-empty and unique; rows empty only with `withdrawn`; `settle` required when the issue body has an escalation marker |
| `pm_block {prereq, reason}` | draft | the issue is not human; `prereq` is an undecided human issue |
| `pm_vote {rows[]}` | vote | exactly the draft's row ids, each once; `oppose` needs a `counterexample`; `veto` needs the `veto` right and a `citation` that is a human issue |

## HTTP API

The daemon serves these; every response carries `Access-Control-Allow-Origin: *`; `OPTIONS` on any path returns `204` with `Access-Control-Allow-Methods: GET, POST, OPTIONS` and `Access-Control-Allow-Headers: Content-Type`. Errors are `{"error":"<code>","message":"…"}`.

| Endpoint | Success | Errors |
|---|---|---|
| `GET /v1/health` | `{"component":"council","version","startedAt"}` | — |
| `GET /v1/queue` | `[{"ref","agenda","kind":"Human\|Agenda","status","blockedBy":[ref],"state","notes":[string],"panel":{"round","version","votes":[seat],"pending":[seat]} or null}]`, human issues first, then by agenda number | — |
| `POST /v1/events` `{issue,event,action}` | `202`; the issue is re-read and the queue recomputed | `400 invalid_request` (issue not `<pm repo>#n`) |
| `GET /v1/sessions?limit=n` (default 100) | `[{"id","kind","label","model","startedAt","endedAt","state":"running\|done\|failed","error","ref","parent"}]`, newest first | `400 invalid_request` |
| `GET /v1/sessions/{id}/log` | the session file, `application/x-ndjson` | `404 unknown_session` |

The app serves only `POST /webhook`: `401` for a missing or wrong signature, `200` for `ping`, `202` otherwise (forwarded or not), `502` when the daemon does not accept the forward, `413` above 25 MiB.

Queue fields: `agenda` and `status` are the Project field values (`null` when unset); `blockedBy` lists the human prerequisites ("Blocked by" pointing at an issue in the Project with the human Kind); any other "Blocked by" is ignored and named in `notes`. `state` is the first that holds:

| `state` | holds when |
|---|---|
| `decided` | `Status` is the done option, or a `pm:decision` comment exists |
| `superseded` | `Status` is the superseded option, or a `pm:superseded` comment exists |
| `blocked` | a human prerequisite is not decided |
| `held` | the issue is not human and some human issue is not decided |
| `deliberating` | the latest `pm:panel` event is `started` or `resumed` |
| `interrupted` | the latest `pm:panel` event is `interrupted` |
| `failed` | the latest `pm:panel` event is `failed` |
| `ready` | otherwise; the issue can be convened |

`panel` is `null` without a `pm:panel` comment; otherwise `round` is the latest panel's round, `version` the highest `pm:draft` version (`null` without a draft), `votes` the seats with a `pm:vote` on that version, `pending` the seats with the `vote` right that have none. Markers are the last line of a comment, `<!-- pm:<kind> <single-line JSON with "v":1> -->`; a marker missing a field, with a wrong type or unknown enum value is ignored and logged.

Sessions: rows come from the omp session files under `sessionsDir` (header `session` line for id, start and `parentSession`, latest `pm-meta` custom entry for kind, seat, model and ref). A session is `running` while this daemon process holds it, `done` when one of its kind's goal tools has a successful result, otherwise `failed` with `error` the reason this process recorded, else the last failed assistant message, else `ended without a goal tool`. `parent` is the session a `resume: fork` session was forked from, `null` otherwise. Entries a fork copied from its parent (timestamped before its header) count only for `pm-meta`.

## Configuration reference

`--config-root` points at a directory with `council/` and `shared/`. Relative paths in council YAML resolve against `<config-root>/council/`. Every key is required; unknown keys are rejected; errors name the file and key path, e.g. `council/council.yaml: agenda.prefix: missing`. The daemon also refuses to start with an empty `GH_TOKEN`, a missing seat prompt file, or when a field or option named in `shared/project.yaml` is missing on the Project.

`council/runtime.yaml`

| Key | Meaning |
|---|---|
| `app.listen` | app HTTP listen address |
| `app.webhookSecretFile` | file holding the webhook secret (surrounding whitespace ignored) |
| `app.daemonURL` | daemon base URL the app forwards events to |
| `daemon.listen` | daemon HTTP listen address |
| `daemon.rebuildIntervalSeconds` | seconds between full rebuilds |
| `sessionsDir` | root of the council's omp session files |
| `checkoutDir` | PM repository checkouts for panels |
| `guardiansURL` | pm-guardians base URL |
| `omp.executable` | omp executable |
| `omp.agentDir` | `PI_CODING_AGENT_DIR` of every omp child |
| `omp.extension` | the plugin entry, `plugin/index.ts` |
| `omp.readyTimeoutSeconds` | seconds an omp child may take to emit its ready frame |
| `omp.exitGraceSeconds` | seconds an omp child may take to exit after its stdin closes |
| `maxContinuePrompts` | program-side continue prompts per session before it fails |

`council/council.yaml`: `agenda.prefix` (letters) and `agenda.first` (positive): agenda numbers are `<prefix><n>`, `n` one above the highest number of that prefix in the Project and at least `first`; `maxConcurrentPanels`: panels at once; `resume`: `fork` or `fresh`, how interrupted sessions continue.

`council/seats.yaml`: `seats[]` with `id` (`[A-Za-z0-9_-]+`, unique), `prompt` (system prompt file relative to `council/`, must exist; e.g. `omp/prompts/<id>.md`), `tools` (built-in tools kept), `model`, `rights` (subset of `draft`, `vote`, `veto`, `arbitrate`). Exactly one seat has `draft`; at least one has `vote`.

`council/goals.yaml`: `draft` and `vote`, each with `goalTools` (draft: a subset of `pm_draft`, `pm_block`; vote: `pm_vote`), `opening` and `resume` (Go `text/template`s with `.Ref .Title .Body .Agenda .Round .Version .Draft` — the draft marker JSON for vote sessions, empty for draft — and `.Escalation`, nil or `{Issue, Ask, AskText}` where `AskText` is the ask comment's text), and `continue` (plain text). Templates are executed once with and once without an escalation at load, so an unknown field is a load error.

`council/proxy-readers.yaml`: `seats[]` with `seat` (a `seats.yaml` id), `default` (model), `others` (models, may be `[]`), `content` (non-empty subset of `code`, `design`, `omp`), `note`. Every seat with `draft` or `vote` has an entry. Every model named here must have a base in pm-guardians `bases.yaml`.

`council/omp/config.yml`: the omp settings of every seat session (copied by `scripts/prepare-omp-dir.sh`).

`shared/repos.yaml`: `pm.repo`, `pm.project.owner`, `pm.project.number`, `code[]` (`repo`, `cwd`), `design[]`, `omp[]`.

`shared/project.yaml`: `fields.status`, `fields.kind`, `fields.proposer` each with `name` (the Project field) and `options` (semantic key → Project option name: status `todo`, `inProgress`, `done`, `superseded`; kind `human`, `agenda`, `implementation`; proposer `human`, `guardians`, `coordination`); `fields.agenda.name` (a text field). Node ids are resolved by name at startup.
