// Package toolchain holds the pure (engine-free, unit-tested) logic of the
// generic CI toolchain primitives: pnpm, go, rust, python-plain and
// container-run. Version detection from the repo's own manifests, the install
// and check scripts, and the argument validation live here so they can be
// tested with `go test ./toolchain/` without a Dagger engine.
package toolchain

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------- pnpm

var pnpmVersionRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}([-+.][0-9A-Za-z.-]+)?$`)

// PnpmVersion returns the pnpm version to install: the explicit --pnpm value
// when set, else the version in package.json's "packageManager" field
// ("pnpm@9.15.9" or "pnpm@9.15.9+sha512.…", the field pnpm/action-setup and
// corepack read). The integrity suffix is dropped (npm installs by version).
func PnpmVersion(explicit, packageJSON string) (string, error) {
	if v := strings.TrimSpace(explicit); v != "" {
		if !pnpmVersionRE.MatchString(v) {
			return "", fmt.Errorf("--pnpm %q is not a version (e.g. 9, 9.15, 9.15.9)", v)
		}
		return v, nil
	}
	if strings.TrimSpace(packageJSON) == "" {
		return "", fmt.Errorf("no package.json and no --pnpm version")
	}
	var pj struct {
		PackageManager string `json:"packageManager"`
	}
	if err := json.Unmarshal([]byte(packageJSON), &pj); err != nil {
		return "", fmt.Errorf("package.json: %w", err)
	}
	pm := strings.TrimSpace(pj.PackageManager)
	if pm == "" {
		return "", fmt.Errorf(`package.json has no "packageManager" field; pass --pnpm=<version> (pnpm/action-setup needs one too)`)
	}
	name, ver, ok := strings.Cut(pm, "@")
	if !ok || name != "pnpm" {
		return "", fmt.Errorf("packageManager %q is not pnpm", pm)
	}
	if i := strings.Index(ver, "+"); i >= 0 {
		ver = ver[:i]
	}
	if !pnpmVersionRE.MatchString(ver) {
		return "", fmt.Errorf("packageManager %q has no usable pnpm version", pm)
	}
	return ver, nil
}

// PnpmInstallScript is the bounded, retried pnpm install (same contract as
// npm's npmCiBounded): each attempt is capped (PNPM_INSTALL_TIMEOUT, default
// 300 s), up to 3 attempts, and with a registry proxy configured the last
// attempt goes straight to registry.npmjs.org. args are appended verbatim
// (already shell-quoted by the caller).
func PnpmInstallScript(cmd string) string {
	return `attempt=1
while :; do
  if [ "$attempt" -ge 3 ] && [ -n "${npm_config_registry:-}${pnpm_config_registry:-}" ]; then
    echo "pnpm attempt $attempt: bypassing ${pnpm_config_registry:-$npm_config_registry}, going to registry.npmjs.org" >&2
    unset npm_config_registry pnpm_config_registry
  fi
  timeout -k 15 "${PNPM_INSTALL_TIMEOUT:-300}" ` + cmd + ` && break
  rc=$?
  if [ "$attempt" -ge 3 ]; then echo "pnpm failed (rc=$rc) after $attempt attempts" >&2; exit "$rc"; fi
  echo "pnpm attempt $attempt failed (rc=$rc; 124 = timed out); retrying" >&2
  attempt=$((attempt + 1))
done`
}

// ---------------------------------------------------------------- go

var (
	goDirectiveRE = regexp.MustCompile(`(?m)^go[ \t]+([0-9]+\.[0-9]+(?:\.[0-9]+)?(?:rc[0-9]+)?)[ \t]*(?://.*)?$`)
	goToolchainRE = regexp.MustCompile(`(?m)^toolchain[ \t]+go([0-9]+\.[0-9]+(?:\.[0-9]+)?)[ \t]*(?://.*)?$`)
	goVersionRE   = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)
)

