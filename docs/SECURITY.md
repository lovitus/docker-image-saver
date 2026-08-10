# Security model

## Trust boundaries

- The GUI HTTP server is local-only and binds to `127.0.0.1`.
- Registry and Harbor endpoints are untrusted network peers.
- SSH execution machines are trusted only after explicit host-key confirmation.
- The browser is not given saved secret values.

## GUI control API

At startup, `dia` creates a random 256-bit token. The launch URL exchanges it for an HttpOnly, SameSite=Strict cookie and redirects to a token-free URL. Requests are accepted only when:

- the HTTP Host is loopback
- Origin host and port match the GUI listener
- the session cookie matches in constant time

Responses include a restrictive local Content Security Policy, no-referrer, no-store, frame denial, and MIME sniffing protection. The server has no public-listen option.

The recent-task API is protected by the same session boundary. It exposes only in-memory, redacted task snapshots; terminal snapshots are removed after 10 minutes and are never persisted.

## Saved secrets

Passwords and SSH passphrases are stored in an AES-256-GCM envelope separate from metadata. A random master key is written with user-only permissions where the OS supports them, or can be supplied via `DIA_CONFIG_KEY`.

The default adjacent key protects against accidental plaintext disclosure, backups that include only `config.json`, and casual inspection. It does not protect against malware or an attacker with full access to the same OS account and both encrypted vault and key. Use OS account protection and an externally supplied `DIA_CONFIG_KEY` for a stronger operational boundary.

Secrets are intentionally excluded from:

- HTML bootstrap data
- settings API responses
- task snapshots and SSE events
- output indexes
- command-line arguments used for `skopeo` or the remote agent
- application logs

Temporary `skopeo` auth files are mode `0600` and removed after the job.

Authenticated proxy URLs supplied temporarily are redacted from HTML bootstrap data, task snapshots, and errors; startup credentials remain server-side behind an explicit checkbox. Registry profiles reject proxy URL userinfo so it cannot be persisted in plaintext. Configure authenticated persistent proxies in the execution machine's environment instead.

## SSH

- First-use probing aborts in the host-key callback before password authentication is attempted.
- Saved SHA-256 fingerprints are strict; changed keys fail closed.
- The remote agent uses one framed request over SSH stdio and opens no listening port.
- Closing the SSH input cancels the remote context, including Registry requests and `skopeo` subprocesses.
- Released remote-agent bootstrap verifies SHA-256 checksums before upload and version after installation.

## Remote filesystem

File operations are restricted to canonical paths under configured roots. Parent symlinks are resolved before the boundary check. Downloads require regular files. Directory deletion is not supported, and configured roots cannot be deleted.

Archive writes use a temporary file in the destination directory. Only a fully validated file is atomically moved to the final name. Concurrent GUI tasks reserve canonical derived output paths to prevent truncation races.

## Registry integrity

The native reader supports digest algorithms implemented by `dia` (`sha256`, `sha384`, and `sha512`) and rejects unsupported declarations. It verifies descriptor sizes, compressed blob digests, config digests, manifest digests, uncompressed layer diffIDs, and completed docker-load structure.

TLS certificate verification is enabled by default. `Insecure TLS` should be limited to controlled environments.

## Reporting a vulnerability

Do not include live passwords, tokens, private keys, or private endpoint details in a public issue. Provide a minimal reproducer with synthetic credentials and redact registry/SSH logs before sharing.
