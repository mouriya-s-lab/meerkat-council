# pm-council

Pepgent PM council: a Go program that keeps the agenda queue of the PM repository and convenes headless omp panels per agenda issue, plus its TypeScript omp plugin. One binary, two subcommands:

- `pm-council app` is the council GitHub App endpoint. It verifies each webhook delivery (`X-Hub-Signature-256`) and, for `issues`, `issue_comment`, `sub_issues` and `issue_dependencies` events of the PM repository, wakes the daemon with `POST /v1/events {"issue":"owner/repo#n","event","action"}`. It keeps no state and needs no `GH_TOKEN`.
- `pm-council daemon` rebuilds the queue from GitHub: at startup and every `daemon.rebuildIntervalSeconds` from every PM-repository issue in the Project, and on each event from that one issue. Issue fields come from the Project, prerequisites from the issue's "Blocked by" relations, panel progress from the `pm:` markers in its comments. It admits each human or agenda issue without an `Agenda` number (next `<prefix><n>`, `Status=Todo`) and serves the queue. State lives only on GitHub; the daemon's copy is replaced by every rebuild.

## Run locally

Requires Go and `gh` logged in (the daemon reads `GH_TOKEN`; locally its value is the output of `gh auth token`).

```sh
mkdir -p /Users/mouriya/Ext/work/pm-council-local
cp -R config.example /Users/mouriya/Ext/work/pm-council-local/config   # then edit paths if needed
openssl rand -hex 32 > /Users/mouriya/Ext/work/pm-council-local/webhook-secret
chmod 600 /Users/mouriya/Ext/work/pm-council-local/webhook-secret
export GH_TOKEN=<output of gh auth token>
go run ./cmd/pm-council daemon --config-root /Users/mouriya/Ext/work/pm-council-local/config
go run ./cmd/pm-council app --config-root /Users/mouriya/Ext/work/pm-council-local/config     # second terminal
```

```sh
curl -s localhost:7421/v1/health
curl -s localhost:7421/v1/queue
curl -s 'localhost:7421/v1/sessions?limit=20'
curl -s localhost:7421/v1/sessions/<session id>/log
gh webhook forward --repo <pm repo> --events issues,issue_comment,sub_issues,issue_dependencies \
  --url http://localhost:7420/webhook --secret <contents of app.webhookSecretFile>
```

Tests: `go vet ./... && go test ./...`.

## HTTP API

The daemon serves these; every response carries `Access-Control-Allow-Origin: *`; `OPTIONS` on any path returns `204` with `Access-Control-Allow-Methods: GET, POST, OPTIONS` and `Access-Control-Allow-Headers: Content-Type`. Errors are `{"error":"<code>","message":"…"}`.

| Endpoint | Success | Errors |
|---|---|---|
| `GET /v1/health` | `{"component":"council","version","startedAt"}` | — |
| `GET /v1/queue` | `[{"ref","agenda","kind":"Human\|Agenda","status","blockedBy":[ref],"state","notes":[string],"panel":{"round","version","votes":[seat],"pending":[seat]} or null}]`, human issues first, then by agenda number | — |
| `POST /v1/events` `{issue,event,action}` | `202`; the issue is re-read and the queue recomputed | `400 invalid_request` (issue not `<pm repo>#n`) |
| `GET /v1/sessions?limit=n` (default 100) | `[{"id","kind","label","model","startedAt","endedAt","state":"running\|done\|failed","error","ref"}]`, newest first | `400 invalid_request` |
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

Sessions: rows come from the omp session files under `sessionsDir` (header `session` line for id and start, latest `pm-meta` custom entry for kind, seat, model and ref, the last failed assistant message for `error`). A session is `running` until its `session_exit` entry, then `failed` if its last assistant message failed and `done` otherwise.

## Configuration reference

`--config-root` points at a directory with `council/` and `shared/`. Relative paths in council YAML resolve against `<config-root>/council/`. Every key is required; unknown keys are rejected; errors name the file and key path, e.g. `council/council.yaml: agenda.prefix: missing`. The daemon also refuses to start with an empty `GH_TOKEN`, or when a field or option named in `shared/project.yaml` is missing on the Project.

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

`checkoutDir`, `guardiansURL`, `omp.*` and `maxContinuePrompts` are validated now and used once panels are convened.

`council/council.yaml`: `agenda.prefix` (letters) and `agenda.first` (positive): agenda numbers are `<prefix><n>`, `n` one above the highest number of that prefix in the Project and at least `first`; `maxConcurrentPanels`: panels at once.

`council/seats.yaml`: `seats[]` with `id` (`[A-Za-z0-9_-]+`, unique), `prompt` (system prompt file, relative), `tools` (built-in tools kept), `model`, `rights` (subset of `draft`, `vote`, `veto`, `arbitrate`).

`shared/repos.yaml`: `pm.repo`, `pm.project.owner`, `pm.project.number`, `code[]` (`repo`, `cwd`), `design[]`, `omp[]`.

`shared/project.yaml`: `fields.status`, `fields.kind`, `fields.proposer` each with `name` (the Project field) and `options` (semantic key → Project option name: status `todo`, `inProgress`, `done`, `superseded`; kind `human`, `agenda`, `implementation`; proposer `human`, `guardians`, `coordination`); `fields.agenda.name` (a text field). Node ids are resolved by name at startup.
