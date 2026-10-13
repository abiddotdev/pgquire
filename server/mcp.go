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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const mcpInstructions = `pgquire is a Postgres workbench in the user's browser. These tools work on databases the user shared with agents in pgquire: PGlite (Postgres in the browser) or remote PostgreSQL servers.
- Start with list_databases, then get_schema. Name the database in every call (its id, or its name if no other has it).
- run_sql: reads run at once; changes usually wait for the user to approve them in pgquire (DECLINED = they said no). Each call must be complete on its own: BEGIN … COMMIT in one call, and SET doesn't carry over.
- A database that isn't open in pgquire can't be used until the user opens it; pgquire offers them to.
- UNKNOWN OUTCOME: the tab went away mid-call; the SQL may or may not have run. Check before trying again.`

func mcpTool(name, desc string, props map[string]any, required ...string) map[string]any {
	if name != "list_databases" {
		props["database"] = map[string]any{"type": "string", "description": "The database's id or name, from list_databases. May be left out while only one is shared."}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return map[string]any{"name": name, "description": desc, "inputSchema": schema}
}

var str = map[string]any{"type": "string"}

var mcpTools = []map[string]any{
	mcpTool("list_databases", "The databases the user shared with agents in pgquire: PGlite in the browser or remote PostgreSQL (and whether production or read-only), whether each is open, and which one the user is looking at. Call it first.", map[string]any{}),
	mcpTool("get_schema", "Tables, views, columns, keys and row counts of a database.", map[string]any{}),
	mcpTool("run_sql", "Run SQL on a database. Several statements are allowed; rows come back truncated. Changes usually wait for the user's approval in pgquire. A transaction must begin and end in the same call.",
		map[string]any{"sql": str}, "sql"),
	mcpTool("open_table", "Show a table in the user's pgquire workspace, optionally filtered by a WHERE clause (without the keyword), to point them at rows.",
		map[string]any{"table": str, "where": str}, "table"),
	mcpTool("open_in_editor", "Put SQL in a new query tab in pgquire for the user to review and run themselves (not run).",
		map[string]any{"sql": str, "title": str}, "sql"),
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

// conn finds the session a request belongs to. An id this server doesn't know (it restarted, say)
// gets a fresh connection rather than an error: nothing in it is worth refusing a call over.
func (ss *mcpSessions) conn(id string) *mcpConn {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.conns == nil {
		ss.conns = map[string]*mcpConn{}
	}
	c := ss.conns[id]
	if c == nil {
		if len(ss.conns) > 1000 { // agents that never said goodbye
			clear(ss.conns)
		}
		c = newMCPConn()
		ss.conns[id] = c
	}
	return c
}

func (s *server) mcpHTTP(w http.ResponseWriter, r *http.Request) {
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
	if m.Method == "initialize" {
		sid = randomHex(16)
	}
	c := s.mcp.conn(sid)
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
	b := &mcpBridge{agentPath: filepath.Join(dir, "agent.json"), hc: &http.Client{}}
	if err := serveMCP(os.Stdin, os.Stdout, b.run); err != nil {
		fmt.Fprintln(os.Stderr, "pgquire mcp:", err)
		os.Exit(1)
	}
}

// serveMCP answers JSON-RPC messages, one per line, until in ends. Calls run side by side (one
// waiting for the user's approval doesn't hold up the rest) and can be cancelled.
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
	hc        *http.Client
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
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(info.URL, "/")+"/api/agent/call", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+info.Token)
	resp, err := b.hc.Do(req)
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
		if tool == "run_sql" { // the server had it: it may have reached the tab
			return fmt.Sprintf("UNKNOWN OUTCOME: pgquire answered %d; the SQL may or may not have run. Check before trying it again.", resp.StatusCode), true
		}
		return fmt.Sprintf("ERROR: pgquire answered %d", resp.StatusCode), true
	}
	return r.Text, r.IsError
}

func constEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
