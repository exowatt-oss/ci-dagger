package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/exowatt-oss/ci-dagger/internal/dagger"
	"github.com/exowatt-oss/ci-dagger/toolchain"
)

// generic CI toolchains: pnpm, go, rust, python-plain, container-run. Same
// shape as python-uv / npm / bun (generic.go): base image + apt → dependency
// layer from lockfiles/manifests only (engine-cached; with --dep-cache
// published to an OCI registry) → full source → optional NATIVE ephemeral database →
// --run. Version detection and scripts: package toolchain (unit-tested).

// Pnpm runs a pnpm project's checks: pnpm (the packageManager version from
// package.json, or --pnpm) installed from npm or a configured mirror, a dependency layer
// built by `pnpm fetch` from pnpm-lock.yaml alone, then a bounded, retried
// `pnpm install --frozen-lockfile` on the full checkout, `pnpm run <script>`
// for each --scripts entry and the --run script, all in --dir.
//
//	dagger call -m github.com/exowatt-oss/ci-dagger@main pnpm --src=. --repo=example-project --node=24 \
//	  --scripts=typecheck,test,lint,check:max-lines-ratchet --npm-registry=https://registry.npmjs.org/
func (m *CiDagger) Pnpm(
	ctx context.Context,
	// Only the ROOT node_modules is left out: repos may commit vendored
	// node_modules trees (e.g. native payloads under resources/), and a fresh
	// CI clone has no other node_modules .
	// +ignore=[".git", "node_modules", "**/.venv", "**/__pycache__", ".ci-dagger"]
	src *dagger.Directory,
	// Repository name ("Owner/name" or "name").
	repo string,
	// Project (workspace root) directory: holds package.json and pnpm-lock.yaml.
	// +optional
	// +default="."
	dir string,
	// Node major version (node:<v>-bookworm-slim).
	// +optional
	// +default="22"
	node string,
	// pnpm version; empty = package.json "packageManager" (as pnpm/action-setup).
	// +optional
	pnpm string,
	// `pnpm install --frozen-lockfile` (true) or plain `pnpm install`.
	// +optional
	// +default=true
	frozen bool,
	// Extra `pnpm install` arguments, e.g. ["--ignore-scripts"].
	// +optional
	installArgs []string,
	// Scripts to run in order (`pnpm run <name>`), e.g. ["typecheck","test","lint"].
	// +optional
	scripts []string,
	// Bash script run after the scripts, in --dir (set -euo pipefail).
	// +optional
	run string,
	// +optional
	apt []string,
	// +optional
	env []string,
	// Extra repo-relative globs `pnpm fetch` needs (pnpm-lock.yaml,
	// pnpm-workspace.yaml, .npmrc, .pnpmfile.cjs and patches/** of --dir are
	// always included).
	// +optional
	depInclude []string,
	// +optional
	pgAdminUrl *dagger.Secret,
	// +optional
	// +default="postgresql"
	databaseUrlScheme string,
	// +optional
	databaseEnv []string,
	// +optional
	sha string,
	// +optional
	depCache string,
	// +optional
	// +default=""
	depCacheUsername string,
	// +optional
	depCachePassword *dagger.Secret,
	// npm registry for pnpm itself and the install (optional mirror), e.g. https://registry.npmjs.org/.
	// +optional
	npmRegistry string,
	// Max test workers (VITEST_MAX_WORKERS, CI_TEST_WORKERS); 0 = runner default.
	// +optional
	// +default=2
	testWorkers int,
	// Synthesize a git repository from the checkout (git init + add -A + commit)
	// for tools that ask git which files are tracked. .git never enters the call.
	// +optional
	gitIndex bool,
	// Run the scripts and --run as an unprivileged user "ci" (uid 1000, owns
	// /src and the store) instead of root, like a GitHub-hosted runner: tests
	// that assert permission errors cannot fail as root .
	// +optional
	nonRoot bool,
) (string, error) {
	name, err := shortRepo(repo)
	if err != nil {
		return "", err
	}
	if dir, err = cleanDir(dir); err != nil {
		return "", err
	}
	if node == "" {
		node = defaultNode
	}
	opts, err := laneOpts(depCache, depCacheUsername, depCachePassword, npmRegistry, "", testWorkers)
	if err != nil {
		return "", err
	}
	pj, _ := src.File(inDir(dir, "package.json")).Contents(ctx)
	ver, err := toolchain.PnpmVersion(pnpm, pj)
	if err != nil {
		return "", err
	}
	var steps []step
	for _, s := range scripts {
		if s = strings.TrimSpace(s); s != "" {
			steps = append(steps, step{s, "pnpm run " + shellJoin([]string{s})})
		}
	}
	if strings.TrimSpace(run) != "" {
		steps = append(steps, step{"run", run})
	}

	img := "node:" + node + "-bookworm-slim"
	installPnpm := "npm install -g --no-fund --no-audit pnpm@" + ver + " >/dev/null && pnpm --version"
	base := aptLayer(dag.Container().From(img), append(strings.Fields(nodeAptPackages), apt...))
	if opts.Indexes.Npm != "" {
		base = base.WithEnvVariable("NPM_CONFIG_REGISTRY", opts.Indexes.Npm).
			WithExec([]string{"sh", "-c", installPnpm}).
			WithoutEnvVariable("NPM_CONFIG_REGISTRY")
	} else {
		base = base.WithExec([]string{"sh", "-c", installPnpm})
	}
	base = base.
		WithEnvVariable("CI", "true").
		WithEnvVariable("NPM_CONFIG_UPDATE_NOTIFIER", "false").
		// The store lives in the image (not a cache volume) so a published
		// dependency image carries it; `pnpm install` then links offline.
		WithEnvVariable("npm_config_store_dir", "/pnpm-store").
		// pnpm 11+ ignores npm_config_* and reads only pnpm_config_*.
		WithEnvVariable("pnpm_config_store_dir", "/pnpm-store").
		WithNewFile("/root/.ssh/known_hosts", githubKnownHosts, dagger.ContainerWithNewFileOpts{Permissions: 0o644})
	wd := workdirFor(dir)
	fetch := toolchain.PnpmInstallScript("pnpm fetch")
	installCmd := "pnpm install --prefer-offline"
	if frozen {
		installCmd += " --frozen-lockfile"
	}
	if len(installArgs) > 0 {
		installCmd += " " + shellJoin(installArgs)
	}
	install := toolchain.PnpmInstallScript(installCmd)
	files := depFiles(src, append([]string{
		inDir(dir, "pnpm-lock.yaml"), inDir(dir, "pnpm-workspace.yaml"), inDir(dir, ".npmrc"),
		inDir(dir, ".pnpmfile.cjs"), inDir(dir, "patches/**"),
	}, depInclude...))
	layer := depLayer{
		repo:      name,
		toolchain: "pnpm" + ver + "-node" + node,
		base:      base,
		files:     files,
		workdir:   wd,
		install: func(c *dagger.Container) *dagger.Container {
			return opts.Indexes.remove(opts.Indexes.apply(c).WithExec([]string{"bash", "-c", fetch}))
		},
		keyParts: []string{img, "pnpm@" + ver, nodeAptPackages, strings.Join(apt, " "), fetch},
		strip:    []string{wd + "/.npmrc"},
	}
	deps, wait, err := layer.build(ctx, opts.Cache)
	if err != nil {
		return "", err
	}
	defer wait()
	c := withTestWorkers(deps, opts.TestWorkers).
		WithDirectory("/src", src, dagger.ContainerWithDirectoryOpts{Exclude: []string{"node_modules"}}).
		WithWorkdir(wd)
	c = opts.Indexes.remove(opts.Indexes.apply(c).WithExec([]string{"bash", "-c", install}))
	if nonRoot {
		c = asCIUser(c, "/src", "/pnpm-store")
	}
	if c, err = withEnvList(c, env); err != nil {
		return "", err
	}
	c, db, err := withDB(ctx, c, pgAdminUrl, name, sha, databaseUrlScheme, databaseEnv)
	if err != nil {
		return "", err
	}
	defer db.drop(ctx)
	if gitIndex {
		c = withGitIndex(c, wd)
	}
	if _, err := runSteps(ctx, c, wd, steps); err != nil {
		return "", err
	}
	return summary(name, "pnpm@"+ver+" "+dir, append([]step{{"pnpm install", ""}}, steps...), ""), nil
}

