# pgquire — inquire into Postgres

A single-file Postgres workbench with a clean, ledger-inspired interface. On its own it runs a real PostgreSQL (PGlite) entirely in your browser; with its small optional server it also works on your real Postgres servers.

## Live Demo

**https://abiddotdev.github.io/pgquire/**

## Features

**Browse and edit**
- The Index lists tables and views, grouped by schema; **Ctrl K** jumps to a table, database or command, runs SQL, or asks the Clerk
- A grid with paging, sorting and SQL filters; edit cells in place, add and delete rows, and **Ctrl+Z** undoes the last change
- Row details: the whole row, the rows it points to and the rows that point at it; right-click a cell to follow a key
- Structure view: columns, indexes, constraints and the table's `CREATE` statement

**Query**
- SQL editor with highlighting; every statement's result is kept, and results download as CSV or copy for a spreadsheet
- Open `.sql` files or save to them; big scripts and `pg_dump` output run in chunks with progress and Stop
- A `BEGIN` stays open across runs, with a banner to commit or roll back
- Saved queries, and a journal of every statement run (yours, the grid's and the Clerk's)

**Design**
- Create tables visually and import CSVs, with types inferred
- An ERD of the whole schema, filtered by schema, plus a relations diagram above each table; export as SVG or PNG, or copy as Mermaid, DBML or text
- 33 extensions load on demand in the browser: fuzzy search, UUIDs, crypto, vectors, PostGIS and more

**The Clerk (optional AI)**
- Any OpenAI-compatible endpoint (OpenRouter, OpenAI, Groq, Ollama, LM Studio); your key stays in the tab's memory and is never saved
- It reads the schema, runs queries and writes SQL into the editor; changes wait for your approval
- Several conversations per database, with token and cost tracking; export them as JSON or Markdown

**Move data**
- Many databases, each with its own colour, tabs, saved queries and chats
- SQL dumps, exact clones, and `.pgquire` session files that carry databases with their tabs, queries and chats
- Share as link: the data rides in the URL fragment (optionally encrypted with a passphrase), which browsers never send to a server
- A Storage view shows how much room pgquire takes against the browser's limit, and lets you export or delete databases to free it

**Look**
- "Ledger" (ink on paper) or "Prism" (crisp, with column profiles in the grid), each in day and night; switch from the top-right Appearance menu
- Right-click menus throughout; the in-app **README** tab lists every feature

Zero build step: one self-contained HTML file.

## Usage

