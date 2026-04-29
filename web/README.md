# workbuddy-web

Preact + Vite single-page app served as the workbuddy dashboard. It renders
the in-flight issue table, per-issue transition timeline, and session
links so operators can replace the "tail issue comments" loop.

## Layout

```
web/
├── package.json           # pnpm @ 9.x
├── tsconfig.json
├── vite.config.ts         # dev proxy + outDir=dist
├── index.html
└── src/
    ├── main.tsx           # mount <App /> on #root + import styles.css
    ├── App.tsx            # Router shell
    ├── styles.css         # global styling (cards, badges, tables)
    ├── api/
    │   ├── client.ts      # fetch wrapper (cookies + 401 → /login) + typed helpers
    │   └── types.ts       # hand-written TS shapes mirroring internal/auditapi structs
    ├── components/
    │   ├── Layout.tsx     # top bar + logout
    │   └── StateBadge.tsx # colored status badge
    ├── utils/
    │   ├── time.ts        # relative-time formatter
    │   └── cycle.ts       # dev↔review cycle helpers + DefaultMaxReviewCycles
    └── pages/
        ├── Dashboard.tsx       # /
        ├── IssueDetail.tsx     # /issues/:owner/:repo/:num
        ├── Sessions.tsx        # /sessions  (placeholder until #5)
        ├── SessionDetail.tsx   # /sessions/:id (placeholder until #5)
        └── NotFound.tsx
```

## Routes

| Path                              | Page          | Notes                                         |
|-----------------------------------|---------------|-----------------------------------------------|
| `/`                               | Dashboard     | health cards + in-flight table, 30s polling   |
| `/issues/:owner/:repo/:num`       | IssueDetail   | transitions timeline + cycle counts + sessions|
| `/sessions`                       | Sessions      | placeholder; full list ships in a follow-up   |
| `/sessions/:id`                   | SessionDetail | placeholder                                   |
| `/login`                          | (server)      | rendered by the coordinator, not the SPA      |

## Development

Requires Node 22 with corepack enabled (which makes `pnpm@9.x` available).

```bash
corepack enable
pnpm -C web install --frozen-lockfile
pnpm -C web dev
```

The dev server listens on `http://127.0.0.1:5173`. API requests under
`/api/`, `/sessions/`, `/health`, `/metrics`, `/events`, `/tasks`,
`/issues/`, `/workers/`, `/login`, and `/logout` are proxied to the
coordinator at `http://127.0.0.1:8090` (override with the
`WORKBUDDY_COORDINATOR_URL` environment variable).

## Building for the Go binary

The Go binary embeds `internal/webui/dist/`. The top-level `Makefile`
target wires this together:

```bash
make build   # runs pnpm build, copies web/dist → internal/webui/dist, then go build
```

If you only need the front-end bundle:

```bash
pnpm -C web build
```

`go build ./...` on a fresh clone still succeeds because
`internal/webui/dist/` ships with a placeholder `index.html` checked into
the repo. `make build` overwrites it with the real bundle.

## Local manual verification

The dashboard is exercised by humans rather than headless tests, so
verify it end-to-end before shipping a change:

1. **Start the coordinator** (auth + audit API both on):

   ```bash
   make build
   ./bin/workbuddy serve --auth-token-file <path-to-token>
   ```

   Or, against a hot-reloaded SPA:

   ```bash
   ./bin/workbuddy coordinator --auth-token-file <path-to-token>
   pnpm -C web dev
   ```

2. **Sign in.** Visit `http://127.0.0.1:5173/` (dev) or
   `http://127.0.0.1:8090/` (embedded). You should be 302'd to
   `/login`. Submit the token; you land back on `/`.

3. **Confirm the dashboard renders.** You should see four health
   cards (In-flight / Stuck > 1h / Done 24h / Failed 24h) and an
   in-flight issue table. Card values match
   `curl -b cookies http://127.0.0.1:8090/api/v1/status`.

4. **Watch the polling refresh.** Open DevTools → Network → filter
   `in-flight`. A request fires every ~30 s.

5. **Trigger a fresh issue.** Add `status:queued` to an unhandled
   issue and assign a worker. Within 30 s the dashboard table grows
   a row showing the right repo, title, state badge, and cycle.

6. **Inspect cycle counts.** After dev hands off to review, the
   "Cycle" column shows `1 / 3`. Force a second hand-off and confirm
   it bumps to `2 / 3`. Above the cap, the cell turns red.

7. **Force a stuck issue.** Update the cached transition row to be
   over an hour old:

   ```bash
   sqlite3 .workbuddy/workbuddy.db \
     "UPDATE events SET ts = '2020-01-01 00:00:00' \
      WHERE type = 'transition' AND issue_num = <num>;"
   ```

   Reload `/`. The "Last Transition" cell renders red.

8. **Drill into the issue.** Click the row. The browser navigates to
   `/issues/:owner/:repo/:num` (no full reload). The page shows the
   transition timeline, cycle counts, and any session refs.

9. **Test 401 redirection.** Delete the `wb_session` cookie in DevTools
   and reload `/`. The SPA should fetch `/api/v1/status`, get a 401,
   and redirect to `/login?next=%2F` automatically.

10. **Log out.** Click the **Log out** button in the top bar. You are
    sent to `/login`; the cookie is gone.

11. **Production bundle smoke test.**

    ```bash
    pnpm -C web build
    du -h web/dist/assets/*.js web/dist/assets/*.css
    ```

    Expect the JS+CSS gzipped total well under 200 KB. Run a Lighthouse
    Performance audit (`chrome --headless --enable-features=…` or the
    DevTools panel) against `http://127.0.0.1:8090/` after `make build`
    and confirm the score is > 90.
