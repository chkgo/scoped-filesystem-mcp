# Scoped Filesystem MCP

`scoped-filesystem-mcp` is a local MCP server. It communicates over standard
input and standard output; it does not listen on a network socket. Its YAML
configuration is shared by Codex tasks and specifies the only filesystem roots
the server can access.

## Platform status

The shared MCP logic uses build-selected native filesystem operations. macOS
and Linux implement all tools, subject to the backing filesystem supporting
atomic rename/exchange. Linux Trash currently supports the home Trash on the
same filesystem; cross-filesystem moves are rejected without copy/delete.

Windows amd64 is a **partial, not yet natively validated implementation**.
Native handle-based read/list/search/create/move/permanent-delete operations
are implemented. `edit_file` and system `trash_path` return
`atomic_replace_unsupported` and `trash_unsupported` before elicitation or
mutation. `list_roots` keeps configured `allow` unchanged and reports unavailable
allowed operations in `unsupported`. Full Windows support and issue #1 remain
open; see [Windows semantics and validation](docs/windows-filesystem-semantics.md).
A successful cross-build does not establish native Windows behavior.

The current verification covers macOS arm64 race tests and native Linux arm64
tests in a Fedora container. CI adds native macOS, Linux, and Windows jobs;
those hosted runs have not been executed as part of this local change.

## Security boundary

The configured directory roots and each root's `allow` list are the security
boundary. A tool approval in Codex does not grant access outside those roots,
does not add an operation missing from `allow`, and does not permit an absolute
or traversing path. The server resolves and authorizes canonical paths while
reporting the configured root name and relative path.
Directory listings report symlink metadata without following the target, so
dangling links and links outside the root do not block sibling entries. Reading
through an out-of-root link remains denied.

Operations in a root's `ask` list require MCP elicitation before the operation
proceeds. Permanent deletion always requires confirmation. `ask` and conflict
prompts are server-side decisions, so `.mcp.json` intentionally has no
per-tool prompt settings.

## Build and configure

Complete the temporary-root checklist below before enabling a real vault.
For that check, use a separate temporary YAML file instead of copying the
example's real directory policy into the default configuration.

Review `config.example.yaml` before copying it: the configuration is outside
the repository so a source update cannot overwrite local policy. From this
repository, run these stages manually:

```bash
mkdir -p bin
go build -o bin/scoped-filesystem-mcp ./cmd/scoped-filesystem-mcp
mkdir -p "$HOME/.config/scoped-filesystem-mcp"
cp config.example.yaml "$HOME/.config/scoped-filesystem-mcp/config.yaml"
mkdir -p "$HOME/plugins"
ln -sfn /absolute/path/to/scoped-filesystem-mcp "$HOME/plugins/scoped-filesystem-mcp"
```

The executable selects configuration in this order: `--config PATH`, then
`SCOPED_FILESYSTEM_MCP_CONFIG`, then the platform default:

| Platform | Default configuration |
| --- | --- |
| macOS | `~/.config/scoped-filesystem-mcp/config.yaml` (existing location preserved) |
| Linux | `$XDG_CONFIG_HOME/scoped-filesystem-mcp/config.yaml`, or `~/.config/scoped-filesystem-mcp/config.yaml` |
| Windows | `%APPDATA%\scoped-filesystem-mcp\config.yaml` |

