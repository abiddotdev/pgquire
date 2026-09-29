// Command pgquire serves the pgquire workbench and lets it talk to real Postgres servers.
//
// The page is the same single-file index.html that runs on its own (PGlite in the browser).
// Served from here, it also finds /api and can open remote connections, which appear next to the
// in-browser databases. Only loopback clients holding the startup token can use the API.
package main

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
	"syscall"
	"time"
)

// Version is the pgquire release; keep it in step with APP_VERSION in index.html.
var Version = "1.0"

//go:embed index.html
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
		token    = flag.String("token", os.Getenv("PGQUIRE_TOKEN"), "access token (default: random each start; env PGQUIRE_TOKEN)")
		showVer  = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
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

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		log.Printf("warning: listening on %s, not loopback — anyone who can reach it and has the token can use your connections", addr)
	}

	srv := newServer(serverConfig{
		token:     *token,
		htmlPath:  *htmlPath,
		maxRows:   *maxRows,
		stmtTO:    *stmtTO,
		idleTO:    *idleTO,
		profiles:  profiles,
		port:      addr.Port,
		allowHost: !addr.IP.IsLoopback(),
	})
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}

	url := fmt.Sprintf("http://%s/?t=%s", browserHost(addr), *token)
	fmt.Printf("pgquire %s\n  open: %s\n  connections: %s (%d saved)\n", Version, url, *cfgPath, profiles.count())
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
