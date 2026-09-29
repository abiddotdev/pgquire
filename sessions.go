package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
)

const maxSessions = 32

type pgConn = *pgconn.PgConn

// A session is one dedicated Postgres connection for one browser tab. PGlite is a single
// connection too, so SET, temp tables and a BEGIN typed in the editor behave the same way.
type session struct {
	id       string
	profile  string
	readOnly bool
	conn     *pgx.Conn

	mu       sync.Mutex   // one request at a time on the connection
	lastUsed atomic.Int64 // unix nanos

	cancelMu sync.Mutex
	cancel   context.CancelFunc // cancels the request in flight, if any
}

var errSessionLost = errors.New("session lost")

// run executes fn on the session's connection, serialised with other requests and cancellable.
func (s *session) run(ctx context.Context, fn func(context.Context, *pgconn.PgConn) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn.IsClosed() {
		return errSessionLost
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.setCancel(cancel)
	defer s.setCancel(nil)
	s.lastUsed.Store(time.Now().UnixNano())
	defer s.lastUsed.Store(time.Now().UnixNano())
	return fn(ctx, s.conn.PgConn())
}

func (s *session) setCancel(c context.CancelFunc) {
	s.cancelMu.Lock()
	s.cancel = c
	s.cancelMu.Unlock()
}

// interrupt cancels the running statement (Postgres cancel request); the connection stays usable.
func (s *session) interrupt() bool {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if s.cancel == nil {
		return false
	}
	s.cancel()
	return true
}

type sessionManager struct {
	mu     sync.Mutex
	byID   map[string]*session
	stmtTO time.Duration
}

func newSessionManager(stmtTO, idleTO time.Duration) *sessionManager {
	m := &sessionManager{byID: map[string]*session{}, stmtTO: stmtTO}
	if idleTO > 0 {
		go m.reap(idleTO)
	}
	return m
}

// connConfig builds the pgx config for a profile: cancel-on-abort, app name, timeouts, read-only.
func (m *sessionManager) connConfig(p *Profile) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(p.DSN)
	if err != nil {
		return nil, err
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}
	// On cancel, ask the server to stop the statement instead of dropping the connection.
	cfg.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, CancelRequestDelay: 0, DeadlineDelay: 15 * time.Second}
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	if _, ok := cfg.RuntimeParams["application_name"]; !ok {
		cfg.RuntimeParams["application_name"] = "pgquire"
	}
	if m.stmtTO > 0 {
		cfg.RuntimeParams["statement_timeout"] = strconv.FormatInt(m.stmtTO.Milliseconds(), 10)
	}
	if p.ReadOnly {
		// A guard rail, not a security boundary: SQL can turn it off. Use a read-only role for that.
		cfg.RuntimeParams["default_transaction_read_only"] = "on"
	}
	return cfg, nil
}

func (m *sessionManager) open(ctx context.Context, p *Profile) (*session, error) {
	m.mu.Lock()
	n := len(m.byID)
	m.mu.Unlock()
	if n >= maxSessions {
		return nil, fmt.Errorf("too many open sessions (%d); close some pgquire tabs", n)
	}
	cfg, err := m.connConfig(p)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &session{id: randomHex(16), profile: p.Name, readOnly: p.ReadOnly, conn: conn}
	s.lastUsed.Store(time.Now().UnixNano())
	m.mu.Lock()
	m.byID[s.id] = s
	m.mu.Unlock()
	return s, nil
}

func (m *sessionManager) get(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byID[id]
}

func (m *sessionManager) close(id string) bool {
	m.mu.Lock()
	s := m.byID[id]
	delete(m.byID, id)
	m.mu.Unlock()
	if s == nil {
		return false
	}
	s.interrupt()
	go func() { // wait for any running request to wind down, then close
		s.mu.Lock()
		defer s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.conn.Close(ctx)
	}()
	return true
}

func (m *sessionManager) closeAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.byID))
	for id := range m.byID {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.close(id)
	}
}

func (m *sessionManager) reap(idle time.Duration) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-idle).UnixNano()
		m.mu.Lock()
		var stale []string
		for id, s := range m.byID {
			if s.lastUsed.Load() < cutoff && s.mu.TryLock() {
				s.mu.Unlock()
				stale = append(stale, id)
			}
		}
		m.mu.Unlock()
		for _, id := range stale {
			m.close(id)
		}
	}
}

/* ---------- results: the same shape PGlite returns ---------- */

type field struct {
	Name       string `json:"name"`
	DataTypeID uint32 `json:"dataTypeID"`
}

// result mirrors PGlite's Results. Values are Postgres text format (or null); the page parses them
// by dataTypeID the way PGlite does, so both kinds of database look the same to the UI.
type result struct {
	Fields       []field     `json:"fields"`
	Rows         [][]*string `json:"rows"`
	AffectedRows int64       `json:"affectedRows"`
	Command      string      `json:"command,omitempty"`
	TotalRows    int64       `json:"totalRows,omitempty"` // set when rows were cut at max-rows
	Truncated    bool        `json:"truncated,omitempty"`
}

