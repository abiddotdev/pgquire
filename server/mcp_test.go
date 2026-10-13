package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* ---------- MCP: POST /mcp and pgquire mcp (fake tabs answer) ---------- */

// mcpPost sends one JSON-RPC message to /mcp as an HTTP agent would.
func mcpPost(t *testing.T, c *client, sid string, msg map[string]any) (int, http.Header, map[string]any) {
	t.Helper()
	msg["jsonrpc"] = "2.0"
	b, _ := json.Marshal(msg)
	req, _ := http.NewRequest("POST", c.base+"/mcp", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer agenttok")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, resp.Header, out
}

func toolText(t *testing.T, reply map[string]any) (string, bool) {
	t.Helper()
	res, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", reply)
	}
	return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"] == true
}

func TestMCPOverHTTP(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	code, h, r := mcpPost(t, c, "", map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]any{"name": "claude-code"}, "capabilities": map[string]any{}}})
	sid := h.Get("Mcp-Session-Id")
	res := r["result"].(map[string]any)
	if code != 200 || sid == "" || res["protocolVersion"] != "2025-06-18" || !strings.Contains(res["instructions"].(string), "list_databases") {
		t.Fatalf("initialize: %d %q %v", code, sid, r)
	}
	if code, _, _ := mcpPost(t, c, sid, map[string]any{"method": "notifications/initialized"}); code != 202 {
		t.Fatalf("notification: %d", code)
	}
	_, _, r = mcpPost(t, c, sid, map[string]any{"id": 2, "method": "tools/list"})
	var names []string
	for _, tool := range r["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "list_databases,get_schema,run_sql,open_table,open_in_editor" {
		t.Fatalf("tools: %v", names)
	}

	tab := openTab(t, c)
	tab.report("db1", true, shop)
	go func() {
		ev := tab.next("call")
		if ev.data["call"].(map[string]any)["client"] != "claude-code" { // the session's name
			ev.data["id"] = "wrong client"
		}
		tab.reply(ev.data["id"], map[string]any{"text": "ok", "db": shop}, false)
	}()
	_, _, r = mcpPost(t, c, sid, map[string]any{"id": 3, "method": "tools/call", "params": map[string]any{"name": "run_sql", "arguments": map[string]any{"sql": "select 1"}}})
	if text, isErr := toolText(t, r); isErr || text != "[pgquire: “shop” — PGlite in the browser]\nok" {
		t.Fatalf("call: %q", text)
	}
	if _, _, r = mcpPost(t, c, sid, map[string]any{"id": 4, "method": "nope"}); r["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("unknown method: %v", r)
	}

	req, _ := http.NewRequest("GET", c.base+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer agenttok")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 405 {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", c.base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 403 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
}

func TestMCPOverHTTPCancel(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	_, h, _ := mcpPost(t, c, "", map[string]any{"id": 1, "method": "initialize", "params": map[string]any{}})
	sid := h.Get("Mcp-Session-Id")
	tab := openTab(t, c)
	tab.report("db1", true, shop)
	done := make(chan map[string]any)
	go func() {
		_, _, r := mcpPost(t, c, sid, map[string]any{"id": 7, "method": "tools/call", "params": map[string]any{"name": "run_sql", "arguments": map[string]any{"sql": "delete from t"}}})
		done <- r
	}()
	ev := tab.next("call")
	tab.reply(ev.data["id"], nil, true) // waiting for approval…
	mcpPost(t, c, sid, map[string]any{"method": "notifications/cancelled", "params": map[string]any{"requestId": 7}})
	if cancel := tab.next("cancel"); cancel.data["id"] != ev.data["id"] { // …withdrawn
		t.Fatalf("cancel: %v", cancel.data)
	}
	if text, _ := toolText(t, <-done); !strings.HasPrefix(text, "UNKNOWN OUTCOME: the call was cancelled") {
		t.Fatalf("%q", text)
	}
}

/* ---------- stdio ---------- */

type stdioMCP struct {
	t   *testing.T
	in  *io.PipeWriter
	out chan map[string]any
}

