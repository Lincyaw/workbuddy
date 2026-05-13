# MySQL backend for workbuddy.store

This package contains the schema bootstrap for the MySQL backend that the
`Store` factory uses when handed a DSN with the `mysql://` scheme. The Go
code that uses this schema lives one directory up in `internal/store` —
the `dbStore` concrete type is dialect-aware and translates SQLite-flavoured
SQL on the fly via `dialect.Rewrite`.

The MySQL backend is the production target for K8s deployments
(`docs/decisions/2026-05-13-k8s-agentm-otel.md` Block 3 § Storage). SQLite
remains the systemd default. The same `Store` interface fronts both.

## DSN form

```
mysql://<user>:<pass>@tcp(<host>:<port>)/<db>?parseTime=true&loc=UTC
```

The scheme prefix is stripped before the DSN is passed to
`github.com/go-sql-driver/mysql`. `parseTime=true` is enforced by the
factory regardless of whether the caller specified it — every timestamp
column in the schema is `DATETIME(6)` and `time.Time` parsing is required
for the store's `ParseTimestamp` helpers to work consistently across
backends.

## Running the integration test suite against MySQL

The full store test suite is normally exercised against SQLite. A
parallel suite gated behind the `mysql_integration` build tag runs the
same tests against a real MySQL. Wire-up:

```sh
# Start a disposable MySQL.
docker run --rm -d --name workbuddy-mysql-it \
    -e MYSQL_ROOT_PASSWORD=secret \
    -e MYSQL_DATABASE=workbuddy \
    -p 3307:3306 \
    mysql:8.0

# Wait until ready (mysql:8.0 takes ~15s on first boot).
until docker exec workbuddy-mysql-it \
        mysqladmin ping -uroot -psecret --silent >/dev/null 2>&1; do sleep 1; done

# Run the parallel suite.
export WORKBUDDY_MYSQL_TEST_DSN='mysql://root:secret@tcp(127.0.0.1:3307)/workbuddy?parseTime=true&loc=UTC&multiStatements=true'
go test -tags mysql_integration ./internal/store/... -count=1

# Tear down.
docker stop workbuddy-mysql-it
```

Or, in docker-compose form (drop into a temporary directory):

```yaml
# docker-compose.yml
services:
  mysql:
    image: mysql:8.0
    environment:
      MYSQL_ROOT_PASSWORD: secret
      MYSQL_DATABASE: workbuddy
    ports:
      - "3307:3306"
    healthcheck:
      test: ["CMD", "mysqladmin", "ping", "-uroot", "-psecret"]
      interval: 2s
      timeout: 2s
      retries: 30
```

Then:

```sh
docker compose up -d
docker compose exec -T mysql mysqladmin --wait=60 -uroot -psecret ping
export WORKBUDDY_MYSQL_TEST_DSN='mysql://root:secret@tcp(127.0.0.1:3307)/workbuddy?parseTime=true&loc=UTC&multiStatements=true'
go test -tags mysql_integration ./internal/store/... -count=1
docker compose down -v
```

Without `WORKBUDDY_MYSQL_TEST_DSN`, the integration tests skip themselves
even with the build tag set.

## CI

The default `go test ./...` run does not include the build tag, so CI
continues to exercise only the SQLite path (which catches the vast
majority of bugs because both backends share the same Go code).
Schema parity between the two backends is enforced by
`TestSchemaParity` in `internal/store/schema_parity_test.go`, which runs
unconditionally and fails CI if the SQLite DDL and MySQL DDL drift at
the table/column level.
