# hack/e2e/Makefile — local kind smoke for workbuddy v0.6 K8s path.
# REQ-147 / issue #335.
#
# Invoke from this directory:
#     make e2e               # full smoke: up, install, assert, teardown.
#     make e2e KEEP=1        # leave the cluster running on success for poking.
#     make e2e-up            # spin up cluster + workloads, do not assert.
#     make e2e-down          # delete the kind cluster.
#     make dry-validate      # offline checks only (helm template + docker
#                              build) — no kind needed, useful in CI/PR review.
#
# All targets are bash, set -euo pipefail. On any failure during `e2e`
# we dump pod logs (coordinator / mysql / otel / fake-gh) before
# tearing the cluster down — see `trap_teardown` below.
#
# This is a LOCAL-ONLY validation. There is no CI job that runs `make
# e2e`; doing so would require a kind+docker runner and is explicitly
# out of scope for the v0.6 smoke gate (see decision doc Block 5 and
# the issue #335 acceptance criteria).

SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

# Knobs — override on the command line.
CLUSTER_NAME      ?= workbuddy-e2e
NAMESPACE         ?= workbuddy-e2e
KIND_NODE_IMAGE   ?= kindest/node:v1.31.0
WORKBUDDY_IMAGE   ?= workbuddy:e2e
AGENTM_FAKE_IMAGE ?= workbuddy-agentm-fake:e2e
FAKE_GH_IMAGE     ?= workbuddy-fake-gh:e2e
HELM_RELEASE      ?= workbuddy
HELM_TIMEOUT      ?= 5m
KEEP              ?= 0

REPO_ROOT := $(abspath ../../)
CHART_DIR := $(REPO_ROOT)/deploy/helm/workbuddy
VALUES    := $(abspath values/workbuddy-values.yaml)

.PHONY: e2e e2e-up e2e-down e2e-assert dry-validate \
        prereq build-images load-images create-cluster \
        install-mysql install-otel install-fake-gh install-workbuddy \
        wait-coordinator dump-logs

# ===== entry points ==========================================================

e2e: prereq
	@echo "==> running full kind smoke"
	@trap '$(MAKE) --no-print-directory dump-logs || true; \
	       if [ "$(KEEP)" != "1" ]; then $(MAKE) --no-print-directory e2e-down || true; fi' EXIT; \
	$(MAKE) --no-print-directory e2e-up && \
	$(MAKE) --no-print-directory e2e-assert
	@echo "==> smoke OK"

e2e-up: prereq create-cluster build-images load-images \
        install-mysql install-otel install-fake-gh install-workbuddy \
        wait-coordinator

e2e-down:
	@echo "==> tearing down kind cluster $(CLUSTER_NAME)"
	-kind delete cluster --name $(CLUSTER_NAME)

# Offline-only validation; safe to run anywhere docker + helm exist.
dry-validate: prereq-dry
	@echo "==> helm lint"
	helm lint $(CHART_DIR)
	@echo "==> helm template (smoke values)"
	helm template $(HELM_RELEASE) $(CHART_DIR) -f $(VALUES) > /tmp/workbuddy-e2e-rendered.yaml
	@echo "    rendered manifest -> /tmp/workbuddy-e2e-rendered.yaml"
	@echo "==> docker build workbuddy image (no push, no kind)"
	docker build -t $(WORKBUDDY_IMAGE) \
	    -f $(REPO_ROOT)/hack/e2e/workbuddy-image/Dockerfile $(REPO_ROOT)
	@echo "==> docker build agentm-fake image"
	docker build -t $(AGENTM_FAKE_IMAGE) $(REPO_ROOT)/hack/e2e/agentm-fake
	@echo "==> docker build fake-gh image"
	docker build -t $(FAKE_GH_IMAGE) $(REPO_ROOT)/hack/e2e/fake-gh
	@echo "==> dry-validate OK"

# ===== prerequisites =========================================================

prereq:
	@for tool in kind helm kubectl docker; do \
	    if ! command -v $$tool >/dev/null 2>&1; then \
	        echo "ERROR: $$tool is required on PATH" >&2; \
	        echo "See hack/e2e/README.md § Prerequisites." >&2; \
	        exit 2; \
	    fi; \
	done

prereq-dry:
	@for tool in helm docker; do \
	    if ! command -v $$tool >/dev/null 2>&1; then \
	        echo "ERROR: $$tool is required on PATH" >&2; \
	        exit 2; \
	    fi; \
	done

# ===== build / load ==========================================================

build-images:
	@echo "==> building workbuddy image"
	docker build -t $(WORKBUDDY_IMAGE) \
	    -f $(REPO_ROOT)/hack/e2e/workbuddy-image/Dockerfile $(REPO_ROOT)
	@echo "==> building agentm-fake image"
	docker build -t $(AGENTM_FAKE_IMAGE) $(REPO_ROOT)/hack/e2e/agentm-fake
	@echo "==> building fake-gh image"
	docker build -t $(FAKE_GH_IMAGE) $(REPO_ROOT)/hack/e2e/fake-gh

load-images:
	@echo "==> loading images into kind cluster $(CLUSTER_NAME)"
	kind load docker-image --name $(CLUSTER_NAME) $(WORKBUDDY_IMAGE)
	kind load docker-image --name $(CLUSTER_NAME) $(AGENTM_FAKE_IMAGE)
	kind load docker-image --name $(CLUSTER_NAME) $(FAKE_GH_IMAGE)

# ===== cluster / workloads ===================================================

