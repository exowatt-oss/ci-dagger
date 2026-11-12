package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/exowatt-oss/ci-dagger/internal/dagger"
)

// EphemeralDb creates an isolated database on a caller-provided PostgreSQL
// server using an admin role with CREATEDB. Pair it with DropDb when finished.
func (m *CiDagger) EphemeralDb(
	ctx context.Context,
	// PostgreSQL admin URL supplied as a Dagger secret.
	pgAdminUrl *dagger.Secret,
	// Short repository identifier used in the database name.
	repo string,
	// Full commit SHA used to distinguish databases for different revisions.
	sha string,
) (string, error) {
	name, err := shortRepo(repo)
	if err != nil {
		return "", err
	}
	sha = strings.ToLower(strings.TrimSpace(sha))
	if len(sha) < 8 || !shaPrefixRE.MatchString(sha[:8]) {
		return "", fmt.Errorf("sha must contain at least eight hexadecimal characters")
	}
	if len(name) > 32 {
		name = name[:32]
	}
	dbName := fmt.Sprintf("ci_%s_%s_%s", sanitize(name), sha[:8], nonce()[:6])
	_, err = psql(pgAdminUrl).
		WithEnvVariable("CI_DB", dbName).
		WithExec([]string{"sh", "-c", `psql "$PG_ADMIN_URL" -v ON_ERROR_STOP=1 -qc "CREATE DATABASE \"$CI_DB\""`}).
		Sync(ctx)
	if err != nil {
		return "", err
	}
	return dbName, nil
}

// DropDb drops a database created by EphemeralDb. It refuses names outside the
// constrained ci_<repo>_<sha>_<nonce> form.
func (m *CiDagger) DropDb(ctx context.Context, pgAdminUrl *dagger.Secret, name string) error {
	if !dbNameRE.MatchString(name) {
		return fmt.Errorf("refusing to drop database name outside the ci_* format")
	}
	_, err := psql(pgAdminUrl).
		WithEnvVariable("CI_DB", name).
		WithExec([]string{"sh", "-c", `psql "$PG_ADMIN_URL" -v ON_ERROR_STOP=1 -qc "DROP DATABASE IF EXISTS \"$CI_DB\" WITH (FORCE)"`}).
		Sync(ctx)
	return err
}

// Use a public image for the PostgreSQL client; no server is bundled or run.
const psqlImage = "public.ecr.aws/docker/library/postgres:18-alpine@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873"

func psql(adminURL *dagger.Secret) *dagger.Container {
	return dag.Container().From(psqlImage).
		WithEnvVariable("CI_DAGGER_NONCE", nonce()).
		WithSecretVariable("PG_ADMIN_URL", adminURL)
}

var (
	shaPrefixRE = regexp.MustCompile(`^[0-9a-f]{8}$`)
	dbNameRE    = regexp.MustCompile(`^ci_[a-z0-9_]{1,60}$`)
)

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