// GoVersion returns the Go version for the golang image: --go when set, else
// go.mod's `toolchain goX.Y.Z` directive, else its `go X.Y[.Z]` directive
// (what actions/setup-go's go-version-file resolves). A bare "1.22" stays
// "1.22" (the image tag golang:1.22-bookworm tracks the newest patch).
func GoVersion(explicit, goMod string) (string, error) {
	if v := strings.TrimSpace(explicit); v != "" {
		v = strings.TrimPrefix(v, "go")
		if !goVersionRE.MatchString(v) {
			return "", fmt.Errorf("--go %q is not a Go version (e.g. 1.24 or 1.24.5)", explicit)
		}
		return v, nil
	}
	if m := goToolchainRE.FindStringSubmatch(goMod); m != nil {
		return m[1], nil
	}
	if m := goDirectiveRE.FindStringSubmatch(goMod); m != nil {
		v := m[1]
		if !goVersionRE.MatchString(v) {
			return "", fmt.Errorf("go.mod go directive %q is a pre-release; pass --go", v)
		}
		return v, nil
	}
	return "", fmt.Errorf("go.mod has no go directive; pass --go")
}

// GoImage is the official golang image (ECR Public's copy: Docker Hub
// rate-limits a shared outbound IP) for version v.
func GoImage(v string) string {
	return "public.ecr.aws/docker/library/golang:" + v + "-bookworm"
}

// DefaultGoRun is the default check chain (spec: vet + race tests).
const DefaultGoRun = "go vet ./... && go test -race ./..."

// ---------------------------------------------------------------- rust

var rustChannelRE = regexp.MustCompile(`^(stable|beta|nightly|[0-9]+\.[0-9]+(\.[0-9]+)?)(-[0-9]{4}-[0-9]{2}-[0-9]{2})?$`)

// RustToolchainFile parses rust-toolchain.toml (or the legacy one-line
// rust-toolchain file) and returns its channel and components. Empty channel
// = no file / no pin.
func RustToolchainFile(content string) (channel string, components []string, err error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", nil, nil
	}
	if !strings.Contains(content, "[toolchain]") && !strings.Contains(content, "=") {
		ch := strings.TrimSpace(strings.SplitN(content, "\n", 2)[0])
		if !rustChannelRE.MatchString(ch) {
			return "", nil, fmt.Errorf("rust-toolchain %q is not a channel", ch)
		}
		return ch, nil, nil
	}
	inTC := false
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inTC = line == "[toolchain]"
			continue
		}
		if !inTC {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "channel":
			channel = strings.Trim(v, `"'`)
		case "components":
			components = tomlStringList(v)
		}
	}
	if channel != "" && !rustChannelRE.MatchString(channel) {
		return "", nil, fmt.Errorf("rust-toolchain.toml channel %q is not a channel", channel)
	}
	return channel, components, nil
}

func tomlStringList(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.Trim(strings.TrimSpace(p), `"'`); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// RustImage is the official rust image (ECR Public copy). The image carries
// rustup, so a rust-toolchain.toml pin is installed on top of it.
func RustImage(v string) string {
	if strings.HasSuffix(v, "-bookworm") || strings.HasSuffix(v, "-trixie") {
		// --rust=1-trixie : glibc 2.41, e.g. for Verus release
		// binaries, which need glibc >= 2.39 (bookworm has 2.36).
		return "public.ecr.aws/docker/library/rust:" + v
	}
	return "public.ecr.aws/docker/library/rust:" + v + "-bookworm"
}

// DefaultRustRun is the default check chain.
const DefaultRustRun = "cargo fmt --all -- --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --locked"

// RustSetupScript installs the toolchain the checkout selects (rustup reads
// rust-toolchain(.toml) in the working directory) plus components, then
// prints versions. channel overrides the file (--toolchain).
func RustSetupScript(channel string, components []string) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	if channel != "" {
		fmt.Fprintf(&b, "rustup toolchain install %s --profile minimal --no-self-update\nrustup default %s\n", shq(channel), shq(channel))
	} else {
		// rustup >= 1.28 needs an explicit install for a toolchain file.
		b.WriteString("if [ -f rust-toolchain.toml ] || [ -f rust-toolchain ]; then rustup toolchain install --no-self-update 2>/dev/null || rustup show active-toolchain || true; fi\n")
	}
	if len(components) > 0 {
		fmt.Fprintf(&b, "rustup component add %s\n", strings.Join(quoteAll(components), " "))
	}
	b.WriteString("rustc --version && cargo --version\n")
	return b.String()
}

