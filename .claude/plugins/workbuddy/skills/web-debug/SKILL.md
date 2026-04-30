---
name: web-debug
description: "Validate the workbuddy webui (SPA + auth + JSON API) end-to-end on the same machine workbuddy runs on, using browser-use's headless harness. Use when the user says 'verify the frontend', 'test the SPA', 'check dashboard loads', '前端验证', '验下 webui', or after merging changes that touch internal/webui, web/, or the auth/cookie path."
---

# web-debug

Headless-server-friendly recipe for driving the workbuddy SPA through a real Chromium via [browser-use/browser-harness](https://github.com/browser-use/browser-harness). The point is to verify, with screenshots, that the dashboard / sessions pages actually render, login works, and the JSON API contracts hold — not just that `go test` passed.

## When to use

- After merging anything that touches `internal/webui`, `web/`, `internal/app/auth_handlers.go`, or `internal/auditapi`.
- Before cutting a release tag.
- When the user reports a regression in the dashboard / login / sessions UI.
- When you need to capture screenshots of the SPA state for an issue / PR description.

Do **not** use for unit-level concerns (`go test ./internal/webui/...` covers those). This skill exists for the integration story: SPA build → embed → serve → real browser → API.

## Step 1 — Make sure browser-harness is installed

```bash
command -v browser-harness >/dev/null || {
  git clone https://github.com/browser-use/browser-harness ~/Developer/browser-harness
  cd ~/Developer/browser-harness && uv tool install -e .
}
```

If you've never used the harness on this machine before, also skim `~/Developer/browser-harness/install.md` and `SKILL.md` for the day-to-day API. The functions you'll actually call (`new_tab`, `goto_url`, `wait_for_load`, `type_text`, `press_key`, `capture_screenshot`, `page_info`) all live in `~/Developer/browser-harness/src/browser_harness/helpers.py` — read that file when an interaction is doing something unexpected.

## Step 2 — Bring up Chromium in headless-server-safe mode

The default `browser-harness --setup` flow assumes a real graphical Chrome session and toggling the `chrome://inspect` checkbox. **On a headless / SSH server that does not work**: Chrome will accept TCP on the debug port but hang HTTP/WS handshakes, and `DevToolsActivePort` is never written. Symptoms:

```
RuntimeError: fatal: DevToolsActivePort not found in [...long list of profile dirs...]
# or
RuntimeError: fatal: CDP WS handshake failed: timed out during opening handshake
```

Workaround: launch Chromium **headless** with an **isolated user-data-dir** and **`--remote-allow-origins='*'`**, then point the harness at the resulting WS URL via `BU_CDP_WS`.

```bash
# 1. Clean any stale Chrome / harness state
pkill -9 -f "remote-debugging" 2>/dev/null; sleep 1
rm -f /tmp/bu-default.sock /tmp/bu-default.pid

# 2. Launch headless Chrome with a dedicated profile directory
mkdir -p ~/.config/google-chrome-bu
nohup google-chrome \
    --headless=new \
    --remote-debugging-port=9222 \
    --remote-allow-origins='*' \
    --user-data-dir=$HOME/.config/google-chrome-bu \
    --disable-gpu --no-sandbox \
    > /tmp/chrome.log 2>&1 &
disown
sleep 5

# 3. Verify Chrome's CDP HTTP endpoint actually responds
curl -sS --max-time 5 http://127.0.0.1:9222/json/version | head
# → must return JSON with "webSocketDebuggerUrl"; if it times out, see Pitfalls below

# 4. Tell the harness exactly which WS to use (skips DevToolsActivePort discovery)
export BU_CDP_WS=$(curl -sS http://127.0.0.1:9222/json/version \
    | python3 -c "import sys,json;print(json.load(sys.stdin)['webSocketDebuggerUrl'])")

# 5. Smoke-test the harness
browser-harness -c 'print(page_info())'
# → {'url': 'about:blank', 'title': '', ...}
```

Why each flag matters:

| Flag | Why |
|---|---|
| `--headless=new` | Avoids requiring a working `DISPLAY`. The non-`new` headless mode is deprecated and renders incorrectly on some recent Chromium builds. |
| `--remote-allow-origins='*'` | Chrome ≥ 128 requires this; without it the port listens but every HTTP/WS request hangs forever. |
| `--user-data-dir=...` | Chrome refuses `--remote-debugging-port` on the **default** user-data-dir for security. You must pass an explicit, non-default dir. |
| `--disable-gpu --no-sandbox` | Stable headless on Linux servers without GL or user-namespace setup. |
| `BU_CDP_WS` | The harness's normal Chrome discovery scans a fixed list of profile directories looking for `DevToolsActivePort`. With an isolated user-data-dir it won't find anything; this env var bypasses discovery and points it at the WS directly. |

This setup is **disposable** — kill the Chrome PID and the dedicated user-data-dir is harmless. It will not interfere with the user's real Chrome profile.

## Step 3 — Build and run workbuddy

```bash
cd $WORKBUDDY_REPO

# Full build: SPA → embed → Go binary
make build

# Start serve in the background. Pick a non-conflicting port; use a throwaway DB.
TOKEN="testtoken123abc"
WORKBUDDY_AUTH_TOKEN="$TOKEN" ./bin/workbuddy serve \
    --listen 127.0.0.1:18090 \
    --auth \
    --report-base-url http://127.0.0.1:18090 \
    --db-path /tmp/wb-frontend-test.db \
    > /tmp/workbuddy.log 2>&1 &
disown
sleep 5

# Confirm health and bearer auth
curl -sS http://127.0.0.1:18090/health
curl -sS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:18090/api/v1/status
```

`workbuddy serve` must run from a directory that has `.github/workbuddy/` (the workbuddy repo itself, or a configured target repo). The `--db-path` flag points the SQLite to a throwaway location so this run does not touch the real `.workbuddy/workbuddy.db`.

## Step 4 — Drive the SPA

A normal end-to-end pass covers: 401 redirect → login form → dashboard → sessions.

```bash
mkdir -p /tmp/wb-shots

browser-harness -c "
# 1. Hit root with no cookie → expect 302 to /login
new_tab('http://127.0.0.1:18090/')
wait_for_load()
print('after-root:', page_info())     # url should be /login?next=%2F
capture_screenshot(path='/tmp/wb-shots/01-login.png')

# 2. Submit token (input is auto-focused on the login page)
type_text('testtoken123abc')
press_key('Enter')
wait_for_load()
print('after-login:', page_info())    # should land on /
capture_screenshot(path='/tmp/wb-shots/02-dashboard.png')

# 3. Click into Sessions
goto_url('http://127.0.0.1:18090/sessions')
wait_for_load()
capture_screenshot(path='/tmp/wb-shots/03-sessions.png')
"
```

Read each PNG with the `Read` tool to actually see what rendered. `page_info()`'s `title` and `url` are necessary but not sufficient — the UI can render blank with a happy title.

## Step 5 — Verify the API contracts curl-side

These checks belong with the SPA pass because the SPA depends on them holding:

```bash
TOKEN=testtoken123abc
BASE=http://127.0.0.1:18090

# Cookie path (used by browser)
curl -sS -i -b "wb_session=$TOKEN" $BASE/api/v1/status | head -3
# Bearer path (used by CLI/curl)
curl -sS -i -H "Authorization: Bearer $TOKEN" $BASE/api/v1/status | head -3
# HTML 401 → 302 /login
curl -sS -i -H "Accept: text/html" $BASE/ | head -3
# JSON 401 stays 401
curl -sS -i $BASE/api/v1/status | head -3
# Legacy session path still works but carries Deprecation header
curl -sS -i -H "Authorization: Bearer $TOKEN" \
    "$BASE/sessions/abc/events.json" | grep -iE "HTTP|deprecation|sunset|link"
# New /api/v1/sessions/{id}/events should NOT carry Deprecation
curl -sS -i -H "Authorization: Bearer $TOKEN" \
    "$BASE/api/v1/sessions/abc/events" | grep -iE "HTTP|deprecation"
# In-flight aggregator returns array (never null)
curl -sS -H "Authorization: Bearer $TOKEN" \
    $BASE/api/v1/issues/in-flight | python3 -m json.tool | head -20
```

Expected outcomes:

- Cookie + Bearer both → 200 (REQ-085 / cookie auth)
- HTML no-creds → 302 with `Location: /login?next=%2F`
- JSON no-creds → 401 `{"error":"unauthorized"}`
- `/sessions/{id}/events.json` → 200 with `Deprecation: true` and `Link: </api/v1/sessions/{id}/events>; rel="successor-version"` (REQ-088)
- `/api/v1/sessions/{id}/events` → 200 with no `Deprecation`
- `/api/v1/issues/in-flight` → JSON array

## Step 6 — Tear down

```bash
pkill -9 -f "workbuddy serve" 2>/dev/null
pkill -9 -f "remote-debugging" 2>/dev/null
rm -f /tmp/wb-frontend-test.db /tmp/workbuddy.log
# Keep ~/.config/google-chrome-bu around; reuses faster on next run.
```

## Pitfalls

- **Chrome's `--remote-debugging-port` fails silently on the default profile.** The log says `DevTools remote debugging requires a non-default data directory. Specify this using --user-data-dir.` Always pass `--user-data-dir`.
- **CDP HTTP times out on Chrome ≥ 128 without `--remote-allow-origins='*'`.** TCP connects fine, then sits forever. Add the flag.
- **Stale `SingletonLock` from a previous session blocks new launches.** If Chrome refuses to start: `rm -f ~/.config/google-chrome-bu/Singleton*`.
- **Harness's profile-dir search misses an isolated user-data-dir.** Always set `BU_CDP_WS` for this setup, or it falls back to scanning standard profile paths and fails with `DevToolsActivePort not found`.
- **`workbuddy serve` requires a config dir.** It looks for `.github/workbuddy/` in the current working directory. Run it from the workbuddy repo or a workbuddy-configured target repo.
- **Build can be silently broken at the TS level.** `tsc -b` runs as part of `make web` and will fail with cryptic union errors (e.g. `Property 'current' does not exist on type ...`) if a Layout prop drift slipped through merge conflict resolution. Treat a `make build` failure as a real bug — do not skip the SPA build to avoid it.
- **`page_info()` lies on broken pages.** A SPA that crashed during hydration may still report a happy title. Always pair `page_info()` with a `capture_screenshot()` and read the image.

## Index — related skills and references

- **browser-harness install** → `~/Developer/browser-harness/install.md` — first-time setup, attach-and-escalate flow, profile picker handling.
- **browser-harness day-to-day API** → `~/Developer/browser-harness/SKILL.md` — `new_tab`, `goto_url`, `click_at_xy`, screenshot-driven workflow, remote browsers via `BU_NAME`.
- **browser-harness function reference** → `~/Developer/browser-harness/src/browser_harness/helpers.py` — authoritative list of pre-imported helpers.
- **interaction skills** → `~/Developer/browser-harness/interaction-skills/` — `dialogs.md`, `iframes.md`, `screenshots.md`, `viewport.md`, etc., when a specific UI mechanic is misbehaving.
- **workbuddy webui contract source of truth** → `internal/auditapi/handler.go` (JSON shapes) and `web/src/api/*.ts` (consumer types). When adding a new endpoint to validate, start there.
- **workbuddy session viewer routing** → `cmd/coordinator.go` mux registration block — confirms which paths are SPA-served vs. JSON-served vs. legacy aliases.

## When NOT to use this skill

- For just-built backend changes that don't touch HTTP/JSON shape: `go test ./...` is enough.
- For pure documentation or skill updates: there's nothing to render.
- For verifying the Codex/Claude agent flow itself: use `pipeline-monitor` instead.
