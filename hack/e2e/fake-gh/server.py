#!/usr/bin/env python3
"""
Fake GitHub API server for the workbuddy v0.6 kind smoke (REQ-147 / #335).

Scope, intentionally narrow:

  - The smoke validates the *deployment plane* — chart installs, pod
    becomes Ready, MySQL connects, OTel exporter wires up — and the
    *coordinator-side gitops + labelwriter call paths* by recording
    every relevant request and exposing them on /_assertions.

  - It is NOT a faithful gh-CLI / GitHub API reimplementation. Routes
    return the minimum body that keeps the coordinator from crashing
    on startup. Anything not implemented is logged and returns 200
    with an empty body or 404 with a useful diagnostic.

  - The coordinator pod talks to this server via the GH_HOST env var
    (gh CLI's "I am an Enterprise instance" override). gh sends
    Bearer GH_TOKEN; we accept any non-empty token.

Endpoints recorded for assertion:

  - POST  /api/v3/repos/{owner}/{repo}/issues/{n}/labels         -> add labels
  - DELETE /api/v3/repos/{owner}/{repo}/issues/{n}/labels/{name} -> remove label
  - PATCH /api/v3/repos/{owner}/{repo}/issues/{n}                -> bulk edit
  - POST  /api/v3/repos/{owner}/{repo}/issues/{n}/comments       -> comment
  - POST  /api/v3/repos/{owner}/{repo}/pulls                     -> create PR
  - GET   /_assertions                                           -> introspection
  - GET   /_healthz                                              -> liveness

Run:
    PORT=8080 ./server.py
"""

from __future__ import annotations

import json
import os
import re
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

_LOCK = threading.Lock()
_RECORDED: list[dict[str, Any]] = []


def _record(method: str, path: str, body: Any) -> None:
    with _LOCK:
        _RECORDED.append({
            "ts": time.time(),
            "method": method,
            "path": path,
            "body": body,
        })


class Handler(BaseHTTPRequestHandler):
    server_version = "fake-gh/0.1"

    # Quiet down the default access-log noise; we have _RECORDED.
    def log_message(self, fmt: str, *args: Any) -> None:  # noqa: A003
        sys.stderr.write("[fake-gh] " + (fmt % args) + "\n")

    # ---- helpers --------------------------------------------------

    def _read_body(self) -> Any:
        n = int(self.headers.get("Content-Length") or 0)
        if n <= 0:
            return None
        raw = self.rfile.read(n)
        try:
            return json.loads(raw.decode("utf-8"))
        except Exception:
            return raw.decode("utf-8", errors="replace")

    def _json(self, code: int, payload: Any) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    # ---- routes ---------------------------------------------------

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/_healthz":
            self._json(200, {"ok": True})
            return
        if self.path == "/_assertions":
            with _LOCK:
                self._json(200, {"requests": list(_RECORDED)})
            return
        if self.path.startswith("/api/v3/rate_limit"):
            # gh CLI calls this on startup; return a permissive quota.
            self._json(200, {
                "resources": {
                    "core": {"limit": 5000, "remaining": 4999, "reset": int(time.time()) + 3600},
                },
            })
            return
        # Issues / PRs list — empty result keeps the poller happy.
        if re.search(r"/api/v3/repos/[^/]+/[^/]+/issues($|\?)", self.path):
            self._json(200, [])
            return
        if re.search(r"/api/v3/repos/[^/]+/[^/]+/pulls($|\?)", self.path):
            self._json(200, [])
            return
        _record("GET", self.path, None)
        self._json(200, {})

    def do_POST(self) -> None:  # noqa: N802
        body = self._read_body()
        _record("POST", self.path, body)
        # PR create — return a fake PR object so `gh pr create` succeeds.
        m = re.match(r"^/api/v3/repos/([^/]+)/([^/]+)/pulls$", self.path)
        if m:
            owner, repo = m.group(1), m.group(2)
            number = 999
            self._json(201, {
                "number": number,
                "html_url": f"http://fake-gh/{owner}/{repo}/pull/{number}",
                "state": "open",
            })
            return
        # GraphQL — gh CLI uses this for issue listing. Empty viewer + nodes.
        if self.path == "/api/graphql":
            self._json(200, {"data": {"viewer": {"login": "workbuddy-bot"}}})
            return
        self._json(200, {})

    def do_PATCH(self) -> None:  # noqa: N802
        body = self._read_body()
        _record("PATCH", self.path, body)
        self._json(200, {})

    def do_PUT(self) -> None:  # noqa: N802
        body = self._read_body()
        _record("PUT", self.path, body)
        self._json(200, {})

    def do_DELETE(self) -> None:  # noqa: N802
        _record("DELETE", self.path, None)
        self._json(200, {})


def main() -> int:
    port = int(os.environ.get("PORT", "8080"))
    srv = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    sys.stderr.write(f"[fake-gh] listening on :{port}\n")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
