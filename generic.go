package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/exowatt-oss/ci-dagger/internal/dagger"
	"github.com/exowatt-oss/ci-dagger/toolchain"
)

// Shared machinery of the generic CI primitives (python-uv, node, bun):
//
//	base image + apt  →  dependency layer (manifests + lockfiles only, cached
//	in the engine and, with --dep-cache, published to an OCI registry)  →  full source  →
//	optional NATIVE ephemeral database  →  named checks  →  --run script.
//
// Everything is an argument, so a repo onboards with a CI workflow and
// no change to this module.

// busyboxImage hashes dependency inputs (content only, never mtimes). ECR
// Public's copy of busybox:1.37.0 pinned by digest (same digest as Docker Hub):
// resolved once per engine, never through Docker Hub.
const busyboxImage = "public.ecr.aws/docker/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// depLayer describes the install layer of one toolchain.
type depLayer struct {
	repo      string            // short repo name (cache path, db name)
	toolchain string            // tag prefix, e.g. "uv-py3.12"
	base      *dagger.Container // base image + apt + toolchain env
	files     *dagger.Directory // repo-root-relative subset of src the install needs
	workdir   string            // where the install runs, e.g. /src/backend
	install   func(*dagger.Container) *dagger.Container
	keyParts  []string // everything else that shapes the layer (images, scripts, args)
	strip     []string // paths removed before publishing (may carry credentials)
}

// contentHash is sha256 over the sorted (path, sha256(content)) list of dir.
func contentHash(ctx context.Context, dir *dagger.Directory) (string, error) {
	out, err := dag.Container().From(busyboxImage).
		WithDirectory("/in", dir).
		WithWorkdir("/in").
		WithExec([]string{"sh", "-c", "find . -type f -print0 | LC_ALL=C sort -z | xargs -0 -r sha256sum | sha256sum"}).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("hash dependency inputs: %w", err)
	}
	return strings.Fields(out + " x")[0], nil
}

// build returns the dependency container, from the published image when the
// cache is on and holds it (cache.go semantics: everything fails soft).
func (l depLayer) build(ctx context.Context, o depCacheOpts) (*dagger.Container, func(), error) {
	local := func() (*dagger.Container, error) {
		c := l.base.WithDirectory("/src", l.files).WithWorkdir(l.workdir)
		return l.install(c), nil
	}
	if !o.enabled() {
		c, err := local()
		return c, func() {}, err
	}
	h, err := contentHash(ctx, l.files)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dep cache: %v; building dependencies locally\n", err)
		c, err := local()
		return c, func() {}, err
	}
	ref := depsRefFor(o, l.repo, l.toolchain, append(append([]string{}, l.keyParts...), h)...)
	return cachedDeps(ctx, o, ref, l.strip, local)
}

// depFiles copies the given repo-relative paths/globs of src (those that
// exist) into a fresh directory, keeping their paths.
func depFiles(src *dagger.Directory, include []string) *dagger.Directory {
	return dag.Directory().WithDirectory("/", src, dagger.DirectoryWithDirectoryOpts{
		Include: include,
		Exclude: []string{"**/node_modules/**", "**/.venv/**", ".git/**"},
	})
}

// cleanDir validates a repo-relative directory argument.
func cleanDir(d string) (string, error) {
	d = strings.TrimSpace(d)
	if d == "" {
		d = "."
	}
	c := path.Clean(d)
	if path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("directory %q must be relative to the repository root", d)
	}
	return c, nil
}

func inDir(dir, file string) string {
	if dir == "." {
		return file
	}
	return dir + "/" + file
}

func workdirFor(dir string) string {
	if dir == "." {
		return "/src"
	}
	return "/src/" + dir
}

// shortRepo turns "Owner/name" or "name" into a safe short name.
func shortRepo(repo string) (string, error) {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repo = repo[i+1:]
	}
	repo = strings.ToLower(strings.TrimSpace(repo))
	if !repoRE.MatchString(repo) {
		return "", fmt.Errorf("invalid repo name %q", repo)
	}
	return repo, nil
}

// withEnvList applies KEY=VALUE pairs (non-secret).
func withEnvList(c *dagger.Container, env []string) (*dagger.Container, error) {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t\n") {
			return nil, fmt.Errorf("--env %q must be KEY=VALUE", kv)
		}
		c = c.WithEnvVariable(k, v)
	}
	return c, nil
}

// aptLayer installs Debian packages (one cached layer).
func aptLayer(c *dagger.Container, pkgs []string) *dagger.Container {
	var clean []string
	for _, p := range pkgs {
		if p = strings.TrimSpace(p); p != "" {
			clean = append(clean, p)
		}
	}
	if len(clean) == 0 {
		return c
	}
	return c.WithExec([]string{"sh", "-c", "apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends " +
		strings.Join(clean, " ") + " >/dev/null && rm -rf /var/lib/apt/lists/*"})
}

// ---------------------------------------------------------------- database

// ephemeralDB is a per-run database on the NATIVE PostgreSQL behind adminURL
// The caller supplies a PostgreSQL server and a role limited to CREATEDB; this module does not start a server container.
type ephemeralDB struct {
	name  string
	admin *dagger.Secret
}

