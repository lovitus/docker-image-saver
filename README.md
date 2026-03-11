# dia (Docker Image Archiver)

`dia` saves Docker/OCI images directly from a registry into a `docker load` compatible tar file, without using the Docker daemon.

## Highlights

- Single binary with both CLI and terminal UI (TUI).
- Direct Docker Registry HTTP API V2 implementation (`net/http`), no local Docker dependency.
- Streams layers from registry into the final archive without intermediate layer files on disk.
- Proxy-aware: `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and explicit `--proxy` (including `socks5://`).
- Multi-arch aware: supports manifest lists and architecture selection.
- Cross-platform builds (Linux, macOS, Windows) via GitHub Actions release workflow.
- Termux support via standard `linux/arm64` binary (no root, no APK packaging).

## Install

Build from source:

```bash
go build -o dia .
```

## Usage

### Interactive mode (TUI)

Run with no arguments:

```bash
./dia
```

### CLI mode

```bash
./dia --image alpine:latest --output alpine.tar
```

Or positional image/output:

```bash
./dia alpine:latest alpine.tar
```

### Multi-arch selection

When the image tag points to a manifest list, use `--arch` to pick entries by 1-based index:

```bash
# single index
./dia --image alpine:latest --arch 1 --output alpine-amd64.tar

# multiple indices and ranges
./dia --image alpine:latest --arch 1,2,5- --output alpine-multi.tar
```

If `--arch` is omitted, all architectures are selected by default.

### Proxy options

- Environment variables: `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`
- Explicit override: `--proxy http://127.0.0.1:8080`
- SOCKS5: `--proxy socks5://127.0.0.1:1080`

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

## Release automation

`.github/workflows/release.yml`:

- runs tests
- cross-compiles all `go tool dist list` targets for OS: `linux`, `darwin`, `windows`
- publishes binaries and checksums on tag push (`v*`)

## Notes

- Registry auth supports standard bearer token flow and optional `--username/--password`.
- `DIA_REGISTRY_USERNAME` and `DIA_REGISTRY_PASSWORD` are also supported.
- zstd-compressed layers are currently not supported in this build.
