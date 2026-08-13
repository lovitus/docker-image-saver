# Contributing

## Required workflow

1. Create a branch and open a pull request against `main`.
2. Perform only syntax and formatting checks locally. Maintainers may orchestrate isolated live-environment checks on approved private hosts over SSH, but those results do not replace CI or serve as release build evidence.
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

## GUI preview

Do not serve `web/index.html` locally. [`.github/workflows/gui-preview.yml`](.github/workflows/gui-preview.yml) renders the embedded GUI against the stub API in [`.github/gui-preview/preview.mjs`](.github/gui-preview/preview.mjs), captures desktop and mobile screenshots of every page, and fails when the page raises a JavaScript error. It runs automatically for changes under `web/`, and can be started manually from the Actions tab; download the `gui-preview` artifact to review the result.

The same workflow publishes a `dia-preview-binaries` artifact so a maintainer can exercise the real GUI without building anything locally. Verify the checksum before running it:

```sh
gh run download <run-id> -n dia-preview-binaries -D preview-bin
(cd preview-bin && shasum -a 256 -c checksums.txt --ignore-missing)
chmod +x preview-bin/dia_darwin_arm64
xattr -d com.apple.quarantine preview-bin/dia_darwin_arm64 2>/dev/null || true
preview-bin/dia_darwin_arm64 gui
```

These artifacts are review aids. Release assets still come only from [`.github/workflows/release.yml`](.github/workflows/release.yml).

## Private environment validation

Registry/Harbor connectivity, SSH execution-machine behavior, remote storage, and `docker load` compatibility are validated on maintainer-approved private hosts instead of GitHub-hosted runners. The workstation may only use `ssh`, `scp`, or `rsync` to orchestrate an isolated remote directory. Execute only checksum-verified workflow artifacts, do not commit private host metadata or credentials, and do not write to production registries unless a maintainer explicitly identifies a test target.

## Releases

Releases are workflow-only:

1. Move completed notes from `Unreleased` to a versioned `CHANGELOG.md` section.
2. Merge the release-ready commit through the normal CI gate.
3. Push the matching `v*` tag.
4. Wait for [`.github/workflows/release.yml`](.github/workflows/release.yml) to test, build, checksum, and publish every asset.

Do not compile or upload release binaries locally.
