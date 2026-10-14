package main

// The MCP side of agent access (agent.go). The same handler answers on two transports:
//   - POST /mcp on the running server (streamable HTTP, plain JSON replies), with the agent token;
//   - `pgquire mcp`, a stdio server the agent starts, which runs each tool through the running pgquire
//     (found through agent.json, read afresh every call, so restarting either side is fine). It
//     answers initialize and tools/list itself, so the agent gets its tools even before pgquire starts.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const mcpInstructions = `pgquire: Postgres databases the user shared from their browser (PGlite or remote PostgreSQL).
- Start with list_databases, then get_schema.
- Changes wait for the user's approval; DECLINED means they said no.
- UNKNOWN OUTCOME: the SQL may or may not have run; check before retrying.`

func mcpTool(name, desc string, props map[string]any, required ...string) map[string]any {
	if name != "list_databases" {
		props["database"] = map[string]any{"type": "string", "description": "Id or name from list_databases; optional if only one is shared."}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return map[string]any{"name": name, "description": desc, "inputSchema": schema}
}

var str = map[string]any{"type": "string"}

// purpose: the line the user sees above the call in pgquire, so they know what it's for.
var purpose = map[string]any{"type": "string", "description": "One line for the user, e.g. \"Adding 4 customers\"."}

var mcpTools = []map[string]any{
	mcpTool("list_databases", "Shared databases: id, engine, production or read-only, open or not.", map[string]any{}),
	mcpTool("get_schema", "Tables, views, columns, keys and row counts. A big schema is cut short; filter with schema or tables.",
		map[string]any{"schema": str, "tables": map[string]any{"type": "array", "items": str}, "purpose": purpose}),
	mcpTool("get_user_view", "What the user has open in pgquire: a table (filter, sort, selected rows) or the query in the editor and its result.",
		map[string]any{"purpose": purpose}),
	mcpTool("run_sql", "Run SQL; returns at most 50 rows per result. Each call stands alone: BEGIN … COMMIT together, SET doesn't persist.",
		map[string]any{"sql": str, "purpose": purpose}, "sql", "purpose"),
	mcpTool("open_table", "Show a table to the user, optionally filtered (where: a condition without WHERE).",
		map[string]any{"table": str, "where": str, "purpose": purpose}, "table"),
	mcpTool("open_in_editor", "Put SQL in a new editor tab for the user to run.",
		map[string]any{"sql": str, "title": str, "purpose": purpose}, "sql"),
}

// toolFunc runs a tool for client and returns the text the agent reads; done: the agent gave up.
type toolFunc func(done <-chan struct{}, tool string, args json.RawMessage, client string) (string, bool)

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// mcpConn is one agent's connection: its name, and the calls it may still cancel.
type mcpConn struct {
	mu      sync.Mutex
	client  string
	pending map[string]chan struct{} // request id → closed when cancelled
	used    time.Time                // last request (under mcpSessions.mu)
}

func newMCPConn() *mcpConn { return &mcpConn{pending: map[string]chan struct{}{}} }

func (c *mcpConn) start(id json.RawMessage) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	c.mu.Lock()
	c.pending[string(id)] = ch
	c.mu.Unlock()
	return ch, func() { c.mu.Lock(); delete(c.pending, string(id)); c.mu.Unlock() }
}

// notify takes a notification; notifications/cancelled gives up on a call (its approval is withdrawn).
func (c *mcpConn) notify(m rpcMsg) {
	if m.Method != "notifications/cancelled" {
		return
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	json.Unmarshal(m.Params, &p)
	c.mu.Lock()
	if ch := c.pending[string(p.RequestID)]; ch != nil {
		close(ch)
		delete(c.pending, string(p.RequestID))
	}
	c.mu.Unlock()
}

// handle answers one request.
func (c *mcpConn) handle(done <-chan struct{}, run toolFunc, m rpcMsg) map[string]any {
	reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	fail := func(code int, msg string) map[string]any {
		reply["error"] = map[string]any{"code": code, "message": msg}
		return reply
	}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		}
		json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		c.client = p.ClientInfo.Name
		c.mu.Unlock()
		version := p.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		reply["result"] = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "pgquire", "version": Version}, "instructions": mcpInstructions}
	case "ping":
		reply["result"] = map[string]any{}
	case "tools/list":
		reply["result"] = map[string]any{"tools": mcpTools}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return fail(-32602, "bad params")
		}
		cancelled, end := c.start(m.ID)
		defer end()
		stop, finished := make(chan struct{}), make(chan struct{})
		go func() {
			select {
			case <-done:
			case <-cancelled:
			case <-finished:
				return
			}
			close(stop)
		}()
		c.mu.Lock()
		client := c.client
		c.mu.Unlock()
		text, isErr := run(stop, p.Name, p.Arguments, client)
		close(finished)
		reply["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
	default:
		return fail(-32601, "method not found: "+m.Method)
	}
	return reply
}

/* ---------- streamable HTTP: POST /mcp ---------- */

type mcpSessions struct {
	mu    sync.Mutex
	conns map[string]*mcpConn
}

// start opens a session (initialize), returning its id.
func (ss *mcpSessions) start() (string, *mcpConn) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.conns == nil {
		ss.conns = map[string]*mcpConn{}
	}
	if len(ss.conns) >= 1000 { // agents that never said goodbye: drop the one idle longest, with no calls in flight
		var old string
		for id, c := range ss.conns {
			c.mu.Lock()
			busy := len(c.pending) > 0
			c.mu.Unlock()
			if !busy && (old == "" || c.used.Before(ss.conns[old].used)) {
				old = id
			}
		}
		delete(ss.conns, old)
	}
	id, c := randomHex(16), newMCPConn()
	c.used = time.Now()
	ss.conns[id] = c
	return id, c
}

// conn finds an open session, or nil: one this server doesn't know (it restarted, say) is answered
// 404, after which the agent starts a new one, telling its name again.
func (ss *mcpSessions) conn(id string) *mcpConn {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	c := ss.conns[id]
	if c != nil {
		c.used = time.Now()
	}
	return c
}

func (s *server) mcpHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.mcpHTTP {
		writeErr(w, http.StatusNotFound, "agents over HTTP are off: start pgquire with -mcp-http (or PGQUIRE_MCP_HTTP=1), or use `pgquire mcp` on stdio")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		s.mcp.mu.Lock()
		delete(s.mcp.conns, r.Header.Get("Mcp-Session-Id"))
		s.mcp.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		w.Header().Set("Allow", "POST, DELETE")
		writeErr(w, http.StatusMethodNotAllowed, "pgquire's MCP endpoint takes POST (no server-sent stream)")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	var m rpcMsg
	if err != nil || json.Unmarshal(body, &m) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error (one JSON-RPC message per request)"}})
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	var c *mcpConn
	switch {
	case m.Method == "initialize":
		sid, c = s.mcp.start()
	case sid == "":
		writeErr(w, http.StatusBadRequest, "Mcp-Session-Id missing: send initialize first")
		return
	default:
		if c = s.mcp.conn(sid); c == nil {
			writeErr(w, http.StatusNotFound, "no such session (pgquire restarted?): send initialize again")
			return
		}
	}
	if len(m.ID) == 0 || m.Method == "" { // a notification, or a reply to something never asked
		c.notify(m)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Mcp-Session-Id", sid)
	writeJSON(w, http.StatusOK, c.handle(r.Context().Done(), s.agentTool, m))
}

/* ---------- stdio: pgquire mcp ---------- */

// mcpMain runs `pgquire mcp [-config path]`.
func mcpMain(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	cfgPath := fs.String("config", os.Getenv("PGQUIRE_CONFIG"), "pgquire's connections file; agent.json is looked for beside it (default: <user config dir>/pgquire/)")
	fs.Parse(args)
	dir := filepath.Dir(*cfgPath)
	if *cfgPath == "" {
		d, err := os.UserConfigDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "pgquire mcp: can't find the config directory:", err)
			os.Exit(1)
		}
		dir = filepath.Join(d, "pgquire")
	}
	b := &mcpBridge{agentPath: filepath.Join(dir, "agent.json")}
	if err := serveMCP(os.Stdin, os.Stdout, b.run); err != nil {
		fmt.Fprintln(os.Stderr, "pgquire mcp:", err)
		os.Exit(1)
	}
}

// serveMCP answers JSON-RPC messages, one per line, until in ends. Calls are handled side by side
// and can be cancelled; ones for the same database still run one at a time in its tab.
func serveMCP(in io.Reader, out io.Writer, run toolFunc) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var mu sync.Mutex
	enc := json.NewEncoder(out)
	send := func(v any) error { mu.Lock(); defer mu.Unlock(); return enc.Encode(v) }
	c := newMCPConn()
	closed := make(chan struct{})
	var wg sync.WaitGroup
	defer func() { close(closed); wg.Wait() }()
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m rpcMsg
		if err := json.Unmarshal(line, &m); err != nil {
			if err := send(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}}); err != nil {
				return err
			}
			continue
		}
		if len(m.ID) == 0 || m.Method == "" {
			c.notify(m)
			continue
		}
		if m.Method == "initialize" { // in order: later calls use the client's name
			if err := send(c.handle(closed, run, m)); err != nil {
				return err
			}
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); send(c.handle(closed, run, m)) }()
	}
	return sc.Err()
}

// mcpBridge runs tools through the running pgquire.
type mcpBridge struct {
	agentPath string
}

func (b *mcpBridge) run(done <-chan struct{}, tool string, args json.RawMessage, client string) (string, bool) {
	info, err := readAgentFile(b.agentPath)
	if err != nil {
		return "ERROR: pgquire isn't running (no " + b.agentPath + "). Ask the user to start pgquire.", true
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	body, _ := json.Marshal(map[string]any{"tool": tool, "args": args, "client": client})
	resp, err := postLocal(ctx, info, "/api/agent/call", body)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			if tool == "run_sql" {
				return "UNKNOWN OUTCOME: the call was cancelled; the SQL may or may not have run. Check before trying it again.", true
			}
			return "ERROR: the call was cancelled", true
		}
		return "ERROR: pgquire isn't answering at " + info.URL + " (" + err.Error() + "). Ask the user to start pgquire.", true
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return "ERROR: pgquire refused the token in " + b.agentPath + " — is another pgquire running? Ask the user to restart pgquire.", true
	}
	var r struct {
		Text    string `json:"text"`
		IsError bool   `json:"isError"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &r) != nil {
		if tool == "run_sql" && resp.StatusCode/100 != 4 { // a 4xx was refused before reaching any tab; anything else may have
			return fmt.Sprintf("UNKNOWN OUTCOME: pgquire answered %d; the SQL may or may not have run. Check before trying it again.", resp.StatusCode), true
		}
		return fmt.Sprintf("ERROR: pgquire answered %d", resp.StatusCode), true
	}
	return r.Text, r.IsError
}

// postLocal sends one POST to the running pgquire, written by hand over TCP: net/http's client (with its
// connection pool and HTTP/2) would add over half a megabyte to the binary for this one request.
// Cancelling ctx closes the connection, which the server takes as the agent giving up.
func postLocal(ctx context.Context, info agentInfo, path string, body []byte) (*http.Response, error) {
	u, err := url.Parse(info.URL)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("bad address %q in agent.json", info.URL)
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer func() { stop(); conn.Close() }() // the body is read in full below
	if _, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		path, u.Host, info.Token, len(body), body); err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(resp.Body) // all of it before the connection goes
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, err
}

func constEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
