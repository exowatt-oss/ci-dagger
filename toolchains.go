package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/exowatt-oss/ci-dagger/internal/dagger"
)

// Pinned toolchain images; bump deliberately (each bump is a dep-cache miss).
const (
	uvImage        = "ghcr.io/astral-sh/uv:0.12.23"
	defaultBun     = "1.3.14"
	defaultPython  = "3.12"
	defaultNode    = "22"
	pythonAptBase  = "git ca-certificates"
	defaultPgSchem = "postgresql"
)

// pythonChecks are the named checks of python-uv, in the order given.
var pythonChecks = map[string]string{
	"ruff":          "uv run ruff check .",
	"ruff-format":   "uv run ruff format --check .",
	"pyright":       "uv run pyright",
	"mypy":          "uv run mypy .",
	"pytest":        "uv run pytest -q",
	"alembic":       "uv run alembic upgrade head",
	"alembic-check": "uv run alembic check",
}

// PythonUv runs a uv project's checks: `uv sync` (supports optional PyPI mirrors), then the
// named --checks in order (ruff, ruff-format, pyright, mypy, pytest, alembic,
// alembic-check), then the --run script, all in --dir. With --pg-admin-url
// each run gets its own database on a caller-provided PostgreSQL server (never a
// Postgres container), exported as DATABASE_URL (scheme --database-url-scheme)
// and PG* variables, dropped at the end.
//
//	dagger call -m github.com/exowatt-oss/ci-dagger@main python-uv --src=. --repo=example-project --dir=backend \
//	  --pg-admin-url=env://PG_ADMIN_URL --checks=alembic,ruff,pytest \
//	  --dep-cache=registry.example.com/ci-cache --dep-cache-password=env://CACHE_PASSWORD
func (m *CiDagger) PythonUv(
	ctx context.Context,
	// Repository checkout. Local caches/venvs and VCS metadata never enter the call.
	// +ignore=[".git", "**/node_modules", "**/.venv", "**/__pycache__", "**/.pytest_cache", "**/.ruff_cache", "**/.mypy_cache", ".ci-dagger"]
	src *dagger.Directory,
	// Repository name ("Owner/name" or "name"): dep-cache path and database name.
	repo string,
	// Project directory (holds pyproject.toml / uv.lock), relative to the repo root.
	// +optional
	// +default="."
	dir string,
	// Python version (python:<v>-slim-bookworm). uv never downloads interpreters.
	// +optional
	// +default="3.12"
	python string,
	// Extra `uv sync` arguments, e.g. ["--extra","ercot"], ["--all-extras"], ["--frozen"].
	// +optional
	syncArgs []string,
	// Named checks, run in order: ruff, ruff-format, pyright, mypy, pytest, alembic, alembic-check.
	// +optional
	checks []string,
	// Bash script run after the checks, in --dir (set -euo pipefail). For
	// chains the presets don't cover, e.g. "uv run alembic upgrade head && cd ../core && uv run --project ../backend pytest -q".
	// +optional
	run string,
	// Extra Debian packages (e.g. postgresql-client, qpdf, libgl1).
	// +optional
	apt []string,
	// Non-secret KEY=VALUE environment for the checks.
	// +optional
	env []string,
	// Extra repo-relative globs the dependency layer needs (path dependencies
	// outside --dir, e.g. "clients/python/**"). Every pyproject.toml/uv.lock is
	// always included.
	// +optional
	depInclude []string,
	// Admin URL of the native PostgreSQL (role with CREATEDB), e.g.
	// env://PG_ADMIN_URL on the caller-provided PostgreSQL server. Omit for no database.
	// +optional
	pgAdminUrl *dagger.Secret,
	// URL scheme of the exported database URL (SQLAlchemy drivers: postgresql+psycopg, postgresql+asyncpg).
	// +optional
	// +default="postgresql+psycopg"
	databaseUrlScheme string,
	// Variables that receive the database URL (default DATABASE_URL).
	// +optional
	databaseEnv []string,
	// Commit SHA (part of the database name).
	// +optional
	sha string,
	// Dependency-image cache "<registry-host>/<path>" (see the README Configuration section).
	// +optional
	depCache string,
	// +optional
	// +default=""
	depCacheUsername string,
	// +optional
	depCachePassword *dagger.Secret,
	// Python simple index for uv (optional mirror), e.g. https://pypi.org/simple/.
	// Projects that pin their own [[tool.uv.index]] keep it.
	// +optional
	pypiIndex string,
	// Max test workers (PYTEST_XDIST_AUTO_NUM_WORKERS, CI_TEST_WORKERS); 0 = runner default.
	// +optional
	// +default=2
	testWorkers int,
	// Read-only deploy keys for git+ssh dependencies (uv sources such as
	// ssh://git@github.com/Owner/repo), repeatable. Secret mounts for `uv sync`
	// and the checks/--run (a test may fetch one, e.g. uvx), never in the
	// cached dependency layer.
	// +optional
	sshKey []*dagger.Secret,
	// GitHub "Owner/name" of each --ssh-key, same order; required with more
	// than one key (GitHub binds a session to the first key it accepts).
	// +optional
	sshKeyRepo []string,
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
	if err := checkGitSSHKeys(sshKey, sshKeyRepo); err != nil {
		return "", err
	}
	if len(sshKey) > 0 {
		apt = append(apt, "openssh-client")
	}
	opts, err := laneOpts(depCache, depCacheUsername, depCachePassword, "", pypiIndex, testWorkers)
	if err != nil {
		return "", err
	}
	var steps []step
	for _, c := range checks {
		c = strings.TrimSpace(c)
		cmd, ok := pythonChecks[c]
		if !ok {
			return "", fmt.Errorf("unknown check %q (known: ruff, ruff-format, pyright, mypy, pytest, alembic, alembic-check)", c)
		}
		steps = append(steps, step{c, cmd})
	}
	if strings.TrimSpace(run) != "" {
		steps = append(steps, step{"run", run})
	}

	pyImage := "python:" + python + "-slim-bookworm"
	base := aptLayer(dag.Container().From(pyImage), append(strings.Fields(pythonAptBase), apt...)).
		WithFile("/usr/local/bin/uv", dag.Container().From(uvImage).File("/uv")).
		WithFile("/usr/local/bin/uvx", dag.Container().From(uvImage).File("/uvx")).
		WithEnvVariable("CI", "true").
		WithEnvVariable("UV_LINK_MODE", "copy").
		WithEnvVariable("UV_PYTHON_DOWNLOADS", "never").
		WithEnvVariable("UV_PYTHON_PREFERENCE", "only-system").
		WithEnvVariable("UV_CACHE_DIR", "/root/.cache/uv").
		WithEnvVariable("UV_NO_PROGRESS", "1").
		WithMountedCache("/root/.cache/uv", dag.CacheVolume("uv-cache-py"+python))
	wd := workdirFor(dir)
	sync := "uv sync"
	if len(syncArgs) > 0 {
		sync += " " + shellJoin(syncArgs)
	}
	// Dependency layer: third-party packages only (no project / workspace
	// members), from the lockfile. Best effort: if something in it needs
	// source that is not in the layer, the full `uv sync` below completes it.
	depInstall := "uv sync --frozen --no-install-project --no-install-workspace"
	if len(syncArgs) > 0 {
		depInstall += " " + shellJoin(filterArgs(syncArgs, "--frozen", "--locked"))
	}
	depScript := depInstall + ` || echo "dependency layer partial (completed after the source is mounted)" >&2`
	layer := depLayer{
		repo:      name,
		toolchain: "uv-py" + python,
		base:      base,
		files:     depFiles(src, append([]string{"**/pyproject.toml", "**/uv.lock", "**/.python-version", "**/uv.toml"}, depInclude...)),
		workdir:   wd,
		install: func(c *dagger.Container) *dagger.Container {
			return opts.Indexes.remove(opts.Indexes.apply(c).WithExec([]string{"bash", "-c", depScript}))
		},
		keyParts: []string{pyImage, uvImage, pythonAptBase, strings.Join(apt, " "), depScript},
	}
	deps, wait, err := layer.build(ctx, opts.Cache)
	if err != nil {
		return "", err
	}
	defer wait()

	c := withTestWorkers(deps, opts.TestWorkers).
		WithDirectory("/src", src, dagger.ContainerWithDirectoryOpts{Exclude: []string{"**/.venv", "**/node_modules"}}).
		WithWorkdir(wd)
	if c, err = withGitSSHKeys(ctx, c, sshKey, sshKeyRepo); err != nil {
		return "", err
	}
	// Same fresh-mtime preamble as every check step (stepScript). uv itself
	// re-builds a path dependency from a fresh checkout (its cache key is not
	// the pinned mtime; the cache-mtime regression test python guards that).
	c = opts.Indexes.apply(c).WithExec([]string{"bash", "-c", stepScript(sync)})
	c = opts.Indexes.remove(c)
	if c, err = withEnvList(c, env); err != nil {
		return "", err
	}
	c, db, err := withDB(ctx, c, pgAdminUrl, name, sha, databaseUrlScheme, databaseEnv)
	if err != nil {
		return "", err
	}
	defer db.drop(ctx)
	if _, err := runSteps(ctx, c, wd, steps); err != nil {
		return "", err
	}
	extra := ""
	if db != nil {
		extra = "; native postgres db " + db.name
	}
	return summary(name, "python-uv "+dir, append([]step{{"uv sync", ""}}, steps...), extra), nil
}

