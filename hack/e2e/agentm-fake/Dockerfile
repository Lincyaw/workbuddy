# syntax=docker/dockerfile:1
#
# Fake AgentM dev container for the kind smoke (REQ-147 / #335).
#
# The chart wires WORKBUDDY_DEV_CONTAINER_IMAGE (#338) onto AgentM agents;
# the coordinator launches this image as the run container. We bake the
# fake binary + the few host tools its post-run path needs:
#
#   - bash       : the fake itself is a shell script.
#   - git        : the coordinator-side gitops.CommitAndPush shells out
#                  to `git` from INSIDE the runner sometimes (when the
#                  runner mounts a workspace volume); harmless if unused.
#   - ca-certs   : for any HTTPS the agent might attempt; the fake does
#                  nothing networked but a real runtime would.

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends bash git ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY agentm /usr/local/bin/agentm
RUN chmod +x /usr/local/bin/agentm

# Workdir matches the AgentM convention; the coordinator passes
# --workspace explicitly so this is informational.
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/agentm"]
