package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/exowatt-oss/ci-dagger/internal/dagger"
)

// Dependency images: the installed dependency layer of a recipe (base image +
// apt packages + `npm ci`) is published to an OCI registry under a tag derived
// from the lockfile, and later runs start FROM it instead of installing again.
// The engine's own cache already does this while it is warm; the published
// image is what survives an engine cache wipe (an unclean engine exit drops the
// whole cache) and what a second engine can share.
//
//	<depCache>/<repo>/deps:<kind><ver>-<sha256(lock inputs)[:32]>
//	registry.example.com/ci-cache/example-project/deps:node22-3f9c...
//
// Everything here fails soft: a registry that is down, slow or rejects the
// credentials only means "build the dependencies locally", never a red run.

// depsSchema is part of every dependency-image key. Bump it whenever the
// recipe's dependency layer changes shape (base image, apt packages, install
// script, image env) so old images stop matching.
const depsSchema = "deps-v1"

// depCacheOpts configures the dependency-image cache. Zero value = disabled.
type depCacheOpts struct {
	// Repository prefix, e.g. "registry.example.com/ci-cache".
	Prefix   string
	Username string
	Password *dagger.Secret
}

func (o depCacheOpts) enabled() bool { return o.Prefix != "" && o.Password != nil }

func (o depCacheOpts) host() string {
	host, _, _ := strings.Cut(o.Prefix, "/")
	return host
}

// depsRefFor computes a deterministic cache image reference from generic inputs.
func depsRefFor(o depCacheOpts, name, toolchain string, inputs ...string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n", depsSchema, name, toolchain)
	for _, in := range inputs {
		fmt.Fprintf(h, "%d\n%s\n", len(in), in)
	}
	tag := toolchain + "-" + hex.EncodeToString(h.Sum(nil))[:32]
	return fmt.Sprintf("%s/%s/deps:%s", strings.TrimSuffix(o.Prefix, "/"), name, tag)
}

// manifestDigest asks the registry (OCI distribution API, basic auth) for
// ref's manifest digest. ("", nil) only on a definite 404; any other failure is
// an error the caller treats as "cache unavailable".
func manifestDigest(ctx context.Context, ref, username, password string) (string, error) {
	host, rest, ok := strings.Cut(ref, "/")
	if !ok {
		return "", fmt.Errorf("bad image ref %q", ref)
	}
	repo, tag, ok := strings.Cut(rest, ":")
	if !ok {
		return "", fmt.Errorf("image ref %q has no tag", ref)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/v2/"+repo+"/manifests/"+tag, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))
	req.SetBasicAuth(username, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		d := resp.Header.Get("Docker-Content-Digest")
		if !strings.HasPrefix(d, "sha256:") {
			return "", fmt.Errorf("HEAD %s: no Docker-Content-Digest", ref)
		}
		return d, nil
	case http.StatusNotFound:
		return "", nil
	default:
		return "", fmt.Errorf("HEAD %s: HTTP %d", ref, resp.StatusCode)
	}
}

// cachedDeps returns the dependency container for ref: pulled from the
// registry when it is there, otherwise built with build(). On a miss the built
// layer is published in the background (best effort, zstd layers) while the
// caller runs its steps on the local build; the returned wait() blocks until
// that publish ends and must be called before the function returns. strip
// lists paths removed before publishing (files that may carry credentials,
// e.g. .npmrc); callers mount them back with the source tree.
func cachedDeps(ctx context.Context, o depCacheOpts, ref string, strip []string, build func() (*dagger.Container, error)) (*dagger.Container, func(), error) {
	noWait := func() {}
	if !o.enabled() {
		c, err := build()
		return c, noWait, err
	}
	password, err := o.Password.Plaintext(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dep cache: cannot read registry password (%v); building dependencies locally\n", err)
		c, err := build()
		return c, noWait, err
	}

	digest, checkErr := manifestDigest(ctx, ref, o.Username, password)
	found := digest != ""
	switch {
	case checkErr != nil:
		fmt.Fprintf(os.Stderr, "dep cache: %v; building dependencies locally\n", checkErr)
	case found:
		// Pinned by digest: same cache key on every engine, no tag re-resolve.
		c, perr := dag.Container().WithRegistryAuth(o.host(), o.Username, o.Password).From(ref + "@" + digest).Sync(ctx)
		if perr == nil {
			fmt.Fprintf(os.Stderr, "dep cache: hit %s\n", ref)
			return c, noWait, nil
		}
		fmt.Fprintf(os.Stderr, "dep cache: pull %s failed (%v); building dependencies locally\n", ref, perr)
	default:
		fmt.Fprintf(os.Stderr, "dep cache: miss %s; building and publishing\n", ref)
	}

	built, err := build()
	if err != nil {
		return nil, noWait, err
	}
	if checkErr != nil || found {
		// Registry unreachable / auth rejected, or the image exists but could
		// not be pulled: don't try to publish over it.
		return built, noWait, nil
	}
	if built, err = built.Sync(ctx); err != nil {
		return nil, noWait, err
	}
	img := built
	for _, p := range strip {
		img = img.WithoutFile(p)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		pushed, perr := img.WithRegistryAuth(o.host(), o.Username, o.Password).
			Publish(pctx, ref, dagger.ContainerPublishOpts{ForcedCompression: dagger.ImageLayerCompressionZstd})
		if perr != nil {
			fmt.Fprintf(os.Stderr, "dep cache: publish %s failed (%v); the next run builds again\n", ref, perr)
			return
		}
		fmt.Fprintf(os.Stderr, "dep cache: published %s\n", pushed)
	}()
	return built, func() { <-done }, nil
}
