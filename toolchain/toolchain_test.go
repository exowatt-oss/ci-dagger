package toolchain

import (
	"reflect"
	"strings"
	"testing"
)

func TestPnpmVersion(t *testing.T) {
	cases := []struct {
		explicit, pj, want string
		err                bool
	}{
		{"", `{"packageManager":"pnpm@9.15.9"}`, "9.15.9", false},
		{"", `{"packageManager":"pnpm@10.4.1+sha512.c753b6c3ad7afa13af388fa6d808035a008e30ea9993f58c6663e2bc5ff21679aa834db094987129aa4d488b86df57f7b634981b2f827cdcacc698cc0cfb88af"}`, "10.4.1", false},
		{"9", `{"packageManager":"pnpm@10.4.1"}`, "9", false},
		{"", `{"name":"x"}`, "", true},
		{"", `{"packageManager":"yarn@4.1.0"}`, "", true},
		{"", ``, "", true},
		{"", `not json`, "", true},
		{"latest; rm -rf /", `{}`, "", true},
	}
	for _, c := range cases {
		got, err := PnpmVersion(c.explicit, c.pj)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("PnpmVersion(%q,%q) = %q,%v; want %q, err=%v", c.explicit, c.pj, got, err, c.want, c.err)
		}
	}
}

func TestPnpmInstallScript(t *testing.T) {
	s := PnpmInstallScript("pnpm install --frozen-lockfile")
	for _, want := range []string{`timeout -k 15 "${PNPM_INSTALL_TIMEOUT:-300}" pnpm install --frozen-lockfile && break`, "unset npm_config_registry pnpm_config_registry", `-ge 3`} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
}

func TestGoVersion(t *testing.T) {
	cases := []struct {
		explicit, mod, want string
		err                 bool
	}{
		{"", "module x\n\ngo 1.24\n", "1.24", false},
		{"", "module x\n\ngo 1.24.5\n\ntoolchain go1.26.5\n", "1.26.5", false},
		{"", "module x\ngo 1.23.0 // comment\n", "1.23.0", false},
		{"1.25", "module x\ngo 1.22\n", "1.25", false},
		{"go1.25.1", "", "1.25.1", false},
		{"", "module x\n", "", true},
		{"", "module x\ngo 1.26rc1\n", "", true},
		{"latest", "", "", true},
	}
	for _, c := range cases {
		got, err := GoVersion(c.explicit, c.mod)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("GoVersion(%q,%q) = %q,%v; want %q, err=%v", c.explicit, c.mod, got, err, c.want, c.err)
		}
	}
	if GoImage("1.24") != "public.ecr.aws/docker/library/golang:1.24-bookworm" {
		t.Error(GoImage("1.24"))
	}
}

func TestRustToolchainFile(t *testing.T) {
	ch, comps, err := RustToolchainFile(`
# pinned
[toolchain]
channel = "1.89.0"
components = ["rustfmt", "clippy"]  # both
profile = "minimal"
`)
	if err != nil || ch != "1.89.0" || !reflect.DeepEqual(comps, []string{"rustfmt", "clippy"}) {
		t.Fatalf("got %q %v %v", ch, comps, err)
	}
	if ch, _, err := RustToolchainFile("stable\n"); err != nil || ch != "stable" {
		t.Fatalf("legacy: %q %v", ch, err)
	}
	if ch, _, err := RustToolchainFile("nightly-2026-01-01"); err != nil || ch != "nightly-2026-01-01" {
		t.Fatalf("dated: %q %v", ch, err)
	}
	if ch, c, err := RustToolchainFile(""); ch != "" || c != nil || err != nil {
		t.Fatal("empty")
	}
	if _, _, err := RustToolchainFile("[toolchain]\nchannel = \"$(evil)\"\n"); err == nil {
		t.Fatal("bad channel accepted")
	}
	if _, _, err := RustToolchainFile("rm -rf /"); err == nil {
		t.Fatal("bad legacy accepted")
	}
}

func TestRustSetupScript(t *testing.T) {
	s := RustSetupScript("1.89.0", []string{"clippy", "rustfmt"})
	if !strings.Contains(s, "rustup toolchain install '1.89.0'") || !strings.Contains(s, "rustup default '1.89.0'") ||
		!strings.Contains(s, "rustup component add 'clippy' 'rustfmt'") {
		t.Fatal(s)
	}
	s = RustSetupScript("", nil)
	if !strings.Contains(s, "rust-toolchain.toml") || strings.Contains(s, "component add") {
		t.Fatal(s)
	}
}

func TestCargoStubs(t *testing.T) {
	got := CargoStubs([]string{"Cargo.toml", "crates/a/Cargo.toml", "crates/a/Cargo.toml", "Cargo.lock"})
	want := []string{"crates/a/src/lib.rs", "crates/a/src/main.rs", "src/lib.rs", "src/main.rs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestPythonImage(t *testing.T) {
	if img, err := PythonImage("3.12"); err != nil || img != "python:3.12-slim-bookworm" {
		t.Fatal(img, err)
	}
	for _, bad := range []string{"", "2.7", "3", "3.12; x"} {
		if _, err := PythonImage(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPipInstallScript(t *testing.T) {
	if PipInstallScript(nil, []string{" "}) != "" {
		t.Fatal("empty install should be empty")
	}
	s := PipInstallScript([]string{"requirements.txt"}, []string{"miss_hit", "pytest==8.3.5"})
	if !strings.Contains(s, "-r 'requirements.txt' 'miss_hit' 'pytest==8.3.5'") || !strings.Contains(s, "PIP_INSTALL_TIMEOUT") {
		t.Fatal(s)
	}
}

func TestCheckImageRef(t *testing.T) {
	pinned := "public.ecr.aws/docker/library/alpine:3.22@sha256:" + strings.Repeat("a", 64)
	if err := CheckImageRef(pinned, false); err != nil {
		t.Fatal(err)
	}
	if err := CheckImageRef("alpine:3.22", false); err == nil {
		t.Fatal("unpinned accepted")
	}
	if err := CheckImageRef("alpine:3.22", true); err != nil {
		t.Fatal(err)
	}
	if err := CheckImageRef("", true); err == nil {
		t.Fatal("empty accepted")
	}
	if err := CheckImageRef("alpine 3", true); err == nil {
		t.Fatal("space accepted")
	}
}

func TestCargoTargetStubs(t *testing.T) {
	m := `[package]
name = "example-project"
build = "build/main.rs"

[lib]
path = "lib/mod.rs"

[[bench]]
name = "dispatch"
harness = false

[[bin]]
name = "tool"
path = "cmd/tool.rs"

[[test]]
name = "e2e"

[[example]]
name = "demo"

[dependencies]
name = "ignored"
path = "../not-a-target"
`
	got := CargoTargetStubs("crates/example-project", m)
	want := []string{"crates/example-project/benches/dispatch.rs", "crates/example-project/build/main.rs", "crates/example-project/cmd/tool.rs",
		"crates/example-project/examples/demo.rs", "crates/example-project/lib/mod.rs", "crates/example-project/tests/e2e.rs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if got := CargoTargetStubs(".", "[[bench]]\nname = \"x\"\npath = \"/etc/passwd\"\n"); len(got) != 0 {
		t.Fatalf("absolute path accepted: %v", got)
	}
}

func TestCIUserScript(t *testing.T) {
	s := CIUserScript([]string{"/src", " ", "/pnpm-store"})
	for _, want := range []string{"useradd -m -u 1000", "chown -R 1000:1000 '/src'", "chown -R 1000:1000 '/pnpm-store'", "safe.directory"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}
