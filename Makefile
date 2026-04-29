.PHONY: web build clean test vet

# Build the front-end bundle and copy it into the embed root so
# `go build` produces a binary with the real SPA. The `.keep` file is
# written last so a fresh checkout still has a valid embed target.
web:
	corepack enable
	pnpm -C web install --frozen-lockfile
	pnpm -C web build
	rm -rf internal/webui/dist
	cp -r web/dist internal/webui/dist
	touch internal/webui/dist/.keep

build: web
	go build -o bin/workbuddy .

test:
	go test ./... -count=1

vet:
	go vet ./...

clean:
	rm -rf web/dist internal/webui/dist web/node_modules bin/
