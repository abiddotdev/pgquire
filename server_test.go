package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type client struct {
	t    *testing.T
	base string
	hc   *http.Client
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
	var e apiErr
	if code := c.do("POST", sp+"/query", map[string]any{"sql": "insert into u values ($1)", "params": []any{"1"}}, &e); code != 400 || e.Error["code"] != "23505" {
		t.Fatalf("duplicate: %d %v", code, e.Error)
	}
	c.do("POST", sp+"/exec", map[string]any{"sql": "rollback"}, nil)
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
