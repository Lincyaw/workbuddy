# workbuddy Helm chart

Helm chart for deploying the workbuddy coordinator into a Kubernetes
cluster. Single-replica, MySQL-backed, OTel-instrumented — the K8s
topology described in `docs/decisions/2026-05-13-k8s-agentm-otel.md`
Block 3 (Storage) and Block 4 (Collector).

The chart is intentionally **service-reusing, not service-bundling**:
MySQL and the OTel collector are reused from a co-located AegisLab
release via plain values references. There is no MySQL subchart and no
OTel-collector subchart — by design, per the decision doc.

## Quickstart

```bash
helm install my-workbuddy deploy/helm/workbuddy \
  --set mysql.dsnSecretRef.name=workbuddy-mysql \
  --set otel.endpoint=http://otel-collector.aegislab.svc.cluster.local:4317 \
  --set giteaToken.secretName=workbuddy-gitea
```

Or, for dev/CI where Secrets are inconvenient:

```bash
helm install my-workbuddy deploy/helm/workbuddy \
  --set mysql.dsn='mysql://wb:wb@tcp(mysql:3306)/workbuddy?parseTime=true' \
  --set otel.endpoint=http://otel:4317 \
  --set giteaToken.token=ghp_xxx
```

## Values

## Ingress

Disabled by default. Enable to expose the coordinator's webui, JSON API,
and webhook entrypoint through a single host:

```bash
helm install my-workbuddy deploy/helm/workbuddy \
  --set mysql.dsn=mysql://user:pass@mysql.aegislab.svc:3306/workbuddy \
  --set giteaToken.secretName=workbuddy-gitea \
  --set ingress.enabled=true \
  --set ingress.host=workbuddy.example.com
```

The chart renders one `Ingress` resource with three Prefix routes, all
backed by the coordinator Service on port `service.port` (default 8080):

| Path | Purpose |
|------|---------|
| `/webui` | SPA served from `internal/webui/dist`. |
| `/api`   | JSON API consumed by the webui and by `workbuddy worker` long-poll. |
| `/webhook` | Gitea / GitHub push webhook entrypoint. |

Optional fields:

| Key | Description |
|-----|-------------|
| `ingress.className` | Sets `ingressClassName` (omit to use cluster default). |
| `ingress.annotations` | Free-form annotations (e.g. nginx rewrite rules). |
| `ingress.tls.enabled` | Render a `tls:` block on the Ingress. |
| `ingress.tls.secretName` | Existing TLS Secret name. cert-manager / ACME is out of scope — wire it externally. |

## What's not here yet

