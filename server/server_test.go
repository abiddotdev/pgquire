package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abiddotdev/pgquire/server/internal/pages"
)

type client struct {
	t    *testing.T
	base string
	hc   *http.Client
	mu   sync.Mutex  // do is called from several goroutines in the cancel tests
	last http.Header // the last response's headers
}

// newTestServer starts pgquire on a loopback port. Pass signIn=false to get an anonymous client.
func newTestServer(t *testing.T, maxRows int, signIn bool) (*client, *server) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	profiles, _ := loadProfiles(filepath.Join(t.TempDir(), "connections.json"))
	srv := newServer(serverConfig{token: "tok", maxRows: maxRows, stmtTO: time.Minute, profiles: profiles, port: port})
	ts.Config.Handler = srv
	ts.Start()
	t.Cleanup(func() { srv.sessions.closeAll(); ts.Close() })
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL, hc: &http.Client{Jar: jar}}
	if signIn {
		resp, err := c.hc.Get(ts.URL + "/?t=tok")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	return c, srv
}

func (c *client) do(method, path string, body any, out any) int {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.base)
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	c.mu.Lock()
	c.last = resp.Header
	c.mu.Unlock()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s: %v (%s)", method, path, err, b)
		}
	}
	return resp.StatusCode
}

/* ---------- guards (no database needed) ---------- */

func TestTokenLinkSetsCookieAndStripsToken(t *testing.T) {
	c, _ := newTestServer(t, 100, false)
	c.hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.hc.Get(c.base + "/?t=tok&x=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?x=1" {
		t.Fatalf("got %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var h struct{ Authed bool }
	c.do("GET", "/api/health", nil, &h)
	if !h.Authed {
		t.Fatal("cookie not accepted after the token link")
	}
}

func TestAPIRequiresToken(t *testing.T) {
	c, _ := newTestServer(t, 100, false)
	var h struct{ Authed bool }
	if c.do("GET", "/api/health", nil, &h) != 200 || h.Authed {
		t.Fatal("health should answer, unauthenticated")
	}
	if code := c.do("GET", "/api/profiles", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("profiles without token: %d", code)
	}
	resp, _ := c.hc.Get(c.base + "/?t=wrong")
	resp.Body.Close()
	if code := c.do("GET", "/api/profiles", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong token accepted: %d", code)
	}
}

func TestPastedTokenSignsIn(t *testing.T) {
	c, _ := newTestServer(t, 100, false)
	if code := c.do("POST", "/api/signin", map[string]string{"token": "nope"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	if code := c.do("GET", "/api/profiles", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("signed in after a wrong token: %d", code)
	}
	if code := c.do("POST", "/api/signin", map[string]string{"token": " tok\n"}, nil); code != http.StatusOK {
		t.Fatalf("right token: %d", code)
	}
	if code := c.do("GET", "/api/profiles", nil, nil); code != http.StatusOK {
		t.Fatalf("not signed in after the right token: %d", code)
	}
}

func TestDomainAndSecureCookie(t *testing.T) {
	c, srv := newTestServer(t, 100, false)
	srv.cfg.domain = "pgquire.example.com"
	c.hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	get := func(host, proto string) *http.Response {
		req, _ := http.NewRequest("GET", c.base+"/?t=tok", nil)
		req.Host = host
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := get("evil.example", ""); r.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host with -domain: %d", r.StatusCode)
	}
	r := get("PGQUIRE.example.com", "https")
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("domain Host: %d", r.StatusCode)
	}
	if ck := r.Cookies(); len(ck) != 1 || !ck[0].Secure {
		t.Fatalf("cookie over https should be Secure: %v", ck)
	}
	if ck := get("pgquire.example.com:8443", "").Cookies(); len(ck) != 1 || ck[0].Secure {
		t.Fatalf("cookie over http should not be Secure: %v", ck)
	}
	req, _ := http.NewRequest("POST", c.base+"/api/signin", strings.NewReader(`{"token":"tok"}`))
	req.Host = "pgquire.example.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://pgquire.example.com")
	resp, _ := c.hc.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("https Origin behind a proxy: %d", resp.StatusCode)
	}
}

func TestRejectsForeignHostAndOrigin(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	req, _ := http.NewRequest("GET", c.base+"/api/health", nil)
	req.Host = "evil.example:" + strings.Split(c.base, ":")[2] // DNS rebinding
	resp, _ := c.hc.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", c.base+"/api/sessions", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ = c.hc.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Origin: %d", resp.StatusCode)
	}
}

