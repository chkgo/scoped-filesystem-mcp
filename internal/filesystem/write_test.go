package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

func TestCreateDirectoryCreatesParentsOnlyWhenRequested(t *testing.T) {
	root := t.TempDir()
	service := writeService(t, root)

	_, err := service.CreateDirectory(context.Background(), "notes", "drafts/2026", false)
	assertFSCode(t, err, "path_not_found")
	if _, err := os.Stat(filepath.Join(root, "drafts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("parent after declined creation = %v, want absent", err)
	}

	got, err := service.CreateDirectory(context.Background(), "notes", "drafts/2026", true)
	if err != nil || got.Root != "notes" || got.Path != "drafts/2026" {
		t.Fatalf("CreateDirectory() = %#v, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(root, "drafts", "2026"))
	if err != nil || !info.IsDir() {
		t.Fatalf("created directory = %#v, %v", info, err)
	}
}

func TestCreateTextCreatesParentsOnlyWhenRequested(t *testing.T) {
	root := t.TempDir()
	service := writeService(t, root)

	_, err := service.CreateText(context.Background(), "notes", "drafts/note.md", "body\n", false)
	assertFSCode(t, err, "path_not_found")

	got, err := service.CreateText(context.Background(), "notes", "drafts/note.md", "body\n", true)
	if err != nil || got.Revision != Revision([]byte("body\n")) || got.Size != 5 {
		t.Fatalf("CreateText() = %#v, %v", got, err)
	}
	assertWriteFileContent(t, root, "drafts/note.md", "body\n")
}

func TestCreateTextRejectsExistingDestination(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.md"), []byte("external\n"))

	_, err := writeService(t, root).CreateText(context.Background(), "notes", "note.md", "agent\n", false)
	assertFSCode(t, err, "destination_exists")
	assertWriteFileContent(t, root, "note.md", "external\n")
}

func TestEditTextRequiresExactOccurrenceCount(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.md"), []byte("todo, todo\n"))
	service := writeService(t, root)

	got, conflict, err := service.EditText(context.Background(), "notes", "note.md", EditRequest{
		ExpectedRevision: Revision([]byte("todo, todo\n")),
		Edits:            []TextEdit{{OldText: "todo", NewText: "done", ExpectedOccurrences: 1}},
		ProposedContent:  "done, done\n",
	})
	if got != nil || conflict != nil {
		t.Fatalf("EditText() = %#v, %#v, %v", got, conflict, err)
	}
	assertFSCode(t, err, "invalid_edit")
	assertWriteFileContent(t, root, "note.md", "todo, todo\n")
}

func TestEditTextAppliesSequentialReplacements(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.md"), []byte("green green\n"))
	service := writeService(t, root)

	got, conflict, err := service.EditText(context.Background(), "notes", "note.md", EditRequest{
		ExpectedRevision: Revision([]byte("green green\n")),
		Edits: []TextEdit{
			{OldText: "green", NewText: "blue", ExpectedOccurrences: 2},
			{OldText: "blue", NewText: "red", ExpectedOccurrences: 2},
		},
		ProposedContent: "red red\n",
	})
	if err != nil || conflict != nil || got == nil || got.Revision != Revision([]byte("red red\n")) {
		t.Fatalf("EditText() = %#v, %#v, %v", got, conflict, err)
	}
	assertWriteFileContent(t, root, "note.md", "red red\n")
}

func TestEditTextReplacesWholeFileWithoutEdits(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.md"), []byte("old\n"))

	got, conflict, err := writeService(t, root).EditText(context.Background(), "notes", "note.md", EditRequest{
		ExpectedRevision: Revision([]byte("old\n")),
		ProposedContent:  "new\n",
	})
	if err != nil || conflict != nil || got == nil || got.Size != 4 {
		t.Fatalf("EditText() = %#v, %#v, %v", got, conflict, err)
	}
	assertWriteFileContent(t, root, "note.md", "new\n")
}

func TestEditTextRejectsProposedContentThatDoesNotMatchEdits(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.md"), []byte("old\n"))

	got, conflict, err := writeService(t, root).EditText(context.Background(), "notes", "note.md", EditRequest{
		ExpectedRevision: Revision([]byte("old\n")),
		Edits:            []TextEdit{{OldText: "old", NewText: "new", ExpectedOccurrences: 1}},
		ProposedContent:  "different\n",
	})
	if got != nil || conflict != nil {
		t.Fatalf("EditText() = %#v, %#v, %v", got, conflict, err)
	}
	assertFSCode(t, err, "invalid_edit")
	assertWriteFileContent(t, root, "note.md", "old\n")
}

func TestEditTextCleansTemporaryFileAfterReplacementFailure(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.md"), []byte("old\n"))
	service := writeService(t, root)
	service.beforeReplace = func() error { return syscall.EIO }

	got, conflict, err := service.EditText(context.Background(), "notes", "note.md", EditRequest{
		ExpectedRevision: Revision([]byte("old\n")),
		ProposedContent:  "new\n",
	})
	if got != nil || conflict != nil {
		t.Fatalf("EditText() = %#v, %#v, %v", got, conflict, err)
	}
	assertFSCode(t, err, "filesystem_unavailable")
	assertWriteFileContent(t, root, "note.md", "old\n")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".scopedfs-recovery-") {
			t.Fatalf("temporary file %q was not removed", entry.Name())
		}
	}
}

func TestEditTextReportsConflictWithoutWriting(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	before := "external\n"
	writeBytes(t, filepath.Join(root, "note.md"), []byte(before))

	got, conflict, err := writeService(t, root).EditText(context.Background(), "notes", "note.md", EditRequest{
		ExpectedRevision: Revision([]byte("old\n")),
		ProposedContent:  "agent\n",
	})
	if err != nil || got != nil || conflict == nil || conflict.CurrentContent != before || conflict.CurrentRevision != Revision([]byte(before)) {
		t.Fatalf("EditText() = %#v, %#v, %v", got, conflict, err)
	}
	assertWriteFileContent(t, root, "note.md", before)
}

func TestOverwriteTextReplacesPromptedRevision(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.md"), []byte("prompted\n"))

	got, conflict, err := writeService(t, root).OverwriteText(context.Background(), "notes", "note.md", Revision([]byte("prompted\n")), "agent\n")
	if err != nil || conflict != nil || got == nil || got.Revision != Revision([]byte("agent\n")) {
		t.Fatalf("OverwriteText() = %#v, %#v, %v", got, conflict, err)
	}
	assertWriteFileContent(t, root, "note.md", "agent\n")
}

func TestOverwriteTextDetectsSecondBackgroundChangeBeforeReplacement(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("prompted\n"))
	service := writeService(t, root)
	service.beforeReplace = func() error { return os.WriteFile(path, []byte("newer\n"), 0o600) }

	got, conflict, err := service.OverwriteText(context.Background(), "notes", "note.md", Revision([]byte("prompted\n")), "agent\n")
	if err != nil || got != nil || conflict == nil || conflict.CurrentContent != "newer\n" || conflict.CurrentRevision != Revision([]byte("newer\n")) {
		t.Fatalf("OverwriteText() = %#v, %#v, %v", got, conflict, err)
	}
	assertWriteFileContent(t, root, "note.md", "newer\n")
}

