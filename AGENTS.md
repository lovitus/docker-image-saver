# Project Gate

These rules are mandatory for every human and automated contributor.

## Local Commands

Local execution is limited to source inspection and syntax/format validation:

- `gofmt` or `gofmt -d`
- `node --check` for the embedded GUI JavaScript
- non-executing repository inspection such as `git diff`, `git status`, `rg`, and file reads

Do not run local unit tests, integration tests, race tests, `go vet`, cross-compilation, packaging, checksum generation, or release uploads. In particular, do not run `go test`, `go vet`, or `go build` locally as a substitute for CI.

## CI Gate

All executable validation must run in GitHub Actions through [`.github/workflows/ci.yml`](.github/workflows/ci.yml). Changes must reach `main` through a pull request after the required `quality` and `cross-build` jobs pass. Do not bypass, disable, or weaken required checks to merge a change.

## Release Gate

All release binaries, checksums, and GitHub Releases must be produced by [`.github/workflows/release.yml`](.github/workflows/release.yml) from a signed-off `v*` tag. Never publish a locally built binary or manually upload release assets. A release entry must have a matching section in `CHANGELOG.md` before its tag is pushed.