Missing configuration is an error; startup never creates or overwrites policy.
Paths beginning with `~/` are expanded; Windows also accepts `~\`.
The Unix launcher forwards arguments and lets the executable select configuration.

For Windows, build and start directly from PowerShell:

```powershell
New-Item -ItemType Directory -Force bin | Out-Null
go build -o bin/scoped-filesystem-mcp.exe ./cmd/scoped-filesystem-mcp
& .\bin\scoped-filesystem-mcp.exe --config C:\path\to\config.yaml
```

Start from `config.example.windows.yaml` and replace its root with your actual
absolute path. The example deliberately omits unavailable edit and Trash
operations. Permanent deletion still always requires confirmation.

Create platform-specific plugin archives with:

```bash
go run ./scripts/package -output dist
```

Use `-goos windows -goarch amd64` (or another listed platform pair) to build one
archive. Each archive contains its native executable and a matching `.mcp.json`;
Windows archives invoke the `.exe` directly without a Unix shell. Unpack the
archive and register that extracted plugin directory through your host's plugin
installation flow. The source checkout's `.mcp.json` retains the Unix launcher.
Build/package automation does not install anything or modify user configuration.

## Install as a personal plugin

The default personal marketplace at `~/.agents/plugins/marketplace.json` is
implicitly discovered by Codex. If the file does not exist, create it with
this complete personal marketplace, including the approved plugin entry:

```json
{
  "name": "personal",
  "interface": {
    "displayName": "Personal"
  },
  "plugins": [
    {
      "name": "scoped-filesystem-mcp",
      "source": {
        "source": "local",
        "path": "./plugins/scoped-filesystem-mcp"
      },
      "policy": {
        "installation": "AVAILABLE",
        "authentication": "ON_INSTALL",
        "products": ["CODEX"]
      },
      "category": "Developer Tools"
    }
  ]
}
```

If `~/.agents/plugins/marketplace.json` already exists, ensure its top-level
`name` is `personal`, preserve its existing `interface` and entries, and add
this exact entry to its `plugins` array:

```json
{
  "name": "scoped-filesystem-mcp",
  "source": {
    "source": "local",
    "path": "./plugins/scoped-filesystem-mcp"
  },
  "policy": {
    "installation": "AVAILABLE",
    "authentication": "ON_INSTALL",
    "products": ["CODEX"]
  },
  "category": "Developer Tools"
}
```

Install it from the implicitly discovered marketplace:

```bash
codex plugin add scoped-filesystem-mcp@personal
```

Use `codex plugin marketplace add` only when the marketplace is a non-default,
explicit location. For example, first register the directory containing that
other marketplace, then install from its confirmed name:

```bash
codex plugin marketplace add /path/to/non-default/marketplace-root
codex plugin add scoped-filesystem-mcp@other-marketplace
```

Fully restart Codex Desktop after installation so it refreshes the available
tools. Existing tasks can then use the newly installed plugin; opening a new
task is optional.

## Temporary-root Codex Desktop verification

**Status: all Codex Desktop UI steps below are unperformed.** The automated
`TestEndToEndScenario` uses the real SDK client/server over in-memory transport
and real temporary files, including a temporary Trash destination. It does
not establish how Codex Desktop renders attachments or presents approvals.
Installation and user-level configuration remain manual steps after review.

Create a fresh disposable directory with `mktemp -d` and record its absolute
path. Inside it, create `vault`, `archive`, and `outside` directories, and an
`archive-link` symlink pointing to `archive`. Put a harmless text file, a PNG,
and a small PDF in `vault`; keep each file below 10 MiB. Put a harmless marker
in `outside`, and create an `escape` symlink inside `vault` pointing to
`outside`. The marker lets you verify denied access without touching private
files. Configure only `vault` and `archive-link` in a separate temporary YAML:

```yaml
version: 1
directories:
  - name: test_vault
    path: /absolute/disposable-directory/vault
    allow: [list, search, read, create, edit, move, trash, permanent_delete]
    ask: []
    on_conflict: ask
  - name: test_archive
    path: /absolute/disposable-directory/archive-link
    allow: [list, search, read, create, edit, move, trash, permanent_delete]
    ask: []
    on_conflict: ask