// Npm runs a Node/npm project's checks: bounded `npm ci` (supports optional npm mirrors,
// git+ssh deps via --ssh-key), then `npm run <script>` for each --scripts
// entry, then the --run script, all in --dir. (Named npm, not node: `node`
// is a reserved GraphQL root field.)
//
//	dagger call -m github.com/exowatt-oss/ci-dagger@main npm --src=. --repo=example-project --dir=frontend --node=24 \
//	  --scripts=typecheck,lint,test,build --npm-registry=https://registry.npmjs.org/
func (m *CiDagger) Npm(
	ctx context.Context,
	// +ignore=[".git", "**/node_modules", "**/.venv", "**/__pycache__", ".ci-dagger"]
	src *dagger.Directory,
	// Repository name ("Owner/name" or "name").
	repo string,
	// Project directory (holds package.json / package-lock.json).
	// +optional
	// +default="."
	dir string,
	// Node major version (node:<v>-bookworm-slim).
	// +optional
	// +default="22"
	node string,
	// npm scripts to run in order (`npm run <name>`), e.g. ["lint","typecheck","test","build"].
	// +optional
	scripts []string,
	// Bash script run after the scripts, in --dir.
	// +optional
	run string,
	// +optional
	apt []string,
	// +optional
	env []string,
	// Read-only deploy key for git+ssh dependencies (install step only).
	// +optional
	sshKey *dagger.Secret,
	// Extra repo-relative globs the install needs (e.g. a postinstall's inputs).
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
	// npm registry for the install (optional mirror), e.g. https://registry.npmjs.org/.
	// +optional
	npmRegistry string,
	// Max test workers (VITEST_MAX_WORKERS, CI_TEST_WORKERS); 0 = runner default.
	// +optional
	// +default=2
	testWorkers int,
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
	var steps []step
	for _, s := range scripts {
		if s = strings.TrimSpace(s); s != "" {
			steps = append(steps, step{s, "npm run " + shellJoin([]string{s})})
		}
	}
	if strings.TrimSpace(run) != "" {
		steps = append(steps, step{"run", run})
	}
	img := "node:" + node + "-bookworm-slim"
	base := aptLayer(dag.Container().From(img), append(strings.Fields(nodeAptPackages), apt...)).
		WithEnvVariable("CI", "true").
		WithEnvVariable("NPM_CONFIG_UPDATE_NOTIFIER", "false").
		WithMountedCache("/root/.npm", dag.CacheVolume("npm-cache-node"+node)).
		WithNewFile("/root/.ssh/known_hosts", githubKnownHosts, dagger.ContainerWithNewFileOpts{Permissions: 0o644}).
		WithNewFile("/usr/local/bin/git-bounded", gitBounded, dagger.ContainerWithNewFileOpts{Permissions: 0o755}).
		WithEnvVariable("npm_config_git", "/usr/local/bin/git-bounded")
	wd := workdirFor(dir)
	files := depFiles(src, append([]string{inDir(dir, "package.json"), inDir(dir, "package-lock.json"), inDir(dir, ".npmrc")}, depInclude...))
	layer := depLayer{
		repo:      name,
		toolchain: "npm-node" + node,
		base:      base,
		files:     files,
		workdir:   wd,
		install: func(c *dagger.Container) *dagger.Container {
			return opts.Indexes.remove(npmInstall(opts.Indexes.apply(c), sshKey))
		},
		keyParts: []string{img, nodeAptPackages, strings.Join(apt, " "), npmCiBounded, gitBounded, githubKnownHosts},
		strip:    []string{wd + "/.npmrc"},
	}
	deps, wait, err := layer.build(ctx, opts.Cache)
	if err != nil {
		return "", err
	}
	defer wait()
	c := withTestWorkers(deps, opts.TestWorkers).
		WithDirectory("/src", src, dagger.ContainerWithDirectoryOpts{Exclude: []string{"**/node_modules"}})
	if c, err = withEnvList(c, env); err != nil {
		return "", err
	}
	c, db, err := withDB(ctx, c, pgAdminUrl, name, sha, databaseUrlScheme, databaseEnv)
	if err != nil {
		return "", err
	}
	defer db.drop(ctx)
	if _, err := runSteps(ctx, c, wd, steps); err != nil {
		return "", err
	}
	return summary(name, "npm "+dir, append([]step{{"npm ci", ""}}, steps...), ""), nil
}

