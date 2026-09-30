// Command gen writes index.html (GitHub Pages, PGlite only) from index-remote.html.
// Run through `go generate` at the repository root.
package main

import (
	"log"
	"os"

	"github.com/abiddotdev/pgquire/internal/pages"
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
	if err := os.WriteFile("index.html", out, 0o644); err != nil {
		log.Fatal(err)
	}
}
