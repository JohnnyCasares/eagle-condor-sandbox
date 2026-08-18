# pstad

An HTTP API that runs the Playwright workflows in `../automation` on behalf of
clients. Go, standard library only, no dependencies.

It owns process supervision and per-run isolation. **It does not know what Excel
is** — the contract is CSV in, artifacts out, which is what keeps all template
generation on the client side (`../python/psrunner/excel/`).

---

## Running it

```bash
export PSTAD_TOKEN=$(openssl rand -hex 32)     # required
go run ./cmd/pstad                             # or: go build -o bin/pstad ./cmd/pstad
```

Validate configuration and the manifest without starting the listener:

```bash
go run ./cmd/pstad -check
```

### Configuration

| Variable | Default | Purpose |
|---|---|---|
| `PSTAD_TOKEN` | — | **Required.** Bearer token for every endpoint except `/v1/health` |
| `PSTAD_ADDR` | `127.0.0.1:8080` | Listen address. Keep it on localhost behind a TLS terminator |
| `PSTAD_AUTOMATION_DIR` | `../automation` | Playwright project root (holds `workflows.json`, `node_modules`) |
| `PSTAD_RUNS_DIR` | `<automation>/.runs` | Parent of every per-run directory |
| `PSTAD_RECEIPTS_DIR` | `<automation>/data/receipts` | Curated attachment library; can be mounted read-only |
| `PSTAD_NODE_BIN` | `node` | Node executable |
| `PSTAD_MAX_CONCURRENT` | `2` | Runs executing at once — see *Capacity* below |
| `PSTAD_QUEUE_DEPTH` | `32` | Backlog before submissions get `429` |
| `PSTAD_MAX_UPLOAD_BYTES` | `10485760` | Per-file upload cap |

---

## Endpoints

```
GET  /v1/health                          no auth; status + pool occupancy
GET  /v1/workflows                       the manifest, served verbatim
GET  /v1/workflows/{id}
GET  /v1/receipts                        the attachment library (ETag'd)
GET  /v1/reference                       captured reference data, if any workflow writes it
POST /v1/runs                            multipart/form-data -> 202
GET  /v1/runs?state=&workflow=&limit=
GET  /v1/runs/{id}
GET  /v1/runs/{id}/log?from=<seq>&limit=
POST /v1/runs/{id}/cancel                202, or 409 if already terminal
GET  /v1/runs/{id}/results               parsed output.json + summary
GET  /v1/runs/{id}/artifacts
GET  /v1/runs/{id}/artifacts/{artifactID}
```

### Submitting a run

One `config` JSON part, plus one file part per `kind: "csv"` input named by
`inputs[].name` in the manifest.

```bash
curl -X POST http://127.0.0.1:8080/v1/runs \
  -H "Authorization: Bearer $PSTAD_TOKEN" \
  -F 'config={"workflowId":"e2e.master-flow","env":"TST","submittedBy":"jc"};type=application/json' \
  -F "ta=@ta.csv"
# -> 202 {"id":"20260811T180553Z-bd7c…","state":"queued", …}
```

Then tail it. `from` makes polling resumable, so nothing is missed between polls:

```bash
curl -H "Authorization: Bearer $PSTAD_TOKEN" \
  "http://127.0.0.1:8080/v1/runs/$ID/log?from=0"
```

Uploads are rejected at submit time — not mid-run — for a missing required input,
an unknown input name, a bad `env`, a CSV whose header row lacks the schema's
columns, or an `attName`/`attachmentFile` naming a receipt not in the library.

---

## Per-run isolation

Each run gets a private tree, and the Playwright process is pointed at it
entirely through `--output=` and four environment variables:

```
$PSTAD_RUNS_DIR/<run-id>/
  input/        uploaded CSVs (0600)      -> inputs[].env
  output/       <- playwright --output=;  testInfo.outputPath() lands here
  report/       <- PW_HTML_REPORT_DIR
  tmp/          <- PS_RUN_TMP_DIR
  report.json   <- PW_RESULT_JSON
  run.log
  meta.json     survives restart
```

`report/` is a **sibling** of `output/`. Nesting them is a hard Playwright error.

Three things in `internal/run/exec.go` are load-bearing:

1. **Not `npx`** — node is invoked on `cli.js` directly. `npx` adds a process
   layer between the server and the tree it has to kill, and can hit the network.
2. **The child environment is an allowlist, not `os.Environ()`.** A stray
   `TA_USER_ID` or `ENV` exported on the server would otherwise silently apply to
   every run, because `core/users.js` and `core/environments.js` fall back to
   whatever is set.
3. **The whole process tree is killed, not just node.** Playwright's browsers are
   children and survive otherwise. Windows uses `taskkill /F /T`; Unix puts the
   child in its own process group and signals the group.

On boot, any run still marked `queued`/`running` belonged to a previous process:
it is moved to `errored` and its recorded PID's tree is killed.

---

## Capacity

`PSTAD_MAX_CONCURRENT` is the real parallelism figure here: each workflow in
this project (`tests/eagle.spec.js`, `tests/condor.spec.js`) is a single
browser session with no internal fan-out, so one run is one Playwright
process. A workflow that spawns multiple browser contexts per run (as some
larger suites do) would make this number a floor, not the actual ceiling —
worth re-checking if a workflow like that gets added later.

---

## Checking for orphaned browsers

Playwright 1.60 runs headless via `chrome-headless-shell.exe`, **not**
`chrome.exe` — and on a workstation `chrome.exe` also counts the user's own
browser. Filter on the executable path:

```powershell
@(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -like "*ms-playwright*" }).Count
```

```bash
pgrep -af ms-playwright | wc -l
```

Should be `0` whenever no run is active.

---

## Layout

```
cmd/pstad/main.go        flags, wire-up, graceful shutdown, boot recovery
internal/config/         paths, pool size, token; validated at startup
internal/catalog/        loads + validates ../automation/workflows.json
internal/logbuf/         seq-numbered ring buffer + subscriber fan-out
internal/receipts/       the attachment library
internal/run/
  run.go                 Run, State, Artifact, Summary
  layout.go              per-run directory tree; path-escape guard
  store.go               in-memory + meta.json; restart recovery
  queue.go               bounded FIFO + worker pool
  exec.go                argv/env construction, spawn, stream, deadline
  results.go             report.json -> Summary + Artifacts
  kill_windows.go        taskkill /F /T          (build-tagged)
  kill_unix.go           setpgid + SIGTERM/SIGKILL (build-tagged)
internal/httpapi/        router, bearer auth, handlers
```

Deployment is a single static binary:

```bash
GOOS=linux GOARCH=amd64 go build -o pstad ./cmd/pstad
```

The target host needs Node and `npx playwright install --with-deps`, plus
whatever network reach the workflows themselves require.
