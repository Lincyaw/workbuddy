# syntax=docker/dockerfile:1
#
# Combined workbuddy + AgentM runtime image for the Kubernetes topology
# (docs/decisions/2026-05-13-k8s-agentm-otel.md).
#
#   - workbuddy (Go binary)            — coordinator + embedded worker.
#   - agentm (Python CLI, on PATH)     — invoked as a subprocess by the
#                                        worker's AgentM backend.
#   - agentm[agent-env]                — includes ARL sandbox support
#                                        (operations_agent_env atom +
#                                        agent_env_repo scenario).
#   - gh + git                         — autonomous gitops (agents push,
#                                        create PRs, flip labels via gh CLI).

ARG GO_VERSION=1.26
ARG PYTHON_VERSION=3.12

# --------------------------------------------------------------------------
# Stage 1 — build the workbuddy Go binary.
# --------------------------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/workbuddy ./

# --------------------------------------------------------------------------
# Stage 2 — runtime: Python base + AgentM from PyPI + gh + git + workbuddy.
# --------------------------------------------------------------------------
FROM python:${PYTHON_VERSION}-slim-bookworm AS runtime

# Pin to a release; bump when upgrading AgentM.
ARG AGENTM_VERSION=0.2.2

# Host tools: git + gh for autonomous gitops; ca-certificates for HTTPS.
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

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

# Install AgentM + agent-env plugin from PyPI into a self-contained venv.
# Entry-point discovery is install-mode-agnostic: the scenario resolves
# BY NAME from a wheel — no editable, no AGENTM_PROJECT_ROOT, no path.
ENV AGENTM_HOME=/opt/agentm
ENV VIRTUAL_ENV=${AGENTM_HOME}/venv
# `agentm[agent-env]` pulls in the ARL sandbox SDK (arl-env); the
# operations_agent_env atom and agent_env_repo scenario are builtin and
# resolve by name — no entry-point package, no AGENTM_PROJECT_ROOT.
RUN uv venv "${VIRTUAL_ENV}" \
    && uv pip install --no-cache --python "${VIRTUAL_ENV}/bin/python" \
         "agentm[agent-env]==${AGENTM_VERSION}" \
    && "${VIRTUAL_ENV}/bin/agentm" --help >/dev/null

ENV PATH=${VIRTUAL_ENV}/bin:${PATH}

COPY --from=build /out/workbuddy /usr/local/bin/workbuddy

WORKDIR /var/lib/workbuddy

ENTRYPOINT ["/usr/local/bin/workbuddy"]
CMD ["serve"]