// npmInstall runs the bounded `npm ci`, with the deploy key mounted only for
// the install exec (the same secret-mount handling used by the toolchain primitives).
func npmInstall(c *dagger.Container, sshKey *dagger.Secret) *dagger.Container {
	if sshKey == nil {
		return c.WithExec([]string{"sh", "-c", npmCiBounded})
	}
	return c.WithMountedSecret("/run/secrets/ssh_key", sshKey, dagger.ContainerWithMountedSecretOpts{Owner: "root", Mode: 0o400}).
		WithMountedTemp("/run/ssh").
		WithEnvVariable("GIT_SSH_COMMAND", "ssh -i /run/ssh/id -o IdentitiesOnly=yes "+sshOpts).
		WithExec([]string{"sh", "-c", `set -e
umask 077
tr -d '\r' < /run/secrets/ssh_key > /run/ssh/id
[ -z "$(tail -c1 /run/ssh/id)" ] || echo >> /run/ssh/id
trap 'rm -f /run/ssh/id' EXIT
umask 022
` + npmCiBounded}).
		WithoutEnvVariable("GIT_SSH_COMMAND").
		WithoutMount("/run/ssh").
		WithoutMount("/run/secrets/ssh_key")
}

// Bun runs a Bun project's checks: `bun install` in each --install-dirs entry
// (frozen lockfile by default), then the --run script at the repo root. Node
// is present too (--node) because tools like jest run on node.
//
//	dagger call -m github.com/exowatt-oss/ci-dagger@main bun --src=. --repo=example-project --install-dirs=.,backend,frontend --frozen=false \
//	  --dep-include='backend/prisma/**' --apt=openssl \
//	  --run='cd backend && bunx prisma generate && cd .. && bun run lint && bun run test:backend'
func (m *CiDagger) Bun(
	ctx context.Context,
	// +ignore=[".git", "**/node_modules", ".ci-dagger"]
	src *dagger.Directory,
	// Repository name ("Owner/name" or "name").
	repo string,
	// Directories to `bun install` in, in order (repo-relative).
	// +optional
	installDirs []string,
	// Bun version (npm package bun@<v>, installed through --npm-registry when set).
	// +optional
	// +default="1.3.14"
	bun string,
	// Node major version of the base image (node:<v>-bookworm-slim).
	// +optional
	// +default="22"
	node string,
	// `bun install --frozen-lockfile` (true) or plain `bun install`.
	// +optional
	// +default=true
	frozen bool,
	// Bash script run after the install, at the repo root.
	// +optional
	run string,
	// +optional
	apt []string,
	// +optional
	env []string,
	// Extra repo-relative globs the install needs (e.g. "backend/prisma/**"
	// for a `prisma generate` postinstall). Every package.json, bun.lock(b),
	// bunfig.toml and .npmrc is always included.
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
	// npm registry for the install (optional mirror).
	// +optional
	npmRegistry string,
	// Max test workers (VITEST_MAX_WORKERS, CI_TEST_WORKERS); 0 = runner default.
	// +optional
	// +default=2
	testWorkers int,
) (string, error) {
	name, err := shortRepo(repo)
	if err != nil {
		return "", err
	}
	if bun == "" {
		bun = defaultBun
	}
	if len(installDirs) == 0 {
		installDirs = []string{"."}
	}
	var dirs []string
	for _, d := range installDirs {
		cd, err := cleanDir(d)
		if err != nil {
			return "", err
		}
		dirs = append(dirs, cd)
	}
	opts, err := laneOpts(depCache, depCacheUsername, depCachePassword, npmRegistry, "", testWorkers)
	if err != nil {
		return "", err
	}
	// Bun comes from the npm registry (the `bun` package ships the platform
	// binary as an optional dependency), not the oven/bun Docker Hub image:
	// Docker Hub may throttle image pulls from shared runner IPs; npm packages use a separate public registry.
	if node == "" || node == "none" {
		node = defaultNode
	}
	nodeImg := "node:" + node + "-bookworm-slim"
	baseRef := nodeImg + "+npm:bun@" + bun
	installBun := "npm install -g --no-fund --no-audit bun@" + bun + " >/dev/null && bun --version"
	base := dag.Container().From(nodeImg)
	if opts.Indexes.Npm != "" {
		base = base.WithEnvVariable("NPM_CONFIG_REGISTRY", opts.Indexes.Npm).
			WithExec([]string{"sh", "-c", installBun}).
			WithoutEnvVariable("NPM_CONFIG_REGISTRY")
	} else {
		base = base.WithExec([]string{"sh", "-c", installBun})
	}
	base = aptLayer(base, append([]string{"git", "ca-certificates"}, apt...)).
		WithEnvVariable("CI", "true").
		WithEnvVariable("BUN_INSTALL_CACHE_DIR", "/root/.bun/install/cache").
		WithMountedCache("/root/.bun/install/cache", dag.CacheVolume("bun-cache-"+bun))
	flag := "--frozen-lockfile"
	if !frozen {
		flag = ""
	}
	// The dependency layer installs with --ignore-scripts (lifecycle scripts
	// such as a workspace package's `prepare` need source the layer does not
	// have); the same install then runs again on the full source, with
	// scripts, exactly like GitHub Actions' `bun install` on a checkout.
	installScript := func(extra string) string {
		var b strings.Builder
		b.WriteString("set -euo pipefail\n")
		for _, d := range dirs {
			fmt.Fprintf(&b, "(cd %s && bun install %s %s)\n", shellJoin([]string{d}), flag, extra)
		}
		return b.String()
	}
	depInstall, fullInstall := installScript("--ignore-scripts"), installScript("")
	var installEnv []string
	if opts.Indexes.Npm != "" {
		installEnv = []string{"NPM_CONFIG_REGISTRY", "BUN_CONFIG_REGISTRY"}
	}
	layer := depLayer{
		repo:      name,
		toolchain: "bun" + bun,
		base:      base,
		files:     depFiles(src, append([]string{"**/package.json", "**/bun.lock", "**/bun.lockb", "**/bunfig.toml", "**/.npmrc"}, depInclude...)),
		workdir:   "/src",
		install: func(c *dagger.Container) *dagger.Container {
			return withNpmEnv(c, installEnv, opts.Indexes.Npm, depInstall)
		},
		keyParts: []string{baseRef, strings.Join(apt, " "), depInstall},
		strip:    []string{"/src/.npmrc"},
	}
	deps, wait, err := layer.build(ctx, opts.Cache)
	if err != nil {
		return "", err
	}
	defer wait()
	c := withTestWorkers(deps, opts.TestWorkers).
		WithDirectory("/src", src, dagger.ContainerWithDirectoryOpts{Exclude: []string{"**/node_modules"}}).
		WithWorkdir("/src")
	c = withNpmEnv(c, installEnv, opts.Indexes.Npm, fullInstall)
	if c, err = withEnvList(c, env); err != nil {
		return "", err
	}
	c, db, err := withDB(ctx, c, pgAdminUrl, name, sha, databaseUrlScheme, databaseEnv)
	if err != nil {
		return "", err
	}
	defer db.drop(ctx)
	var steps []step
	if strings.TrimSpace(run) != "" {
		steps = append(steps, step{"run", run})
	}
	if _, err := runSteps(ctx, c, "/src", steps); err != nil {
		return "", err
	}
	return summary(name, "bun", append([]step{{"bun install " + strings.Join(dirs, ","), ""}}, steps...), ""), nil
}

// withNpmEnv runs script with the registry variables set for that exec only.
func withNpmEnv(c *dagger.Container, names []string, registry, script string) *dagger.Container {
	for _, k := range names {
		c = c.WithEnvVariable(k, registry)
	}
	c = c.WithExec([]string{"bash", "-c", script})
	for _, k := range names {
		c = c.WithoutEnvVariable(k)
	}
	return c
}

// shellJoin single-quotes each argument for bash.
func shellJoin(args []string) string {
	q := make([]string, 0, len(args))
	for _, a := range args {
		q = append(q, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return strings.Join(q, " ")
}

func filterArgs(args []string, drop ...string) []string {
	var out []string
outer:
	for _, a := range args {
		for _, d := range drop {
			if a == d {
				continue outer
			}
		}
		out = append(out, a)
	}
	return out
}
