// Command gen writes index.html (GitHub Pages, PGlite only) from index-remote.html.
// Run through `go generate` in server/; index.html goes to the repository root for GitHub Pages.
package main

import (
	"log"
	"os"

	"github.com/abiddotdev/pgquire/server/internal/pages"
)

func main() {
	src, err := os.ReadFile("index-remote.html")
	if err != nil {
		log.Fatal(err)
	}
	out, err := pages.Strip(src)
	if err != nil {
		log.Fatalf("index-remote.html: %v", err)
	}
	if err := os.WriteFile("../index.html", out, 0o644); err != nil {
		log.Fatal(err)
	}
}
