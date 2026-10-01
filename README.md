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

Then pick **Connect remote Postgres…** in the database switcher and paste a connection string (`postgres://user:password@host:5432/db`). If the server has several databases, you choose which ones to add. They're grouped under the server in the switcher, and **Other databases on this server…** adds more later.

- Passwords stay with the server. Connections are kept in its memory until it stops, unless you tick **Remember on this computer**; remembered ones go in `~/.config/pgquire/connections.json` (readable only by you) and never reach the browser.
- The server listens on 127.0.0.1 only and needs the token from the link it prints. A page opened another way (a bookmark, or after a restart) asks you to paste that link or token. The token is new each time pgquire starts; set a fixed one with `-token` or `PGQUIRE_TOKEN`.
- Behind a reverse proxy, or listening beyond 127.0.0.1, set `-domain` (or `PGQUIRE_DOMAIN`) to the host name you reach it by; requests for any other name are refused. When the proxy serves HTTPS and sends `X-Forwarded-Proto: https`, the sign-in cookie is marked Secure.
- Connections marked **Production** get a red marker, start read-only, and ask before any write once you make them writable. Read-only here is a guard rail (SQL can switch it off), so pgquire checks what the login can really do: if a "read-only" connection's login can write, it says so and offers **Create a read-only login…**. That creates a login that can read everything and change nothing (`pg_read_all_data`, or per-schema grants before PostgreSQL 14), switches the connection to it, and keeps its generated password on the server. If your login can't create logins, **Copy SQL** gives your admin the statements. The login is named `pgquire_ro` by default; forgetting the connection offers to remove it again (`drop owned by …; drop role …`).
- Big tables show estimated row counts (`~`). Results in the page are capped at 50,000 rows (`-max-rows`); **CSV (all)** and table **Export CSV** stream every row. Running queries can be stopped.
- The browser keeps in-browser databases per address, so ones made on the live page don't appear at `127.0.0.1:8432`, and the other way round. Move them with **Export session / Open session file**.

- **Move data…** in the switcher gathers every way data goes in or out: **Copy to a local database** (a sample or all rows, to experiment on safely), **Download SQL dump** (no `pg_dump` needed), session export, and importing files.
- The connect dialog takes a connection string or separate fields, and **Test** shows the server version and whether the connection is encrypted. The green dot next to the database name says the same, and turns red with a **Reconnect** banner if the pgquire server stops.

`./pgquire -h` lists the options. Tests: `PGQUIRE_TEST_DSN=postgres://… go test ./...`

### Editing the page

Edit `index-remote.html`: it's the page the server embeds. `index.html` (the GitHub Pages copy, PGlite only) is generated from it by `go generate`, which drops everything marked remote-only (`/* @remote */`, `// @remote` or `<!-- @remote -->` at the end of a line, `/* @remote { */ … /* } @remote */` blocks, inline `/*remote:*/ … /*:remote*/` in JS and CSS) and switches on `// @pages:` lines. A test fails when `index.html` is out of date.

### Releasing

Bump `APP_VERSION` in `index-remote.html` and `Version` in `main.go` together (a test checks that they match), run `go generate`, then push a tag like `v1.1`. The release workflow builds Linux, macOS and Windows binaries and attaches them to a GitHub release.

## License

MIT