// readResult reads one result set, keeping at most max rows (the rest are drained and counted,
// not cancelled: cancelling mid-script would roll back the earlier statements).
func readResult(rr *pgconn.ResultReader, max int) (result, error) {
	fds := rr.FieldDescriptions()
	res := result{Fields: make([]field, len(fds)), Rows: [][]*string{}}
	for i, fd := range fds {
		res.Fields[i] = field{Name: fd.Name, DataTypeID: fd.DataTypeOID}
	}
	var n int64
	for rr.NextRow() {
		n++
		if len(res.Rows) >= max {
			continue
		}
		vals := rr.Values()
		row := make([]*string, len(vals))
		for i, v := range vals {
			if v != nil {
				s := string(v)
				row[i] = &s
			}
		}
		res.Rows = append(res.Rows, row)
	}
	tag, err := rr.Close()
	if err != nil {
		return res, err
	}
	res.Command = commandName(tag)
	if len(fds) == 0 || !tag.Select() {
		res.AffectedRows = tag.RowsAffected()
	}
	if n > int64(len(res.Rows)) {
		res.Truncated, res.TotalRows = true, n
	}
	return res, nil
}

func commandName(tag pgconn.CommandTag) string {
	s := tag.String()
	for i, c := range s {
		if c == ' ' {
			return s[:i]
		}
	}
	return s
}

// execScript runs one or more statements with the simple protocol, like PGlite's exec().
func execScript(ctx context.Context, c *pgconn.PgConn, sql string, max int) ([]result, error) {
	mrr := c.Exec(ctx, sql)
	out := []result{}
	var firstErr error
	for mrr.NextResult() {
		r, err := readResult(mrr.ResultReader(), max)
		if err != nil {
			firstErr = err
			break
		}
		out = append(out, r)
	}
	if err := mrr.Close(); err != nil {
		return nil, err
	}
	return out, firstErr
}

// queryParams runs one statement with parameters (extended protocol, types inferred), like PGlite's query().
func queryParams(ctx context.Context, c *pgconn.PgConn, sql string, params [][]byte, max int) (result, error) {
	return readResult(c.ExecParams(ctx, sql, params, nil, nil, nil), max)
}

type stmt struct {
	SQL    string            `json:"sql"`
	Params []json.RawMessage `json:"params"`
}

// runTx runs statements in one transaction, rolling back on the first error.
func runTx(ctx context.Context, c *pgconn.PgConn, stmts []stmt, max int) ([]result, error) {
	if _, err := c.Exec(ctx, "begin").ReadAll(); err != nil {
		return nil, err
	}
	out := make([]result, 0, len(stmts))
	for _, s := range stmts {
		params, err := encodeParams(s.Params)
		if err == nil {
			var r result
			r, err = queryParams(ctx, c, s.SQL, params, max)
			out = append(out, r)
		}
		if err != nil {
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			c.Exec(rctx, "rollback").ReadAll()
			cancel()
			return nil, err
		}
	}
	if _, err := c.Exec(ctx, "commit").ReadAll(); err != nil {
		return nil, err
	}
	return out, nil
}

// encodeParams turns JSON values into Postgres text parameters. The page sends strings (it already
// formats dates, arrays and bytea); numbers, booleans and objects are accepted for convenience.
func encodeParams(raw []json.RawMessage) ([][]byte, error) {
	out := make([][]byte, len(raw))
	for i, r := range raw {
		var v any
		if err := json.Unmarshal(r, &v); err != nil {
			return nil, fmt.Errorf("parameter $%d: %v", i+1, err)
		}
		switch x := v.(type) {
		case nil:
			out[i] = nil
		case string:
			out[i] = []byte(x)
		case bool:
			if x {
				out[i] = []byte("t")
			} else {
				out[i] = []byte("f")
			}
		case float64:
			out[i] = []byte(string(r)) // keep the exact digits as sent
		default:
			out[i] = []byte(r) // objects and arrays go in as JSON text
		}
	}
	return out, nil
}

// pgErrorJSON carries Postgres error fields the UI already shows for PGlite errors.
func pgErrorJSON(err error) map[string]any {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		e := map[string]any{"message": pe.Message, "code": pe.Code, "severity": pe.Severity}
		for k, v := range map[string]string{"detail": pe.Detail, "hint": pe.Hint, "where": pe.Where, "schema": pe.SchemaName, "table": pe.TableName, "column": pe.ColumnName, "constraint": pe.ConstraintName} {
			if v != "" {
				e[k] = v
			}
		}
		if pe.Position > 0 {
			e["position"] = strconv.Itoa(int(pe.Position))
		}
		return e
	}
	if errors.Is(err, context.Canceled) {
		return map[string]any{"message": "canceling statement due to user request", "code": "57014"}
	}
	return map[string]any{"message": err.Error()}
}
