# Windows filesystem semantics and acceptance status

## Status as of 2026-09-10

The Windows backend is an **unverified native preview**, not Windows feature parity or closure of issue #1. The native primitive package and its tests cross-compile for Windows amd64, and Windows-targeted `go vet` passes. No Windows operating system or filesystem has executed these tests in this development session. The host is macOS arm64; a Linux container cannot validate Windows kernel or Shell behavior. No installed Windows VM or Wine command was found during the local availability check.

The OS-independent Windows path validator was also executed on the macOS host using an unchanged temporary copy of its source and the path tests. This verifies lexical validation only. It caught and corrected an overbroad reserved-name check (`COM12` and `LPT123` are ordinary names); it does not validate native path resolution.

Native acceptance must start on **Windows 11 amd64 with local NTFS**, with symlink creation enabled and a disposable case-sensitive directory. UNC/SMB, ReFS, FAT, cloud placeholders, remote redirects, and Windows arm64 have no runtime acceptance evidence. Filesystems lacking required handle metadata or disposition queries fail instead of receiving a path-based fallback.

## Implemented boundary

- A configured native absolute root is opened once with `CreateFileW`, directory semantics, and final-component reparse protection. Shared access logic remains responsible for canonical-root identity revalidation and authorization. Root acquisition is the only absolute-path primitive.
- Children open through `NtCreateFile` using the parent handle and one validated component. `FILE_OPEN_REPARSE_POINT` opens the final object itself. Reads/truncations inspect that handle and reject reparse points before data mutation. The same handle remains pinned during inspection and use.
- Reopening `.` obtains a fresh directory open description for each enumeration. Handles permit read/write/delete sharing; sharing rules of independently opened handles can still reject a mutation.
- Metadata comes from handles. Identity preserves the 64-bit volume serial and complete 128-bit file ID. Creation time comes from the actual creation-time field. Symbolic links and mount-point junctions are reported as links; unknown reparse tags are irregular and cannot be traversed by the native open helpers.
- Parent case-sensitivity is queried from its handle. Explicitly case-sensitive directories keep distinct names; insensitive directories accept aliases. Path spelling is never an identity substitute. A failed case-information query is an error.
- Exclusive creation uses `FILE_CREATE`. No-replace rename opens the source without following its final link and sends `FILE_RENAME_INFORMATION` with replacement disabled and a pinned destination directory. There is no copy/delete or existence-check fallback. The documented contract rejects existing targets and limits rename to one volume. Native execution must verify these properties on the target runner. [Microsoft rename contract](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_rename_information).