Open the [live page](https://abiddotdev.github.io/pgquire/) or `index.html` directly in your browser. Everything runs client-side; nothing leaves your machine unless you ask the Clerk, share a link or export a file.

## Remote Postgres (optional server)

To work with a real Postgres server, run pgquire through its small Go server. It serves the same page and lets it connect to remote databases, which show up in the database switcher next to your in-browser ones.

Download the build for your system from [Releases](https://github.com/abiddotdev/pgquire/releases) (about 4 MB), unpack it and run `pgquire`; it opens the workbench in your browser.

| System | File |
|---|---|
| macOS, Apple silicon (M1 and later) | `pgquire_<version>_darwin_arm64.tar.gz` |
| macOS, Intel | `pgquire_<version>_darwin_amd64.tar.gz` |
| Windows | `pgquire_<version>_windows_amd64.zip` (`arm64` for ARM PCs) |
| Linux | `pgquire_<version>_linux_amd64.tar.gz` (`arm64` for ARM, e.g. Raspberry Pi) |

The builds aren't signed, so macOS refuses to open it the first time: run `xattr -d com.apple.quarantine pgquire` once, or allow it under System Settings → Privacy & Security. Windows SmartScreen may ask too (More info → Run anyway). `checksums.txt` on the release lets you verify the download.

Or build it yourself:

```sh
cd server
go build -o pgquire .   # Go 1.27+
./pgquire               # opens http://127.0.0.1:8432/?t=<token>
```

Or run it with Docker (`docker logs` shows the link; without `PGQUIRE_TOKEN` the token is new each start):

```sh
docker run -d -p 127.0.0.1:8432:8432 -v pgquire-config:/config -e PGQUIRE_TOKEN=<secret> ghcr.io/abiddotdev/pgquire
```

Keep the `127.0.0.1:` in `-p`: your saved connections are reachable through pgquire, so don't publish it to the network. `docker build -t pgquire .` builds the image from a clone.

To open straight into a database, give it with `-dsn` (or `PGQUIRE_DSN`, which keeps the password out of your shell history). pgquire connects before it starts, so a wrong address or password stops it with the reason, and the page opens in that database:

```sh
./pgquire -dsn postgres://me@db.example.com:5432/shop        # password from ~/.pgpass
./pgquire -dsn "postgres://me@db.example.com/shop?sslmode=require"
PGQUIRE_DSN=postgres://me:secret@db.example.com/shop ./pgquire
```

It's kept for this run only. To keep it, add it once through **Connect remote Postgres…** with **Remember on the server** ticked; from then on `-dsn` with the same string uses that remembered connection (its name, production tag and read-only setting). Each start opens it once; after that the page stays wherever you go. In Docker, `localhost` is the container itself: use `host.docker.internal` for a database on your machine.

Otherwise pick **Connect remote Postgres…** in the database switcher and paste a connection string (`postgres://user:password@host:5432/db`) or fill in separate fields. **Test** shows the server version and whether the connection is encrypted. If the server has several databases, you choose which ones to add; they're grouped under the server in the switcher, and **Other databases on this server…** adds more later.

**Credentials and access**
- Passwords stay with the server. Connections are kept in its memory until it stops, unless you tick **Remember on the server**. Remembered ones go in `pgquire/connections.json` in your OS config folder (`~/.config` on Linux, `~/Library/Application Support` on macOS, `%AppData%` on Windows; change it with `-config`), readable only by you, and never reach the browser.
- The server listens on 127.0.0.1 only and needs the token from the link it prints. A page opened another way (a bookmark, or after a restart) asks you to paste that link or token. The token is new each time pgquire starts; set a fixed one with `-token` or `PGQUIRE_TOKEN`.
- Behind a reverse proxy, or listening beyond 127.0.0.1, set `-domain` (or `PGQUIRE_DOMAIN`) to the host name you reach it by; requests for any other name are refused. When the proxy serves HTTPS and sends `X-Forwarded-Proto: https`, the sign-in cookie is marked Secure.

**Safety**
- Connections marked **Production** get a red marker, start read-only, and ask before any write once you make them writable.
- Read-only is a guard rail (SQL can switch it off), so pgquire checks what the login can really do. If a "read-only" connection's login can write, it says so and offers **Create a read-only login…**. That creates a login that can read everything and change nothing (`pg_read_all_data`, or per-schema grants before PostgreSQL 14), switches the connection to it, and keeps its generated password on the server. If your login can't create logins, **Copy SQL** gives your admin the statements. The login is named `pgquire_ro` by default; forgetting the connection offers to remove it again (`drop owned by …; drop role …`).
- Statements stop after 5 minutes (`-statement-timeout`), a transaction left idle for 2 minutes is rolled back, and sessions idle for 30 minutes are closed (`-idle-timeout`). Running queries can be stopped.
- The green dot next to the database name shows the connection is up (its tooltip says whether it's encrypted). It turns red with a **Reconnect** banner if the pgquire server stops.

**Data**
- Big tables show estimated row counts (`~`). Results in the page are capped at 50,000 rows (`-max-rows`); **CSV (all)** and table **Export CSV** stream every row.
- **Move data…** in the switcher gathers every way data goes in or out:
  - **Copy to a local database**: a sample or all rows, from the tables you pick by schema (with the tables they reference), checked against the room the browser allows. Use it to experiment safely.
  - **Download SQL dump**: read in one consistent snapshot, no `pg_dump` needed.
  - Session export, and importing files.
- The browser keeps in-browser databases per address, so ones made on the live page don't appear at `127.0.0.1:8432`, and the other way round. Move them with **Export session / Open session file**.

**Options**

Options are `-name value`, in any order; each can also be an environment variable (handy for Docker), and the command line wins. `./pgquire -h` lists them.

| Option | Variable | Default |
|---|---|---|
| `-listen` | `PGQUIRE_LISTEN` | `127.0.0.1:8432`; e.g. `-listen 127.0.0.1:9000` for another port |
| `-token` | `PGQUIRE_TOKEN` | new each start |
| `-domain` | `PGQUIRE_DOMAIN` | none |
| `-dsn` | `PGQUIRE_DSN` | none; a database to connect to and open at start |
| `-config` | `PGQUIRE_CONFIG` | `pgquire/connections.json` in your OS config folder |
| `-no-open` | `PGQUIRE_NO_OPEN` | opens a browser |
| `-max-rows` | `PGQUIRE_MAX_ROWS` | `50000` |
| `-statement-timeout` | `PGQUIRE_STATEMENT_TIMEOUT` | `5m` |
| `-idle-timeout` | `PGQUIRE_IDLE_TIMEOUT` | `30m` |

In Docker, change the port with both: `-e PGQUIRE_LISTEN=0.0.0.0:9000 -p 127.0.0.1:9000:9000`.

Tests (in `server/`): `PGQUIRE_TEST_DSN=postgres://… go test ./...`

Browser tests (in `e2e/`, needs Go, Node and Docker): `npm ci && npx playwright install chromium && npm test`. They build the server, start a throwaway Postgres container and run the page in Chromium: connecting, queries, editing tables, read-only logins, `-dsn`, and PGlite (from its CDN) in the app and the Pages copy. To use a Postgres of your own instead of the container, set `PGQUIRE_E2E_PG=postgres://user:pass@host/postgres` (it creates the `e2e_shop` database and the `pgquire_e2e_ro` login, and drops them first if they're there). The remote tests build on each other, so run `tests/remote.spec.js` whole rather than one test with `-g`.

### Editing the page

Edit `server/index-remote.html`: it's the page the server embeds. `index.html` at the root (the GitHub Pages copy, PGlite only) is generated from it by `go generate` in `server/`. Generation:
- drops everything marked remote-only: `/* @remote */`, `// @remote` or `<!-- @remote -->` at the end of a line, `/* @remote { */ … /* } @remote */` blocks, and inline `/*remote:*/ … /*:remote*/` in JS and CSS;
- switches on `// @pages:` lines.

A test fails when `index.html` is out of date.

### Releasing

Bump `APP_VERSION` in `server/index-remote.html` and `Version` in `server/main.go` together (a test checks that they match), run `go generate`, then push a tag like `v1.1`. The release workflow builds Linux, macOS and Windows binaries (amd64 and arm64) with GoReleaser (`.goreleaser.yaml`), attaches them to a GitHub release, and publishes the Docker image to `ghcr.io/abiddotdev/pgquire` (amd64 and arm64). `goreleaser release --snapshot --clean` tries the build locally.

## License

MIT
