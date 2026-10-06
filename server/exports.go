package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// CSV exports stream every row with COPY, past the max-rows cap, on their own connection so the
// tab's session stays free. The page first POSTs the query: it's checked (EXPLAIN catches syntax
// and permission errors while they can still be shown) and parked under a one-time token. Then a
// plain GET of /api/exports/{token} downloads it, so the browser streams straight to a file.

type pendingExport struct {
	profile, database, sql, filename string
	expires                          time.Time
}

type exportStore struct {
	mu sync.Mutex
	m  map[string]pendingExport
}

func (e *exportStore) put(x pendingExport) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.m == nil {
		e.m = map[string]pendingExport{}
	}
	for k, v := range e.m {
		if time.Now().After(v.expires) {
			delete(e.m, k)
		}
	}
	t := randomHex(16)
	e.m[t] = x
	return t
}

func (e *exportStore) take(token string) (pendingExport, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	x, ok := e.m[token]
	delete(e.m, token)
	return x, ok && time.Now().Before(x.expires)
}

var unsafeFilename = regexp.MustCompile(`[^\w.\- ]+`)

func (s *server) prepareExport(w http.ResponseWriter, r *http.Request, sess *session) {
	var in struct {
		SQL      string `json:"sql"`
		Filename string `json:"filename"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	query := strings.TrimRight(strings.TrimSpace(in.SQL), "; \n\t")
	if query == "" {
		writeErr(w, http.StatusBadRequest, "nothing to export")
		return
	}
	err := sess.run(r.Context(), 0, func(ctx context.Context, c *pgconn.PgConn) error {
		// Extended protocol: one statement only, so a filter can't smuggle in "; delete …".
		return c.ExecParams(ctx, "explain "+query, nil, nil, nil, nil).Read().Err
	})
	if err != nil {
		s.reply(w, sess, nil, err)
		return
	}
	name := strings.TrimLeft(strings.TrimSpace(unsafeFilename.ReplaceAllString(in.Filename, "")), ". ")
	if name == "" {
		name = "export"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".csv") {
		name += ".csv"
	}
	token := s.exports.put(pendingExport{profile: sess.profile, database: sess.database, sql: query, filename: name, expires: time.Now().Add(time.Minute)})
	writeJSON(w, http.StatusOK, map[string]any{"url": "/api/exports/" + token})
}

func (s *server) runExport(w http.ResponseWriter, r *http.Request) {
	x, ok := s.exports.take(r.PathValue("token"))
	p := s.cfg.profiles.get(x.profile)
	if !ok || p == nil {
		http.Error(w, "This download link has expired. Start the export again.", http.StatusGone)
		return
	}
	cfg, err := s.sessions.connConfig(p, x.database)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	delete(cfg.RuntimeParams, "statement_timeout") // a big export may take a while; closing the download cancels it
	ctx := r.Context()
	pc, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer pc.Close(context.Background())
	if _, err := pc.Exec(ctx, "begin transaction isolation level repeatable read, read only").ReadAll(); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, x.filename))
	w.Header().Set("Cache-Control", "no-store")
	if _, err := pc.CopyTo(ctx, w, "copy ("+x.sql+") to stdout with (format csv, header)"); err != nil {
		// Headers are gone by now; end the file with a visible marker rather than a silent cut.
		fmt.Fprintf(w, "\n# export stopped: %v\n", err)
	}
	pc.Exec(context.Background(), "rollback").ReadAll()
}