func TestServesPage(t *testing.T) {
	c, _ := newTestServer(t, 100, false)
	resp, err := c.hc.Get(c.base + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(b, []byte("<title>pgquire")) {
		t.Fatal("index.html not served")
	}
}

func TestEncodeParams(t *testing.T) {
	raw := []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`"x"`), json.RawMessage(`true`), json.RawMessage(`12.50`), json.RawMessage(`{"a":1}`)}
	got, err := encodeParams(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"<nil>", "x", "t", "12.50", `{"a":1}`}
	for i, g := range got {
		s := "<nil>"
		if g != nil {
			s = string(g)
		}
		if s != want[i] {
			t.Errorf("param %d = %q, want %q", i, s, want[i])
		}
	}
}

func TestProfilesSavedPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "connections.json")
	s, _ := loadProfiles(path)
	if err := s.put(&Profile{Name: "a", DSN: "postgres://u:pw@h/db", saved: true}); err != nil {
		t.Fatal(err)
	}
	s.put(&Profile{Name: "adhoc", DSN: "postgres://h/x"}) // not saved
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	again, _ := loadProfiles(path)
	if again.count() != 1 || again.get("a") == nil {
		t.Fatalf("reloaded %d profiles", again.count())
	}
	if pub := again.all()[0]; pub.Host != "h" || pub.User != "u" || pub.Database != "db" {
		t.Fatalf("public view: %+v", pub)
	}
}

/* ---------- against a real Postgres: PGQUIRE_TEST_DSN=postgres://… go test ./... ---------- */

type apiResult struct {
	Fields []struct {
		Name       string
		DataTypeID uint32
	}
	Rows         [][]*string
	AffectedRows int64
	Truncated    bool
	TotalRows    int64
}
type apiErr struct {
	Error map[string]any
}

func remote(t *testing.T, maxRows int, readOnly bool) (*client, string) {
	t.Helper()
	dsn := os.Getenv("PGQUIRE_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGQUIRE_TEST_DSN to run against a real Postgres")
	}
	c, _ := newTestServer(t, maxRows, true)
	if code := c.do("POST", "/api/profiles", map[string]any{"name": "test", "dsn": dsn, "readOnly": readOnly}, nil); code != 200 {
		t.Fatalf("add profile: %d", code)
	}
	var s struct{ ID string }
	if code := c.do("POST", "/api/sessions", map[string]any{"profile": "test"}, &s); code != 200 {
		t.Fatalf("open session: %d", code)
	}
	return c, "/api/sessions/" + s.ID
}

// -dsn: connected before serving, kept for this run, and opened by a signed-in page (once per start).
func TestConnectAtStart(t *testing.T) {
	dsn := os.Getenv("PGQUIRE_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGQUIRE_TEST_DSN to run against a real Postgres")
	}
	c, srv := newTestServer(t, 100, true)
	if _, err := srv.connectAtStart("postgres://postgres:nope@127.0.0.1:1/x?connect_timeout=2"); err == nil || srv.cfg.profiles.count() != 0 || srv.startup != nil {
		t.Fatalf("a bad -dsn: err %v, %d connections", err, srv.cfg.profiles.count())
	}
	line, err := srv.connectAtStart(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		Open struct {
			ID      string
			Profile struct {
				Name  string
				Saved bool
			}
		}
	}
	c.do("GET", "/api/health", nil, &h)
	name := h.Open.Profile.Name
	if h.Open.ID == "" || name == "" || h.Open.Profile.Saved || !strings.HasPrefix(line, name+" (PostgreSQL ") {
		t.Fatalf("health open = %+v, line %q", h.Open, line)
	}
	var s struct{ ID string }
	if code := c.do("POST", "/api/sessions", map[string]any{"profile": name}, &s); code != 200 {
		t.Fatalf("open a session on it: %d", code)
	}
	// Not for a page that hasn't signed in.
	anon := &client{t: t, base: c.base, hc: &http.Client{}}
	var raw json.RawMessage
	anon.do("GET", "/api/health", nil, &raw)
	if bytes.Contains(raw, []byte(`"open"`)) {
		t.Fatalf("anonymous health names the connection: %s", raw)
	}
	// The same string again (a saved one, say) is the same connection, not a second.
	first := srv.startup.id
	if _, err := srv.connectAtStart(dsn); err != nil || srv.cfg.profiles.count() != 1 || srv.startup.profile != name || srv.startup.id == first {
		t.Fatalf("again: err %v, %d connections, startup %+v", err, srv.cfg.profiles.count(), srv.startup)
	}
}