- Full values surface — split MySQL fields, OTel docs, AegisLab topology
  defaults (tracked in **#334**).
- Worker deployment — K8s mode collapses worker into the coordinator
  process per decision doc Block 2 § Architectural consequences. AgentM
  execution goes through the agent-env Gateway, which has its own Helm
  release.

### MySQL

| Key | Default | Description |
|-----|---------|-------------|
| `mysql.dsn` | `""` | Full `mysql://` DSN. Plaintext — dev only. |
| `mysql.dsnSecretRef.name` | `""` | Read the DSN from this Secret instead. Recommended for prod. |
| `mysql.dsnSecretRef.key` | `dsn` | Key inside the referenced Secret. |

Exactly one of `mysql.dsn` and `mysql.dsnSecretRef.name` should be set.
When both are empty, the coordinator falls back to its built-in SQLite
default (suitable for dev only — pods are ephemeral, so data is lost on
restart).

DSN format (consumed by `internal/store/store.go::New`):

```
mysql://<go-mysql-driver DSN>
```

For example:

```
mysql://wb:secret@tcp(mysql.aegislab.svc.cluster.local:3306)/workbuddy?parseTime=true&loc=UTC&multiStatements=true
```

### OpenTelemetry

| Key | Default | Description |
|-----|---------|-------------|
| `otel.endpoint` | `""` | OTLP collector endpoint. Empty disables export. |
| `otel.protocol` | `grpc` | `grpc` or `http/protobuf` (alias `http`). |
| `otel.serviceName` | `workbuddy` | `OTEL_SERVICE_NAME`. |

These map onto the **standard OTel SDK env vars**
(`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`,
`OTEL_SERVICE_NAME`) consumed by `internal/tracing/tracing.go`. All
other `OTEL_*` env vars supported by the SDK pass through unmodified —
override via `podAnnotations` or by extending the deployment.

### Gitea / GitHub token

| Key | Default | Description |
|-----|---------|-------------|
| `giteaToken.secretName` | `""` | Existing Secret holding the token. Required for prod. |
| `giteaToken.secretKey` | `token` | Key inside the Secret. |
| `giteaToken.token` | `""` | Plaintext token. Dev only — ignored when `secretName` is set. |

The chart mounts the same Secret value into **two** env vars so a single
token covers both backends:

- `GH_TOKEN` — read by `gh` CLI in the GitHub poller / reporter path.
- `GITEA_TOKEN` — read by `internal/labelwriter` for AgentM-mode label
  writes against the Gitea REST API.

### Agent definitions

| Key | Default | Description |
|-----|---------|-------------|
| `agents.configMapName` | `""` | Existing ConfigMap mounted at `/etc/workbuddy/agents`. |

If empty, the chart renders a ConfigMap from
`deploy/helm/workbuddy/agents/*.md` (empty by default — bundle your
defaults there or override with `configMapName`).

## Env contract (pod env vars the coordinator actually consumes)

| Pod env var | Source code | Notes |
|-------------|-------------|-------|
| `WORKBUDDY_MYSQL_DSN` | `internal/store/store.go::New` | DSN entry point. Currently surfaced for forward-compatibility; flag-level switch to consume this as the default coordinator DB is tracked alongside this change. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `internal/tracing/tracing.go` | Standard SDK var. Empty = exporter disabled. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `internal/tracing/tracing.go` | `grpc` (default) or `http/protobuf`. |
| `OTEL_SERVICE_NAME` | `internal/tracing/tracing.go` | Overrides default service name. |
| `GH_TOKEN` | `gh` CLI invoked by poller / reporter | Standard `gh` env var. |
| `GITEA_TOKEN` | `internal/labelwriter/labelwriter.go` | Bearer for AgentM-mode label writes. |
| `WORKBUDDY_AGENTS_DIR` | `cmd/coordinator.go` (planned) | Path where agent ConfigMap is mounted. |

## AegisLab topology

This is the canonical "reuse AegisLab services" deployment per the
decision doc. Workbuddy and AegisLab co-exist in the same cluster;
workbuddy points at AegisLab's already-running MySQL and OTel collector
via plain DNS references.

```yaml
# values-aegislab.yaml
mysql:
  dsnSecretRef:
    name: aegislab-mysql-workbuddy
    key: dsn

otel:
  endpoint: http://otel-collector.aegislab.svc.cluster.local:4317
  protocol: grpc
  serviceName: workbuddy

giteaToken:
  secretName: gitea-bot-token
  secretKey: token

agents:
  configMapName: workbuddy-agents
```

Prerequisites (created out-of-band — these are NOT managed by this chart):

- Secret `aegislab-mysql-workbuddy` with key `dsn` containing the full
  `mysql://wb:...@tcp(mysql.aegislab.svc.cluster.local:3306)/workbuddy?...`
  string. Typically created by ExternalSecrets or sealed-secrets and
  rotated independently of workbuddy releases.
- Secret `gitea-bot-token` with key `token` containing the Gitea PAT
  used by the coordinator's poller and the labelwriter.
- ConfigMap `workbuddy-agents` containing the `.github/workbuddy/agents/*.md`
  files for this deployment.
- AegisLab's OTel collector listening on
  `otel-collector.aegislab.svc.cluster.local:4317` (OTLP/gRPC). Workbuddy
  emits OTLP and AegisLab handles fan-out / tail-based sampling / storage.

Install:

```bash
helm install workbuddy deploy/helm/workbuddy -f values-aegislab.yaml
```

## What's not here

- **Worker deployment** — the K8s topology collapses the worker process
  into the coordinator (decision doc Block 2 § Architectural
  consequences). AgentM execution goes through the agent-env Gateway,
  which has its own Helm release. agent-env values are explicitly NOT in
  this chart.
- **MySQL / OTel collector subcharts** — explicit non-goal per decision
  doc. Reuse AegisLab.
- **Multi-replica + leader election** — single-replica + `Recreate`
  strategy only. Horizontal scale-out is deferred.

## Validation

```bash
helm lint deploy/helm/workbuddy/
helm template my deploy/helm/workbuddy/ \
  --set mysql.dsnSecretRef.name=workbuddy-mysql \
  --set otel.endpoint=http://otel:4317 \
  --set giteaToken.secretName=workbuddy-gitea
```

Both should complete with no errors.
