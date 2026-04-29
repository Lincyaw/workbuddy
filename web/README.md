# workbuddy-web

Preact + Vite single-page app served as the workbuddy dashboard. Real
pages (dashboard, sessions, issue detail) ship in follow-up issues — this
scaffold renders a placeholder so the build, embed, and proxy plumbing
can be exercised end-to-end.

## Layout

```
web/
├── package.json           # pnpm @ 9.x
├── tsconfig.json
├── vite.config.ts         # dev proxy + outDir=dist
├── index.html
└── src/
    ├── main.tsx           # mount <App /> on #root
    ├── App.tsx            # Router shell + placeholder route
    ├── api/client.ts      # fetch wrapper (cookies + 401 → /login)
    └── pages/
        └── Placeholder.tsx
```

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