func startMCP(t *testing.T, agentPath string) *stdioMCP {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	b := &mcpBridge{agentPath: agentPath, hc: &http.Client{Timeout: 5 * time.Second}}
	go func() { serveMCP(inR, outW, b.run); outW.Close() }()
	t.Cleanup(func() { inW.Close() })
	m := &stdioMCP{t: t, in: inW, out: make(chan map[string]any, 8)}
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var r map[string]any
			json.Unmarshal(sc.Bytes(), &r)
			m.out <- r
		}
	}()
	return m
}

func (m *stdioMCP) write(msg map[string]any) {
	msg["jsonrpc"] = "2.0"
	b, _ := json.Marshal(msg)
	go m.in.Write(append(b, '\n'))
}

func (m *stdioMCP) read() map[string]any {
	m.t.Helper()
	select {
	case r := <-m.out:
		return r
	case <-time.After(5 * time.Second):
		m.t.Fatal("no reply")
	}
	return nil
}

func (m *stdioMCP) tool(id int, name string, args map[string]any) (string, bool) {
	m.t.Helper()
	m.write(map[string]any{"id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	return toolText(m.t, m.read())
}

func TestMCPStdioWithoutPgquire(t *testing.T) {
	m := startMCP(t, filepath.Join(t.TempDir(), "agent.json"))
	m.write(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "codex"}}})
	if r := m.read(); r["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "pgquire" {
		t.Fatalf("initialize: %v", r)
	}
	m.write(map[string]any{"id": 2, "method": "tools/list"})
	if r := m.read(); len(r["result"].(map[string]any)["tools"].([]any)) != 5 { // tools are there before pgquire runs
		t.Fatalf("tools/list: %v", r)
	}
	if text, isErr := m.tool(3, "list_databases", nil); !isErr || !strings.Contains(text, "pgquire isn't running") {
		t.Fatalf("%q", text)
	}
}

func TestMCPStdioThroughPgquire(t *testing.T) {
	c, _ := newTestServer(t, 100, true)
	agentPath := filepath.Join(t.TempDir(), "agent.json")
	writeAgentFile(agentPath, agentInfo{URL: c.base, Token: "agenttok", PID: 1})
	m := startMCP(t, agentPath)
	m.write(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "codex"}}})
	m.read()
	tab := openTab(t, c)
	tab.report("db1", true, shop, notes)
	if text, _ := m.tool(2, "list_databases", nil); !strings.Contains(text, "id db1") || !strings.Contains(text, "id db2") {
		t.Fatalf("list: %q", text)
	}

	// A call waiting for approval doesn't hold up the next one, and cancelling it withdraws it.
	m.write(map[string]any{"id": 3, "method": "tools/call", "params": map[string]any{"name": "run_sql", "arguments": map[string]any{"database": "db1", "sql": "delete from t"}}})
	first := tab.next("call")
	if call := first.data["call"].(map[string]any); call["client"] != "codex" {
		t.Fatalf("client: %v", call)
	}
	tab.reply(first.data["id"], nil, true)
	m.write(map[string]any{"id": 4, "method": "tools/call", "params": map[string]any{"name": "get_schema", "arguments": map[string]any{"database": "db1"}}})
	second := tab.next("call")
	tab.reply(second.data["id"], map[string]any{"text": "tables"}, false)
	if r := m.read(); r["id"] != float64(4) {
		t.Fatalf("the second call should answer first: %v", r)
	}
	m.write(map[string]any{"method": "notifications/cancelled", "params": map[string]any{"requestId": 3}})
	tab.next("cancel")
	r := m.read()
	if text, _ := toolText(t, r); r["id"] != float64(3) || !strings.HasPrefix(text, "UNKNOWN OUTCOME") {
		t.Fatalf("cancelled: %v", r)
	}

	writeAgentFile(agentPath, agentInfo{URL: c.base, Token: "stale", PID: 1}) // read afresh each call
	if text, _ := m.tool(5, "list_databases", nil); !strings.Contains(text, "refused the token") {
		t.Fatalf("stale token: %q", text)
	}
}
