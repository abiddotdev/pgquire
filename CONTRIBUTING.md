# Contributing to pgquire

Issues and pull requests are welcome. For anything bigger than a fix, open an issue first so we can agree on the approach.

## Layout

- `server/index-remote.html`: the page, as the server embeds it. This is the one to edit.
- `index.html`: the GitHub Pages copy (PGlite only), generated from it. Don't edit it by hand.
- `server/`: the Go server (remote Postgres, agent access over MCP).
- `e2e/`: browser tests (Playwright).
- `docs/`: README assets and the sample share links.

## Editing the page

Edit `server/index-remote.html`, then run `go generate` in `server/` to regenerate `index.html`. Generation:
- drops everything marked remote-only: `/* @remote */`, `// @remote` or `<!-- @remote -->` at the end of a line, `/* @remote { */ … /* } @remote */` blocks, and inline `/*remote:*/ … /*:remote*/` in JS and CSS;
- switches on `// @pages:` lines.

A test fails when `index.html` is out of date.

## Building

```sh
cd server
go build -o pgquire .   # Go 1.27+
./pgquire
```

`docker build -t pgquire .` builds the image from the repository root.

## Tests

Server tests (in `server/`):

```sh
PGQUIRE_TEST_DSN=postgres://… go test ./...
```

Browser tests (in `e2e/`, needs Go, Node and Docker):

```sh
npm ci && npx playwright install chromium && npm test
```

They build the server, start a throwaway Postgres container and run the page in Chromium: connecting, queries, editing tables, read-only logins, `-dsn`, agent access over HTTP and stdio, and PGlite (from its CDN) in the app and the Pages copy.

- To use a Postgres of your own instead of the container, set `PGQUIRE_E2E_PG=postgres://user:pass@host/postgres`. It creates the `e2e_shop` database and the `pgquire_e2e_ro` login, and drops them first if they're there.
- The remote tests build on each other, so run `tests/remote.spec.js` whole rather than one test with `-g`.

## Releasing

1. Bump `APP_VERSION` in `server/index-remote.html` and `Version` in `server/main.go` together (a test checks that they match).
2. Run `go generate` in `server/`.
3. Push a tag like `v1.1.0` (`v` + that version).

The release workflow builds Linux, macOS and Windows binaries (amd64 and arm64) with GoReleaser (`.goreleaser.yaml`). It attaches them and the standalone page (`pgquire_<version>.html`, listed in `checksums.txt`) to a GitHub release, and publishes the Docker image to `ghcr.io/abiddotdev/pgquire` (amd64 and arm64). `goreleaser release --snapshot --clean` tries the build locally.
