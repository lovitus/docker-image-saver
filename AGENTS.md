# Project Gate

These rules are mandatory for every human and automated contributor.

## Local Commands

Local execution is limited to source inspection, syntax/format validation, and orchestration of approved private validation hosts:

- `gofmt` or `gofmt -d`
- `node --check` for the embedded GUI JavaScript
- non-executing repository inspection such as `git diff`, `git status`, `rg`, and file reads
- `ssh`, `scp`, or `rsync` commands whose only purpose is to run an isolated environment validation on a maintainer-approved private host

Do not run local unit tests, integration tests, race tests, `go vet`, cross-compilation, packaging, checksum generation, or release uploads. In particular, do not run `go test`, `go vet`, or `go build` locally as a substitute for CI.

## Private Environment Validation

Live SSH, Registry, Harbor, proxy, remote-storage, and `docker load` validation must run on maintainer-approved private hosts rather than GitHub-hosted runners. The local workstation may only orchestrate these checks over SSH; compilation, test processes, image traffic, temporary archives, and Docker commands must execute on the private host.

- Use a unique temporary directory and remove it after validation.
- Execute only a workflow-built release binary or CI artifact whose checksum has been verified. Stop if the downloaded asset and published checksum differ.
- Treat production Registry and Harbor endpoints as read-only unless the maintainer explicitly designates a writable test target.
- Never commit host addresses, credentials, private keys, tokens, captured configuration, or private endpoint details.
- Do not install packages, alter daemon configuration, or modify persistent remote state without explicit approval.
- Report the tested commit, remote OS/architecture, validation scenarios, and cleanup result.

## CI Gate

All repository quality validation must run in GitHub Actions through [`.github/workflows/ci.yml`](.github/workflows/ci.yml). Changes must reach `main` through a pull request after the required `quality` and `cross-build` jobs pass. Private environment validation supplements these checks but never replaces or bypasses them.

## Release Gate

All release binaries, checksums, and GitHub Releases must be produced by [`.github/workflows/release.yml`](.github/workflows/release.yml) from a signed-off `v*` tag. Never publish a locally built binary or manually upload release assets. A release entry must have a matching section in `CHANGELOG.md` before its tag is pushed.
