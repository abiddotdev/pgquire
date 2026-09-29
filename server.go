package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const cookieName = "pgquire_t"

type serverConfig struct {
	token     string
	htmlPath  string // serve from disk (development) instead of the embedded page
	maxRows   int
	stmtTO    time.Duration
	idleTO    time.Duration
	profiles  *profileStore
	port      int
	allowHost bool // listening beyond loopback: skip the Host check
}

type server struct {
	cfg      serverConfig
	sessions *sessionManager
	mux      *http.ServeMux
}

func newServer(cfg serverConfig) *server {
	s := &server{cfg: cfg, sessions: newSessionManager(cfg.stmtTO, cfg.idleTO), mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.page)
	s.mux.HandleFunc("GET /index.html", s.page)
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("GET /api/profiles", s.auth(s.listProfiles))
	s.mux.HandleFunc("POST /api/profiles", s.auth(s.addProfile))
	s.mux.HandleFunc("DELETE /api/profiles/{name}", s.auth(s.deleteProfile))
	s.mux.HandleFunc("POST /api/sessions", s.auth(s.openSession))
	s.mux.HandleFunc("POST /api/sessions/{id}/query", s.auth(s.withSession(s.query)))
	s.mux.HandleFunc("POST /api/sessions/{id}/exec", s.auth(s.withSession(s.exec)))
	s.mux.HandleFunc("POST /api/sessions/{id}/cancel", s.auth(s.cancel))
	s.mux.HandleFunc("POST /api/sessions/{id}/close", s.auth(s.closeSession))
	return s
}

// ServeHTTP guards every request against DNS rebinding (Host) and cross-site calls (Origin), and
// swaps a ?t=<token> link for a cookie so the token doesn't linger in the address bar.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.allowHost && !s.loopbackHost(r.Host) {
		http.Error(w, "unexpected Host header", http.StatusMisdirectedRequest)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if o := r.Header.Get("Origin"); o != "http://"+r.Host {
			writeErr(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
	}
	if t := r.URL.Query().Get("t"); t != "" && r.Method == http.MethodGet {
		if s.tokenOK(t) {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		}
		u := *r.URL
		q := u.Query()
		q.Del("t")
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *server) loopbackHost(host string) bool {
	h, port, err := net.SplitHostPort(host)
	if err != nil || port != fmt.Sprint(s.cfg.port) {
		return false
	}
	return h == "localhost" || net.ParseIP(h).IsLoopback()
}

func (s *server) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.cfg.token)) == 1
}

func (s *server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && s.tokenOK(c.Value)
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeErr(w, http.StatusUnauthorized, "not signed in — open the link pgquire printed when it started")
			return
		}
		next(w, r)
	}
}

func (s *server) page(w http.ResponseWriter, r *http.Request) {
	body := indexHTML
	if s.cfg.htmlPath != "" {
		b, err := os.ReadFile(s.cfg.htmlPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body = b
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Write(body)
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"app": "pgquire", "version": Version, "authed": s.authed(r), "maxRows": s.cfg.maxRows})
}

/* ---------- connection profiles ---------- */

func (s *server) listProfiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"profiles": s.cfg.profiles.all()})
}

func (s *server) addProfile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		DSN      string `json:"dsn"`
		Tag      string `json:"tag"`
		ReadOnly *bool  `json:"readOnly"`
		Save     bool   `json:"save"`
		Replace  bool   `json:"replace"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Name, in.DSN, in.Tag = strings.TrimSpace(in.Name), strings.TrimSpace(in.DSN), strings.TrimSpace(in.Tag)
	if in.Name == "" || in.DSN == "" {
		writeErr(w, http.StatusBadRequest, "a name and a connection string are both needed")
		return
	}
	if !in.Replace && s.cfg.profiles.get(in.Name) != nil {
		writeErr(w, http.StatusConflict, fmt.Sprintf("there's already a connection called %q", in.Name))
		return
	}
	p := &Profile{Name: in.Name, DSN: in.DSN, Tag: in.Tag, ReadOnly: in.Tag == "prod", saved: in.Save}
	if in.ReadOnly != nil {
		p.ReadOnly = *in.ReadOnly
	}
	// Try it before keeping it.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	sess, err := s.sessions.open(ctx, p)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": pgErrorJSON(err)})
		return
	}
	ver := sess.conn.PgConn().ParameterStatus("server_version")
	s.sessions.close(sess.id)
	if err := s.cfg.profiles.put(p); err != nil {
		writeErr(w, http.StatusInternalServerError, "connected, but couldn't save: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": p.public(), "serverVersion": ver})
}

func (s *server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	ok, err := s.cfg.profiles.remove(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "no such connection")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ---------- sessions ---------- */

func (s *server) openSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Profile string `json:"profile"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	p := s.cfg.profiles.get(in.Profile)
	if p == nil {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("no connection called %q on this pgquire server", in.Profile))
		return
	}
	sess, err := s.sessions.open(r.Context(), p)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": pgErrorJSON(err)})
		return
	}
	pc := sess.conn.PgConn()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": sess.id, "profile": p.public(), "readOnly": p.ReadOnly,
		"serverVersion": pc.ParameterStatus("server_version"), "maxRows": s.cfg.maxRows,
	})
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, sess *session)

func (s *server) withSession(next sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.sessions.get(r.PathValue("id"))
		if sess == nil {
			// Nothing ran, so the page can reconnect and retry.
			writeJSON(w, http.StatusGone, map[string]any{"error": map[string]any{"message": "session closed", "code": "session_closed"}})
			return
		}
		next(w, r, sess)
	}
}

// reply sends results, or the Postgres error (400) — or 410 if the connection dropped (the
// statement may or may not have run, so the page reconnects but doesn't retry).
func (s *server) reply(w http.ResponseWriter, sess *session, v any, err error) {
	switch {
	case errors.Is(err, errSessionLost) || (err != nil && sess.conn.IsClosed()):
		s.sessions.close(sess.id)
		writeJSON(w, http.StatusGone, map[string]any{"error": map[string]any{"message": "the connection to the server was lost: " + err.Error(), "code": "session_lost"}})
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": pgErrorJSON(err)})
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

func (s *server) query(w http.ResponseWriter, r *http.Request, sess *session) {
	var in stmt
	if !readJSON(w, r, &in) {
		return
	}
	params, err := encodeParams(in.Params)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var res result
	err = sess.run(r.Context(), func(ctx context.Context, c pgConn) error {
		var e error
		res, e = queryParams(ctx, c, in.SQL, params, s.cfg.maxRows)
		return e
	})
	s.reply(w, sess, res, err)
}

func (s *server) exec(w http.ResponseWriter, r *http.Request, sess *session) {
	var in struct {
		SQL string `json:"sql"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var res []result
	err := sess.run(r.Context(), func(ctx context.Context, c pgConn) error {
		var e error
		res, e = execScript(ctx, c, in.SQL, s.cfg.maxRows)
		return e
	})
	s.reply(w, sess, res, err)
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	sess := s.sessions.get(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": sess != nil && sess.interrupt()})
}

func (s *server) closeSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"closed": s.sessions.close(r.PathValue("id"))})
}

/* ---------- JSON helpers ---------- */

const maxBody = 64 << 20 // imports can send big scripts

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writing response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg}})
}