// createDB creates ci_<repo>_<sha8>_<rand> and returns the container with
// the connection exported: every name in envNames gets the URL with the given
// scheme (secret), plus PGHOST/PGPORT/PGUSER/PGDATABASE and secret PGPASSWORD.
func createDB(ctx context.Context, c *dagger.Container, admin *dagger.Secret, repo, sha, scheme string, envNames []string) (*dagger.Container, *ephemeralDB, error) {
	raw, err := admin.Plaintext(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read --pg-admin-url: %w", err)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "postgresql" && u.Scheme != "postgres") || u.Host == "" || u.User == nil || u.User.Username() == "" {
		return nil, nil, fmt.Errorf("--pg-admin-url must be postgresql://user:pass@host:port/db")
	}
	tag := "nosha00"
	if len(sha) >= 8 {
		tag = sha[:8]
	}
	name := fmt.Sprintf("ci_%s_%s_%s", sanitize(repo), sanitize(strings.ToLower(tag)), nonce()[:6])
	if len(name) > 63 {
		name = name[:63]
	}
	if _, err := psql(admin).
		WithEnvVariable("CI_DB", name).
		WithExec([]string{"sh", "-c", `psql "$PG_ADMIN_URL" -v ON_ERROR_STOP=1 -qc "CREATE DATABASE \"$CI_DB\""`}).
		Sync(ctx); err != nil {
		return nil, nil, fmt.Errorf("create ephemeral database: %w", err)
	}
	if scheme == "" {
		scheme = "postgresql"
	}
	dbURL := *u
	dbURL.Scheme = scheme
	dbURL.Path = "/" + name
	pw, _ := u.User.Password()
	if len(envNames) == 0 {
		envNames = []string{"DATABASE_URL"}
	}
	id := nonce()
	urlSecret := dag.SetSecret("ci-db-url-"+id, dbURL.String())
	for _, n := range envNames {
		if n = strings.TrimSpace(n); n != "" {
			c = c.WithSecretVariable(n, urlSecret)
		}
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	c = c.WithEnvVariable("PGHOST", u.Hostname()).
		WithEnvVariable("PGPORT", port).
		WithEnvVariable("PGUSER", u.User.Username()).
		WithEnvVariable("PGDATABASE", name).
		WithEnvVariable("CI_DATABASE_NAME", name).
		WithSecretVariable("PGPASSWORD", dag.SetSecret("ci-db-pw-"+id, pw))
	fmt.Fprintf(os.Stderr, "native postgres: created %s\n", name)
	return c, &ephemeralDB{name: name, admin: admin}, nil
}

func (d *ephemeralDB) drop(ctx context.Context) {
	if d == nil {
		return
	}
	if err := (&CiDagger{}).DropDb(context.WithoutCancel(ctx), d.admin, d.name); err != nil {
		fmt.Fprintf(os.Stderr, "native postgres: drop %s failed (run drop-db or clean up the database manually): %v\n", d.name, err)
		return
	}
	fmt.Fprintf(os.Stderr, "native postgres: dropped %s\n", d.name)
}

// ---------------------------------------------------------------- steps

type step struct {
	name string
	cmd  string // bash script, run in the step's workdir
}

// runSteps executes the steps in order (one span each), then syncs. Every
// step first stamps the checkout with its own start time
// (toolchain.FreshSourcesScript): some CI checkouts or archives normalise file mtimes to 1980-01-01, and cargo, go test and uv judge a persistent cache entry
// by mtime, so a changed file must be newer than anything a previous run left
// in a cache volume. The touch is part of the step's own exec, never a
// separate (cacheable) layer.
func runSteps(ctx context.Context, c *dagger.Container, workdir string, steps []step) (*dagger.Container, error) {
	c = c.WithWorkdir(workdir)
	for _, s := range steps {
		c = c.WithExec([]string{"bash", "-c", stepScript(s.cmd)})
	}
	return c.Sync(ctx)
}

// stepScript is the bash script of one step: strict mode, fresh source
// mtimes, then the command.
func stepScript(cmd string) string {
	return "set -euo pipefail\n" + toolchain.FreshSourcesScript + "\n" + cmd
}

func summary(repo, kind string, steps []step, extra string) string {
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.name)
	}
	return fmt.Sprintf("%s: %s ok (%s)%s\n", repo, kind, strings.Join(names, ", "), extra)
}

// ---------------------------------------------------------------- shared args

// laneOpts bundles the dep-cache / index / worker options.
func laneOpts(depCache, depCacheUsername string, depCachePassword *dagger.Secret, npmRegistry, pypiIndex string, testWorkers int) (ciOpts, error) {
	return newCiOpts(depCache, depCacheUsername, depCachePassword, npmRegistry, pypiIndex, testWorkers)
}

// withTestWorkers caps every runner the generic primitives know about.
// CI_TEST_WORKERS is for runners without an env knob (e.g. `jest
// --maxWorkers=$CI_TEST_WORKERS`, `pytest -n $CI_TEST_WORKERS`).
func withTestWorkers(c *dagger.Container, n int) *dagger.Container {
	if n <= 0 {
		return c
	}
	return withTestWorkerCap(c, n).WithEnvVariable("CI_TEST_WORKERS", fmt.Sprint(n))
}

func withDB(ctx context.Context, c *dagger.Container, admin *dagger.Secret, repo, sha, scheme string, envNames []string) (*dagger.Container, *ephemeralDB, error) {
	if admin == nil {
		return c, nil, nil
	}
	return createDB(ctx, c, admin, repo, sha, scheme, envNames)
}
