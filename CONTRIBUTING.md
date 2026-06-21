# Contributing

Thanks for your interest in improving the provider.

## Building

```sh
make build   # build the provider
make fmt     # gofmt
make vet     # go vet
```

`golangci-lint` runs in CI; you can run it locally with `golangci-lint run`.

## Tests

Unit tests need no network:

```sh
make test
```

Acceptance tests run against a real PostgreSQL instance and create real
resources. A local instance can be started with Docker:

```sh
make testacc_setup      # start a local PostgreSQL container
make testacc            # run the acceptance suite
make testacc_cleanup    # tear the container down
```

To run a single acceptance test against the local container:

```sh
TF_ACC=true PGHOST=127.0.0.1 PGPORT=25432 PGUSER=postgres PGPASSWORD=postgres \
  PGSSLMODE=disable PGSUPERUSER=true \
  go test -v ./postgresql -run '^TestAccRoleFW_Basic$'
```

Use `127.0.0.1` rather than `localhost` so the connection resolves to the IPv4
port the container publishes.

The transport tunnels (SSH bastion and GCP IAP) have end-to-end tests that stand
up an in-process server and run a query through them; they only need the local
database, not a real cloud. The Azure Bastion transport is experimental and is
not covered by an end-to-end test.

## Pull requests

- Run `make fmt`, `make vet`, and `make test` before submitting.
- Add or update tests for behavior changes.
- If you change a resource or provider schema, regenerate the docs.
