# docker-image-saver (`dia`)

`dia` saves Docker/OCI images directly from a registry into a `docker load` compatible tar file, without using the Docker daemon.

## Highlights

- Single binary with both CLI mode and interactive wizard mode.
- Direct Docker Registry HTTP API V2 implementation (`net/http`), no local Docker dependency.
- Streams layers from registry into the final archive without intermediate layer files on disk.
- Proxy-aware: `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and explicit `--proxy` (including `socks5://`).
- Multi-arch aware: supports manifest lists and architecture selection.
- When multiple architectures are selected, each platform is exported into its own tar file.
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

After a successful save (CLI or wizard), the tool prints:

- `Status: SUCCESS`
- absolute output path(s)
- output file size(s) in bytes and MiB
- selected platform(s)
- generated platform index file (`*_platforms.json`)
- manual `docker load -i` commands for each tar

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