```

Replace both placeholder paths with the actual temporary paths. Set
`SCOPED_FILESYSTEM_MCP_CONFIG` to this temporary YAML's absolute path in the
environment used to start Codex Desktop. Run the built binary with
`--config /absolute/path/to/temporary.yaml` to check startup: invalid YAML or
roots cause a nonzero exit and an error on stderr; a valid server waits for
MCP input until interrupted. After restarting Desktop, use `list_roots` to
verify that only the two temporary roots are active and that `test_archive`
reports the configured symlink path before trying any mutation.

Use a directory such as `draft` containing `note.md` for edits and moves.
Retain each returned revision. For the conflict check, modify `note.md` from
another editor, then submit an edit using the older revision. Choose
`reload_and_rebase` and verify the current content plus the complete pending
proposal, expected revision, and all structured edits are returned. Build a
new proposal that retains the external change and retry with the returned
current revision. For deletion checks, use only `disposable.txt`: decline
once, verify it remains, move it to Trash, recreate it, and then explicitly
accept permanent deletion of the recreated file. Verify the trashed first
copy still exists. On Windows the edit/recycling steps remain unavailable; do
not mark the full checklist complete. Empty `ask` lists let the move/Trash checks distinguish
the server's configured policy from any additional host approval.

```text
[ ] Build the binary and validate the temporary YAML root.
[ ] Install/reinstall the personal plugin and fully restart Codex Desktop.
[ ] Open an existing or new task outside the temporary root.
[ ] Call list_roots and read a harmless text file.
[ ] List the vault and verify the escape symlink appears without exposing its target contents.
[ ] Read a PNG and a small PDF through read_binary_file.
[ ] Create and revision-edit a harmless note.
[ ] Modify the note externally and verify reload_and_rebase preserves the proposal.
[ ] Move a non-empty test directory without a redundant Codex approval.
[ ] Move a test item to Trash without a redundant Codex approval.
[ ] Confirm permanent_delete_path elicits exactly once and decline it.
[ ] Accept deletion of a recreated disposable file and verify it is gone.
[ ] Attempt an absolute path, ../ traversal, and symlink escape; verify all are denied.
[ ] Replace the temporary YAML root with the real Obsidian root only after every check passes.
```

For the three denial checks, try the harmless outside marker's absolute path,
`../outside/marker.txt`, and `escape/marker.txt` under `test_vault`; verify no
contents are returned and the marker is unchanged. Inspect the actual system
Trash and verify restoration after the Trash checks. Keep these checklist items unchecked until
their visible results have been verified. If any check fails, retain the
temporary configuration and record the observed result before enabling the
real vault with its separately reviewed permissions.

## Filesystem limitations and recovery

- Search results default to 100 and are capped at 1,000. Narrow the root path
  or query instead of expecting a larger result set. Content search is literal;
  it treats valid UTF-8 containing NUL or control bytes other than tab/newline
  (including DEL byte `0x7f`) as binary and does not return matches from that file. It also skips files
  that exceed the response-size limit and caps aggregate returned match text.
  Capped results are the globally lexicographically earliest paths (and then
  earliest line numbers), independent of filesystem enumeration order. Search
  retains only the prefix that fits the 10 MiB aggregate estimate plus one
  path/line boundary key per file while it is classified. Those boundaries are
  merged globally, so an earlier over-budget match prevents later paths from
  entering the result even when the later path was enumerated first. Discarded
  matching-line strings are released during traversal rather than accumulating
  up to the 1,000-result count limit.
- Text and binary responses are limited to 10 MiB. Binary writes, batches,
  nested rules, profiles, cross-filesystem copy/delete, and automatic semantic
  merging are not supported.
- On macOS, removals move to macOS Trash. A cross-filesystem Trash move fails rather than
  copying and deleting. The destination directory is opened, revalidated, and
  pinned before the move, including the default `~/.Trash`; replacing its path
  afterward cannot redirect the move. If macOS privacy controls deny opening
  or validating Trash, the operation fails and leaves the source in place.
  There is no path-based fallback. Permanent deletion is a separately elicited
  operation.
- New text files are synced under a private name and atomically published with
  a no-replace operation, so a concurrent destination is never overwritten or
  removed during cleanup.
- Linux Trash creates freedesktop `files/` and `info/` entries with restoration
  metadata under `$XDG_DATA_HOME/Trash` (default `~/.local/share/Trash`). Destinations
  are checked for ownership/permissions and pinned. If a native rename has an
  uncertain outcome, `trash_outcome_uncertain` retains the metadata for inspection;
  `trash_metadata_cleanup_failed` reports that a failed attempt could not safely
  remove its reserved metadata. Neither case falls back to permanent deletion.
- Text replacement verifies the expected revision and uses macOS `RENAME_SWAP`
  or Linux `RENAME_EXCHANGE`. If the backing filesystem does not support atomic swap, the
  edit fails with `atomic_replace_unsupported`; it never falls back to a lossy
  ordinary rename. If a concurrent change is discovered after a swap, the
  server restores it when possible.
- Every successful edit deliberately retains the displaced prior inode beside
  the canonical target under a hidden `.scopedfs-recovery-*` name and returns
  its root-relative `recovery_path`. A rollback retains the proposal, or any
  third-writer version displaced by that rollback, in the same way. The server
  never automatically unlinks these post-swap files because another process
  may still have the inode open and write to it after verification. Inspect the
  recovery path, then explicitly call `trash_path` on it when the root allows
  Trash (or clean it up manually). Recovery artifacts otherwise accumulate.
  Exceptional recovery results also include the last-known target state and
  content/revision when safely available; oversized or unreadable recovery
  content is reported as unavailable while its path remains disclosed. Do not
  assume a reported state describes the file's current live state. The server
  preserves versions it displaces; it cannot prevent independent external
  writers from overwriting each other's changes.
- `edit_file` requires both `read` and `edit`, because normal edits and conflict
  choices may return current content. An `ask` rule on either operation is
  honored before that content is read or changed.
- A pending edit's exact UTF-8 JSON envelope—expected revision, proposed text,
  and complete structured edit array—is limited to 10 MiB. The server rejects
  a larger request without truncating or storing it. The combined conflict
  output is checked against the same exact serialized 10 MiB limit before
  pending state is stored and again for every later conflict outcome. If file
  growth makes a later result too large, the server returns `response_too_large`
  without truncation, keeps the complete proposal when it fits in that bounded
  error, and always returns the recovery-path ledger. An indivisible proposal
  that cannot fit in the error is omitted rather than truncated; the caller
  still owns the exact text supplied in its request.
- When a rollback has already retained a recovery file, `recovery_paths`
  carries every such path through reload, overwrite, repeated conflicts,
  failures, cancellation, and expired confirmation. While elicitation is
  pending, the paths appear in the elicitation message because MCP forbids a
  tool result from combining normal structured content with input requests.
- Before an edit can perform an atomic swap, its existing recovery ledger must
  leave room for the worst-case JSON encoding of one additional 4,096-byte
  root-relative recovery path. If not, the edit stops before mutation and
  returns the complete existing ledger. Structured error fallback drops
  oversized root/path context before recoverable data. An already-invalid,
  externally constructed ledger that cannot itself fit is reported with
  `recovery_paths_omitted`; operational edit paths are protected from that case
  by the pre-swap reservation.
- Directory deletion reads bounded batches and checks cancellation throughout.
  If an error occurs after some descendants were removed, `partial_delete`
  explicitly reports that the requested tree was only partly deleted.
- Permanent-delete approval fingerprints the requested entry's type, size,
  modification time and native file identity immediately before deletion. It is not
  an atomic snapshot: another process can still change the target after the
  check, metadata-preserving content changes can evade metadata checks, and a directory
  fingerprint does not cover every descendant.
- Confirmation tokens are one-use, argument-bound, and expire after five
  minutes. Later prompt housekeeping removes ordinary expired records. An
  expired conflict that owns retained recovery files is reduced to a compact
  tombstone containing its target and `recovery_paths`, so a late continuation
  can still identify every cleanup obligation without retaining the potentially
  10 MiB proposal. The token model assumes one client on the local stdio
  connection. The SDK may reject malformed continuation requests before the
  server can return a structured tool error.
- Presenting a known token with a different tool or changed arguments never
  authorizes or consumes it. The rejection includes its recovery ledger, and
  the exact original continuation may subsequently consume it once. This also
  applies when the changed edit exceeds the pending-envelope size limit: the
  server returns `invalid_confirmation` with the known token's recovery ledger
  before filesystem access. A fresh oversized edit returns `response_too_large`.
