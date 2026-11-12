package toolchain

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerusPin(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	cases := []struct {
		v, sum, url, want string
		err               bool
	}{
		{"", "", "", "", false},
		{"0.2026.10.26.d17fbd0", sum, "", DefaultVerusURL("0.2026.10.26.d17fbd0"), false},
		{"0.2026.10.26.d17fbd0", strings.ToUpper(sum), "https://mirror.example/v.zip", "https://mirror.example/v.zip", false},
		{"0.2026.10.26.d17fbd0", "", "", "", true},                // no sha256: refused
		{"0.2026.10.26.d17fbd0", "abc", "", "", true},             // not a sha256
		{"0.2026.10.26.d17fbd0", sum, "http://x/v.zip", "", true}, // not https
		{"0.2026.10.26.d17fbd0", sum, "https://x/$(id).zip", "", true},
		{"1.0; rm -rf /", sum, "", "", true},
		{"", sum, "", "", true}, // sha256 without a version
	}
	for _, c := range cases {
		got, err := VerusPin(c.v, c.sum, c.url)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("VerusPin(%q, %q, %q) = %q, %v; want %q, err=%v", c.v, c.sum, c.url, got, err, c.want, c.err)
		}
	}
	if u := DefaultVerusURL("0.2026.10.26.d17fbd0"); u != "https://github.com/verus-lang/verus/releases/download/release%2F0.2026.10.26.d17fbd0/verus-0.2026.10.26.d17fbd0-x86-linux.zip" {
		t.Errorf("DefaultVerusURL = %s", u)
	}
}

func writeZip(t *testing.T, path string, files map[string]string) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for name, body := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o644)
		if strings.HasSuffix(name, "/verus") || name == "verus" {
			h.SetMode(0o755)
		}
		fw, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestVerusInstallScript(t *testing.T) {
	for _, tool := range []string{"bash", "unzip", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("needs " + tool)
		}
	}
	dir := t.TempDir()
	run := func(zipPath, sum, dest string) (string, error) {
		out, err := exec.Command("bash", "-c", verusInstallScript(zipPath, sum, dest)).CombinedOutput()
		return string(out), err
	}
	// A release-shaped zip installs, with the binary executable.
	good := filepath.Join(dir, "good.zip")
	sum := writeZip(t, good, map[string]string{"verus-x86-linux/verus": "#!/bin/sh\necho verus\n", "verus-x86-linux/vstd.vir": "v"})
	dest := filepath.Join(dir, "opt-verus")
	if out, err := run(good, sum, dest); err != nil {
		t.Fatalf("good zip: %v\n%s", err, out)
	}
	if st, err := os.Stat(filepath.Join(dest, "verus")); err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("verus not installed executable: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "vstd.vir")); err != nil {
		t.Fatal(err)
	}
	// A flat zip works too.
	flat := filepath.Join(dir, "flat.zip")
	fsum := writeZip(t, flat, map[string]string{"verus": "#!/bin/sh\n", "z3": "z"})
	if out, err := run(flat, fsum, filepath.Join(dir, "flat-dest")); err != nil {
		t.Fatalf("flat zip: %v\n%s", err, out)
	}
	// Any byte of difference from the pin is refused.
	if out, err := run(good, strings.Repeat("0", 64), filepath.Join(dir, "x")); err == nil || !strings.Contains(out, "does not match") {
		t.Errorf("wrong sha256 accepted: %v %s", err, out)
	}
	// A zip without a single executable verus is refused.
	bad := filepath.Join(dir, "bad.zip")
	bsum := writeZip(t, bad, map[string]string{"a/verus": "#!/bin/sh\n", "b/verus": "#!/bin/sh\n"})
	if out, err := run(bad, bsum, filepath.Join(dir, "y")); err == nil || !strings.Contains(out, "exactly one") {
		t.Errorf("ambiguous zip accepted: %v %s", err, out)
	}
}
