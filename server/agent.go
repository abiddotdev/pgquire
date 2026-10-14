package main

// Agent access lets a local AI agent (Claude Code, Codex… over MCP: /mcp, or `pgquire mcp` on stdio)
// use databases the user shared with agents, PGlite ones in the browser included. Each pgquire tab
// that has anything shared holds a server-sent-events stream and tells the hub which database it has
// open and which ones its browser shares; an agent's call names a database and goes to the tab that
// has it open, which runs it (asking the user before changes) and posts the answer back.
//
// A call never goes to another database: if no tab has the one it names open, it fails and the tabs
// offer the user to open it. A database is open in one tab at a time in a browser (Web Locks); a
// remote one can be open in several, and then the tab the user looked at last answers.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type agentHub struct {
	mu     sync.Mutex
	tabs   map[string]*agentTab
	calls  map[string]*agentCall
	callTO time.Duration // a call with no answer and no hold fails after this
	holdTO time.Duration // …or after this, once the tab says it is waiting for the user
	beat   time.Duration
}

// agentDB is a database shared with agents, as a tab describes it.
type agentDB struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Engine     string `json:"engine"`
	Production bool   `json:"production,omitempty"`
	ReadOnly   bool   `json:"readOnly,omitempty"`
}

type agentTab struct {
	id      string
	send    chan agentEvent
	cancels []string      // calls it had and should drop: kept here, as send can be full (under agentHub.mu)
	wake    chan struct{} // there are cancels
	open    string        // the shared database open in this tab, or ""
	shared  []agentDB     // what this tab's browser shares
	seen    time.Time     // when the user last had this tab in front
}

type agentEvent struct {
	name string
	data any
	call *agentCall // marked delivered once written to the stream
}

type agentCall struct {
	id, tab   string
	done      chan agentAnswer
	hold      chan struct{}
	delivered bool
}

type agentAnswer struct {
	raw json.RawMessage
	err error
}

// agentError is a call that got no answer. Unknown: the tab had it, so it may have run.
type agentError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Unknown bool   `json:"unknown,omitempty"`
}

func (e *agentError) Error() string { return e.Message }

func newAgentHub() *agentHub {
	return &agentHub{tabs: map[string]*agentTab{}, calls: map[string]*agentCall{},
		callTO: 60 * time.Second, holdTO: 10 * time.Minute, beat: 15 * time.Second}
}

func (h *agentHub) join() *agentTab {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := &agentTab{id: randomHex(8), send: make(chan agentEvent, 64), wake: make(chan struct{}, 1), seen: time.Now()}
	h.tabs[t.id] = t
	return t
}

// leave drops a tab whose stream ended, failing the calls it had.
func (h *agentHub) leave(t *agentTab) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.tabs, t.id)
	for id, c := range h.calls {
		if c.tab == t.id {
			delete(h.calls, id)
			c.done <- agentAnswer{err: &agentError{Code: "tab_gone", Unknown: c.delivered,
				Message: "the pgquire tab with the database open closed or lost its connection"}}
		}
	}
}

// report records what a tab has open and shares; front: the user is looking at it now.
func (h *agentHub) report(tab, open string, shared []agentDB, front bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.tabs[tab]
	if t == nil {
		return false
	}
	t.open, t.shared = open, shared
	if front {
		t.seen = time.Now()
	}
	return true
}

// agentDBState is a shared database as the agent sees it.
type agentDBState struct {
	agentDB
	Open    bool // a tab has it open
	Looking bool // …the tab the user had in front last
}

