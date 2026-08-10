# Contributing

## Required workflow

1. Create a branch and open a pull request against `main`.
2. Perform only syntax and formatting checks locally. The repository does not accept local build or test results as release evidence.
3. Wait for the GitHub Actions `quality` and `cross-build` checks to pass.
4. Merge without bypassing required checks.

The complete repository gate is documented in [`AGENTS.md`](AGENTS.md).

## Local validation allowed

```sh
gofmt -d $(git ls-files '*.go')
awk '/<script>/{flag=1;next}/<\/script>/{flag=0}flag' web/index.html | node --check -
git diff --check
```

Unit tests, race detection, `go vet`, module reproducibility checks, and all supported target builds run only in [`.github/workflows/ci.yml`](.github/workflows/ci.yml).

## Releases

Releases are workflow-only:

1. Move completed notes from `Unreleased` to a versioned `CHANGELOG.md` section.
2. Merge the release-ready commit through the normal CI gate.
3. Push the matching `v*` tag.
4. Wait for [`.github/workflows/release.yml`](.github/workflows/release.yml) to test, build, checksum, and publish every asset.

Do not compile or upload release binaries locally.
