package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
	domain    string // also accept requests for this host name (behind a proxy, say)
	allowHost bool   // listening beyond loopback with no domain set: skip the Host check
}

type server struct {
	cfg      serverConfig
	sessions *sessionManager
	mux      *http.ServeMux
	exports  exportStore
	startup  *startupOpen // the -dsn connection, set before serving
}

// startupOpen tells the page which connection to open; id is new each start, so each browser
// jumps to it once and keeps its own choice after that.
type startupOpen struct {
	id      string
	profile string
}

func newServer(cfg serverConfig) *server {
	s := &server{cfg: cfg, sessions: newSessionManager(cfg.stmtTO, cfg.idleTO), mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.page)
	s.mux.HandleFunc("GET /index.html", s.page)
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("POST /api/signin", s.signIn)
	s.mux.HandleFunc("GET /api/profiles", s.auth(s.listProfiles))
	s.mux.HandleFunc("POST /api/profiles", s.auth(s.addProfile))
	s.mux.HandleFunc("DELETE /api/profiles/{name}", s.auth(s.deleteProfile))
	s.mux.HandleFunc("GET /api/profiles/{name}/databases", s.auth(s.listDatabases))
	s.mux.HandleFunc("POST /api/profiles/test", s.auth(s.testProfile))
	s.mux.HandleFunc("POST /api/profiles/{name}/readonly-role", s.auth(s.readOnlyRole))
	s.mux.HandleFunc("POST /api/sessions/{id}/exports", s.auth(s.withSession(s.prepareExport)))
	s.mux.HandleFunc("GET /api/exports/{token}", s.auth(s.runExport))
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
	if !s.cfg.allowHost && !s.loopbackHost(r.Host) && !s.domainHost(r.Host) {
		http.Error(w, "unexpected Host header", http.StatusMisdirectedRequest)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if o := r.Header.Get("Origin"); o != "http://"+r.Host && o != "https://"+r.Host {
			writeErr(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
	}
	if t := r.URL.Query().Get("t"); t != "" && r.Method == http.MethodGet {
		if s.tokenOK(t) {
			setTokenCookie(w, r, t)
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

// domainHost: the -domain name, on any port (a proxy usually sends it without one).
func (s *server) domainHost(host string) bool {
	if s.cfg.domain == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.EqualFold(host, s.cfg.domain)
}

func (s *server) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.cfg.token)) == 1
}

// setTokenCookie marks the cookie Secure when the page came over HTTPS, directly or through a proxy
// that says so, so the browser never sends it over plain HTTP. A forged header only makes the
// cookie stricter.
func setTokenCookie(w http.ResponseWriter, r *http.Request, t string) {
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	secure := r.TLS != nil || strings.EqualFold(strings.TrimSpace(proto), "https")
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: t, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
}

// signIn is the pasted-token way in, for a page opened without the link (a bookmark, or a restart
// that changed the token). Same cookie as the link.
func (s *server) signIn(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if t := strings.TrimSpace(in.Token); t == "" || !s.tokenOK(t) {
		writeErr(w, http.StatusUnauthorized, "wrong token")
		return
	}
	setTokenCookie(w, r, strings.TrimSpace(in.Token))
	writeJSON(w, http.StatusOK, map[string]any{"authed": true})
}

func (s *server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && s.tokenOK(c.Value)
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeErr(w, http.StatusUnauthorized, "not signed in — open the link pgquire printed when it started, or paste its token")
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
	out := map[string]any{"app": "pgquire", "version": Version, "authed": s.authed(r), "maxRows": s.cfg.maxRows}
	if s.startup != nil && s.authed(r) {
		if p := s.cfg.profiles.get(s.startup.profile); p != nil {
			out["open"] = map[string]any{"id": s.startup.id, "profile": p.public()}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// connectAtStart adds the -dsn connection for this run (or uses a saved one with the same string)
// and tries it, so a bad one fails in the terminal. It returns a line saying where it connected.
func (s *server) connectAtStart(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	p := s.cfg.profiles.byDSN(dsn)
	if p == nil {
		c, err := pgconn.ParseConfig(dsn)
		if err != nil {
			return "", err
		}
		host := c.Host
		if strings.HasPrefix(host, "/") {
			host = "localhost" // a Unix socket
		}
		name := host
		if c.Database != "" {
			name = c.Database + " on " + host
		}
		p = &Profile{Name: s.cfg.profiles.freeName(name), DSN: dsn}
	}
	probe, err := s.probe(context.Background(), p)
	if err != nil {
		return "", err
	}
	if !p.saved {
		if err := s.cfg.profiles.put(p); err != nil {
			return "", err
		}
	}
	s.startup = &startupOpen{id: randomHex(8), profile: p.Name}
	enc := "not encrypted"
	if probe["tls"] == true {
		enc = "encrypted"
	}
	return fmt.Sprintf("%s (PostgreSQL %s, %s)", p.Name, probe["serverVersion"], enc), nil
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
	probe, err := s.probe(r.Context(), p)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": pgErrorJSON(err)})
		return
	}
	if err := s.cfg.profiles.put(p); err != nil {
		writeErr(w, http.StatusInternalServerError, "connected, but couldn't save: "+err.Error())
		return
	}
	probe["profile"] = p.public()
	writeJSON(w, http.StatusOK, probe)
}

// probe connects once and reports the server version and whether the link is encrypted.
func (s *server) probe(ctx context.Context, p *Profile) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sess, err := s.sessions.open(ctx, p, "")
	if err != nil {
		return nil, err
	}
	defer s.sessions.close(sess.id)
	pc := sess.conn.PgConn()
	_, encrypted := pc.Conn().(*tls.Conn)
	out := map[string]any{"serverVersion": pc.ParameterStatus("server_version"), "tls": encrypted, "database": sess.conn.Config().Database, "user": sess.conn.Config().User}
	if ri, err := checkRole(ctx, sess.conn); err == nil {
		out["role"] = ri
	}
	return out, nil
}

// testProfile tries a connection string without keeping anything.
func (s *server) testProfile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DSN string `json:"dsn"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.DSN) == "" {
		writeErr(w, http.StatusBadRequest, "a connection string is needed")
		return
	}
	probe, err := s.probe(r.Context(), &Profile{Name: "(test)", DSN: strings.TrimSpace(in.DSN)})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": pgErrorJSON(err)})
		return
	}
	writeJSON(w, http.StatusOK, probe)
}

func (s *server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	// ?dropRole=1: first remove the login pgquire created for this connection. If that fails, the
	// connection stays, so the page can show the SQL and ask.
	if r.URL.Query().Get("dropRole") == "1" {
		if p := s.cfg.profiles.get(r.PathValue("name")); p != nil && p.CreatedRole != "" {
			if err := s.dropCreatedRole(r.Context(), p); err != nil {
				code := http.StatusBadGateway
				var pe *pgconn.PgError
				if errors.As(err, &pe) && pe.Code == "42501" {
					code = http.StatusForbidden
				}
				writeJSON(w, code, map[string]any{"error": pgErrorJSON(err)})
				return
			}
		}
	}
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

func (s *server) listDatabases(w http.ResponseWriter, r *http.Request) {
	p := s.cfg.profiles.get(r.PathValue("name"))
	if p == nil {
		writeErr(w, http.StatusNotFound, "no such connection")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	names, def, err := s.sessions.listDatabases(ctx, p)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": pgErrorJSON(err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": names, "default": def})
}

/* ---------- sessions ---------- */

func (s *server) openSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Profile  string `json:"profile"`
		Database string `json:"database"` // optional: another database on the same server
	}
	if !readJSON(w, r, &in) {
		return
	}
	p := s.cfg.profiles.get(in.Profile)
	if p == nil {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("no connection called %q on this pgquire server — if it wasn't remembered, it was forgotten when pgquire stopped; connect again", in.Profile))
		return
	}
	sess, err := s.sessions.open(r.Context(), p, in.Database)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": pgErrorJSON(err)})
		return
	}
	pc := sess.conn.PgConn()
	_, encrypted := pc.Conn().(*tls.Conn)
	out := map[string]any{
		"tls": encrypted,
		"id":  sess.id, "profile": p.public(), "readOnly": p.ReadOnly, "database": sess.conn.Config().Database,
		"serverVersion": pc.ParameterStatus("server_version"), "maxRows": s.cfg.maxRows,
	}
	// What this login may actually do — the page warns when "read-only" is only a guard rail.
	if ri, err := checkRole(r.Context(), sess.conn); err == nil {
		out["role"] = ri
	}
	writeJSON(w, http.StatusOK, out)
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
// statement may or may not have run, so the page reconnects but doesn't retry). X-Pgquire-Tx
// carries the transaction status, so the page knows when a BEGIN of the user's is still open.
func (s *server) reply(w http.ResponseWriter, sess *session, v any, err error) {
	w.Header().Set("X-Pgquire-Tx", sess.tx())
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
	err = sess.run(r.Context(), in.Req, func(ctx context.Context, c pgConn) error {
		var e error
		res, e = queryParams(ctx, c, in.SQL, params, s.cfg.maxRows)
		return e
	})
	s.reply(w, sess, res, err)
}

func (s *server) exec(w http.ResponseWriter, r *http.Request, sess *session) {
	var in struct {
		SQL string `json:"sql"`
		Req uint64 `json:"req"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var res []result
	err := sess.run(r.Context(), in.Req, func(ctx context.Context, c pgConn) error {
		var e error
		res, e = execScript(ctx, c, in.SQL, s.cfg.maxRows)
		return e
	})
	s.reply(w, sess, res, err)
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	sess := s.sessions.get(r.PathValue("id"))
	req, _ := strconv.ParseUint(r.URL.Query().Get("req"), 10, 64) // absent: whatever is running
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": sess != nil && sess.interrupt(req)})
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
