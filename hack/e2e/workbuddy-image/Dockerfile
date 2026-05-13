# syntax=docker/dockerfile:1
#
# Minimal workbuddy container for the local kind smoke (REQ-147 / #335).
#
# This image is ONLY for `hack/e2e/Makefile`. Production images, if/when
# they exist, will live elsewhere with proper multi-arch + SBOM hygiene.
#
# Two-stage build keeps the runtime layer tiny so `kind load docker-image`
# is fast in the inner loop.

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
# Cache go mod downloads independently of source edits.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
# Copy the rest of the tree. .dockerignore at the repo root keeps
# this lean; for the e2e we accept a slightly fatter context.
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/workbuddy ./

FROM debian:bookworm-slim AS runtime
# gh CLI is workbuddy's only host dependency for GitHub interaction;
# install via the official apt repo. ca-certificates is needed both
# for gh and for outbound HTTPS in general.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gnupg git \
    && mkdir -p -m 755 /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
         | tee /etc/apt/keyrings/githubcli-archive-keyring.gpg > /dev/null \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
         > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/workbuddy /usr/local/bin/workbuddy

# Default workdir matches the chart's expectation; the chart sets
# WORKBUDDY_AGENTS_DIR explicitly so this is informational only.
WORKDIR /var/lib/workbuddy

# No ENTRYPOINT here — the chart specifies `args: [coordinator]`, so
# leaving a clean CMD lets `kubectl run --image=workbuddy:e2e <cmd>`
# work for ad-hoc debugging in the smoke.
ENTRYPOINT ["/usr/local/bin/workbuddy"]
