package webui

import "embed"

// distFS holds the production SPA bundle produced by `pnpm -C web build`
// and copied into internal/webui/dist/ by `make web`. The directory ships
// with a placeholder index.html so a fresh `go build` succeeds without
// running the front-end pipeline first; `make web` overwrites it with
// the real bundle.
//
//go:embed dist
var distFS embed.FS
