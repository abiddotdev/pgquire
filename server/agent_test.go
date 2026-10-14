package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* ---------- agent access (no database needed: fake tabs answer) ---------- */

type sseEvent struct {
	name string
	data map[string]any
}

type fakeTab struct {
	t      *testing.T
	c      *client
	id     string
	events chan sseEvent
}

// openTab connects a tab's stream, as the page does, and reads its hello.
func openTab(t *testing.T, c *client) *fakeTab {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/api/agent/stream", nil)
	resp, err := c.hc.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream: %v %v", err, resp)
	}
	ft := &fakeTab{t: t, c: c, events: make(chan sseEvent, 16)}
	t.Cleanup(stop)
	go func() {
		defer resp.Body.Close()
		defer close(ft.events)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = line[7:]
			case strings.HasPrefix(line, "data: "):
				json.Unmarshal([]byte(line[6:]), &ev.data)
			case line == "" && ev.name != "":
				ft.events <- ev
				ev = sseEvent{}
			}
		}
	}()
	ft.id = ft.next("hello").data["tab"].(string)
	return ft
}

// report says what the tab has open and what its browser shares.
func (ft *fakeTab) report(open string, front bool, shared ...agentDB) {
	ft.t.Helper()
	if code := ft.c.do("POST", "/api/agent/state", map[string]any{"tab": ft.id, "open": open, "shared": shared, "front": front}, nil); code != 200 {
		ft.t.Fatalf("state: %d", code)
	}
}

// next waits for the next event named name, skipping others.
func (ft *fakeTab) next(name string) sseEvent {
	ft.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-ft.events:
			if !ok {
				ft.t.Fatalf("stream ended waiting for %q", name)
			}
			if ev.name == name {
				return ev
			}
		case <-deadline:
			ft.t.Fatalf("no %q event", name)
		}
	}
}

// none checks that no event named name arrives for a moment.
func (ft *fakeTab) none(name string) {
	ft.t.Helper()
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case ev := <-ft.events:
			if ev.name == name {
				ft.t.Fatalf("unexpected %q event: %v", name, ev.data)
			}
		case <-deadline:
			return
		}
	}
}

func (ft *fakeTab) reply(id any, result any, hold bool) int {
	return ft.c.do("POST", "/api/agent/reply", map[string]any{"tab": ft.id, "id": id, "hold": hold, "result": result}, nil)
}

