# ci-dagger

A standalone Dagger module of reusable CI checks and toolchain primitives. It does not contain product-specific deployment recipes.

![Status: provided as-is](https://img.shields.io/badge/status-provided--as--is-lightgrey) ![License: Apache--2.0](https://img.shields.io/badge/license-Apache--2.0-blue)

Created at [Exowatt](https://exowatt.com).

## Status

Provided as-is. The functions are reusable examples and primitives, not a managed CI service or a support commitment.

## Install

Install [Dagger](https://docs.dagger.io/) CLI v0.21.10 or later and use this module by its public Go module path:

```sh
dagger call -m github.com/exowatt-oss/ci-dagger@main go --src=. --repo=my-project
```

The first call resolves the module, starts a Dagger engine and runs the Go checks (`go vet ./...` and `go test -race ./...`) in a container. Pin a released module version in production workflows rather than using `@main`.

## Prerequisites

Use Dagger CLI v0.21.10 or later and a Dagger engine. The engine needs access to public package registries and container images used by the selected toolchain; no Exowatt service or credential is required.

## Usage / Quickstart

Choose a primitive for the repository's toolchain. For example, run the default Go checks:

```sh
dagger call -m github.com/exowatt-oss/ci-dagger@main go --src=. --repo=my-project
```

Run a Node project's test and build scripts:

```sh
dagger call -m github.com/exowatt-oss/ci-dagger@main npm \
  --src=. --repo=my-project --dir=frontend \
  --scripts=typecheck,test,build
```

Run a command in a digest-pinned container for a toolchain that does not have a dedicated primitive:

```sh
dagger call -m github.com/exowatt-oss/ci-dagger@main container-run \
  --src=. --repo=my-project \
  --image=public.ecr.aws/docker/library/alpine:3.22@sha256:<verified-digest> \
  --run='make test'
```

The generic toolchain functions include `go`, `rust`, `python-uv`, `python-plain`, `npm`, `pnpm`, `bun`, and `container-run`. The `ephemeral-db` and `drop-db` functions are optional helpers for test runs using a caller-provided PostgreSQL server.

After checkout and installing the pinned Dagger CLI, a GitHub Actions job can call a function:

```yaml
- name: Run checks with Dagger
  run: dagger call -m github.com/exowatt-oss/ci-dagger@main go --src=. --repo=my-project
```

The same Dagger invocation can run from a Woodpecker pipeline on any runner with Dagger CLI and engine access. This repository ships only its public GitHub Actions workflow; callers provide their own runner configuration and credentials. No internal deployment lane or runner setup is included.

This module's own `.github/workflows/ci.yml` shows the public GitHub-hosted lane for formatting, module generation and tests. The primitives themselves are runner-agnostic.

## Configuration

- **Defaults are public and service-free.** Package-index overrides and the dependency-image cache are unset by default. The toolchains use their public package registries unless you explicitly pass `--npm-registry` or `--pypi-index`.
- **Dependency cache is optional.** Pass `--dep-cache=<registry>/<path>`, `--dep-cache-username` and `--dep-cache-password` to reuse an OCI dependency image. The registry must support HTTP Basic authentication for the OCI Distribution API; credentials are supplied as a Dagger secret. This cache is disabled by default, and no registry is assumed. Without these arguments, Dagger's engine cache is used.
- **Test workers** default to 2 where supported; pass `--test-workers=0` to leave worker selection to the tool.
- **PostgreSQL** is optional. The `--pg-admin-url` input is a Dagger secret for an external PostgreSQL server; it is never configured by default. Omit it when tests do not need a database.
- **Source filtering** excludes VCS metadata, the local `.ci-dagger` scratch directory and common build outputs. Use the `--dir`, `--apt`, `--env`, `--dep-include` and `--run` arguments to adapt generic checks to a repository without changing this module.
- **`container-run` images** must be pinned by digest. Set `--allow-tag=true` only when you intentionally accept a moving image tag.

No Exowatt identity, credentials, internal registry or service is needed. Optional registry, cache and database settings are supplied by each caller.

## License

Original code is licensed under [Apache-2.0](LICENSE). See [NOTICE](NOTICE) for attribution. The Dagger SDK and Go dependencies retain their respective licenses; consult their module metadata and upstream license files when redistributing binaries.

## Contributing

Contributions are welcome through GitHub pull requests. Sign off each commit with `git commit -s` to certify the Developer Certificate of Origin (DCO); the repository's DCO check enforces this. No CLA is required.

## Security

Please report vulnerabilities privately using [GitHub private vulnerability reporting](https://github.com/exowatt-oss/ci-dagger/security/advisories/new), or contact software@exowatt.com. Do not open a public issue for security reports.