// databases lists every shared database, each once, open ones first.
func (h *agentHub) databases() (dbs []agentDBState, tabs int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var front *agentTab
	for _, t := range h.tabs {
		if t.open != "" && (front == nil || t.seen.After(front.seen)) {
			front = t
		}
	}
	at := map[string]int{}
	for _, t := range h.tabs {
		for _, d := range t.shared {
			i, ok := at[d.ID]
			if !ok {
				i = len(dbs)
				at[d.ID] = i
				dbs = append(dbs, agentDBState{agentDB: d})
			}
			if t.open == d.ID { // the holder's description is the freshest
				dbs[i].agentDB, dbs[i].Open = d, true
			}
		}
	}
	if front != nil {
		if i, ok := at[front.open]; ok {
			dbs[i].Looking = true
		}
	}
	slices.SortStableFunc(dbs, func(a, b agentDBState) int {
		switch {
		case a.Open != b.Open:
			if a.Open {
				return -1
			}
			return 1
		case a.Looking != b.Looking:
			if a.Looking {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return dbs, len(h.tabs)
}

// resolve finds the shared database ref names: its id, else its name (if only one has it).
// An empty ref is fine while exactly one database is shared.
func (h *agentHub) resolve(ref string) (agentDBState, error) {
	dbs, _ := h.databases()
	ref = strings.TrimSpace(ref)
	if len(dbs) == 0 {
		return agentDBState{}, &agentError{Code: "none_shared", Message: noneShared}
	}
	if ref == "" {
		if len(dbs) == 1 {
			return dbs[0], nil
		}
		return agentDBState{}, &agentError{Code: "which", Message: "several databases are shared — say which one with `database` (list_databases shows them)"}
	}
	for _, d := range dbs {
		if d.ID == ref {
			return d, nil
		}
	}
	var hits []agentDBState
	for _, d := range dbs {
		if strings.EqualFold(d.Name, ref) {
			hits = append(hits, d)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return agentDBState{}, &agentError{Code: "not_shared", Message: fmt.Sprintf("no database called %q is shared with agents (list_databases shows the ones that are)", ref)}
	}
	ids := make([]string, len(hits))
	for i, d := range hits {
		ids[i] = d.ID
	}
	return agentDBState{}, &agentError{Code: "which", Message: fmt.Sprintf("several shared databases are called %q — use the id instead: %s", ref, strings.Join(ids, ", "))}
}

// call sends one call for database db to the tab that has it open and waits for its answer.
func (h *agentHub) call(done <-chan struct{}, db agentDB, client string, body any) (json.RawMessage, error) {
	h.mu.Lock()
	var t *agentTab
	for _, x := range h.tabs {
		if x.open == db.ID && (t == nil || x.seen.After(t.seen)) {
			t = x
		}
	}
	if t == nil {
		for _, x := range h.tabs { // offer the user to open it, in the tabs that share it
			if slices.ContainsFunc(x.shared, func(d agentDB) bool { return d.ID == db.ID }) {
				h.queueLocked(x, agentEvent{name: "want", data: map[string]any{"id": db.ID, "client": client}})
			}
		}
		h.mu.Unlock()
		return nil, &agentError{Code: "not_open", Message: fmt.Sprintf("“%s” isn't open in pgquire right now. Nothing ran. pgquire is offering the user to open it — ask them to, then try again", db.Name)}
	}
	c := &agentCall{id: randomHex(8), tab: t.id, done: make(chan agentAnswer, 1), hold: make(chan struct{}, 1)}
	if !h.queueLocked(t, agentEvent{name: "call", data: map[string]any{"id": c.id, "call": body}, call: c}) {
		h.mu.Unlock()
		return nil, &agentError{Code: "busy", Message: "the pgquire tab isn't keeping up — try again in a moment"}
	}
	h.calls[c.id] = c
	h.mu.Unlock()

	timer := time.NewTimer(h.callTO)
	defer timer.Stop()
	for {
		select {
		case a := <-c.done:
			return a.raw, a.err
		case <-c.hold:
			timer.Reset(h.holdTO)
		case <-timer.C:
			return h.abandon(c, &agentError{Code: "timeout", Message: "the pgquire tab didn't answer in time"})
		case <-done:
			return h.abandon(c, &agentError{Code: "cancelled", Message: "the call was cancelled"})
		}
	}
}

// queueLocked hands an event to a tab's stream; a tab too far behind to take it doesn't get it.
func (h *agentHub) queueLocked(t *agentTab, ev agentEvent) bool {
	select {
	case t.send <- ev:
		return true
	default:
		return false
	}
}

// abandon gives up on a call, telling the tab so (an approval it is waiting on is withdrawn).
func (h *agentHub) abandon(c *agentCall, err *agentError) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.calls[c.id]; !ok { // answered (or its tab left) meanwhile
		a := <-c.done
		return a.raw, a.err
	}
	delete(h.calls, c.id)
	err.Unknown = c.delivered
	if t := h.tabs[c.tab]; t != nil && c.delivered { // one not written yet never will be (deliver)
		t.cancels = append(t.cancels, c.id)
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
	return nil, err
}

// takeCancels returns the calls a tab should drop, and forgets them.
func (h *agentHub) takeCancels(t *agentTab) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := t.cancels
	t.cancels = nil
	return ids
}

// answer takes a tab's reply to a call it was sent: the result, or hold (waiting for the user).
func (h *agentHub) answer(tab, id string, hold bool, raw json.RawMessage) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.calls[id]
	if c == nil || c.tab != tab {
		return false
	}
	if hold {
		select {
		case c.hold <- struct{}{}:
		default:
		}
		return true
	}
	delete(h.calls, id)
	c.done <- agentAnswer{raw: raw}
	return true
}

// deliver marks a call as about to be written to its tab; false: it was given up first, so it isn't.
func (h *agentHub) deliver(c *agentCall) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls[c.id] != c {
		return false
	}
	c.delivered = true
	return true
}

/* ---------- the tools, as text for the agent ---------- */

// agentTool runs one tool for an agent (client: its name, shown to the user) and returns what the
// agent reads. done: the agent gave up waiting.
func (s *server) agentTool(done <-chan struct{}, tool string, args json.RawMessage, client, conn string) (string, bool) {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	if tool == "list_databases" {
		return s.listAgentDatabases(), false
	}
	if !slices.Contains([]string{"get_schema", "get_user_view", "run_sql", "open_table", "open_in_editor"}, tool) {
		return "ERROR: unknown tool " + tool, true
	}
	var a struct {
		Database string `json:"database"`
		Purpose  string `json:"purpose"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "ERROR: arguments must be an object", true
	}
	if tool == "run_sql" && strings.TrimSpace(a.Purpose) == "" {
		return "ERROR: say what this is for in `purpose`: one short line the user sees above the SQL in pgquire. Nothing ran.", true
	}
	db, err := s.agent.resolve(a.Database)
	if err != nil {
		return "ERROR: " + err.Error(), true
	}
	raw, err := s.agent.call(done, db.agentDB, client, map[string]any{"tool": tool, "args": args, "client": client, "conn": conn, "database": db.ID})
	var ae *agentError
	if errors.As(err, &ae) {
		if ae.Unknown && tool == "run_sql" {
			return "UNKNOWN OUTCOME: " + ae.Message + "; the SQL may or may not have run. Check before trying it again.", true
		}
		return "ERROR: " + ae.Message, true
	}
	var r struct {
		Text  string   `json:"text"`
		Error bool     `json:"error"`
		DB    *agentDB `json:"db"`
	}
	if err != nil || json.Unmarshal(raw, &r) != nil {
		return "ERROR: unreadable answer from the pgquire tab", true
	}
	if r.DB == nil {
		return r.Text, r.Error
	}
	return "[pgquire: " + describeAgentDB(*r.DB) + "]\n" + r.Text, r.Error
}

// noneShared: pages connect only while something is shared, so "no page open" looks the same.
const noneShared = "no database is shared with agents, or pgquire isn't open in the browser — ask the user to open pgquire, open the database and choose Agents → Share with agents"

func describeAgentDB(d agentDB) string {
	s := fmt.Sprintf("“%s” — %s", d.Name, d.Engine)
	if d.Production {
		s += ", PRODUCTION"
	}
	if d.ReadOnly {
		s += ", read-only"
	}
	return s
}

func (s *server) listAgentDatabases() string {
	dbs, _ := s.agent.databases()
	if len(dbs) == 0 {
		return noneShared
	}
	var b strings.Builder
	b.WriteString("Databases the user shared with agents. Pass the id (or the name, if no other has it) as `database`.\n")
	for _, d := range dbs {
		fmt.Fprintf(&b, "- %s · id %s", describeAgentDB(d.agentDB), d.ID)
		switch {
		case d.Looking:
			b.WriteString(" · open — the user is looking at it")
		case d.Open:
			b.WriteString(" · open")
		default:
			b.WriteString(" · not open: calls on it ask the user to open it")
		}
		b.WriteString("\n")
	}
	return b.String()
}

/* ---------- HTTP: the tab's side ---------- */

// agentStream is a tab's stream: hello (its id), then call, cancel and want events.
func (s *server) agentStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	t := s.agent.join()
	defer s.agent.leave(t)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(name string, data any) error {
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
	if write("hello", map[string]any{"tab": t.id}) != nil {
		return
	}
	beat := time.NewTicker(s.agent.beat)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-t.send:
			if ev.call != nil && !s.agent.deliver(ev.call) { // before writing: once any of it is out, the tab may run it
				continue
			}
			if write(ev.name, ev.data) != nil {
				return
			}
		case <-t.wake:
			for _, id := range s.agent.takeCancels(t) {
				if write("cancel", map[string]any{"id": id}) != nil {
					return
				}
			}
		case <-beat.C:
			if _, err := fmt.Fprint(w, ": beat\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *server) agentState(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tab    string    `json:"tab"`
		Open   string    `json:"open"`
		Shared []agentDB `json:"shared"`
		Front  bool      `json:"front"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !s.agent.report(in.Tab, in.Open, in.Shared, in.Front) {
		writeErr(w, http.StatusNotFound, "that tab isn't connected")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) agentReply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tab    string          `json:"tab"`
		ID     string          `json:"id"`
		Hold   bool            `json:"hold"`
		Result json.RawMessage `json:"result"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !s.agent.answer(in.Tab, in.ID, in.Hold, in.Result) {
		writeErr(w, http.StatusGone, "no such call for this tab (it was given up)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) agentToken() string {
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	return s.agentKey
}

// newAgentToken replaces the agent token (one that leaked, say): agents set up over HTTP need the new
// one; `pgquire mcp` reads it from agent.json, so it carries on.
func (s *server) newAgentToken(w http.ResponseWriter, r *http.Request) {
	t := randomHex(24)
	f := s.cfg.agentFiles
	if f.token != "" {
		if err := writePrivate(f.token, []byte(t+"\n")); err != nil {
			writeErr(w, http.StatusInternalServerError, "keeping the new token: "+err.Error())
			return
		}
	}
	s.agentMu.Lock()
	s.agentKey = t // in effect even if agent.json can't be updated: the old token may have leaked
	s.agentMu.Unlock()
	out := s.setupInfo(r)
	if f.info != "" {
		if info, err := readAgentFile(f.info); err == nil && info.PID == os.Getpid() {
			info.Token = t
			if err := writeAgentFile(f.info, info); err != nil {
				log.Printf("warning: writing %s: %v", f.info, err)
				out["warning"] = "New agent token in effect, but pgquire mcp couldn't be given it (" + err.Error() + "). Restart pgquire so it can."
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// agentSetup is what the page shows for setting up an agent.
func (s *server) agentSetup(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.setupInfo(r))
}

func (s *server) setupInfo(r *http.Request) map[string]any {
	out := map[string]any{"url": requestScheme(r) + "://" + r.Host + "/mcp", "token": s.agentToken(), "http": s.cfg.mcpHTTP}
	if exe, err := os.Executable(); err == nil && !strings.Contains(exe, "go-build") { // not `go run`'s temporary build
		out["exe"] = exe
	}
	return out
}

/* ---------- HTTP: the agent's side ---------- */

// agentCall is how `pgquire mcp` runs a tool: {tool, args, client} → {text, isError}. Unless agents
// over HTTP are on (-mcp-http), only from this machine: `pgquire mcp` always runs beside pgquire.
func (s *server) agentCall(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.mcpHTTP && !fromThisMachine(r) {
		writeErr(w, http.StatusForbidden, "agent calls come only from this machine (`pgquire mcp`); for agents elsewhere, start pgquire with -mcp-http")
		return
	}
	var in struct {
		Tool   string          `json:"tool"`
		Args   json.RawMessage `json:"args"`
		Client string          `json:"client"`
		Conn   string          `json:"conn"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	text, isErr := s.agentTool(r.Context().Done(), in.Tool, in.Args, in.Client, in.Conn)
	writeJSON(w, http.StatusOK, map[string]any{"text": text, "isError": isErr})
}

// bearer guards the agent's endpoints with the agent token (not the page's).
func (s *server) bearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key := s.agentToken(); !ok || key == "" || !constEq(t, key) {
			writeErr(w, http.StatusUnauthorized, "wrong or missing agent token")
			return
		}
		next(w, r)
	}
}

// fromThisMachine: the request came straight from this machine, not through a proxy (which would
// look local too) or Docker's port forwarding (which doesn't). From this machine means a loopback
// address, or the very address it arrived on (pgquire listening on one LAN address, say).
func fromThisMachine(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" || r.Header.Get("X-Real-Ip") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil {
		return false
	}
	local, _ := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	return ip.IsLoopback() || (local != nil && local.IP.Equal(ip))
}