func TestEditTextSwapsAndRetainsExpectedDisplacedRevision(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	service := writeService(t, root)
	called := false
	service.renameSwap = func(parent *os.File, from, to string) error {
		called = true
		return platform.Exchange(parent, from, to)
	}

	got, conflict, err := service.EditText(context.Background(), "notes", "note.md", EditRequest{
		ExpectedRevision: Revision([]byte("old\n")),
		ProposedContent:  "new\n",
	})
	if err != nil || conflict != nil || got == nil || !called {
		t.Fatalf("EditText() = %#v, %#v, %v; swap called=%t", got, conflict, err, called)
	}
	assertWriteFileContent(t, root, "note.md", "new\n")
	assertWriteFileContent(t, root, got.RecoveryPath, "old\n")
}

func TestEditTextRollsBackMismatchedDisplacedRevision(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	service := writeService(t, root)
	service.beforeSwap = func() error { return os.WriteFile(path, []byte("second\n"), 0o600) }

	got, conflict, err := service.EditText(context.Background(), "notes", "note.md", EditRequest{
		ExpectedRevision: Revision([]byte("old\n")),
		ProposedContent:  "new\n",
	})
	if err != nil || got != nil || conflict == nil || conflict.CurrentContent != "second\n" || conflict.RecoveryPath == "" {
		t.Fatalf("EditText() = %#v, %#v, %v", got, conflict, err)
	}
	assertWriteFileContent(t, root, "note.md", "second\n")
	assertWriteFileContent(t, root, conflict.RecoveryPath, "new\n")
}

