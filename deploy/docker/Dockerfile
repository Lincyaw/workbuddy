# syntax=docker/dockerfile:1
#
# Combined workbuddy + AgentM runtime image for the Kubernetes topology
# (docs/decisions/2026-05-13-k8s-agentm-otel.md). Unlike hack/e2e/* (which
# bakes a *fake* agentm), this image bundles the REAL AgentM CLI so the pod
# can run `workbuddy serve --runtime agentm`:
#
#   - workbuddy (Go binary)            — coordinator + embedded worker.
#   - agentm (Python CLI, on PATH)     — invoked as a subprocess by the
#                                        worker's AgentM backend.
#   - AgentM contrib tree at a stable  — the `agent_env_repo` scenario the
#     path (/opt/agentm/contrib)         agent configs reference; drives the
#                                        agent-env sandbox + cwd<->pod sync.
#   - gh + git                         — the coordinator-managed gitops +
#                                        labelwriter publish path.
#
# The agentm subprocess talks to the in-cluster agent-env Gateway over HTTP
# (AGENTM_AGENT_ENV_GATEWAY_URL); workbuddy Go never calls the Gateway. No
# GitHub credentials ever enter the sandbox pod — they live only in this
# process (GH_TOKEN), per the credential boundary.

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
# Stage 2 — runtime: Python base + real AgentM + gh + git + workbuddy.
# --------------------------------------------------------------------------
FROM python:${PYTHON_VERSION}-slim-bookworm AS runtime

# AgentM source to bundle. Defaults to the public mirror @ main; pin
# AGENTM_REF to a commit/tag for reproducible builds.
ARG AGENTM_REPO=https://github.com/Lincyaw/AgentM.git
ARG AGENTM_REF=main

# Host tools: git + gh for the gitops/labelwriter publish path; curl/gnupg
# only to add the gh apt repo; ca-certificates for outbound HTTPS.
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

# uv: AgentM builds with the uv_build backend and is uv-native, so we use
# uv to materialize a self-contained venv with the agent-env extra.
COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

# Clone AgentM at the requested ref and install — NON-editable — both the
# core `agentm` package and the `agentm-agentenv` plugin distribution
# (carries arl-env + registers the `operations_agent_env` atom and the
# `agent_env_repo` scenario via the agentm.atoms / agentm.scenarios entry
# points). Entry-point discovery is install-mode-agnostic, so the scenario
# resolves BY NAME from a wheel — no editable, no AGENTM_PROJECT_ROOT, no
# path. (See AgentM src/agentm/extensions/{discover,loader}.py.)
#
# The clone exists only because `agentm` / `agentm-agentenv` aren't on PyPI
# yet; once published this collapses to `uv pip install "agentm[agent-env]"`.
ENV AGENTM_HOME=/opt/agentm
ENV VIRTUAL_ENV=${AGENTM_HOME}/venv
RUN git clone --depth 1 --branch "${AGENTM_REF}" "${AGENTM_REPO}" "${AGENTM_HOME}" \
      || git clone "${AGENTM_REPO}" "${AGENTM_HOME}" \
    && ( cd "${AGENTM_HOME}" && git checkout "${AGENTM_REF}" || true ) \
    && uv venv "${VIRTUAL_ENV}" \
    && uv pip install --python "${VIRTUAL_ENV}/bin/python" \
         "${AGENTM_HOME}" "${AGENTM_HOME}/contrib/extensions/agentenv" \
    && "${VIRTUAL_ENV}/bin/agentm" --help >/dev/null

# Put the agentm console script on PATH for the worker subprocess lookup
# (internal/agent/agentm/backend.go DefaultBinary = "agentm").
ENV PATH=${VIRTUAL_ENV}/bin:${PATH}

COPY --from=build /out/workbuddy /usr/local/bin/workbuddy

# Working directory holds the per-repo clones + the SQLite DB under
# .workbuddy/. The chart mounts a volume here.
WORKDIR /var/lib/workbuddy

ENTRYPOINT ["/usr/local/bin/workbuddy"]
CMD ["serve"]
