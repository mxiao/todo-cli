package web

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(Assets, name)
	if err != nil {
		t.Fatalf("embedded %s: %v", name, err)
	}
	return string(b)
}

// Every file the page loads is embedded, so the binary serves a complete UI.
func TestPageReferencesResolve(t *testing.T) {
	index := read(t, "index.html")
	refs := regexp.MustCompile(`(?:src|href)="/([^"]+)"`).FindAllStringSubmatch(index, -1)
	if len(refs) < 2 {
		t.Fatalf("index.html loads %d assets", len(refs))
	}
	for _, m := range refs {
		read(t, m[1])
	}
	importRe := regexp.MustCompile(`(?m)^\s*(?:import|export)\b[^'"]*from\s+'(\.[^']+)'`)
	files, _ := fs.Glob(Assets, "js/*.js")
	for _, f := range files {
		for _, m := range importRe.FindAllStringSubmatch(read(t, f), -1) {
			target := path.Join(path.Dir(f), m[1])
			if _, err := fs.Stat(Assets, target); err != nil {
				t.Errorf("%s imports missing %s", f, m[1])
			}
		}
	}
}

// The page runs under a strict Content-Security-Policy (no inline code).
func TestNoInlineCode(t *testing.T) {
	index := read(t, "index.html")
	if regexp.MustCompile(`<script(?:\s[^>]*)?>\s*[^<\s]`).MatchString(index) {
		t.Error("index.html has an inline script")
	}
	for _, bad := range []*regexp.Regexp{regexp.MustCompile(`\sstyle="`), regexp.MustCompile(`\son[a-z]+="`), regexp.MustCompile(`<style`)} {
		if bad.MatchString(index) {
			t.Errorf("index.html matches %s (blocked by the CSP)", bad)
		}
	}
	files, _ := fs.Glob(Assets, "js/*.js")
	for _, f := range files {
		src := read(t, f)
		for _, bad := range []string{".innerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s uses %s; build DOM nodes with dom.js instead", f, bad)
			}
		}
	}
}

// Every element the scripts look up by id exists in index.html.
func TestScriptElementIDsExist(t *testing.T) {
	index := read(t, "index.html")
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(index, -1) {
		if ids[m[1]] {
			t.Errorf("duplicate id %q", m[1])
		}
		ids[m[1]] = true
	}
	files, _ := fs.Glob(Assets, "js/*.js")
	lookup := regexp.MustCompile(`\$\$?\('#([A-Za-z][\w-]*)`)
	for _, f := range files {
		for _, m := range lookup.FindAllStringSubmatch(read(t, f), -1) {
			if !ids[m[1]] {
				t.Errorf("%s looks up #%s, which index.html does not define", f, m[1])
			}
		}
	}
}
