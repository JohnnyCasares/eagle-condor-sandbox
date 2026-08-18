# eagle-condor-sandbox

A standalone testbed for the "endpoint + UI + multi-user concurrency" architecture,
with no dependency on any internal system. Split out of a larger project so it
can be pushed to its own GitHub repo and exercised by Actions on every commit.

Two Playwright tests against a public site (birdsoftheworld.org — search for a
species, assert the scientific name shows up) stand in for real workflows. What's
actually being proven isn't the bird site — it's that two people can submit runs
at the same time and each gets an isolated, uncorrupted result.

## Layout

```
tests/            eagle.spec.js, condor.spec.js — the two workflows
public/           index.html — a plain-JS UI: submit a run, poll for its result
server/           pstad — a small Go HTTP API that queues and runs the tests
workflows.json    the manifest pstad reads: two workflows, zero inputs
docker-compose.yml   both services, containerized
.github/workflows/ci.yml   runs the tests on every push/PR
```

## Run the tests directly

```bash
npm install
npx playwright test
```

## Run the full stack (server + UI)

```bash
cd server
PSTAD_AUTOMATION_DIR=.. PSTAD_TOKEN=sandbox-dev-token PSTAD_ADDR=127.0.0.1:8090 go run ./cmd/pstad
```

Then open `public/index.html` directly in a browser (or a second one / incognito
window to simulate a second user), fill in the server URL and token fields, and
click through both workflows. Results land under `.runs/<run-id>/`.

## Run it in Docker

```bash
PSTAD_TOKEN=sandbox-dev-token docker compose up --build
```

`pstad` on `http://localhost:8090`, the UI on `http://localhost:8091`. The
`pstad` service uses Microsoft's own Playwright image pinned to the same
version as `package.json`, so there's no OS/dependency guessing — the browsers
are already installed in the image.

## CI

`.github/workflows/ci.yml` runs on every push and PR: installs dependencies,
runs the Playwright tests on GitHub's own runner, and separately does a
`docker compose build` as a smoke test that the Dockerfiles still match the
repo. GitHub-hosted runners can only reach the public internet, which is
exactly what this project needs (a public site) — that's what makes plain
`on: push` CI possible here at all.

## Provenance

`server/` is a copy of a generic Go service (`pstad`) that also drives a
similar internal tool elsewhere, pointed at a different target. It has no
target-specific code — it's driven entirely by whatever `workflows.json` and
spec files `PSTAD_AUTOMATION_DIR` points it at, which is what made this split
possible without touching a single line of Go. The two copies are not
automatically kept in sync; a fix made in one won't appear in the other
unless it's copied over by hand.