// Go runs a Go module's checks: the official golang image for go.mod's
// toolchain/go version (or --go), a dependency layer (`go mod download` from
// go.mod/go.sum only), a persistent build cache, then --run (default
// `go vet ./... && go test -race ./...`) in --dir.
//
//	dagger call -m github.com/exowatt-oss/ci-dagger@main go --src=. --repo=example-project --run='go vet ./... && go test ./...'
func (m *CiDagger) Go(
	ctx context.Context,
	// +ignore=[".git", "**/node_modules", ".ci-dagger"]
	src *dagger.Directory,
	// Repository name ("Owner/name" or "name").
	repo string,
	// Module directory (holds go.mod), relative to the repo root.
	// +optional
	// +default="."
	dir string,
	// Go version (golang:<v>-bookworm); empty = go.mod toolchain / go directive.
	// +optional
	goVersion string,
	// Bash script run in --dir (set -euo pipefail). Default: go vet ./... && go test -race ./...
	// +optional
	run string,
	// +optional
	apt []string,
	// +optional
	env []string,
	// Extra repo-relative globs `go mod download` needs (replace targets outside --dir).
	// +optional
	depInclude []string,
	// GOPROXY for the download (default: Go's default, proxy.golang.org).
	// +optional
	goproxy string,
	// +optional
	pgAdminUrl *dagger.Secret,
	// +optional
	// +default="postgresql"
	databaseUrlScheme string,
	// +optional
	databaseEnv []string,
	// +optional
	sha string,
	// +optional
	depCache string,
	// +optional
	// +default=""
	depCacheUsername string,
	// +optional
	depCachePassword *dagger.Secret,
	// Max test workers (GOFLAGS -p, CI_TEST_WORKERS); 0 = runner default.
	// +optional
	// +default=2
	testWorkers int,
	// Synthesize a git repository from the checkout (git init + add -A + commit)
	// for tools that ask git which files are tracked. .git never enters the call.
	// +optional
	gitIndex bool,
) (string, error) {
	name, err := shortRepo(repo)
	if err != nil {
		return "", err
	}
	if dir, err = cleanDir(dir); err != nil {
		return "", err
	}
	opts, err := laneOpts(depCache, depCacheUsername, depCachePassword, "", "", testWorkers)
	if err != nil {
		return "", err
	}
	gomod, _ := src.File(inDir(dir, "go.mod")).Contents(ctx)
	ver, err := toolchain.GoVersion(goVersion, gomod)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(run) == "" {
		run = toolchain.DefaultGoRun
	}
	img := toolchain.GoImage(ver)
	base := aptLayer(dag.Container().From(img), apt).
		WithEnvVariable("CI", "true").
		WithEnvVariable("GOTOOLCHAIN", "local").
		WithEnvVariable("GOFLAGS", "-buildvcs=false").
		WithEnvVariable("GOMODCACHE", "/go/pkg/mod")
	if p := strings.TrimSpace(goproxy); p != "" {
		base = base.WithEnvVariable("GOPROXY", p)
	}
	wd := workdirFor(dir)
	download := `go mod download || echo "dependency layer partial (completed after the source is mounted)" >&2`
	layer := depLayer{
		repo:      name,
		toolchain: "go" + ver,
		base:      base,
		files:     depFiles(src, append([]string{"**/go.mod", "**/go.sum", "go.work", "go.work.sum"}, depInclude...)),
		workdir:   wd,
		install: func(c *dagger.Container) *dagger.Container {
			return c.WithExec([]string{"bash", "-c", download})
		},
		keyParts: []string{img, strings.Join(apt, " "), goproxy, download},
	}
	deps, wait, err := layer.build(ctx, opts.Cache)
	if err != nil {
		return "", err
	}
	defer wait()
	c := withTestWorkers(deps, opts.TestWorkers)
	if opts.TestWorkers > 0 {
		c = c.WithEnvVariable("GOFLAGS", fmt.Sprintf("-buildvcs=false -p=%d", opts.TestWorkers*2))
	}
	c = c.WithMountedCache("/root/.cache/go-build", dag.CacheVolume("go-build-"+ver)).
		WithDirectory("/src", src).
		WithWorkdir(wd)
	if c, err = withEnvList(c, env); err != nil {
		return "", err
	}
	c, db, err := withDB(ctx, c, pgAdminUrl, name, sha, databaseUrlScheme, databaseEnv)
	if err != nil {
		return "", err
	}
	defer db.drop(ctx)
	steps := []step{{"run", run}}
	if gitIndex {
		c = withGitIndex(c, wd)
	}
	if _, err := runSteps(ctx, c, wd, steps); err != nil {
		return "", err
	}
	return summary(name, "go"+ver+" "+dir, append([]step{{"go mod download", ""}}, steps...), ""), nil
}

