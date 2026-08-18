# Build context is the repo root (docker-compose.yml passes `.`) — this whole
# repo IS the project pstad runs, so no nested subfolder to account for.

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY server/go.mod ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=0 go build -o /out/pstad ./cmd/pstad

# Microsoft's own image, pinned to the exact Playwright version package.json
# depends on — the browsers are already installed for this build.
FROM mcr.microsoft.com/playwright:v1.60.0-noble

COPY --from=build /out/pstad /usr/local/bin/pstad

WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
# Only what pstad/Playwright actually reads at runtime — not server/ (already
# built above) or public/ (the UI container's job), so the Go source and
# nginx-only files don't end up duplicated inside this image.
COPY playwright.config.js workflows.json ./
COPY tests/ ./tests/

ENV PSTAD_AUTOMATION_DIR=/app
ENV PSTAD_ADDR=0.0.0.0:8090
# PSTAD_TOKEN is deliberately not set here — supplied at `docker run`/compose
# time, never baked into an image layer.

EXPOSE 8090
ENTRYPOINT ["pstad"]
