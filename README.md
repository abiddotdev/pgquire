# pgquire — inquire into Postgres

A single-file Postgres workbench. Run queries, browse schema, and explore your database from a clean, notebook-inspired interface that runs entirely in the browser.

## Live Demo

**https://abiddotdev.github.io/pgquire/**

## Features

- SQL query editor with results grid
- Database schema browser
- Light/dark "Ledger" theming
- Zero build step — one self-contained HTML file

## Usage

Open the [live page](https://abiddotdev.github.io/pgquire/) or `index.html` directly in your browser. Everything runs client-side; no server required.

## Remote Postgres (optional server)

To work with a real Postgres server, run pgquire through its small Go server. It serves the same page and lets it connect to remote databases, which show up in the database switcher next to your in-browser ones.

Download a build for your system from [Releases](https://github.com/abiddotdev/pgquire/releases), or build it yourself:

```sh
go build -o pgquire .   # Go 1.22+
./pgquire               # opens http://127.0.0.1:8432/?t=<token>
```

Then pick **Connect remote Postgres…** in the database switcher and paste a connection string (`postgres://user:password@host:5432/db`).

- Passwords stay with the server. Saved connections go in `~/.config/pgquire/connections.json` (readable only by you) and never reach the browser.
- The server listens on 127.0.0.1 only and needs the token from the link it prints.
- Connections marked **Production** get a red marker, start read-only, and ask before any write once you make them writable. Read-only here is a guard rail: for a hard guarantee, connect as a read-only role.
- Big tables show estimated row counts (`~`). Results are capped at 50,000 rows (`-max-rows`), and running queries can be stopped.
- The browser keeps in-browser databases per address, so ones made on the live page don't appear at `127.0.0.1:8432`, and the other way round. Move them with **Export session / Open session file**.

- For a remote database, the switcher also offers **Copy to a local database…** (a sample or all rows, to experiment on safely) and **Download SQL dump**. Neither needs `pg_dump` installed.

`./pgquire -h` lists the options. Tests: `PGQUIRE_TEST_DSN=postgres://… go test ./...`

### Releasing

Bump `APP_VERSION` in `index.html` and `Version` in `main.go` together (a test checks that they match), then push a tag like `v1.1`. The release workflow builds Linux, macOS and Windows binaries and attaches them to a GitHub release.

## License

MIT