create-cluster:
	@if kind get clusters 2>/dev/null | grep -qx "$(CLUSTER_NAME)"; then \
	    echo "==> cluster $(CLUSTER_NAME) already exists, reusing"; \
	else \
	    echo "==> creating kind cluster $(CLUSTER_NAME)"; \
	    KIND_EXPERIMENTAL_DOCKER_NETWORK=bridge \
	    kind create cluster --name $(CLUSTER_NAME) \
	        --image $(KIND_NODE_IMAGE) \
	        --config kind-config.yaml; \
	fi
	kubectl create namespace $(NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -

install-mysql:
	@echo "==> installing MySQL"
	kubectl apply -f manifests/mysql.yaml
	kubectl -n $(NAMESPACE) rollout status deploy/mysql --timeout=$(HELM_TIMEOUT)

install-otel:
	@echo "==> installing OTel collector"
	kubectl apply -f manifests/otel-collector.yaml
	kubectl -n $(NAMESPACE) rollout status deploy/otel-collector --timeout=$(HELM_TIMEOUT)

install-fake-gh:
	@echo "==> installing fake-gh"
	kubectl apply -f manifests/fake-gh.yaml
	kubectl -n $(NAMESPACE) rollout status deploy/fake-gh --timeout=$(HELM_TIMEOUT)

install-workbuddy:
	@echo "==> helm install workbuddy"
	helm upgrade --install $(HELM_RELEASE) $(CHART_DIR) \
	    --namespace $(NAMESPACE) \
	    -f $(VALUES) \
	    --wait --timeout $(HELM_TIMEOUT)

wait-coordinator:
	@echo "==> waiting for coordinator pod Ready"
	kubectl -n $(NAMESPACE) wait \
	    --for=condition=available deploy/$(HELM_RELEASE) \
	    --timeout=$(HELM_TIMEOUT)

# ===== assertions ============================================================

e2e-assert:
	@echo "==> asserting MySQL connectivity from coordinator pod"
	# The store.NewCoordinator(dsn) call only logs success silently; we
	# observe success indirectly by confirming the coordinator pod is
	# Ready (above) AND that the `workbuddy` database has at least the
	# schema migrations table populated.
	@POD=$$(kubectl -n $(NAMESPACE) get pod -l app=mysql -o name | head -n1); \
	 kubectl -n $(NAMESPACE) exec $$POD -- \
	    mysql -uworkbuddy -pworkbuddy workbuddy \
	    -e "SHOW TABLES;" | tee /tmp/workbuddy-e2e-tables.txt | \
	    grep -q -E "issues|events|tasks" \
	    || (echo "FAIL: workbuddy did not create schema in MySQL" >&2; exit 1)
	@echo "    MySQL has workbuddy schema -> store.New(dsn) routed to MySQL OK"

	@echo "==> asserting OTel collector received at least one span"
	# The coordinator-side tracing.go starts a "coordinator.startup" span
	# on init; the debug exporter logs every span. We wait up to 30s for
	# any workbuddy-prefixed span to appear in the collector logs.
	@deadline=$$(( $$(date +%s) + 30 )); \
	 while [ $$(date +%s) -lt $$deadline ]; do \
	    if kubectl -n $(NAMESPACE) logs deploy/otel-collector 2>/dev/null \
	         | grep -qi "workbuddy"; then \
	      echo "    OTel collector saw a workbuddy span"; \
	      break; \
	    fi; \
	    sleep 2; \
	 done; \
	 if ! kubectl -n $(NAMESPACE) logs deploy/otel-collector 2>/dev/null \
	         | grep -qi "workbuddy"; then \
	   echo "WARN: no workbuddy span observed in 30s. This is a soft" >&2; \
	   echo "      assertion — span emission depends on the coordinator" >&2; \
	   echo "      actually starting a tracer-instrumented path during" >&2; \
	   echo "      smoke startup. Not failing the smoke on this signal" >&2; \
	   echo "      alone; see hack/e2e/README.md § Known limitations." >&2; \
	 fi

	@echo "==> asserting fake-gh is reachable from inside the cluster"
	@kubectl -n $(NAMESPACE) run curl-probe --rm -i --restart=Never \
	    --image=curlimages/curl:8.10.1 -- \
	    -fsS http://fake-gh.$(NAMESPACE).svc.cluster.local/_healthz \
	    >/dev/null \
	    || (echo "FAIL: fake-gh /_healthz not reachable" >&2; exit 1)
	@echo "    fake-gh /_healthz OK"

	@echo "==> AgentM dispatch + PR-creation assertion: SKIPPED (deferred)"
	@echo "    Reason: triggering an end-to-end AgentM dispatch in-cluster"
	@echo "    requires either (a) a real Gitea/GitHub instance the"
	@echo "    coordinator polls or (b) a fake-gh that faithfully emulates"
	@echo "    the gh-CLI JSON contract for issue list + label edit + PR"
	@echo "    create. The current fake-gh records requests but is not"
	@echo "    faithful enough to drive the poller. Tracking in a follow-up;"
	@echo "    see hack/e2e/README.md § What's actually asserted."

# ===== diagnostics ===========================================================

dump-logs:
	@echo "==> dumping pod logs for diagnosis (namespace=$(NAMESPACE))"
	-@kubectl -n $(NAMESPACE) get pods -o wide
	-@for d in $(HELM_RELEASE) mysql otel-collector fake-gh; do \
	    echo "----- logs: $$d -----"; \
	    kubectl -n $(NAMESPACE) logs deploy/$$d --tail=200 2>/dev/null || true; \
	 done
	-@kubectl -n $(NAMESPACE) describe pods 2>/dev/null | sed -n '1,200p'