// Rust runs a Cargo project's checks: the official rust image (--rust, default
// the current stable "1"), the toolchain + components the checkout pins in
// rust-toolchain(.toml) (or --toolchain), optional cargo tools (--tools, e.g.
// cargo-deny), a dependency layer (`cargo fetch` from the manifests and
// Cargo.lock only), a per-repo target-dir cache, then --run (default
// `cargo fmt --check && cargo clippy -D warnings && cargo test`) in --dir.
//
//	dagger call -m github.com/exowatt-oss/ci-dagger@main rust --src=. --repo=example-project --tools=cargo-deny \
//	  --run='cargo fmt --all -- --check && cargo clippy --all-targets -- -D warnings && cargo test && cargo deny check'
func (m *CiDagger) Rust(
	ctx context.Context,
	// +ignore=[".git", "**/node_modules", "**/target", ".ci-dagger"]
	src *dagger.Directory,
	// Repository name ("Owner/name" or "name").
	repo string,
	// Workspace directory (holds Cargo.toml / Cargo.lock), relative to the repo root.
	// +optional
	// +default="."
	dir string,
	// Base image version (rust:<v>-bookworm), e.g. "1" (current stable) or "1.89";
	// "<v>-trixie" selects the Debian 13 image (glibc 2.41, e.g. for Verus
	// release binaries).
	// +optional
	// +default="1"
	rust string,
	// Toolchain to install and use (overrides rust-toolchain.toml), e.g. "stable", "1.89.0".
	// +optional
	toolchainChannel string,
	// rustup components (default clippy, rustfmt; plus any rust-toolchain.toml lists).
	// +optional
	components []string,
	// Cargo tools installed with `cargo install --locked` into the cached layer, e.g. ["cargo-deny@0.18.3"].
	// +optional
	tools []string,
	// Bash script run in --dir (set -euo pipefail). Default: cargo fmt --check, clippy -D warnings, cargo test.
	// +optional
	run string,
	// +optional
	apt []string,
	// +optional
	env []string,
	// Extra repo-relative globs `cargo fetch` needs (path dependencies outside --dir).
	// +optional
	depInclude []string,
	// +optional
	pgAdminUrl *dagger.Secret,
	// +optional
	// +default="postgresql"
	databaseUrlScheme string,
	// +optional
	databaseEnv []string,
	// +optional
	sha string,
	// +optional
	depCache string,
	// +optional
	// +default=""
	depCacheUsername string,
	// +optional
	depCachePassword *dagger.Secret,
	// Max test threads (RUST_TEST_THREADS, CI_TEST_WORKERS) and 2x build jobs; 0 = runner default.
	// +optional
	// +default=2
	testWorkers int,
	// Synthesize a git repository from the checkout (git init + add -A + commit)
	// for tools that ask git which files are tracked. .git never enters the call.
	// +optional
	gitIndex bool,
	// Verus version to provide to --run , e.g. 0.2026.10.26.d17fbd0.
	// The pinned zip (--verus-url, default the x86-linux asset of the GitHub
	// release release/<version>) is fetched with its --verus-sha256 checked
	// and unpacked into an immutable, content-addressed layer mounted
	// read-only at /opt/verus: $VERUS is the binary, $VERUS_HOME its
	// directory, and it is on PATH. No cache volume ever holds Verus.
	// +optional
	verus string,
	// sha256 (64 hex digits) of the Verus zip. Required with --verus.
	// +optional
	verusSha256 string,
	// https URL of the Verus zip (e.g. a third-party mirror). Default: the GitHub
	// release asset verus-<version>-x86-linux.zip.
	// +optional
	verusUrl string,
) (string, error) {
	name, err := shortRepo(repo)
	if err != nil {
		return "", err
	}
	if dir, err = cleanDir(dir); err != nil {
		return "", err
	}
	if rust == "" {
		rust = "1"
	}
	verusZipURL, err := toolchain.VerusPin(verus, verusSha256, verusUrl)
	if err != nil {
		return "", err
	}
	opts, err := laneOpts(depCache, depCacheUsername, depCachePassword, "", "", testWorkers)
	if err != nil {
		return "", err
	}
	tcFile, _ := src.File(inDir(dir, "rust-toolchain.toml")).Contents(ctx)
	if tcFile == "" {
		tcFile, _ = src.File(inDir(dir, "rust-toolchain")).Contents(ctx)
	}
	pinned, fileComps, err := toolchain.RustToolchainFile(tcFile)
	if err != nil {
		return "", err
	}
	channel := strings.TrimSpace(toolchainChannel)
	if channel != "" {
		if _, _, err := toolchain.RustToolchainFile(channel); err != nil {
			return "", fmt.Errorf("--toolchain-channel: %w", err)
		}
	}
	if len(components) == 0 {
		components = []string{"clippy", "rustfmt"}
	}
	components = append(components, fileComps...)
	if strings.TrimSpace(run) == "" {
		run = toolchain.DefaultRustRun
	}
	img := toolchain.RustImage(rust)
	wd := workdirFor(dir)
	// The toolchain is installed in the workdir so rustup sees the pin file.
	tcFiles := depFiles(src, []string{inDir(dir, "rust-toolchain.toml"), inDir(dir, "rust-toolchain")})
	setup := toolchain.RustSetupScript(channel, components)
	base := aptLayer(dag.Container().From(img), append([]string{"git", "ca-certificates"}, apt...)).
		WithEnvVariable("CI", "true").
		WithEnvVariable("CARGO_TERM_COLOR", "never").
		WithEnvVariable("CARGO_INCREMENTAL", "0").
		WithEnvVariable("CARGO_NET_RETRY", "5").
		WithDirectory("/src", tcFiles).
		WithWorkdir(wd).
		WithExec([]string{"bash", "-c", setup})
	for _, t := range tools {
		if t = strings.TrimSpace(t); t != "" {
			base = base.WithExec([]string{"bash", "-c", "set -o pipefail; cargo install --locked " + shellJoin([]string{t}) + " 2>&1 | tail -5"})
		}
	}
	// cargo fetch must parse every manifest without the sources: create stub
	// targets (the default src/lib.rs where a crate has neither lib.rs nor
	// main.rs, plus every target a manifest declares explicitly), fetch, then
	// remove exactly the stubs created.
	depSrc := depFiles(src, append([]string{"**/Cargo.toml", "**/Cargo.lock", "**/.cargo/config.toml", "**/.cargo/config", "**/rust-toolchain.toml", "**/rust-toolchain"}, depInclude...))
	manifests, err := depSrc.Glob(ctx, "**/Cargo.toml")
	if err != nil {
		return "", fmt.Errorf("list Cargo.toml: %w", err)
	}
	var stubs []string
	for _, mf := range manifests {
		content, err := depSrc.File(mf).Contents(ctx)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", mf, err)
		}
		d := mf[:max(0, strings.LastIndex(mf, "/"))]
		if d == "" {
			d = "."
		}
		stubs = append(stubs, toolchain.CargoTargetStubs(d, content)...)
	}
	fetch := `set -uo pipefail
created=()
mk() { if [ ! -e "$1" ]; then mkdir -p "$(dirname "$1")" && : > "$1" && created+=("$1"); fi; }
while IFS= read -r -d '' m; do
  d=$(dirname "$m")
  if [ ! -e "$d/src/lib.rs" ] && [ ! -e "$d/src/main.rs" ]; then mk "$d/src/lib.rs"; fi
done < <(find /src -name Cargo.toml -print0)
while IFS= read -r f; do [ -n "$f" ] && mk "/src/$f"; done <<< "${CARGO_STUBS:-}"
cargo fetch --locked || cargo fetch || echo "dependency layer partial (completed after the source is mounted)" >&2
for f in "${created[@]}"; do rm -f "$f"; d=$(dirname "$f"); while [ "$d" != /src ] && rmdir "$d" 2>/dev/null; do d=$(dirname "$d"); done; done
exit 0`
	stubList := strings.Join(stubs, "\n")
	layer := depLayer{
		repo:      name,
		toolchain: "rust" + rust + "-" + firstNonEmpty(channel, pinned, "image"),
		base:      base,
		files:     depSrc,
		workdir:   wd,
		install: func(c *dagger.Container) *dagger.Container {
			return c.WithEnvVariable("CARGO_STUBS", stubList).WithExec([]string{"bash", "-c", fetch}).WithoutEnvVariable("CARGO_STUBS")
		},
		keyParts: []string{img, setup, strings.Join(tools, " "), strings.Join(apt, " "), fetch, stubList},
	}
	deps, wait, err := layer.build(ctx, opts.Cache)
	if err != nil {
		return "", err
	}
	defer wait()
	c := withTestWorkers(deps, opts.TestWorkers)
	if opts.TestWorkers > 0 {
		c = c.WithEnvVariable("RUST_TEST_THREADS", fmt.Sprint(opts.TestWorkers)).
			WithEnvVariable("CARGO_BUILD_JOBS", fmt.Sprint(opts.TestWorkers*2))
	}
	// The target dir is a cache volume, so cargo's mtime freshness must only
	// ever compare this run's sources with outputs of runs that finished
	// before it started: runSteps stamps the sources with the step's start
	// time, and PRIVATE sharing gives a concurrent run (another PR, the verus
	// step of the same pipeline) its own instance instead of interleaving
	// builds of two trees in one target dir. v2 : fresh volumes, so no
	// artifact a older run left behind is ever reused.
	c = c.WithMountedCache("/cargo-target", dag.CacheVolume(rustTargetVolume(name, rust, firstNonEmpty(channel, pinned, "image"))),
		dagger.ContainerWithMountedCacheOpts{Sharing: dagger.CacheSharingModePrivate}).
		WithEnvVariable("CARGO_TARGET_DIR", "/cargo-target").
		WithDirectory("/src", src, dagger.ContainerWithDirectoryOpts{Exclude: []string{"**/target"}}).
		WithWorkdir(wd)
	if c, err = withEnvList(c, env); err != nil {
		return "", err
	}
	extra := ""
	if verusZipURL != "" {
		sum := strings.ToLower(strings.TrimSpace(verusSha256))
		c = c.WithMountedDirectory(toolchain.VerusDir, verusTree(img, verusZipURL, sum), dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
		run = toolchain.VerusEnvScript + "\n" + run
		extra = "; verus " + strings.TrimSpace(verus) + " (sha256 " + sum[:12] + ")"
	}
	c, db, err := withDB(ctx, c, pgAdminUrl, name, sha, databaseUrlScheme, databaseEnv)
	if err != nil {
		return "", err
	}
	defer db.drop(ctx)
	steps := []step{{"run", run}}
	if gitIndex {
		c = withGitIndex(c, wd)
	}
	if _, err := runSteps(ctx, c, wd, steps); err != nil {
		return "", err
	}
	return summary(name, "rust "+firstNonEmpty(channel, pinned, rust)+" "+dir, append([]step{{"cargo fetch", ""}}, steps...), extra), nil
}

