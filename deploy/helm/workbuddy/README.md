# workbuddy Helm chart (skeleton)

Skeleton chart for deploying the workbuddy coordinator into a Kubernetes
cluster. Single-replica, MySQL-backed, OTel-instrumented — the K8s topology
described in `docs/decisions/2026-05-13-k8s-agentm-otel.md` Block 3.

This chart is intentionally minimal at v0.6.0. The full values surface
(MySQL host/user/pass/db split, OTel resource attributes, AegisLab topology
wiring) is hardened in **#334**. Ingress shipped via **#333** (REQ-144).

## Install

```bash
helm install my-workbuddy deploy/helm/workbuddy \
  --set mysql.dsn=mysql://user:pass@mysql.aegislab.svc:3306/workbuddy \
  --set otel.endpoint=http://otel-collector.aegislab.svc:4317 \
  --set giteaToken.secretName=workbuddy-gitea
```

## Required values

| Key | Description |
|-----|-------------|
| `mysql.dsn` | MySQL DSN (reused from AegisLab; see decision doc Block 3). |
| `otel.endpoint` | OTel collector endpoint. Empty disables export. |
| `giteaToken.secretName` | Name of an existing Secret with key `token`. |
| `agents.configMapName` | Name of an existing ConfigMap holding `.github/workbuddy/agents/*.md`. |

If `giteaToken.secretName` is empty **and** `giteaToken.token` is set, the
chart will materialize a Secret for dev use. Likewise, if
`agents.configMapName` is empty, the chart renders a ConfigMap populated
from `deploy/helm/workbuddy/agents/*.md` (empty by default — bundle your
defaults there or override).

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
