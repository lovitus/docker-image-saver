# Remote management

## Prerequisites

Controller:

- any released Windows, macOS, or Linux `dia` binary
- a browser for GUI mode
- network access to the SSH endpoint

Execution machine:

- Linux or macOS
- an SSH server and a non-root user
- a writable workspace
- network access to the selected registries

Docker and `skopeo` are optional. The native engine works without either.

Source images must be available through a saved Registry V2 profile. Local Docker daemon images are intentionally not used, which keeps execution identical on machines without Docker.

Harbor management supports two explicit paths: direct access from the controller process, or access through a saved SSH execution machine. Registry synchronization and `local_tar` still always require an execution machine.

## 1. Save registries and accounts

Open **Connection settings** and create each Registry or Harbor profile:

- `Endpoint`: origin only, for example `https://registry.example.com`
- `Namespace`: optional prefix applied to relative image names
- `Proxy`: optional proxy used on the execution machine
- `Insecure TLS`: only for intentionally untrusted/private test certificates

Create any number of accounts for a profile. Password/token fields are write-only in the browser: leaving an existing secret blank keeps it, while selecting **clear saved secret** removes it.

On the Harbor page, choose **Direct from this machine** when the controller has network access, or choose **via _machine_** to execute through SSH. The profile proxy is used by whichever process performs the request.

## 2. Save and trust an execution machine

Create an SSH profile with address, user, authentication, workspace, and optional storage roots. Press **Test**.

On first connection, `dia` reads the server host key without sending the saved password. Compare the displayed SHA-256 fingerprint with an independently trusted value, then confirm it. Future mismatches are rejected and never silently replaced.

Released controllers deploy the matching remote agent when necessary:

1. detect remote `uname -s` and `uname -m`
2. download the matching release asset and `checksums.txt`
3. verify SHA-256 locally
4. upload to a temporary remote path
5. atomically install and verify the agent version

A development build can upload itself to the same OS/architecture. It cannot bootstrap a different platform; install a released `dia` on that host or use a release controller.

## 3. Choose the default machine

The Sync page remembers one default execution machine. Selecting another machine affects only the current job until **Set as default** is pressed.

Only one synchronization task may run on a given execution machine from one GUI process at a time. This prevents overlapping jobs from competing for the same generated archive paths.

## 4. Save an image list

Each non-comment line is either:

```text
source/repository:tag
source/repository:tag -> target/repository:tag
```

Digest references are accepted as sources. Duplicate `local_tar` target paths are rejected before downloading data.

For `local_tar`, the right-hand target controls both the remote filename and the tar archive's `RepoTags`. A single-platform archive loads with that target; a multi-platform export appends `-<os>-<arch>[-<variant>]` to every filename and tag. The target must use a tag because docker-load archives cannot encode a digest in `RepoTags`.

## 5. Run Registry synchronization

Select:

- execution machine
- source Registry and account
- target type `registry`
- target Registry and account
- transfer engine
- image list

The native engine recursively copies manifest lists, child manifests, configs, and layers. Existing target blobs are skipped; same-registry cross-repository mounts are attempted before downloading a source blob. Content descriptors are verified while streaming.

`auto` uses optional [`skopeo`](https://github.com/containers/skopeo) only when one process can safely apply the configured proxy and credentials. A failed `skopeo` item is retried natively. Explicit `skopeo` mode does not fall back.

## 6. Run remote `local_tar`

Choose target type `local_tar`. Unless an output root is specified, the remote agent probes:

- configured storage roots
- workspace archive directory
- user archive directory
- writable mounted filesystems

It selects the candidate filesystem with the most available bytes. Each platform is written to a separate tar and validated before atomic replacement. The task result includes all paths and `docker load -i` commands.

## 7. Browse or retrieve remote files

Directory browsing transfers metadata only. File bytes are sent only after the user confirms Download or starts a confirmed drag-out.

Remote paths are constrained to the workspace, configured storage roots, and selected archive directories. Parent symlinks are resolved before authorization; a symlink cannot escape an allowed root. Deleting a symlink removes the link itself, not its target.

The remote agent calculates the full SHA-256 before transfer and sends size/checksum metadata. Programmatic local downloads use a `.part` file, can retry a stale partial file from zero, verify the complete hash, and atomically replace the destination.

## Troubleshooting

### Remote agent cannot bootstrap

- Confirm the controller is a tagged release rather than `dev` when OS/architecture differs.
- Confirm the release has a `dia_<os>_<arch>` asset and matching checksum entry.
- Confirm the SSH user can create the configured remote dia directory.

### `skopeo` is not used in auto mode

Auto mode intentionally chooses native when:

- `skopeo` is absent
- source and target use different proxy settings
- source and target are the same Registry host but require different accounts

Select explicit `skopeo` to receive its exact compatibility/install error.

### Remote `local_tar` chooses an unexpected filesystem

Use **View execution-machine disks** to inspect candidates. Configure storage roots or enter an explicit output root under an allowed root when policy requires a specific location.