// verusTree is the Verus of a pinned zip: the engine fetches the zip with
// the pinned checksum, then an exec in the rust image checks it again and
// unpacks it. The result is an immutable layer keyed by the zip's content:
// unlike a cache volume, nothing a run does can change what the next run
// gets (toolchain.VerusInstallScript).
func verusTree(img, url, sum string) *dagger.Directory {
	const zip = "/ci-dagger/verus.zip"
	f := dag.HTTP(url, dagger.HTTPOpts{Checksum: "sha256:" + sum, Name: "verus.zip"})
	return dag.Container().From(img).
		WithMountedFile(zip, f).
		WithExec([]string{"bash", "-c", toolchain.VerusInstallScript(zip, sum)}).
		Directory(toolchain.VerusDir)
}

// PythonPlain runs a plain Python project (no uv project: requirements files
// or nothing at all): python:<v>-slim-bookworm, `pip install -r` each
// --requirements file plus --pip packages (dependency layer keyed on those
// files), then --run in --dir. For uv projects use python-uv.
//
//	dagger call -m github.com/exowatt-oss/ci-dagger@main python-plain --src=. --repo=example-project --run='python -m unittest discover -s tests -v'
func (m *CiDagger) PythonPlain(
	ctx context.Context,
	// +ignore=[".git", "**/node_modules", "**/.venv", "**/__pycache__", "**/.pytest_cache", ".ci-dagger"]
	src *dagger.Directory,
	// Repository name ("Owner/name" or "name").
	repo string,
	// Working directory, relative to the repo root.
	// +optional
	// +default="."
	dir string,
	// Python version (python:<v>-slim-bookworm).
	// +optional
	// +default="3.12"
	python string,
	// Requirement files (relative to --dir) installed with `pip install -r`.
	// +optional
	requirements []string,
	// Extra packages for `pip install` (e.g. ["miss_hit", "pytest"]).
	// +optional
	pip []string,
	// Bash script run in --dir (set -euo pipefail).
	run string,
	// +optional
	apt []string,
	// +optional
	env []string,
	// Extra repo-relative globs the install needs (constraint files, local wheels).
	// +optional
	depInclude []string,
	// +optional
	pgAdminUrl *dagger.Secret,
	// +optional
	// +default="postgresql"
	databaseUrlScheme string,
	// +optional
	databaseEnv []string,
	// +optional
	sha string,
	// +optional
	depCache string,
	// +optional
	// +default=""
	depCacheUsername string,
	// +optional
	depCachePassword *dagger.Secret,
	// Python simple index for pip (optional mirror), e.g. https://pypi.org/simple/.
	// +optional
	pypiIndex string,
	// Max test workers (PYTEST_XDIST_AUTO_NUM_WORKERS, CI_TEST_WORKERS); 0 = runner default.
	// +optional
	// +default=2
	testWorkers int,
	// Synthesize a git repository from the checkout (git init + add -A + commit)
	// for tools that ask git which files are tracked (`git ls-files`). .git
	// itself never enters the call.
	// +optional
	gitIndex bool,
) (string, error) {
	name, err := shortRepo(repo)
	if err != nil {
		return "", err
	}
	if dir, err = cleanDir(dir); err != nil {
		return "", err
	}
	if python == "" {
		python = defaultPython
	}
	img, err := toolchain.PythonImage(python)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(run) == "" {
		return "", fmt.Errorf("--run is required")
	}
	opts, err := laneOpts(depCache, depCacheUsername, depCachePassword, "", pypiIndex, testWorkers)
	if err != nil {
		return "", err
	}
	wd := workdirFor(dir)
	base := aptLayer(dag.Container().From(img), append(strings.Fields(pythonAptBase), apt...)).
		WithEnvVariable("CI", "true").
		WithEnvVariable("PYTHONDONTWRITEBYTECODE", "1").
		WithEnvVariable("PIP_DISABLE_PIP_VERSION_CHECK", "1").
		WithEnvVariable("PIP_NO_CACHE_DIR", "1")
	install := toolchain.PipInstallScript(requirements, pip)
	var reqGlobs []string
	for _, r := range requirements {
		if r = strings.TrimSpace(r); r != "" {
			reqGlobs = append(reqGlobs, inDir(dir, r))
		}
	}
	c := base
	installed := "no install"
	if install != "" {
		layer := depLayer{
			repo:      name,
			toolchain: "pip-py" + python,
			base:      base,
			files:     depFiles(src, append(reqGlobs, depInclude...)),
			workdir:   wd,
			install: func(c *dagger.Container) *dagger.Container {
				return opts.Indexes.remove(opts.Indexes.apply(c).WithExec([]string{"bash", "-c", install}))
			},
			keyParts: []string{img, pythonAptBase, strings.Join(apt, " "), install},
		}
		deps, wait, err := layer.build(ctx, opts.Cache)
		if err != nil {
			return "", err
		}
		defer wait()
		c = deps
		installed = "pip install"
	}
	c = withTestWorkers(c, opts.TestWorkers).
		WithDirectory("/src", src, dagger.ContainerWithDirectoryOpts{Exclude: []string{"**/.venv", "**/__pycache__"}}).
		WithWorkdir(wd)
	if c, err = withEnvList(c, env); err != nil {
		return "", err
	}
	c, db, err := withDB(ctx, c, pgAdminUrl, name, sha, databaseUrlScheme, databaseEnv)
	if err != nil {
		return "", err
	}
	defer db.drop(ctx)
	if gitIndex {
		c = withGitIndex(c, wd)
	}
	steps := []step{{"run", run}}
	if _, err := runSteps(ctx, c, wd, steps); err != nil {
		return "", err
	}
	return summary(name, "python-plain py"+python+" "+dir, append([]step{{installed, ""}}, steps...), ""), nil
}

