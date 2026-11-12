package main

import (
	"fmt"
	"strings"

	"github.com/exowatt-oss/ci-dagger/internal/dagger"
)

// ciOpts contains optional registry, cache and worker settings for the generic
// toolchain primitives. The zero value disables registry overrides and the
// external dependency-image cache.
type ciOpts struct {
	Cache       depCacheOpts
	Indexes     packageIndexes
	TestWorkers int
}

// packageIndexes optionally directs package managers through caller-provided
// mirrors during dependency installation. Empty values retain each tool's
// public default registry.
type packageIndexes struct {
	Npm  string
	Pypi string
}

var (
	npmIndexEnv  = []string{"npm_config_registry", "pnpm_config_registry"}
	pypiIndexEnv = []string{"UV_DEFAULT_INDEX", "UV_INDEX_URL", "PIP_INDEX_URL"}
)

func (p packageIndexes) apply(c *dagger.Container) *dagger.Container {
	if p.Npm != "" {
		for _, k := range npmIndexEnv {
			c = c.WithEnvVariable(k, p.Npm)
		}
	}
	if p.Pypi != "" {
		for _, k := range pypiIndexEnv {
			c = c.WithEnvVariable(k, p.Pypi)
		}
	}
	return c
}

func (p packageIndexes) remove(c *dagger.Container) *dagger.Container {
	if p.Npm != "" {
		for _, k := range npmIndexEnv {
			c = c.WithoutEnvVariable(k)
		}
	}
	if p.Pypi != "" {
		for _, k := range pypiIndexEnv {
			c = c.WithoutEnvVariable(k)
		}
	}
	return c
}

var testWorkerEnv = []string{"VITEST_MAX_WORKERS", "PYTEST_XDIST_AUTO_NUM_WORKERS"}

func withTestWorkerCap(c *dagger.Container, n int) *dagger.Container {
	if n <= 0 {
		return c
	}
	for _, k := range testWorkerEnv {
		c = c.WithEnvVariable(k, fmt.Sprint(n))
	}
	return c
}

func newCiOpts(depCache, depCacheUsername string, depCachePassword *dagger.Secret, npmRegistry, pypiIndex string, testWorkers int) (ciOpts, error) {
	o := ciOpts{
		Indexes:     packageIndexes{Npm: strings.TrimSpace(npmRegistry), Pypi: strings.TrimSpace(pypiIndex)},
		TestWorkers: testWorkers,
	}
	for _, u := range []string{o.Indexes.Npm, o.Indexes.Pypi} {
		if u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return o, fmt.Errorf("package index %q must be an http(s) URL", u)
		}
	}
	depCache = strings.TrimSuffix(strings.TrimSpace(depCache), "/")
	if depCache != "" {
		if strings.Contains(depCache, "://") || !strings.Contains(depCache, "/") {
			return o, fmt.Errorf("--dep-cache %q must be <registry-host>/<path>, for example registry.example.com/ci-cache", depCache)
		}
		if depCachePassword == nil {
			return o, fmt.Errorf("--dep-cache needs --dep-cache-password")
		}
		o.Cache = depCacheOpts{Prefix: depCache, Username: depCacheUsername, Password: depCachePassword}
	}
	return o, nil
}
