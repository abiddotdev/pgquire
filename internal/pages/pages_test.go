package pages

import (
	"strings"
	"testing"
)

func TestStrip(t *testing.T) {
	src := strings.Join([]string{
		"<!DOCTYPE html>",
		".a { x: 1; }",
		".conn { y: 2; } /* @remote */",
		"<i id=dot></i> <!-- @remote -->",
		"const cap = 5; // @remote",
		"  /* @remote { */",
		"  class RemoteDb {}",
		"  /* } @remote */",
		"  const ro = a/*remote:*/ && !readOnly()/*:remote*/ && b/*remote:*/ || c/*:remote*/;",
		"  // @pages: showSample();",
		"end",
	}, "\n")
	want := strings.Join([]string{
		"<!DOCTYPE html>",
		Header,
		".a { x: 1; }",
		"  const ro = a && b;",
		"  showSample();",
		"end",
	}, "\n")
	got, err := Strip([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestStripRejectsBadMarkers(t *testing.T) {
	for name, src := range map[string]string{
		"unclosed block": "x\n/* @remote { */\ny",
		"stray close":    "x\n/* } @remote */",
		"nested block":   "/* @remote { */\n/* @remote { */\n/* } @remote */",
		"split inline":   "a /*remote:*/ b\nc /*:remote*/",
		"mid-line mark":  "a /* @remote */ b",
	} {
		if _, err := Strip([]byte(src)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