// ContainerRun is the generic escape hatch: any image (pinned by digest
// unless --allow-tag) + optional apt packages + an optional --setup script
// (cached, before the source: e.g. install helm) + the --run script on the
// checkout, in --dir, through --shell (default sh, for busybox images).
// Covers tools no toolchain primitive models (helm, promtool, cmake, …).
//
//	dagger call -m github.com/exowatt-oss/ci-dagger@main container-run --src=. --repo=example-project \
//	  --image=public.ecr.aws/docker/library/alpine:3.22@sha256:… \
//	  --setup='apk add --no-cache helm' --run='helm template charts/x >/dev/null'
func (m *CiDagger) ContainerRun(
	ctx context.Context,
	// +ignore=[".git", "**/node_modules", ".ci-dagger"]
	src *dagger.Directory,
	// Repository name ("Owner/name" or "name").
	repo string,
	// Image reference, pinned by digest (<ref>@sha256:<64 hex>).
	image string,
	// Script run on the checkout, in --dir.
	run string,
	// Accept an unpinned tag for --image.
	// +optional
	allowTag bool,
	// Working directory, relative to the repo root.
	// +optional
	// +default="."
	dir string,
	// Debian/Ubuntu packages (only for apt-based images).
	// +optional
	apt []string,
	// Script run once in the image before the source is mounted (cached by the engine).
	// +optional
	setup string,
	// Shell for --run ("sh" or "bash"); errexit is always on. --setup always runs through sh.
	// +optional
	// +default="sh"
	shell string,
	// +optional
	env []string,
	// +optional
	pgAdminUrl *dagger.Secret,
	// +optional
	// +default="postgresql"
	databaseUrlScheme string,
	// +optional
	databaseEnv []string,
	// +optional
	sha string,
	// Max test workers (CI_TEST_WORKERS etc.); 0 = unset.
	// +optional
	// +default=2
	testWorkers int,
	// Synthesize a git repository from the checkout (needs git in the image).
	// +optional
	gitIndex bool,
) (string, error) {
	name, err := shortRepo(repo)
	if err != nil {
		return "", err
	}
	if dir, err = cleanDir(dir); err != nil {
		return "", err
	}
	if err := toolchain.CheckImageRef(image, allowTag); err != nil {
		return "", err
	}
	if strings.TrimSpace(run) == "" {
		return "", fmt.Errorf("--run is required")
	}
	if shell == "" {
		shell = "sh"
	}
	if shell != "sh" && shell != "bash" {
		return "", fmt.Errorf("--shell must be sh or bash")
	}
	prefix := "set -eu\n"
	if shell == "bash" {
		prefix = "set -euo pipefail\n"
	}
	wd := workdirFor(dir)
	c := aptLayer(dag.Container().From(strings.TrimSpace(image)), apt).WithEnvVariable("CI", "true")
	if strings.TrimSpace(setup) != "" {
		// Always POSIX sh: --setup is what installs bash on images without it .
		c = c.WithExec([]string{"sh", "-c", "set -eu\n" + setup})
	}
	c = withTestWorkers(c, testWorkers).
		WithDirectory("/src", src).
		WithWorkdir(wd)
	if c, err = withEnvList(c, env); err != nil {
		return "", err
	}
	c, db, err := withDB(ctx, c, pgAdminUrl, name, sha, databaseUrlScheme, databaseEnv)
	if err != nil {
		return "", err
	}
	defer db.drop(ctx)
	if gitIndex {
		c = withGitIndex(c, wd)
	}
	if _, err := c.WithExec([]string{shell, "-c", prefix + run}).Sync(ctx); err != nil {
		return "", err
	}
	steps := []step{{"run", ""}}
	if strings.TrimSpace(setup) != "" {
		steps = append([]step{{"setup", ""}}, steps...)
	}
	return summary(name, "container-run "+dir, steps, ""), nil
}

// withGitIndex turns /src into a one-commit git repository holding exactly
// the checkout (minus .gitignore'd paths, which a fresh clone lacks anyway).
func withGitIndex(c *dagger.Container, wd string) *dagger.Container {
	return c.WithWorkdir("/src").WithExec([]string{"sh", "-c", toolchain.GitIndexScript}).WithWorkdir(wd)
}

// asCIUser creates user ci (uid 1000, home /home/ci), hands it the given
// trees and switches the container to it. git trusts every directory (the
// checkout is owned by ci, the git index may be created later).
func asCIUser(c *dagger.Container, paths ...string) *dagger.Container {
	return c.WithExec([]string{"bash", "-c", toolchain.CIUserScript(paths)}).
		WithUser("ci").
		WithEnvVariable("HOME", "/home/ci")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// rustTargetVolume names the per-repo /cargo-target cache volume.
func rustTargetVolume(repo, image, toolchain string) string {
	return "cargo-target-v2-" + repo + "-" + image + "-" + toolchain
}