func TestFreeName(t *testing.T) {
	ps, _ := loadProfiles(filepath.Join(t.TempDir(), "connections.json"))
	ps.put(&Profile{Name: "shop on db", DSN: "a"})
	ps.put(&Profile{Name: "shop on db (2)", DSN: "b"})
	if n := ps.freeName("shop on db"); n != "shop on db (3)" {
		t.Fatalf("freeName = %q", n)
	}
	if n := ps.freeName("other"); n != "other" {
		t.Fatalf("freeName = %q", n)
	}
}

func TestRemoteProfileHidesDSN(t *testing.T) {
	c, _ := remote(t, 100, false)
	var buf json.RawMessage
	c.do("GET", "/api/profiles", nil, &buf)
	if bytes.Contains(buf, []byte("secret")) || bytes.Contains(buf, []byte("dsn")) {
		t.Fatalf("profile list leaks the connection string: %s", buf)
	}
	var e apiErr
	if code := c.do("POST", "/api/profiles", map[string]any{"name": "bad", "dsn": "postgres://postgres:nope@127.0.0.1:1/x?connect_timeout=2"}, &e); code != http.StatusBadGateway {
		t.Fatalf("unreachable server accepted: %d %v", code, e)
	}
}

func TestRemoteExecQueryAndSessionState(t *testing.T) {
	c, sp := remote(t, 100, false)
	var rs []apiResult
	code := c.do("POST", sp+"/exec", map[string]any{"sql": `
		create temp table t(a int, b text, c jsonb, d bool);
		insert into t values (1, 'one', '{"k":1}', true), (2, null, null, false);
		select * from t order by a`}, &rs)
	if code != 200 || len(rs) != 3 {
		t.Fatalf("exec: %d, %d results", code, len(rs))
	}
	if rs[1].AffectedRows != 2 {
		t.Errorf("insert affected %d", rs[1].AffectedRows)
	}
	sel := rs[2]
	if len(sel.Fields) != 4 || sel.Fields[0].DataTypeID != 23 || sel.Fields[2].DataTypeID != 3802 {
		t.Fatalf("fields: %+v", sel.Fields)
	}
	if *sel.Rows[0][1] != "one" || sel.Rows[1][1] != nil || *sel.Rows[0][3] != "t" {
		t.Fatalf("rows: %v", sel.Rows)
	}
	// The temp table is still there on the next request: same connection.
	var r apiResult
	if code := c.do("POST", sp+"/query", map[string]any{"sql": "select count(*) + $1::int as n from t", "params": []any{"10"}}, &r); code != 200 || *r.Rows[0][0] != "12" {
		t.Fatalf("query: %d %v", code, r.Rows)
	}
}

func TestRemoteOtherDatabases(t *testing.T) {
	c, sp := remote(t, 100, false)
	c.do("POST", sp+"/exec", map[string]any{"sql": "drop database if exists pgquire_t2"}, nil)
	if code := c.do("POST", sp+"/exec", map[string]any{"sql": "create database pgquire_t2"}, nil); code != 200 {
		t.Fatalf("create database: %d", code)
	}
	defer c.do("POST", sp+"/exec", map[string]any{"sql": "drop database if exists pgquire_t2 with (force)"}, nil)
	var list struct {
		Databases []string
		Default   string
	}
	if code := c.do("GET", "/api/profiles/test/databases", nil, &list); code != 200 {
		t.Fatalf("list: %d", code)
	}
	if !slices.Contains(list.Databases, "pgquire_t2") || !slices.Contains(list.Databases, "postgres") || slices.Contains(list.Databases, "template0") || list.Default != "postgres" {
		t.Fatalf("databases: %+v", list)
	}
	var s struct{ ID, Database string }
	if code := c.do("POST", "/api/sessions", map[string]any{"profile": "test", "database": "pgquire_t2"}, &s); code != 200 || s.Database != "pgquire_t2" {
		t.Fatalf("open other database: %d %+v", code, s)
	}
	var r apiResult
	c.do("POST", "/api/sessions/"+s.ID+"/query", map[string]any{"sql": "select current_database()"}, &r)
	if *r.Rows[0][0] != "pgquire_t2" {
		t.Fatalf("connected to %s", *r.Rows[0][0])
	}
	c.do("POST", "/api/sessions/"+s.ID+"/close", nil, nil)
	var e apiErr
	if code := c.do("POST", "/api/sessions", map[string]any{"profile": "test", "database": "no_such_db"}, &e); code != http.StatusBadGateway {
		t.Fatalf("missing database: %d %v", code, e)
	}
}