// agentRequest: a request from an agent rather than the page. It has no Origin (a browser always
// sends one with a POST) and carries the agent token itself.
func agentRequest(r *http.Request) bool {
	return (r.URL.Path == "/mcp" || r.URL.Path == "/api/agent/call") && r.Header.Get("Origin") == "" &&
		strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
}

/* ---------- files beside connections.json ---------- */

// agentFiles: where the agent token is kept, and agent.json ("" in tests: memory only).
type agentFiles struct{ token, info string }

// agentInfo is agent.json: where the running pgquire is, for `pgquire mcp`. It exists while pgquire runs.
type agentInfo struct {
	URL   string `json:"url"`
	Host  string `json:"host,omitempty"` // the Host to send, when not URL's (-domain, listening on one address)
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// loadAgentToken reads the agent token kept in path, making one the first time. It lasts across
// restarts, so an agent set up with it over HTTP keeps working.
func loadAgentToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	}
	t := randomHex(24)
	return t, writePrivate(path, []byte(t+"\n"))
}

// writePrivate writes a file only this user can read, atomically.
func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeAgentFile(path string, info agentInfo) error {
	b, _ := json.MarshalIndent(info, "", "  ")
	return writePrivate(path, b)
}

// removeAgentFile deletes agent.json if it is still this process's (another pgquire may have started since).
func removeAgentFile(path string) {
	if info, err := readAgentFile(path); err == nil && info.PID == os.Getpid() {
		os.Remove(path)
	}
}

func readAgentFile(path string) (agentInfo, error) {
	var info agentInfo
	b, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(b, &info)
}
