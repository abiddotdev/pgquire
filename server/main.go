// Command pgquire serves the pgquire workbench and lets it talk to real Postgres servers.
//
// The page is index-remote.html: the single-file workbench (PGlite in the browser) plus remote support.
// Served from here, it finds /api and can open remote connections, which appear next to the
// in-browser databases. Only loopback clients holding the startup token can use the API.
// index.html, the GitHub Pages copy, is generated from it without the remote parts (internal/pages).
//
// Local AI agents reach databases the user shared with them over MCP: POST /mcp, or `pgquire mcp`
// on stdio (agent.go, mcp.go).
package main

//go:generate go run ./internal/pages/gen

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Version is the pgquire release; keep it in step with APP_VERSION in index-remote.html.
var Version = "1.0.4"

//go:embed index-remote.html
var indexHTML []byte

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		mcpMain(os.Args[2:])
		return
	}
	var (
		listen   = flag.String("listen", "127.0.0.1:8432", "address to listen on (keep it on loopback)")
		noOpen   = flag.Bool("no-open", false, "don't open a browser")
		htmlPath = flag.String("html", "", "serve this HTML file from disk instead of the built-in copy (for development)")
		cfgPath  = flag.String("config", "", "saved connections file (default: <user config dir>/pgquire/connections.json)")
		maxRows  = flag.Int("max-rows", 50000, "most rows returned per result; the rest are counted but not sent")
		stmtTO   = flag.Duration("statement-timeout", 5*time.Minute, "statement_timeout for remote sessions (0 = server default)")
		idleTO   = flag.Duration("idle-timeout", 30*time.Minute, "close remote sessions idle this long")
		token    = flag.String("token", "", "access token (default: random each start)")
		domain   = flag.String("domain", "", "host name pgquire is reached by, e.g. behind a proxy; requests for other names are refused")
		mcpHTTP  = flag.Bool("mcp-http", false, "let AI agents connect over HTTP at /mcp with the agent token (pgquire in Docker or on a server; use HTTPS beyond this machine). Off: only `pgquire mcp` on this machine")
		dsn      = flag.String("dsn", "", "connect to this Postgres at start and open it in the page, e.g. postgres://user@host/db (kept for this run only; a password can come from ~/.pgpass)")
		showVer  = flag.Bool("version", false, "print the version and exit")
	)
	describeEnv(flag.CommandLine)
	flag.Parse()
	if err := applyEnv(flag.CommandLine, os.Getenv); err != nil {
		log.Fatal(err)
	}
	if *showVer {
		fmt.Println("pgquire", Version)
		return
	}

	if *cfgPath == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			log.Fatalf("can't find a config directory: %v (use -config)", err)
		}
		*cfgPath = filepath.Join(dir, "pgquire", "connections.json")
	}
	profiles, err := loadProfiles(*cfgPath)
	if err != nil {
		log.Fatalf("reading %s: %v", *cfgPath, err)
	}
	if *token == "" {
		*token = randomHex(24)
	}
	*domain = strings.ToLower(strings.TrimSpace(*domain))
	if strings.ContainsAny(*domain, ":/") {
		log.Fatalf("-domain takes a host name only, like pgquire.example.com (got %q)", *domain)
	}

	// The agent token lasts across restarts, so an agent set up with it over HTTP keeps working.
	files := agentFiles{token: filepath.Join(filepath.Dir(*cfgPath), "agent-token"), info: filepath.Join(filepath.Dir(*cfgPath), "agent.json")}
	agentToken, err := loadAgentToken(files.token)
	if err != nil {
		log.Printf("warning: keeping the agent token: %v — it changes each start", err)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		log.Printf("warning: listening on %s, not loopback — anyone who can reach it and has the token can use your connections", addr)
		if *domain == "" {
			log.Printf("warning: no -domain set, so requests for any host name are accepted; set it to the name you reach pgquire by")
		}
	}

	srv := newServer(serverConfig{
		token:      *token,
		agentToken: agentToken,
		agentFiles: files,
		mcpHTTP:    *mcpHTTP,
		htmlPath:   *htmlPath,
		maxRows:    *maxRows,
		stmtTO:     *stmtTO,
		idleTO:     *idleTO,
		profiles:   profiles,
		port:       addr.Port,
		domain:     *domain,
		allowHost:  !addr.IP.IsLoopback() && *domain == "",
	})
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}

	saved := profiles.count() // before -dsn adds its own, which isn't saved
	var connected string
	if *dsn != "" {
		if connected, err = srv.connectAtStart(*dsn); err != nil {
			log.Fatalf("-dsn: %v", err)
		}
	}

	url := fmt.Sprintf("http://%s/?t=%s", browserHost(addr), *token)
	fmt.Printf("pgquire %s\n  open: %s\n", Version, url)
	if *domain != "" {
		fmt.Printf("    or: https://%s/?t=%s\n", *domain, *token)
	}
	if connected != "" {
		fmt.Printf("  connected: %s\n", connected)
	}
	fmt.Printf("  connections: %s (%d saved)\n", *cfgPath, saved)
	if *mcpHTTP {
		fmt.Printf("  agents over HTTP: on, at /mcp with the token in %s\n", files.token)
	}
	// For `pgquire mcp`: where this server is, while it runs.
	agentPath := files.info
	if err := writeAgentFile(agentPath, agentInfo{URL: "http://" + browserHost(addr), Token: agentToken, PID: os.Getpid()}); err != nil {
		log.Printf("warning: writing %s: %v — `pgquire mcp` won't find this server", agentPath, err)
	}
	defer removeAgentFile(agentPath)
	if !*noOpen {
		openBrowser(url)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	srv.sessions.closeAll()
}

// envFlags: options that can also come from the environment (Docker, services). A flag on the
// command line wins over its variable.
var envFlags = []struct{ flag, env string }{
	{"listen", "PGQUIRE_LISTEN"},
	{"no-open", "PGQUIRE_NO_OPEN"},
	{"config", "PGQUIRE_CONFIG"},
	{"max-rows", "PGQUIRE_MAX_ROWS"},
	{"statement-timeout", "PGQUIRE_STATEMENT_TIMEOUT"},
	{"idle-timeout", "PGQUIRE_IDLE_TIMEOUT"},
	{"token", "PGQUIRE_TOKEN"},
	{"domain", "PGQUIRE_DOMAIN"},
	{"dsn", "PGQUIRE_DSN"},
	{"mcp-http", "PGQUIRE_MCP_HTTP"},
}

// describeEnv names each option's variable in -h.
func describeEnv(fs *flag.FlagSet) {
	for _, e := range envFlags {
		f := fs.Lookup(e.flag)
		f.Usage += " (env " + e.env + ")"
	}
}

// applyEnv sets, from the environment, the options not given on the command line.
func applyEnv(fs *flag.FlagSet, getenv func(string) string) error {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	for _, e := range envFlags {
		if v := getenv(e.env); v != "" && !given[e.flag] {
			if err := fs.Set(e.flag, v); err != nil {
				return fmt.Errorf("%s=%q: %v", e.env, v, err)
			}
		}
	}
	return nil
}

func browserHost(a *net.TCPAddr) string {
	switch {
	case a.IP.IsLoopback() && a.IP.To4() == nil: // listening on [::1] only: 127.0.0.1 wouldn't reach it
		return fmt.Sprintf("[::1]:%d", a.Port)
	case a.IP.IsUnspecified() || a.IP.IsLoopback():
		return fmt.Sprintf("127.0.0.1:%d", a.Port)
	}
	return a.String()
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		go cmd.Wait()
	}
}
