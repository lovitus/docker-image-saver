# docker-image-saver (`dia`)

`dia` saves Docker/OCI images directly from a registry into `docker load` compatible tar files, without using the Docker daemon.

## Highlights

- Single binary with CLI mode, terminal wizard mode, and a local web GUI mode.
- Direct Docker Registry HTTP API V2 implementation (`net/http`), no local Docker dependency.
- Streams layers from registry into the final archive without intermediate layer files on disk.
- Proxy-aware: `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and explicit `--proxy` (including `socks5://` and `socks5h://`).
- Multi-arch aware: supports manifest lists and architecture selection.
- When multiple architectures are selected, each platform is exported into its own tar file.
- Supports gzip and zstd-compressed OCI layers.
- Reports export progress with stage, bytes transferred, speed, and ETA.
- Verifies downloaded manifests/configs/layers by digest and size during export.
- Verifies uncompressed layer `diff_ids` against image config before finalizing output.
- Re-opens each generated docker-load tar and validates archive structure before reporting success.
- Cross-platform builds (Linux, macOS, Windows) via GitHub Actions release workflow.
- Termux support via standard `linux/arm64` binary (no root, no APK packaging).

## Install

Build from source:

```bash
go build -o docker-image-saver .
```

## Usage

### Wizard mode (no arguments)

Run with no arguments:

```bash
./docker-image-saver
```

Wizard flow:
- image name/tag
- output tar path
- optional proxy/auth settings
- fetch manifest and show architecture list
- architecture selection (`all`, `1,2,5-`, etc.)
- export separate tar files per selected platform (`os/arch[/variant]`)

Wizard proxy hint examples:
- `socks5://127.0.0.1:7897`
- `socks5h://127.0.0.1:7897`
- `http://127.0.0.1:7890`

### GUI mode

Launch the embedded local web UI:

```bash
./docker-image-saver gui
```

Or:

```bash
./docker-image-saver --gui
./docker-image-saver --gui --no-browser
```

GUI mode:
- starts a local HTTP server bound to `127.0.0.1` only
- opens the default browser unless `--no-browser` is used
- reuses the same export engine as CLI/Wizard mode
- shows platform inspection, selection, progress, output files, and `docker load -i` commands
- preserves platform selection by manifest digest, so export does not silently switch architectures if a tag is reordered upstream

### CLI mode

```bash
./docker-image-saver --image alpine:latest --output alpine.tar
```

Subcommand style is also supported:

```bash
./docker-image-saver pull alpine:latest alpine.tar
```

### Multi-arch selection

When the image tag points to a manifest list, use `--arch` to pick entries by 1-based index:

```bash
# single index
./docker-image-saver --image alpine:latest --arch 1 --output alpine-amd64.tar

# multiple indices and ranges
./docker-image-saver --image alpine:latest --arch 1,2,5- --output alpine-multi.tar
```

If `--arch` is omitted, all architectures are selected by default.

### Proxy options

- Environment variables: `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`
- Explicit override: `--proxy http://127.0.0.1:8080`
- SOCKS5 (local DNS): `--proxy socks5://127.0.0.1:1080`
- SOCKS5H (proxy DNS): `--proxy socks5h://127.0.0.1:1080`

## Completion output

After a successful save (CLI, wizard, or GUI task), the tool prints or exposes:

- `Status: SUCCESS`
- absolute output path(s)
- output file size(s) in bytes and MiB
- selected platform(s)
- generated platform index file (`*_platforms.json`)
- manual `docker load -i` commands for each tar

## Default Validation

`dia` now validates exports by default:

- manifest responses are checked against registry digest headers or manifest-list descriptor digests
- config and layer blobs are checked against descriptor digest and size while downloading
- uncompressed layers are checked against config `rootfs.diff_ids`
- finished docker-load tars are re-opened and validated for:
  - `manifest.json`
  - `repositories`
  - config JSON presence and naming
  - referenced `layer.tar` entries
  - `repositories` to top-layer mapping

If any of these checks fail, export stops with an error instead of reporting a broken tar as successful.

## Docker load compatibility

The generated tar includes:

- `manifest.json`
- `repositories`
- image config JSON
- layer directories containing `layer.tar`, `VERSION`, and layer `json`

Load the image with:

```bash
docker load -i alpine.tar
```

When multiple platforms are selected, load each tar separately:

```bash
docker load -i ./image_linux_amd64.tar
docker load -i ./image_linux_arm64.tar
docker image ls
```

## Release automation

`.github/workflows/release.yml`:

- runs tests
- cross-compiles all `go tool dist list` targets for OS: `linux`, `darwin`, `windows`
- uploads artifacts
- publishes binaries and checksums on tag push (`v*`) via GitHub CLI

## Notes

- Registry auth supports standard bearer token flow and optional `--username/--password`.
- `DIA_REGISTRY_USERNAME` and `DIA_REGISTRY_PASSWORD` are also supported.
- When multiple platforms are exported, a `*_platforms.json` file records the exact `os/arch[/variant]` mapping for each tar.