func TestEditTextRetainsThirdVersionDisplacedDuringRollback(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	service := writeService(t, root)
	service.beforeSwap = func() error { return os.WriteFile(path, []byte("second\n"), 0o600) }
	service.beforeRollback = func() error { return os.WriteFile(path, []byte("third\n"), 0o600) }

	got, conflict, err := service.EditText(context.Background(), "notes", "note.md", EditRequest{
		ExpectedRevision: Revision([]byte("old\n")),
		ProposedContent:  "new\n",
	})
	if got != nil || conflict != nil {
		t.Fatalf("EditText() = %#v, %#v, %v", got, conflict, err)
	}
	outcome := assertRecovery(t, err, TargetCurrentRestored, "third\n")
	assertWriteFileContent(t, root, "note.md", "second\n")
	assertWriteFileContent(t, root, outcome.RecoveryPath, "third\n")
}

func TestEditTextReturnsSafeErrorWhenSwapUnsupported(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	service := writeService(t, root)
	service.renameSwap = func(*os.File, string, string) error { return platform.ErrAtomicReplaceUnsupported }

	got, conflict, err := service.EditText(context.Background(), "notes", "note.md", EditRequest{
		ExpectedRevision: Revision([]byte("old\n")),
		ProposedContent:  "new\n",
	})
	if got != nil || conflict != nil {
		t.Fatalf("EditText() = %#v, %#v, %v", got, conflict, err)
	}
	assertFSCode(t, err, "atomic_replace_unsupported")
	assertWriteFileContent(t, root, "note.md", "old\n")
	assertNoTemporaryFiles(t, root)
}

func TestEditTextReturnsRecoveryErrorWhenRollbackCannotRun(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	service := writeService(t, root)
	service.beforeSwap = func() error { return os.WriteFile(path, []byte("second\n"), 0o600) }
	service.beforeRollback = func() error { return syscall.EIO }

	got, conflict, err := service.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
	if got != nil || conflict != nil {
		t.Fatalf("EditText() = %#v, %#v, %v", got, conflict, err)
	}
	o := assertRecovery(t, err, TargetProposalApplied, "second\n")
	assertWriteFileContent(t, root, "note.md", "new\n")
	assertWriteFileContent(t, root, o.RecoveryPath, "second\n")
}

func TestEditTextVerifiedRollbackRetainsProposal(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	service := writeService(t, root)
	service.beforeSwap = func() error { return os.WriteFile(path, []byte("second\n"), 0o600) }
	got, conflict, err := service.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
	if got != nil || conflict == nil || err != nil {
		t.Fatalf("shape = %#v %#v %v", got, conflict, err)
	}
	assertWriteFileContent(t, root, "note.md", "second\n")
	assertWriteFileContent(t, root, conflict.RecoveryPath, "new\n")
}

func TestEditTextReturnsRecoveryErrorWhenRollbackDisplacesThirdVersion(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	service := writeService(t, root)
	service.beforeSwap = func() error { return os.WriteFile(path, []byte("second\n"), 0o600) }
	service.beforeRollback = func() error { return os.WriteFile(path, []byte("third\n"), 0o600) }

	got, conflict, err := service.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
	if got != nil || conflict != nil {
		t.Fatalf("EditText() = %#v, %#v, %v", got, conflict, err)
	}
	outcome := assertRecovery(t, err, TargetCurrentRestored, "third\n")
	assertWriteFileContent(t, root, "note.md", "second\n")
	assertWriteFileContent(t, root, outcome.RecoveryPath, "third\n")
}

func TestEditTextRecoveryAfterDisplacedReadFailure(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	s := writeService(t, root)
	s.beforeReadAt = func(name string) error {
		if strings.HasPrefix(name, ".scopedfs-recovery-") {
			return syscall.EIO
		}
		return nil
	}
	got, conflict, err := s.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
	if got != nil || conflict != nil {
		t.Fatalf("shape = %#v %#v %v", got, conflict, err)
	}
	o := assertRecoveryUnknown(t, err, TargetUnknown)
	assertWriteFileContent(t, root, "note.md", "new\n")
	assertWriteFileContent(t, root, o.RecoveryPath, "old\n")
}

