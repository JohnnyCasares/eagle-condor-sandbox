#!/usr/bin/env bash
# Deploy this project on an OCI Ampere A1 instance running Oracle Linux.
#
# Docker is the reason the host OS doesn't matter here: the pstad container
# runs Microsoft's own Playwright image (Ubuntu-based) regardless of what
# the VM itself runs. Oracle Linux uses dnf, not apt, and firewalld, not
# ufw — that's the only real difference from an Ubuntu deploy.
#
# Run this ON the VM, over SSH, as a user with sudo.
set -euo pipefail

echo "== Removing podman/buildah if present (conflicts with docker-ce) =="
sudo dnf remove -y podman buildah 2>/dev/null || true

echo "== Installing Docker CE via the CentOS repo (works for Oracle Linux — same RHEL family) =="
sudo dnf install -y dnf-utils
sudo dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
sudo dnf install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
sudo systemctl enable --now docker

echo "== Adding \$USER to the docker group (avoids needing sudo for every docker command) =="
sudo usermod -aG docker "$USER"
echo "   -> log out and back in (or run 'newgrp docker') for this to take effect"

echo "== Opening firewalld for pstad (8090) and the UI (8091) =="
sudo firewall-cmd --permanent --add-port=8090/tcp
sudo firewall-cmd --permanent --add-port=8091/tcp
sudo firewall-cmd --reload

echo "== Cloning the project =="
if [ ! -d eagle-condor-sandbox ]; then
  git clone https://github.com/JohnnyCasares/eagle-condor-sandbox.git
fi
cd eagle-condor-sandbox

echo "== Generating a real token (do not use the sandbox-dev-token default on a reachable box) =="
TOKEN=$(openssl rand -hex 32)
echo "PSTAD_TOKEN=$TOKEN" > .env
echo "   -> saved to .env (gitignored). Save this token somewhere — you'll need it for every request:"
echo "   $TOKEN"

echo ""
echo "== Ready. Two things still needed before 'docker compose up': =="
echo "  1. OCI's Security List / Network Security Group must ALSO allow ingress on 8090"
echo "     and 8091 — firewalld alone is not enough, same lesson as last night's port-8080"
echo "     issue but at the cloud layer instead of the OS layer."
echo "  2. If containers fail with permission-denied errors that don't make sense, it's"
echo "     probably SELinux (enabled by default on Oracle Linux, not on Ubuntu). Confirm"
echo "     with 'sudo ausearch -m avc -ts recent' before reaching for 'setenforce 0' —"
echo "     that disables SELinux entirely rather than fixing the actual policy."
echo ""
echo "Then: docker compose up --build"
echo "(docker compose reads .env from this directory automatically — no --env-file flag needed)"