// CargoStubs returns, for every crate manifest path in manifests (repo-
// relative ".../Cargo.toml"), the stub source files `cargo fetch` needs to
// parse the workspace without the real source: src/lib.rs and src/main.rs
// (empty). The dependency layer is built from manifests + Cargo.lock only.
func CargoStubs(manifests []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range manifests {
		if path.Base(m) != "Cargo.toml" {
			continue
		}
		d := path.Dir(m)
		for _, f := range []string{"src/lib.rs", "src/main.rs"} {
			p := path.Join(d, f)
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// CargoTargetStubs returns the explicit target files a manifest declares
// ([lib] path, [[bin]]/[[bench]]/[[test]]/[[example]] name/path,
// package.build), relative to the repo root (crateDir is the manifest's
// directory). `cargo fetch` refuses a manifest whose declared targets are
// missing, so the dependency layer creates them empty for the fetch only.
func CargoTargetStubs(crateDir, manifest string) []string {
	defaults := map[string]string{"bin": "src/bin/%s.rs", "bench": "benches/%s.rs", "test": "tests/%s.rs", "example": "examples/%s.rs"}
	var out []string
	add := func(p string) {
		p = strings.Trim(strings.TrimSpace(p), `"'`)
		if p == "" || path.IsAbs(p) || strings.HasPrefix(path.Clean(p), "..") {
			return
		}
		out = append(out, path.Join(crateDir, p))
	}
	section, name, pth := "", "", ""
	flush := func() {
		if f, ok := defaults[section]; ok {
			if pth != "" {
				add(pth)
			} else if name != "" {
				add(fmt.Sprintf(f, name))
			}
		} else if section == "lib" && pth != "" {
			add(pth)
		}
		name, pth = "", ""
	}
	for _, raw := range strings.Split(manifest, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if strings.HasPrefix(line, "[") {
			flush()
			section = strings.Trim(line, "[] ")
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case k == "name":
			name = strings.Trim(v, `"'`)
		case k == "path":
			pth = v
		case section == "package" && k == "build" && strings.HasPrefix(v, `"`):
			add(v)
		}
	}
	flush()
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- python-plain

var pyVersionRE = regexp.MustCompile(`^3\.[0-9]+(\.[0-9]+)?$`)

// PythonImage validates --python and returns python:<v>-slim-bookworm.
func PythonImage(v string) (string, error) {
	v = strings.TrimSpace(v)
	if !pyVersionRE.MatchString(v) {
		return "", fmt.Errorf("--python %q is not a CPython version (e.g. 3.12)", v)
	}
	return "python:" + v + "-slim-bookworm", nil
}

// PipInstallScript installs requirement files (repo-relative to the workdir)
// and loose packages into the image's interpreter (no venv; the container is
// throwaway), bounded per attempt with up to 3 attempts.
func PipInstallScript(requirements, packages []string) string {
	var args []string
	for _, r := range requirements {
		if r = strings.TrimSpace(r); r != "" {
			args = append(args, "-r", shq(r))
		}
	}
	for _, p := range packages {
		if p = strings.TrimSpace(p); p != "" {
			args = append(args, shq(p))
		}
	}
	if len(args) == 0 {
		return ""
	}
	cmd := "pip install --disable-pip-version-check --no-input --root-user-action=ignore " + strings.Join(args, " ")
	return `attempt=1
until timeout -k 15 "${PIP_INSTALL_TIMEOUT:-600}" ` + cmd + `; do
  rc=$?
  if [ "$attempt" -ge 3 ]; then echo "pip install failed (rc=$rc) after $attempt attempts" >&2; exit "$rc"; fi
  echo "pip install attempt $attempt failed (rc=$rc); retrying" >&2
  attempt=$((attempt + 1))
done`
}

// ---------------------------------------------------------------- container-run

var digestRE = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// CheckImageRef validates --image for container-run: a digest pin is required
// unless allowTag is set (the image is the whole toolchain, so an unpinned tag
// silently changes the lane).
func CheckImageRef(ref string, allowTag bool) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fmt.Errorf("--image is required")
	}
	if strings.ContainsAny(ref, " \t\n") {
		return fmt.Errorf("--image %q contains whitespace", ref)
	}
	if !digestRE.MatchString(ref) && !allowTag {
		return fmt.Errorf("--image %q must be pinned by digest (<ref>@sha256:<64 hex>); pass --allow-tag=true to accept a moving tag", ref)
	}
	return nil
}

// ---------------------------------------------------------------- git index

// GitIndexScript makes the current directory a one-commit repository of the
// checkout, for tools that ask git which files are tracked. Identity and
// signing are forced so no user/global config is needed.
const GitIndexScript = `set -eu
git config --global --add safe.directory '*'
git init -q -b main .
git add -A
git -c user.name=ci-dagger -c user.email=ci-dagger@users.noreply.github.com -c commit.gpgsign=false commit -q --no-verify -m "checkout (ci-dagger git index)"
echo "git index: $(git ls-files | wc -l) files"`

// CIUserScript creates the unprivileged user ci (uid/gid 1000; an existing
// uid-1000 user, e.g. node's "node", is renamed) and chowns paths to it.
func CIUserScript(paths []string) string {
	var b strings.Builder
	b.WriteString(`set -eu
if getent passwd 1000 >/dev/null; then
  old=$(getent passwd 1000 | cut -d: -f1)
  [ "$old" = ci ] || { usermod -l ci -d /home/ci -m "$old" 2>/dev/null || usermod -l ci "$old"; groupmod -n ci "$(getent group 1000 | cut -d: -f1)" 2>/dev/null || true; }
else
  groupadd -g 1000 ci 2>/dev/null || true
  useradd -m -u 1000 -g 1000 -d /home/ci -s /bin/bash ci
fi
mkdir -p /home/ci && chown 1000:1000 /home/ci
git config --system --add safe.directory '*' 2>/dev/null || true
`)
	for _, p := range paths {
		if p = strings.TrimSpace(p); p != "" {
			fmt.Fprintf(&b, "[ ! -e %s ] || chown -R 1000:1000 %s\n", shq(p), shq(p))
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- shared

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func quoteAll(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, shq(s))
		}
	}
	return out
}

// ---------------------------------------------------------------- fresh sources

// FreshSourcesScript stamps every file of the checkout (/src) with the current
// time; ci-dagger runs it at the start of every check step, inside the step's
// own exec.
//
// Why: some CI checkouts or archives normalise workspace-file mtimes to 1980-01-01
// so Dagger's keys depend on content only, but persistent cache volumes are
// judged by mtime: cargo (workspace crates in /cargo-target) and go test (a
// cached result is keyed by the size + mtime of every file the test opened).
// A changed file that is never newer than the cached output is treated as
// unchanged and a stale result PASSES (a clippy violation, a broken golden
// file: the cache-mtime regression test). Stamped with the step's start time, every
// source is newer than anything a run that finished earlier left in a cache.
// It also covers whatever mtime-driven tool a --run script points at a cache
// volume. node_modules / .venv are dependency trees, not sources; .git is
// never part of the checkout.
const FreshSourcesScript = `find /src -xdev \( -name .git -o -name node_modules -o -name .venv \) -prune -o -exec touch -h -c -- {} + ` +
	`|| { echo "ci-dagger: cannot stamp /src with the run's time; refusing to check against a warm cache" >&2; exit 1; }`

// ---------------------------------------------------------------- verus

var (
	verusVersionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)
	sha256HexRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// VerusDir is where `rust --verus` mounts the pinned Verus tree (read-only)
// for the run; $VERUS_HOME points at it and $VERUS at its verus binary.
const VerusDir = "/opt/verus"

// DefaultVerusURL is the x86-linux asset of the durable (weekly, never
// replaced) GitHub release release/<version> — the URL the executor uses.
func DefaultVerusURL(version string) string {
	return "https://github.com/verus-lang/verus/releases/download/release%2F" + version +
		"/verus-" + version + "-x86-linux.zip"
}

// VerusPin validates `rust --verus/--verus-sha256/--verus-url` and returns
// the zip URL. A Verus install is only ever the exact bytes of a pinned zip:
// a sha256 is mandatory and the URL must be https.
func VerusPin(version, sum, url string) (string, error) {
	version, sum, url = strings.TrimSpace(version), strings.ToLower(strings.TrimSpace(sum)), strings.TrimSpace(url)
	if version == "" {
		if sum != "" || url != "" {
			return "", fmt.Errorf("--verus-sha256/--verus-url need --verus=<version>")
		}
		return "", nil
	}
	if !verusVersionRE.MatchString(version) {
		return "", fmt.Errorf("--verus %q is not a Verus version (e.g. 0.2026.10.26.d17fbd0)", version)
	}
	if !sha256HexRE.MatchString(sum) {
		return "", fmt.Errorf("--verus needs --verus-sha256=<64 hex digits of the zip's sha256>")
	}
	if url == "" {
		return DefaultVerusURL(version), nil
	}
	if !strings.HasPrefix(url, "https://") || strings.ContainsAny(url, " \t\r\n'\"\\`$") {
		return "", fmt.Errorf("--verus-url %q must be a plain https:// URL", url)
	}
	return url, nil
}

// VerusInstallScript unpacks the zip mounted at zip into VerusDir, after
// checking it against the pinned sha256 (the engine already fetched it with
// that checksum; this is the second, independent check). It runs as its own
// exec, so its output is an immutable layer keyed by the zip's content and
// this script: no run can write to it and no cache volume holds Verus (before
// older revisions installed Verus into the shared, PR-writable /cargo-target).
// Release zips hold one directory (verus-x86-linux/); a flat zip works too.
func VerusInstallScript(zip, sum string) string { return verusInstallScript(zip, sum, VerusDir) }

func verusInstallScript(zip, sum, dest string) string {
	return fmt.Sprintf(`set -euo pipefail
printf '%%s  %%s\n' %[2]s %[1]s | sha256sum -c --status - || { echo "ci-dagger: the Verus zip does not match --verus-sha256=%[2]s" >&2; exit 1; }
rm -rf %[3]s /tmp/verus-unzip && mkdir -p /tmp/verus-unzip
unzip -q %[1]s -d /tmp/verus-unzip
bins=$(find /tmp/verus-unzip -mindepth 1 -maxdepth 2 -type f -name verus -perm -u+x)
[ "$(printf '%%s\n' "$bins" | grep -c .)" = 1 ] || { echo "ci-dagger: the Verus zip must hold exactly one executable verus (found: ${bins:-none})" >&2; exit 1; }
mv "$(dirname "$bins")" %[3]s
rm -rf /tmp/verus-unzip
chmod -R a+rX,go-w %[3]s
ls %[3]s
`, shq(zip), sum, shq(dest))
}

// VerusEnvScript exports the Verus of VerusDir to the run.
const VerusEnvScript = `export VERUS_HOME=` + VerusDir + ` VERUS=` + VerusDir + `/verus PATH=` + VerusDir + `:$PATH
[ -x "$VERUS" ] || { echo "ci-dagger: no Verus at $VERUS" >&2; exit 1; }`
