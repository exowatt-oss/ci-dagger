package toolchain

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The script runs against /src in the container; here it runs against a temp
// copy of the same layout (the path is the only substitution).
func TestFreshSourcesScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	root := t.TempDir()
	old := time.Unix(315532800, 0) // what some CI workflows may stamp
	files := []string{"src/lib.rs", "Cargo.toml", "crates/a/build.rs", "testdata/golden.txt",
		"node_modules/x/index.js", "web/node_modules/y/index.js", ".venv/bin/python", ".git/HEAD"}
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("lib.rs", filepath.Join(root, "src/link.rs")); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(FreshSourcesScript, "/src", root)
	if script == FreshSourcesScript {
		t.Fatal("script does not target /src")
	}
	start := time.Now().Add(-time.Second)
	out, err := exec.Command("bash", "-c", "set -euo pipefail\n"+script).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	for _, f := range files {
		st, err := os.Lstat(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		dep := strings.Contains(f, "node_modules") || strings.HasPrefix(f, ".venv") || strings.HasPrefix(f, ".git")
		switch {
		case dep && !st.ModTime().Equal(old):
			t.Errorf("%s: dependency/VCS tree touched (mtime %v)", f, st.ModTime())
		case !dep && st.ModTime().Before(start):
			t.Errorf("%s: still stale (mtime %v)", f, st.ModTime())
		}
	}
	// The symlink itself is stamped (-h), and its target is not dereferenced into a no-op.
	if st, err := os.Lstat(filepath.Join(root, "src/link.rs")); err != nil || st.ModTime().Before(start) {
		t.Errorf("symlink not stamped: %v %v", st, err)
	}
	// A file the script cannot stamp is an error, never a silent stale run
	// (a touch that fails, e.g. a file owned by another user).
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "touch"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+script+"\necho reached")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err == nil || strings.Contains(string(out), "reached") || !strings.Contains(string(out), "refusing") {
		t.Errorf("an unstampable file must fail the step: err=%v out=%s", err, out)
	}
}