// agentTool calls a tool the way `pgquire mcp` does: agent token, no Origin, no cookie.
func agentTool(t *testing.T, c *client, token, tool string, args any) (int, string, bool) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"tool": tool, "args": args, "client": "claude-code"})
	req, _ := http.NewRequest("POST", c.base+"/api/agent/call", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Text    string `json:"text"`
		IsError bool   `json:"isError"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Text, out.IsError
}

type toolResult struct {
	text  string
	isErr bool
}

func agentToolAsync(t *testing.T, c *client, tool string, args any) chan toolResult {
	ch := make(chan toolResult, 1)
	go func() { _, text, isErr := agentTool(t, c, "agenttok", tool, args); ch <- toolResult{text, isErr} }()
	return ch
}

func wait(t *testing.T, ch chan toolResult) toolResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
	return toolResult{}
}

var (
	shop  = agentDB{ID: "db1", Name: "shop", Engine: "PGlite in the browser"}
	prod  = agentDB{ID: "r:prod", Name: "orders", Engine: "PostgreSQL on db.example.com", Production: true}
	notes = agentDB{ID: "db2", Name: "notes", Engine: "PGlite in the browser"}
)

func TestAgentTokenGuards(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	if code, _, _ := agentTool(t, c, "", "list_databases", nil); code != 403 { // no token: a stray request, refused before anything
		t.Fatalf("no token: %d", code)
	}
	if code, _, _ := agentTool(t, c, "tok", "list_databases", nil); code != 401 { // the page's token isn't the agent's
		t.Fatalf("page token: %d", code)
	}
	if code := c.do("POST", "/api/agent/call", map[string]any{"tool": "list_databases"}, nil); code != 401 { // nor is its cookie
		t.Fatalf("cookie: %d", code)
	}
	// The agent token opens nothing else, and a browser (it sends Origin) can't use it cross-site.
	req, _ := http.NewRequest("GET", c.base+"/api/profiles", nil)
	req.Header.Set("Authorization", "Bearer agenttok")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("agent token on the page's API: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", c.base+"/api/agent/call", strings.NewReader(`{"tool":"list_databases"}`))
	req.Header.Set("Authorization", "Bearer agenttok")
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 403 {
		t.Fatalf("cross-origin: %d", resp.StatusCode)
	}
	if code, _, _ := agentTool(t, c, "agenttok", "list_databases", nil); code != 200 {
		t.Fatalf("agent token: %d", code)
	}
}

func TestAgentListDatabases(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	if _, text, _ := agentTool(t, c, "agenttok", "list_databases", nil); text != noneShared {
		t.Fatalf("no tabs: %q", text)
	}
	if _, text, isErr := agentTool(t, c, "agenttok", "run_sql", map[string]any{"purpose": "test", "sql": "select 1"}); !isErr || text != "ERROR: "+noneShared {
		t.Fatalf("no tabs, run_sql: %q", text)
	}
	a := openTab(t, c)
	if _, text, _ := agentTool(t, c, "agenttok", "list_databases", nil); text != noneShared {
		t.Fatalf("nothing shared: %q", text)
	}
	b := openTab(t, c)
	a.report("db1", false, shop, notes, prod)
	b.report("r:prod", true, shop, notes, prod)
	_, text, _ := agentTool(t, c, "agenttok", "list_databases", nil)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 4 || !strings.Contains(lines[1], "“orders” — PostgreSQL on db.example.com, PRODUCTION · id r:prod · open — the user is looking at it") ||
		!strings.Contains(lines[2], "“shop”") || !strings.HasSuffix(lines[2], "· open") || !strings.Contains(lines[3], "“notes”") || !strings.Contains(lines[3], "not open") {
		t.Fatalf("list:\n%s", text)
	}
	a.report("db1", true, shop, notes, prod) // the user went to the other tab
	if _, text, _ := agentTool(t, c, "agenttok", "list_databases", nil); !strings.Contains(text, "“shop” — PGlite in the browser · id db1 · open — the user is looking at it") {
		t.Fatalf("front tab moved:\n%s", text)
	}
}

func TestAgentCallGoesToTheTabWithTheDatabase(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	a, b := openTab(t, c), openTab(t, c)
	a.report("db1", false, shop, notes)
	b.report("db2", true, shop, notes)
	ch := agentToolAsync(t, c, "run_sql", map[string]any{"database": "shop", "purpose": "test", "sql": "select 1"}) // by name
	ev := a.next("call")
	call := ev.data["call"].(map[string]any)
	if call["tool"] != "run_sql" || call["database"] != "db1" || call["client"] != "claude-code" || call["args"].(map[string]any)["sql"] != "select 1" {
		t.Fatalf("call: %v", call)
	}
	b.none("call")
	if code := b.reply(ev.data["id"], map[string]any{"text": "forged"}, false); code != 410 { // only the tab it went to answers
		t.Fatalf("reply from another tab: %d", code)
	}
	a.reply(ev.data["id"], map[string]any{"text": "1 row", "db": shop}, false)
	if r := wait(t, ch); r.isErr || r.text != "[pgquire: “shop” — PGlite in the browser]\n1 row" {
		t.Fatalf("answer: %+v", r)
	}
	// By id, to the other tab.
	ch = agentToolAsync(t, c, "get_schema", map[string]any{"database": "db2"})
	ev = b.next("call")
	b.reply(ev.data["id"], map[string]any{"text": "ERROR: nope", "error": true}, false)
	if r := wait(t, ch); !r.isErr || r.text != "ERROR: nope" {
		t.Fatalf("error answer: %+v", r)
	}
}

func TestAgentDatabaseMustBeNamedWhenSeveral(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	a := openTab(t, c)
	a.report("db1", true, shop)
	ch := agentToolAsync(t, c, "get_schema", nil) // only one shared: no need to name it
	a.reply(a.next("call").data["id"], map[string]any{"text": "tables"}, false)
	if r := wait(t, ch); r.text != "tables" {
		t.Fatalf("only one: %+v", r)
	}
	a.report("db1", true, shop, notes, agentDB{ID: "db3", Name: "Shop", Engine: "PGlite in the browser"})
	for _, tc := range []struct{ ref, want string }{
		{"", "several databases are shared"},
		{"shop", "use the id instead: db1, db3"},
		{"nope", `no database called "nope" is shared`},
	} {
		if _, text, isErr := agentTool(t, c, "agenttok", "get_schema", map[string]any{"database": tc.ref}); !isErr || !strings.Contains(text, tc.want) {
			t.Fatalf("%q: %q", tc.ref, text)
		}
	}
	a.none("call")
}

func TestAgentDatabaseNotOpenAsksTheUser(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	a, b := openTab(t, c), openTab(t, c)
	a.report("db1", true, shop, notes)
	b.report("", false, shop, notes)
	_, text, isErr := agentTool(t, c, "agenttok", "run_sql", map[string]any{"database": "notes", "purpose": "test", "sql": "delete from x"})
	if !isErr || !strings.Contains(text, "“notes” isn't open in pgquire right now. Nothing ran.") {
		t.Fatalf("not open: %q", text)
	}
	for _, tab := range []*fakeTab{a, b} { // every tab that shares it offers to open it
		if ev := tab.next("want"); ev.data["id"] != "db2" || ev.data["client"] != "claude-code" {
			t.Fatalf("want: %v", ev.data)
		}
	}
	a.none("call")
	b.none("call")
}

func TestAgentRemoteDatabaseInTwoTabsGoesToTheFrontOne(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	a, b := openTab(t, c), openTab(t, c)
	a.report("r:prod", true, prod)
	time.Sleep(5 * time.Millisecond)
	b.report("r:prod", true, prod)
	ch := agentToolAsync(t, c, "get_schema", map[string]any{"database": "r:prod"})
	b.reply(b.next("call").data["id"], map[string]any{"text": "from b"}, false)
	a.none("call")
	if r := wait(t, ch); r.text != "from b" {
		t.Fatalf("%+v", r)
	}
}

func TestAgentHoldCancelAndTabGone(t *testing.T) {
	c, srv := newTestServer(t, 100, true)
	srv.agent.callTO = 300 * time.Millisecond
	srv.agent.holdTO = 3 * time.Second

	// A hold (waiting for the user's approval) outlasts the short timeout.
	a := openTab(t, c)
	a.report("db1", true, shop)
	ch := agentToolAsync(t, c, "run_sql", map[string]any{"purpose": "test", "sql": "delete from t"})
	ev := a.next("call")
	a.reply(ev.data["id"], nil, true)
	time.Sleep(500 * time.Millisecond)
	a.reply(ev.data["id"], map[string]any{"text": "DECLINED: no", "error": true}, false)
	if r := wait(t, ch); r.text != "DECLINED: no" {
		t.Fatalf("held: %+v", r)
	}

	// No answer and no hold: it times out, and the tab, which had it, is told to drop it; maybe run.
	ch = agentToolAsync(t, c, "run_sql", map[string]any{"purpose": "test", "sql": "delete from t"})
	ev = a.next("call")
	if r := wait(t, ch); !r.isErr || !strings.HasPrefix(r.text, "UNKNOWN OUTCOME: the pgquire tab didn't answer in time") {
		t.Fatalf("timeout: %+v", r)
	}
	if cancel := a.next("cancel"); cancel.data["id"] != ev.data["id"] {
		t.Fatalf("cancel: %v", cancel.data)
	}
	if code := a.reply(ev.data["id"], map[string]any{"text": "late"}, false); code != 410 {
		t.Fatalf("late reply: %d", code)
	}

	// The tab goes away holding a call.
	srv.agent.callTO = 5 * time.Second
	ctx, stop := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/api/agent/stream", nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var hello struct {
		Tab string `json:"tab"`
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			json.Unmarshal([]byte(d), &hello)
			break
		}
	}
	gone := &fakeTab{t: t, c: c, id: hello.Tab}
	gone.report("db9", true, agentDB{ID: "db9", Name: "scratch", Engine: "PGlite in the browser"})
	ch = agentToolAsync(t, c, "run_sql", map[string]any{"database": "db9", "purpose": "test", "sql": "insert into t values (1)"})
	for sc.Scan() && sc.Text() != "event: call" {
	}
	stop()
	resp.Body.Close()
	if r := wait(t, ch); !strings.HasPrefix(r.text, "UNKNOWN OUTCOME: the pgquire tab with the database open closed") {
		t.Fatalf("tab gone: %+v", r)
	}
}

func TestAgentTokenKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "agent-token")
	a, err := loadAgentToken(path)
	if err != nil || len(a) != 48 {
		t.Fatalf("%q %v", a, err)
	}
	if b, _ := loadAgentToken(path); b != a {
		t.Fatalf("not kept: %q %q", a, b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestAgentSetupForThePage(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	var out map[string]any
	c.do("GET", "/api/agent/setup", nil, &out)
	if out["token"] != "agenttok" || !strings.HasSuffix(out["url"].(string), "/mcp") {
		t.Fatalf("%v", out)
	}
	anon, _ := newTestServer(t, 100, false)
	if code := anon.do("GET", "/api/agent/setup", nil, nil); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
}

func TestAgentRunSQLNeedsAPurpose(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	a := openTab(t, c)
	a.report("db1", true, shop)
	for _, purpose := range []string{"", "  "} {
		if _, text, isErr := agentTool(t, c, "agenttok", "run_sql", map[string]any{"sql": "select 1", "purpose": purpose}); !isErr || !strings.Contains(text, "say what this is for in `purpose`") {
			t.Fatalf("%q: %q", purpose, text)
		}
	}
	a.none("call")
}

func TestAgentNewToken(t *testing.T) {
	c, srv := newTestServer(t, 100, true)
	dir := t.TempDir()
	srv.cfg.agentFiles = agentFiles{token: filepath.Join(dir, "agent-token"), info: filepath.Join(dir, "agent.json")}
	writeAgentFile(srv.cfg.agentFiles.info, agentInfo{URL: c.base, Token: "agenttok", PID: os.Getpid()})
	var out map[string]any
	if code := c.do("POST", "/api/agent/token", nil, &out); code != 200 {
		t.Fatalf("new token: %d", code)
	}
	tok := out["token"].(string)
	if len(tok) != 48 || tok == "agenttok" {
		t.Fatalf("token %q", tok)
	}
	if code, _, _ := agentTool(t, c, "agenttok", "list_databases", nil); code != 401 { // the old one stops working
		t.Fatalf("old token: %d", code)
	}
	if code, _, _ := agentTool(t, c, tok, "list_databases", nil); code != 200 {
		t.Fatalf("new token: %d", code)
	}
	if kept, _ := loadAgentToken(srv.cfg.agentFiles.token); kept != tok { // kept for the next start
		t.Fatalf("kept %q", kept)
	}
	if info, _ := readAgentFile(srv.cfg.agentFiles.info); info.Token != tok { // pgquire mcp carries on
		t.Fatalf("agent.json %v", info)
	}
	anon, _ := newTestServer(t, 100, false)
	if code := anon.do("POST", "/api/agent/token", nil, nil); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
}

func TestAgentCallsOnlyFromThisMachine(t *testing.T) {
	c, srv := newTestServer(t, 100, true)
	call := func(header string) int {
		req, _ := http.NewRequest("POST", c.base+"/api/agent/call", strings.NewReader(`{"tool":"list_databases"}`))
		req.Header.Set("Authorization", "Bearer agenttok")
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set(header, "203.0.113.9")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := call(""); code != 200 { // pgquire mcp, beside it
		t.Fatalf("local: %d", code)
	}
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip"} { // through a proxy: from elsewhere
		if code := call(h); code != 403 {
			t.Fatalf("%s: %d", h, code)
		}
	}
	srv.cfg.mcpHTTP = true
	if code := call("X-Forwarded-For"); code != 200 {
		t.Fatalf("with -mcp-http: %d", code)
	}
}

func TestFromThisMachine(t *testing.T) {
	lan := &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 8432}
	for _, tc := range []struct {
		remote string
		local  *net.TCPAddr
		proxy  bool
		want   bool
	}{
		{"127.0.0.1:5000", nil, false, true},
		{"[::1]:5000", nil, false, true},
		{"192.168.1.5:5000", lan, false, true},                                                     // listening on a LAN address, called from this machine
		{"192.168.1.9:5000", lan, false, false},                                                    // another machine on the LAN
		{"172.17.0.1:5000", &net.TCPAddr{IP: net.ParseIP("172.17.0.2"), Port: 8432}, false, false}, // Docker's port forwarding
		{"127.0.0.1:5000", nil, true, false},                                                       // a proxy on this machine that says so
	} {
		r := httptest.NewRequest("POST", "/api/agent/call", nil)
		r.RemoteAddr = tc.remote
		if tc.local != nil {
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, tc.local))
		}
		if tc.proxy {
			r.Header.Set("X-Forwarded-For", "203.0.113.9")
		}
		if got := fromThisMachine(r); got != tc.want {
			t.Errorf("%s (local %v, proxy %v): %v, want %v", tc.remote, tc.local, tc.proxy, got, tc.want)
		}
	}
}

func TestBrowserHost(t *testing.T) {
	for addr, want := range map[string]string{"127.0.0.1:8432": "127.0.0.1:8432", "0.0.0.0:8432": "127.0.0.1:8432", "[::]:8432": "127.0.0.1:8432",
		"[::1]:8432": "[::1]:8432", "192.168.1.5:8432": "192.168.1.5:8432"} {
		a, _ := net.ResolveTCPAddr("tcp", addr)
		if got := browserHost(a); got != want {
			t.Errorf("%s: %s, want %s", addr, got, want)
		}
	}
}
