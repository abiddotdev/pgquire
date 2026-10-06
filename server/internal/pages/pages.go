// Package pages derives index.html, the GitHub Pages copy of pgquire (PGlite only), from
// index-remote.html, the page the server embeds, by dropping everything marked remote-only:
//
//	a line ending in  /* @remote */  or  // @remote  or  <!-- @remote -->
//	the lines from    /* @remote { */  to  /* } @remote */  (each marker alone on its line)
//	inline            /*remote:*/ … /*:remote*/  (opened and closed on the same line; JS and CSS
//	                  only — in HTML markup or inside a `template string` it isn't a comment)
//
// and by switching on Pages-only lines, kept commented out in the remote page:
//
//	// @pages: <code>   becomes   <code>   (same indent)
//
// The copy starts with a note (after the doctype) saying where it comes from.
package pages

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

var (
	lineMarks = []string{"/* @remote */", "// @remote", "<!-- @remote -->"}
	inline    = regexp.MustCompile(`/\*remote:\*/.*?/\*:remote\*/`)
	pagesOnly = regexp.MustCompile(`^(\s*)// @pages: (.*)$`)
)

// Header goes after the first line (the doctype) of the Pages copy.
const Header = "<!-- Generated from index-remote.html by `go generate` — edit that file, not this one. -->"

const (
	blockOpen  = "/* @remote { */"
	blockClose = "/* } @remote */"
)

// Strip returns src without its remote-only parts.
func Strip(src []byte) ([]byte, error) {
	lines := strings.Split(string(src), "\n")
	out := make([]string, 0, len(lines))
	block := 0 // line where the open block started; 0 = none
	for i, l := range lines {
		n, t := i+1, strings.TrimSpace(l)
		switch {
		case t == blockOpen:
			if block != 0 {
				return nil, fmt.Errorf("line %d: remote block inside the one opened on line %d", n, block)
			}
			block = n
			continue
		case t == blockClose:
			if block == 0 {
				return nil, fmt.Errorf("line %d: remote block closed but never opened", n)
			}
			block = 0
			continue
		case block != 0:
			continue
		}
		if hasLineMark(l) {
			continue
		}
		if m := pagesOnly.FindStringSubmatch(l); m != nil {
			l = m[1] + m[2]
		}
		l = inline.ReplaceAllString(l, "")
		if strings.Contains(l, "@remote") || strings.Contains(l, "remote:*/") || strings.Contains(l, "// @pages") {
			return nil, fmt.Errorf("line %d: stray remote marker: %s", n, strings.TrimSpace(l))
		}
		out = append(out, l)
	}
	if block != 0 {
		return nil, fmt.Errorf("line %d: remote block never closed", block)
	}
	if len(out) > 0 {
		out = append(out[:1], append([]string{Header}, out[1:]...)...)
	}
	return []byte(strings.Join(out, "\n")), nil
}

func hasLineMark(l string) bool {
	l = strings.TrimRight(l, " \t\r")
	for _, m := range lineMarks {
		if strings.HasSuffix(l, m) {
			return true
		}
	}
	return false
}

// Same reports whether gen is what Strip makes of src.
func Same(src, gen []byte) (bool, error) {
	want, err := Strip(src)
	if err != nil {
		return false, err
	}
	return bytes.Equal(want, gen), nil
}
