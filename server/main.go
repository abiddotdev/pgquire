// Command pgquire serves the pgquire workbench and lets it talk to real Postgres servers.
//
// The page is index-remote.html: the single-file workbench (PGlite in the browser) plus remote support.
// Served from here, it finds /api and can open remote connections, which appear next to the
// in-browser databases. Only loopback clients holding the startup token can use the API.
// index.html, the GitHub Pages copy, is generated from it without the remote parts (internal/pages).
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
var Version = "1.0"

//go:embed index-remote.html
var indexHTML []byte

func main() {
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
		token:     *token,
		htmlPath:  *htmlPath,
		maxRows:   *maxRows,
		stmtTO:    *stmtTO,
		idleTO:    *idleTO,
		profiles:  profiles,
		port:      addr.Port,
		domain:    *domain,
		allowHost: !addr.IP.IsLoopback() && *domain == "",
	})
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}

	url := fmt.Sprintf("http://%s/?t=%s", browserHost(addr), *token)
	fmt.Printf("pgquire %s\n  open: %s\n", Version, url)
	if *domain != "" {
		fmt.Printf("    or: https://%s/?t=%s\n", *domain, *token)
	}
	fmt.Printf("  connections: %s (%d saved)\n", *cfgPath, profiles.count())
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
	if a.IP.IsUnspecified() || a.IP.IsLoopback() {
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
