# Deploying to an OCI Ampere VM — step by step

What actually happened deploying this project to Oracle Linux 9 (aarch64) on
an OCI `VM.Standard.A1.Flex` instance, written up so it's repeatable — at
work, or on a fresh instance if this one ever needs rebuilding.

## Prerequisites (done in the OCI Console, before any of this)

1. Create the instance: `VM.Standard.A1.Flex`, Oracle Linux 9, at least
   2 OCPU / 12 GB (this project needs roughly 1.2 GB per two concurrent
   browser sessions — see the capacity note near the bottom).
2. Assign a public IPv4 address.
3. Download the SSH private key when prompted — this is the **only** point
   it's offered. Without it there is no way into the box.
4. Note the public IP and the default username. Oracle Linux images use
   `opc`, not `ubuntu` or `root`.

## Step 1 — Connect and sanity-check the instance

```bash
chmod 600 path/to/your-key.key
ssh -i path/to/your-key.key opc@<PUBLIC_IP> "uname -m; cat /etc/os-release | head -3; free -h; nproc"
```

Confirm `aarch64` (Ampere is ARM, not x86 — this matters for step 2) and that
memory/CPU match what you provisioned. `free -h` should also show a few GB of
swap already configured by default on this image — a real safety net if a
container ever approaches the memory ceiling.

## Step 2 — Confirm the Docker images actually support arm64

Skip this once you trust it, but the first time on a new base image it's
worth 30 seconds: an amd64-only image on an ARM host either fails to pull or
silently falls back to slow, occasionally-flaky QEMU emulation, and either
way you won't find out until several minutes into a build.

```bash
curl -s -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  "https://mcr.microsoft.com/v2/playwright/manifests/v1.60.0-noble" | grep architecture
```

Should list `arm64` alongside `amd64`. `golang:*-alpine` and `nginx:alpine`
(the other two base images this project uses) have shipped multi-arch,
including arm64, for years — not worth re-checking those specifically.

## Step 3 — Run the deploy script

`deploy/oracle-linux.sh` in this repo does the rest: installs Docker CE via
`dnf` (Oracle Linux is RHEL-family, not Debian — different package manager
and repo than an Ubuntu box would use), opens the two ports in `firewalld`,
clones this repo, and generates a real `PSTAD_TOKEN` instead of the
`sandbox-dev-token` placeholder the compose file defaults to.

```bash
ssh -i path/to/your-key.key opc@<PUBLIC_IP> 'bash -s' < deploy/oracle-linux.sh
```

**Two things this script does NOT do, on purpose:**

- It doesn't open the ports at the *cloud* level. `firewalld` only controls
  the OS's own firewall — OCI's Security List / Network Security Group is a
  separate gate in front of that, and both have to allow the port or traffic
  never reaches the instance at all. Console → your VCN → Security Lists →
  Add Ingress Rule → TCP, ports 8090 and 8091, source `0.0.0.0/0` (or your
  IP only, if you want it locked down).
- It doesn't run `docker compose up` itself — see Step 4, because doing that
  reliably over SSH turned out to be its own lesson.

## Step 4 — Build and start the stack (the part that actually bit us)

The naive version:

```bash
ssh -i key.key opc@<IP> 'cd eagle-condor-sandbox && docker compose up --build -d'
```

**This can silently die partway through.** The `pstad` image pulls Microsoft's
Playwright base image, which is ~800 MB — on a real run, the SSH connection
itself dropped mid-transfer (ordinary network hiccup, nothing exotic), and
because `docker compose` was running *attached* to that SSH session, the
session dying took the build down with it. `docker compose ps` afterward
showed nothing running and no `pstad` image built — the small `ui` image had
finished, the large one hadn't.

The fix: detach the build from the SSH session itself, not just from the
resulting containers, so a dropped connection can't touch it.

```bash
ssh -i key.key opc@<IP> \
  "cd eagle-condor-sandbox && nohup docker compose up --build -d > build.log 2>&1 < /dev/null & disown"
```

`nohup` + `disown` means the process keeps running on the VM even if the SSH
session dies. Reconnect later (fresh `ssh` call) and check on it:

```bash
ssh -i key.key opc@<IP> "cd eagle-condor-sandbox && tail -30 build.log; docker compose ps"
```

## Step 5 — Verify it's actually up

```bash
curl -s http://<PUBLIC_IP>:8090/v1/health
```

Should return `{"status":"ok",...}`. Then open `http://<PUBLIC_IP>:8091` in a
browser for the UI — update the "pstad server URL" field there to
`http://<PUBLIC_IP>:8090` (it defaults to `127.0.0.1`, which only works when
the page and the server are on the same machine).

## One thing that was assumed and turned out wrong

The script originally assumed `git` was preinstalled. It isn't, on this
minimal Oracle Linux cloud image — the clone step failed with
`git: command not found` the first time this ran for real. Fixed by adding
an explicit `dnf install -y git` before the clone. Left in here as a reminder
that "should be fine" and "verified" are different things, even for
something as basic as git being present.

## Capacity, for reference

Measured locally before any of this: one headless Chromium session runs
about 610 MB RSS; two concurrent ones, about 1.26 GB. A 12 GB instance has
roughly 9-10x that — comfortable headroom, not a tight fit.