func TestEditTextSuccessDoesNotAttemptRecoveryCleanup(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	s := writeService(t, root)
	got, conflict, err := s.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
	if got == nil || conflict != nil || err != nil {
		t.Fatalf("shape = %#v %#v %v", got, conflict, err)
	}
	assertWriteFileContent(t, root, "note.md", "new\n")
	assertWriteFileContent(t, root, got.RecoveryPath, "old\n")
}

func TestEditTextRecoveryAfterRollbackSwapFailure(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	s := writeService(t, root)
	calls := 0
	s.beforeSwap = func() error { return os.WriteFile(path, []byte("second\n"), 0o600) }
	s.renameSwap = func(parent *os.File, from, to string) error {
		calls++
		if calls == 2 {
			return syscall.EIO
		}
		return platform.Exchange(parent, from, to)
	}
	got, conflict, err := s.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
	if got != nil || conflict != nil {
		t.Fatalf("shape = %#v %#v %v", got, conflict, err)
	}
	o := assertRecovery(t, err, TargetProposalApplied, "second\n")
	assertWriteFileContent(t, root, "note.md", "new\n")
	assertWriteFileContent(t, root, o.RecoveryPath, "second\n")
}

func TestEditTextRecoveryAfterRollbackReadFailures(t *testing.T) {
	if !platform.SupportsAtomicEdit {
		t.Skip("atomic edit contract unavailable on this platform; refusal covered by Windows tests")
	}
	for _, failTemp := range []bool{false, true} {
		t.Run(strconv.FormatBool(failTemp), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "note.md")
			writeBytes(t, path, []byte("old\n"))
			s := writeService(t, root)
			rollback := false
			tempReads := 0
			s.beforeSwap = func() error { return os.WriteFile(path, []byte("second\n"), 0o600) }
			s.beforeRollback = func() error { rollback = true; return nil }
			s.beforeReadAt = func(name string) error {
				if rollback {
					if name == "note.md" && !failTemp {
						return syscall.EIO
					}
					if strings.HasPrefix(name, ".scopedfs-recovery-") {
						tempReads++
						if failTemp && tempReads >= 1 {
							return syscall.EIO
						}
					}
				}
				return nil
			}
			got, conflict, err := s.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
			if got != nil || conflict != nil {
				t.Fatalf("shape %#v %#v %v", got, conflict, err)
			}
			var o RecoveryOutcome
			if failTemp {
				o = assertRecoveryUnknown(t, err, TargetCurrentRestored)
			} else {
				o = assertRecovery(t, err, TargetCurrentRestored, "new\n")
			}
			assertWriteFileContent(t, root, "note.md", "second\n")
			assertWriteFileContent(t, root, o.RecoveryPath, "new\n")
		})
	}
}

func writeService(t *testing.T, root string) *Service {
	t.Helper()
	manager, err := access.New([]config.DirectoryRule{{
		Name:  "notes",
		Path:  root,
		Allow: []config.Operation{config.OpRead, config.OpCreate, config.OpEdit},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return New(manager, Options{})
}

func assertWriteFileContent(t *testing.T, root, relative, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil || string(got) != want {
		t.Fatalf("file %q = %q, %v; want %q", relative, got, err, want)
	}
}

func assertNoTemporaryFiles(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".scopedfs-tmp-") || strings.HasPrefix(entry.Name(), ".scopedfs-recovery-") {
			t.Fatalf("unexpected temporary file %q", entry.Name())
		}
	}
}

func assertRecovery(t *testing.T, err error, state TargetState, content string) RecoveryOutcome {
	t.Helper()
	var recovery *RecoveryError
	if !errors.As(err, &recovery) || recovery.Outcome.TargetState != state || recovery.Outcome.RecoveryPath == "" || !recovery.Outcome.RecoveryContentAvailable || recovery.Outcome.RecoveryContent != content || recovery.Outcome.RecoveryRevision != Revision([]byte(content)) {
		t.Fatalf("recovery error = %#v, want state=%q content=%q", err, state, content)
	}
	return recovery.Outcome
}

func assertRecoveryUnknown(t *testing.T, err error, state TargetState) RecoveryOutcome {
	t.Helper()
	var r *RecoveryError
	if !errors.As(err, &r) || r.Outcome.TargetState != state || r.Outcome.RecoveryPath == "" || r.Outcome.RecoveryContentAvailable || r.Outcome.RecoveryContent != "" || r.Outcome.RecoveryRevision != "" {
		t.Fatalf("recovery=%#v", err)
	}
	return r.Outcome
}
