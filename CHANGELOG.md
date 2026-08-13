# Changelog

## Unreleased

### Added

- Added a `gui-preview` workflow that renders every GUI page against a stub API in CI, uploads desktop and mobile screenshots, and fails on page JavaScript errors.
- Added checksum-verified `dia-preview-binaries` artifacts to the same workflow so maintainers can exercise the GUI from CI output instead of building locally.

### Changed

- Rebuilt the embedded GUI with a light, high-density layout: sidebar navigation, per-page cards, and a dedicated task page replacing the cramped task rail.
- Replaced every dependent dropdown (registries, accounts, execution machines, Harbor projects and repositories, image lists) with searchable comboboxes that load their dependants automatically.
- Added inline field validation, panel-level result banners, empty states, and busy indicators to the sync, Harbor, remote-file, export, and settings forms.
- Remote file browsing now offers clickable path breadcrumbs, row-level directory navigation, and disk-usage cards with utilization bars.

## v1.3.1 - 2026-08-10

### Added

- Added a recent-task picker and automatic reconnection to running GUI jobs after browser refresh.
- Added URL-hash page restoration and direct browsing actions for remote storage candidates.

### Changed

- Sync, local export, image-list, and connection-setting save controls now reject duplicate submissions while requests are pending.
- Live SSH, Registry, Harbor, remote-storage, and docker-load validation now runs only on approved private hosts; GitHub Actions remains the mandatory quality, cross-build, and release gate.

### Fixed

- Canceling a GUI confirmation dialog with Escape now resolves the pending operation instead of leaving it suspended.
- Stale task, Harbor browse, remote-file, and storage-probe responses can no longer overwrite a newer GUI selection.
- Task-store proxy redaction is now enforced at the storage boundary.

## v1.3.0 - 2026-08-10

### Added

- Persistent Registry and Harbor profiles with multiple encrypted accounts.
- Persistent SSH execution machines, strict host-key confirmation, and remembered default execution machine.
- Editable persistent image lists with optional source-to-target mappings.
- Native streaming Registry-to-Registry synchronization without Docker.
- Optional `skopeo` engine with compatibility checks and automatic native fallback in `auto` mode.
- Remote `local_tar` jobs that select the writable filesystem with the most available space.
- SSH stdio remote agent with checksum-verified release bootstrap for Linux/macOS execution machines.
- SSH-routed Harbor project, repository, artifact, and tag management.
- Direct Harbor management from the controller with an explicit local/remote access-path selector.
- Metadata-only remote file browser with explicit confirmed file transfer.
- Encrypted AES-256-GCM secret vault and loopback GUI session protection.
- Repository-level workflow gate: local work is syntax-only, while tests, race detection, cross-builds, packaging, and releases run exclusively in GitHub Actions.

### Changed

- GUI redesigned as a five-page control deck for sync, Harbor, remote files, single export, and connection settings.
- Release CI now runs module verification, `go vet`, and race-enabled tests before cross-compilation.
- Registry copy preserves the source manifest digest algorithm and reports per-image blob progress.
- Local and remote downloads use atomic final replacement.
- Harbor resource selectors use stable names, support nested repositories, and paginate large result sets.
- Multi-platform archives use platform-qualified Docker tags so loading one platform does not overwrite another platform's tag.

### Fixed

- Startup GUI credentials remain usable without exposing passwords in HTML.
- Local GUI export cancellation now cancels active Registry requests.
- Stale inspect results cannot overwrite a newer image/account selection.
- Concurrent and aliased output paths are reserved before export.
- Remote symlink deletion removes the link rather than its target.
- Stale or oversized partial downloads retry safely from zero.
- Unsafe repository/namespace path rewriting and mismatched explicit Registry hosts are rejected.
- Child-process diagnostic output and all in-memory Registry/Harbor responses are bounded.
- Compressed layer descriptors are verified through underlying EOF even when a decompressor finishes first.
- Split multi-platform tar archives now preserve distinct `RepoTags` such as `tag-linux-amd64` and `tag-linux-arm64-v8`.

## v1.2.0

- Added the embedded local GUI, zstd layer support, byte/rate/ETA progress, bounded blob reuse, and stricter docker-load archive validation.