func TestRemoteErrorsCarryPostgresFields(t *testing.T) {
	c, sp := remote(t, 100, false)
	var e apiErr
	if code := c.do("POST", sp+"/query", map[string]any{"sql": "select * from no_such_table"}, &e); code != 400 {
		t.Fatalf("status %d", code)
	}
	if e.Error["code"] != "42P01" || e.Error["position"] != "15" {
		t.Fatalf("error: %v", e.Error)
	}
}

func TestRemoteTransactionSpansRequests(t *testing.T) {
	c, sp := remote(t, 100, false)
	c.do("POST", sp+"/exec", map[string]any{"sql": "create temp table u(a int primary key)"}, nil)
	c.do("POST", sp+"/exec", map[string]any{"sql": "begin"}, nil)
	c.do("POST", sp+"/query", map[string]any{"sql": "insert into u values ($1)", "params": []any{"1"}}, nil)
	if tx := c.last.Get("X-Pgquire-Tx"); tx != "T" {
		t.Fatalf("status inside BEGIN: %q, want T", tx)
	}
	var e apiErr
	if code := c.do("POST", sp+"/query", map[string]any{"sql": "insert into u values ($1)", "params": []any{"1"}}, &e); code != 400 || e.Error["code"] != "23505" {
		t.Fatalf("duplicate: %d %v", code, e.Error)
	}
	if tx := c.last.Get("X-Pgquire-Tx"); tx != "E" {
		t.Fatalf("status after a failed statement in BEGIN: %q, want E", tx)
	}
	c.do("POST", sp+"/exec", map[string]any{"sql": "rollback"}, nil)
	if tx := c.last.Get("X-Pgquire-Tx"); tx != "I" {
		t.Fatalf("status after rollback: %q, want I", tx)
	}
	var r apiResult
	c.do("POST", sp+"/query", map[string]any{"sql": "select count(*) from u"}, &r)
	if *r.Rows[0][0] != "0" {
		t.Fatalf("rows left after rollback: %s", *r.Rows[0][0])
	}
}

func TestRemoteMaxRows(t *testing.T) {
	c, sp := remote(t, 5, false)
	var r apiResult
	c.do("POST", sp+"/query", map[string]any{"sql": "select generate_series(1, 12)"}, &r)
	if len(r.Rows) != 5 || !r.Truncated || r.TotalRows != 12 {
		t.Fatalf("got %d rows, truncated=%v total=%d", len(r.Rows), r.Truncated, r.TotalRows)
	}
}