Windows tool paths accept `/` or `\` and normalize to `/`. Validation rejects `..` before cleaning, absolute/rooted/drive-relative/device/UNC tool paths, alternate streams, reserved device names (including superscript device digits), trailing dots/spaces, invalid UTF-8, NUL/control characters, and Windows wildcard characters. Each component is at most 255 UTF-16 code units. The submitted relative path is at most **2,048 UTF-8 bytes**, leaving room under the existing 4,096-byte recovery-path allowance for generated recovery names. Configured absolute root names do not enter tool recovery results. The cap is conservative and applies before cleaning. Enabling Windows edits later still requires complete serialized-envelope boundary tests.

## Permanent deletion

Removal operates on the final object handle and requests `FILE_DISPOSITION_INFORMATION_EX` with delete, POSIX semantics, and an image-section check. Readonly attributes are never cleared. Directory deletion verifies object type; a junction can be unlinked as a final link without entering its target. Recursive walking, bounds, authorization, and partial-delete reporting belong to the shared service.

With POSIX disposition, the name disappears when the deleting handle closes, while other open handles can retain access to the underlying streams. Without that flag, deletion can remain pending until other handles close. The implementation deliberately has **no legacy deferred-deletion fallback**; an unsupported disposition or sharing/access/readonly failure is returned. The native tests require an open reader to retain its bytes while the removed name becomes unavailable. This behavior is documented, not yet observed here. [Microsoft disposition semantics](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntddk/ns-ntddk-_file_disposition_information_ex).

## Edit parity: no-go pending native evidence

`SupportsAtomicEdit` is false, and `Exchange`/permission-preserving replacement return `atomic_replace_unsupported` without touching either object. The service must reject unavailable edits before confirmation or temporary-file creation.

`ReplaceFileW` accepts names rather than pinned parent handles. Its backup option does not establish this service's exchange/rollback contract. In particular, `ERROR_UNABLE_TO_MOVE_REPLACEMENT_2` can leave the displaced object under its backup name while the replacement retains its name after acquiring streams/attributes. Ignore-ACL-error flags explicitly weaken ACL preservation and are unacceptable here. These are concrete reasons not to adapt it by blind temporary-file cleanup or three separate renames. [Microsoft replacement API and partial failures](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-replacefilew).

Still required: prove parent containment during replacement, displaced-object identity and late writes, ACL preservation, every partial failure's recovery ledger, rollback with a third writer, and complete JSON size accounting. No native `ReplaceFileW` experiment was executed; no result is being inferred from cross-compilation. A different edit contract would require an explicit proposal and user acceptance before enabling it.

## Recycle Bin parity: no-go pending native evidence

The native Recycle Bin operation stays `trash_unsupported`. It must never fall through to permanent deletion or an ordinary Shell delete.

`IFileOperation` is the relevant future API. `FOFX_RECYCLEONDELETE` requests recycling; default undo flags only preserve undo information where possible, and warning flags discuss destruction. A successful queued call is insufficient. [Microsoft operation flags](https://learn.microsoft.com/en-us/windows/win32/api/shobjidl_core/nf-shobjidl_core-ifileoperation-setoperationflags).

`PerformOperations` can return success after cancellation. Completion needs its result, `GetAnyOperationsAborted`, and per-item completion reporting. `PostDeleteItem` supplies the actual deletion result; its resulting Shell item is null after full deletion. Detecting permanent deletion afterward would already be too late for this service's authorization guarantee. [Operation execution](https://learn.microsoft.com/en-us/windows/win32/api/shobjidl_core/nf-shobjidl_core-ifileoperation-performoperations), [per-item delete result](https://learn.microsoft.com/en-us/windows/win32/api/shobjidl_core/nf-shobjidl_core-ifileoperationprogresssink-postdeleteitem).

Still required: a COM-threading implementation, proof that disabled/full/unavailable recycling cannot permanently destroy an item, source pinning under parent/junction retargeting, and restoration to the original location. Any staging design must first establish every failure/cancellation rollback and disclose all retained paths. No Shell recycling experiment was executed.

## Native acceptance checklist

The source test suite is prepared to cover exclusive rename collisions (including concurrent contenders), pinned-parent replacement, fresh enumeration, recognized links and actual junctions, readonly/open-handle deletion, full metadata identity, case aliases, case-sensitive distinct names, Unicode, paths beyond 260 characters, and unsupported exchange leaving both files intact. The tests are source artifacts; **they have not passed on Windows yet**.

Run the Windows package tests and vet on the native runner. Enable symlink privilege/developer mode; symlink and case-sensitive test failures are not skipped. Supply `SCOPEDFS_WINDOWS_SECOND_VOLUME` pointing to a writable second NTFS volume for the cross-volume test; without it that one acceptance check is explicitly skipped and remains unverified. Then run the complete service/server suites and launch the packaged stdio executable with disposable roots. Record OS build, filesystem, privilege setup, test output, real second-volume results, and recovery behavior before changing the support claim.

```powershell
go test -count=1 ./internal/platform
go vet ./internal/platform
go test -count=1 ./...
go test -count=1 -race ./...
go vet ./...
go build ./cmd/scoped-filesystem-mcp
```

The native race run requires a compatible C toolchain. Neither Windows-targeted compilation nor a passing Linux run substitutes for this checklist.