func TestRemoteCancelKeepsSession(t *testing.T) {
	c, sp := remote(t, 100, false)
	done := make(chan apiErr, 1)
	go func() {
		var e apiErr
		c.do("POST", sp+"/query", map[string]any{"sql": "select pg_sleep(30)"}, &e)
		done <- e
	}()
	time.Sleep(500 * time.Millisecond)
	var cr struct{ Cancelled bool }
	c.do("POST", sp+"/cancel", nil, &cr)
	select {
	case e := <-done:
		if e.Error["code"] != "57014" {
			t.Fatalf("cancelled query: %v", e.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel didn't stop the query")
	}
	var r apiResult
	if code := c.do("POST", sp+"/query", map[string]any{"sql": "select 1"}, &r); code != 200 {
		t.Fatalf("session unusable after cancel: %d", code)
	}
}

func TestRemoteCancelOnlyHitsItsRequest(t *testing.T) {
	c, sp := remote(t, 100, false)
	done := make(chan apiErr, 1)
	go func() {
		var e apiErr
		c.do("POST", sp+"/query", map[string]any{"sql": "select pg_sleep(1)", "req": 7}, &e)
		done <- e
	}()
	time.Sleep(300 * time.Millisecond)
	var cr struct{ Cancelled bool }
	c.do("POST", sp+"/cancel?req=6", nil, &cr) // a Stop meant for an earlier request
	if cr.Cancelled {
		t.Fatal("cancel for request 6 stopped request 7")
	}
	if e := <-done; e.Error != nil {
		t.Fatalf("request 7: %v", e.Error)
	}
}

func TestRemoteReadOnly(t *testing.T) {
	c, sp := remote(t, 100, true)
	var e apiErr
	if code := c.do("POST", sp+"/exec", map[string]any{"sql": "create table ro_test(a int)"}, &e); code != 400 || e.Error["code"] != "25006" {
		t.Fatalf("write on read-only session: %d %v", code, e.Error)
	}
}

func TestRemoteClosedSessionIsGone(t *testing.T) {
	c, sp := remote(t, 100, false)
	c.do("POST", sp+"/close", nil, nil)
	var e apiErr
	if code := c.do("POST", sp+"/query", map[string]any{"sql": "select 1"}, &e); code != http.StatusGone || e.Error["code"] != "session_closed" {
		t.Fatalf("closed session: %d %v", code, e.Error)
	}
}

func TestVersionMatchesPage(t *testing.T) {
	m := regexp.MustCompile(`const APP_VERSION = '([^']+)'`).FindSubmatch(indexHTML)
	if m == nil {
		t.Fatal("APP_VERSION not found in index-remote.html")
	}
	if string(m[1]) != Version {
		t.Fatalf("index-remote.html APP_VERSION %q, server Version %q: bump both", m[1], Version)
	}
}

// Options come from PGQUIRE_* variables unless given on the command line; a bad value is an error.
func TestOptionsFromEnv(t *testing.T) {
	parse := func(env map[string]string, args ...string) (*flag.FlagSet, error) {
		fs := flag.NewFlagSet("pgquire", flag.ContinueOnError)
		fs.String("listen", "127.0.0.1:8432", "")
		fs.Bool("no-open", false, "")
		fs.String("config", "", "")
		fs.Int("max-rows", 50000, "")
		fs.Duration("statement-timeout", 5*time.Minute, "")
		fs.Duration("idle-timeout", 30*time.Minute, "")
		fs.String("token", "", "")
		fs.String("domain", "", "")
		fs.String("dsn", "", "")
		describeEnv(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return fs, applyEnv(fs, func(k string) string { return env[k] })
	}
	get := func(fs *flag.FlagSet, name string) string { return fs.Lookup(name).Value.String() }

	fs, err := parse(map[string]string{
		"PGQUIRE_LISTEN": "0.0.0.0:9000", "PGQUIRE_NO_OPEN": "1", "PGQUIRE_MAX_ROWS": "10",
		"PGQUIRE_IDLE_TIMEOUT": "1h", "PGQUIRE_TOKEN": "env-token",
	}, "-token", "flag-token")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"listen": "0.0.0.0:9000", "no-open": "true", "max-rows": "10", "idle-timeout": "1h0m0s",
		"token": "flag-token", "statement-timeout": "5m0s", "config": "",
	} {
		if got := get(fs, name); got != want {
			t.Errorf("-%s = %q, want %q", name, got, want)
		}
	}
	if u := fs.Lookup("listen").Usage; !strings.Contains(u, "PGQUIRE_LISTEN") {
		t.Errorf("-h doesn't name the variable: %q", u)
	}

	if _, err := parse(map[string]string{"PGQUIRE_MAX_ROWS": "lots"}); err == nil || !strings.Contains(err.Error(), "PGQUIRE_MAX_ROWS") {
		t.Fatalf("bad value: err = %v", err)
	}
}

// index.html (GitHub Pages) is generated from index-remote.html; a stale copy fails here and in CI.
func TestPagesCopyUpToDate(t *testing.T) {
	gen, err := os.ReadFile("../index.html")
	if err != nil {
		t.Fatal(err)
	}
	same, err := pages.Same(indexHTML, gen)
	if err != nil {
		t.Fatalf("index-remote.html: %v", err)
	}
	if !same {
		t.Fatal("index.html is out of date with index-remote.html: run `go generate`")
	}
}

func TestRemoteTestConnectionAndCSVExport(t *testing.T) {
	c, sp := remote(t, 5, false) // max-rows 5: the export must still stream everything
	var probe struct {
		ServerVersion string
		TLS           bool
		Database      string
	}
	if code := c.do("POST", "/api/profiles/test", map[string]any{"dsn": os.Getenv("PGQUIRE_TEST_DSN")}, &probe); code != 200 || probe.ServerVersion == "" || probe.Database != "postgres" {
		t.Fatalf("test connection: %d %+v", code, probe)
	}
	var bad apiErr
	if code := c.do("POST", sp+"/exports", map[string]any{"sql": "select * from no_such_table"}, &bad); code != 400 || bad.Error["code"] != "42P01" {
		t.Fatalf("bad export not caught up front: %d %v", code, bad.Error)
	}
	var prep struct{ URL string }
	if code := c.do("POST", sp+"/exports", map[string]any{"sql": "select g, 'x,\"y' as s from generate_series(1, 20) g;", "filename": "../evil name"}, &prep); code != 200 {
		t.Fatalf("prepare: %d", code)
	}
	resp, err := c.hc.Get(c.base + prep.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if resp.StatusCode != 200 || len(lines) != 21 || lines[0] != "g,s" || lines[1] != `1,"x,""y"` {
		t.Fatalf("csv: %d, %d lines, first %q %q", resp.StatusCode, len(lines), lines[0], lines[1])
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="evil name.csv"` {
		t.Fatalf("filename not sanitised: %s", cd)
	}
	again, _ := c.hc.Get(c.base + prep.URL)
	again.Body.Close()
	if again.StatusCode != http.StatusGone {
		t.Fatalf("export link reusable: %d", again.StatusCode)
	}
	// The export runs in a read-only transaction, even for a query that would write.
	c.do("POST", sp+"/exec", map[string]any{"sql": "drop table if exists pgq_export_ro; create table pgq_export_ro(a int)"}, nil)
	defer c.do("POST", sp+"/exec", map[string]any{"sql": "drop table if exists pgq_export_ro"}, nil)
	var w struct{ URL string }
	if code := c.do("POST", sp+"/exports", map[string]any{"sql": "insert into pgq_export_ro values (1) returning a"}, &w); code != 200 {
		t.Fatalf("prepare write: %d", code)
	}
	resp, _ = c.hc.Get(c.base + w.URL)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var n apiResult
	c.do("POST", sp+"/query", map[string]any{"sql": "select count(*) from pgq_export_ro"}, &n)
	if !strings.Contains(string(body), "read-only transaction") || *n.Rows[0][0] != "0" {
		t.Fatalf("export wrote data: %q, rows %s", body, *n.Rows[0][0])
	}
}

func TestRemoteReadOnlyLogin(t *testing.T) {
	dsn := os.Getenv("PGQUIRE_TEST_DSN")
	c, sp := remote(t, 100, true) // marked read-only, but logs in as the superuser
	admin, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	role := fmt.Sprintf("pgq_ro_%d", time.Now().UnixNano()%1e9)
	admin.Exec(context.Background(), "drop table if exists pgq_ro_data; create table pgq_ro_data(a int); insert into pgq_ro_data values (1)")
	defer func() {
		admin.Exec(context.Background(), "drop table if exists pgq_ro_data")
		admin.Exec(context.Background(), fmt.Sprintf("drop owned by %s; drop role if exists %s", role, role))
	}()

	// 1. the check: a superuser behind a read-only flag
	var open struct {
		Role roleInfo
	}
	c.do("POST", "/api/sessions", map[string]any{"profile": "test"}, &open)
	if !open.Role.Superuser || !open.Role.Writable || !open.Role.CreateRole {
		t.Fatalf("role check: %+v", open.Role)
	}

	// 2a. dry run: the SQL, with a placeholder instead of a password
	var plan struct {
		Create, GrantAll []string
		CanCreate        bool
	}
	c.do("POST", "/api/profiles/test/readonly-role", map[string]any{"role": role, "dryRun": true}, &plan)
	if !plan.CanCreate || !strings.Contains(plan.Create[0], "'<choose a password>'") || len(plan.GrantAll) != 1 {
		t.Fatalf("dry run: %+v", plan)
	}
	var bad apiErr
	if code := c.do("POST", "/api/profiles/test/readonly-role", map[string]any{"role": "x; drop table y"}, &bad); code != 400 {
		t.Fatalf("bad name accepted: %d", code)
	}

	// 2b. create it and switch the connection over
	var made struct {
		Grants   string
		Switched bool
	}
	if code := c.do("POST", "/api/profiles/test/readonly-role", map[string]any{"role": role, "switch": true}, &made); code != 200 || made.Grants != "pg_read_all_data" || !made.Switched {
		t.Fatalf("create: %d %+v", code, made)
	}
	var e apiErr
	if code := c.do("POST", sp+"/query", map[string]any{"sql": "select 1"}, &e); code != http.StatusGone {
		t.Fatalf("old session still open after switching logins: %d", code)
	}
	var now struct {
		ID   string
		Role roleInfo
	}
	c.do("POST", "/api/sessions", map[string]any{"profile": "test"}, &now)
	if now.Role.User != role || now.Role.Writable || now.Role.Superuser {
		t.Fatalf("new login: %+v", now.Role)
	}
	np := "/api/sessions/" + now.ID
	var rd apiResult
	if code := c.do("POST", np+"/query", map[string]any{"sql": "select a from pgq_ro_data"}, &rd); code != 200 || *rd.Rows[0][0] != "1" {
		t.Fatalf("read as the new login: %d", code)
	}
	// The guard-rail escape from before now fails on privileges.
	var esc apiErr
	code := c.do("POST", np+"/exec", map[string]any{"sql": "set transaction_read_only = off; set default_transaction_read_only = off; commit; insert into pgq_ro_data values (2)"}, &esc)
	var n int
	admin.QueryRow(context.Background(), "select count(*) from pgq_ro_data").Scan(&n)
	if code != 400 || esc.Error["code"] != "42501" || n != 1 {
		t.Fatalf("escape: %d %v, rows %d", code, esc.Error, n)
	}
	var stored string
	admin.QueryRow(context.Background(), "select rolpassword from pg_authid where rolname = $1", role).Scan(&stored)
	if !strings.HasPrefix(stored, "SCRAM-SHA-256$4096:") {
		t.Fatalf("password not stored as SCRAM: %q", stored)
	}

	// Without the right to create logins: refused, the page shows the SQL instead.
	var deny apiErr
	if code := c.do("POST", "/api/profiles/test/readonly-role", map[string]any{"role": role + "_2"}, &deny); code != http.StatusForbidden {
		t.Fatalf("read-only login created another login: %d %v", code, deny.Error)
	}

	// The connection remembers the login it created, and forgetting it can remove the login —
	// using the connection's original login, since the read-only one can't drop itself.
	var list struct{ Profiles []publicProfile }
	c.do("GET", "/api/profiles", nil, &list)
	if len(list.Profiles) != 1 || list.Profiles[0].CreatedRole != role || list.Profiles[0].User != role {
		t.Fatalf("profile after switching: %+v", list.Profiles)
	}
	if code := c.do("DELETE", "/api/profiles/test?dropRole=1", nil, nil); code != 200 {
		t.Fatalf("forget with dropRole: %d", code)
	}
	var left bool
	admin.QueryRow(context.Background(), "select exists (select 1 from pg_roles where rolname = $1)", role).Scan(&left)
	c.do("GET", "/api/profiles", nil, &list)
	if left || len(list.Profiles) != 0 {
		t.Fatalf("after forgetting: role still there=%v, profiles %d", left, len(list.Profiles))
	}
}

func TestReadOnlyRolePlanListsNeverNull(t *testing.T) {
	// Before PostgreSQL 14 there's no grant-all, and with no schemas no per-schema grants: both must
	// still go out as [] — the page spreads them.
	_, all, per := readOnlyRolePlan("ro", "db", "'x'", nil, false)
	b, _ := json.Marshal(map[string]any{"grantAll": all, "perSchema": per})
	if string(b) != `{"grantAll":[],"perSchema":[]}` {
		t.Fatalf("got %s", b)
	}
}
