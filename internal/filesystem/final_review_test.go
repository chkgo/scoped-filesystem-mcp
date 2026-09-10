package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"golang.org/x/sys/unix"
)

func TestEditRetainsDisplacedInodeAfterSuccessfulVerification(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "note.md")
	writeBytes(t, target, []byte("old\n"))
	openOld, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer openOld.Close()
	s := writeService(t, root)
	s.afterDisplacedRead = func() error {
		_, err := openOld.WriteAt([]byte("late\n"), 0)
		return err
	}
	got, conflict, err := s.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
	if err != nil || conflict != nil || got == nil || got.RecoveryPath == "" {
		t.Fatalf("edit = %#v %#v %v", got, conflict, err)
	}
	assertWriteFileContent(t, root, "note.md", "new\n")
	assertWriteFileContent(t, root, got.RecoveryPath, "late\n")
}

func TestEditRetainsProposalInodeAfterVerifiedRollback(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "note.md")
	writeBytes(t, target, []byte("old\n"))
	s := writeService(t, root)
	s.beforeSwap = func() error { return os.WriteFile(target, []byte("second\n"), 0o600) }
	var proposal *os.File
	s.beforeRollback = func() error {
		file, err := os.OpenFile(target, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		proposal = file
		return nil
	}
	s.afterRollbackRead = func(*os.File, string) error { _, err := proposal.WriteAt([]byte("late\n"), 0); return err }
	got, conflict, err := s.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
	if err != nil || got != nil || conflict == nil || conflict.RecoveryPath == "" {
		t.Fatalf("edit = %#v %#v %v", got, conflict, err)
	}
	defer proposal.Close()
	assertWriteFileContent(t, root, conflict.RecoveryPath, "late\n")
}

func TestCreateTextPublishesPrivateTempWithoutTouchingCollision(t *testing.T) {
	root := t.TempDir()
	s := writeService(t, root)
	s.beforeCreatePublish = func() error { return os.WriteFile(filepath.Join(root, "note.md"), []byte("external\n"), 0o600) }
	_, err := s.CreateText(context.Background(), "notes", "note.md", "agent\n", false)
	assertFSCode(t, err, "destination_exists")
	assertWriteFileContent(t, root, "note.md", "external\n")
	assertNoTemporaryFiles(t, root)
}

func TestCreateTextWriteAndSyncFailuresNeverPublish(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Service)
	}{
		{"write", func(s *Service) { s.writeFile = func(*os.File, []byte) error { return syscall.EIO } }},
		{"sync", func(s *Service) { s.syncFile = func(*os.File) error { return syscall.EIO } }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeBytes(t, filepath.Join(root, "note.md"), []byte("external\n"))
			s := writeService(t, root)
			tc.set(s)
			_, err := s.CreateText(context.Background(), "notes", "note.md", "agent\n", false)
			assertFSCode(t, err, "filesystem_unavailable")
			assertWriteFileContent(t, root, "note.md", "external\n")
			assertNoTemporaryFiles(t, root)
		})
	}
}

func TestReadAndStatFIFOFailPromptly(t *testing.T) {
	root := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := queryService(t, root)
	writer := writeService(t, root)
	for name, call := range map[string]func() error{
		"read":   func() error { _, err := s.ReadText(context.Background(), "notes", "pipe"); return err },
		"binary": func() error { _, err := s.ReadBinary(context.Background(), "notes", "pipe"); return err },
		"list":   func() error { _, err := s.ListDirectory(context.Background(), "notes", "pipe"); return err },
		"search_paths": func() error {
			_, err := s.SearchPaths(context.Background(), "notes", "pipe", "x", SearchOptions{})
			return err
		},
		"search_text": func() error {
			_, err := s.SearchText(context.Background(), "notes", "pipe", "x", SearchOptions{})
			return err
		},
		"edit": func() error {
			_, _, err := writer.EditText(context.Background(), "notes", "pipe", EditRequest{})
			return err
		},
	} {
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			assertFSCode(t, err, "unsupported_file_type")
		case <-time.After(time.Second):
			t.Fatalf("%s blocked on FIFO", name)
		}
	}
	done := make(chan error, 1)
	go func() {
		entry, err := s.Stat(context.Background(), "notes", "pipe")
		if err == nil && entry.Type != "other" {
			err = errors.New("FIFO stat type was not other")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stat blocked on FIFO")
	}
}

func TestSearchTextSkipsBinaryUTF8WithEmbeddedMatch(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "binary.dat"), []byte("prefix\x00needle\n"))
	writeBytes(t, filepath.Join(root, "control.dat"), []byte("prefix\x01needle\n"))
	writeBytes(t, filepath.Join(root, "text.txt"), []byte("needle\n"))
	matches, err := queryService(t, root).SearchText(context.Background(), "notes", ".", "needle", SearchOptions{})
	if err != nil || len(matches) != 1 || matches[0].Path != "text.txt" {
		t.Fatalf("matches = %#v, %v", matches, err)
	}
}

func TestSearchTextIsCaseSensitiveAndLiteral(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note.txt"), []byte("Needle a.*b aZZb\n"))
	s := queryService(t, root)
	got, err := s.SearchText(context.Background(), "notes", ".", "needle", SearchOptions{CaseSensitive: true})
	if err != nil || len(got) != 0 {
		t.Fatalf("case-sensitive = %#v %v", got, err)
	}
	got, err = s.SearchText(context.Background(), "notes", ".", "a.*b", SearchOptions{CaseSensitive: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("literal = %#v %v", got, err)
	}
}

func TestSearchPathsIsCaseSensitiveAndLiteral(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "Needle-[x].txt"), []byte("x"))
	s := queryService(t, root)
	got, err := s.SearchPaths(context.Background(), "notes", ".", "needle", SearchOptions{CaseSensitive: true})
	if err != nil || len(got) != 0 {
		t.Fatalf("case-sensitive = %#v %v", got, err)
	}
	got, err = s.SearchPaths(context.Background(), "notes", ".", "[x]", SearchOptions{CaseSensitive: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("literal = %#v %v", got, err)
	}
}

func TestSearchTextSkipsFileThatGrowsPastBound(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.txt")
	writeBytes(t, path, []byte("needle\n"))
	s := queryService(t, root)
	s.maxReadBytes = 8
	s.beforeRead = func() error { return os.WriteFile(path, []byte("needle\nextra"), 0o600) }
	got, err := s.SearchText(context.Background(), "notes", ".", "needle", SearchOptions{})
	if err != nil || len(got) != 0 {
		t.Fatalf("growth result = %#v %v", got, err)
	}
}

func TestSearchTextBoundsAggregateResponseAndHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "a"), []byte("hit\n"))
	writeBytes(t, filepath.Join(root, "b"), []byte("hit\n"))
	s := queryService(t, root)
	s.maxReadBytes = 100
	got, err := s.SearchText(context.Background(), "notes", ".", "hit", SearchOptions{})
	if err != nil || len(got) != 1 {
		t.Fatalf("aggregate bound = %#v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.beforeRead = func() error { cancel(); return nil }
	if _, err := s.SearchText(ctx, "notes", ".", "hit", SearchOptions{}); err == nil {
		t.Fatal("cancelled search succeeded")
	}
}

func TestEditConflictReadThatGrowsPastBoundIsRejected(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	s := writeService(t, root)
	s.maxReadBytes = 5
	s.afterReadAtStat = func(name string) error {
		if name == "note.md" {
			return os.WriteFile(path, []byte("too large"), 0o600)
		}
		return nil
	}
	got, conflict, err := s.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: "stale", ProposedContent: "new"})
	if got != nil || conflict != nil {
		t.Fatalf("shape = %#v %#v", got, conflict)
	}
	assertFSCode(t, err, "response_too_large")
}

func TestOversizedPostSwapRecoveryRetainsPathWithoutContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	writeBytes(t, path, []byte("old\n"))
	s := writeService(t, root)
	s.maxReadBytes = 5
	s.beforeReadAt = func(name string) error {
		if strings.HasPrefix(name, ".scopedfs-recovery-") {
			return os.WriteFile(filepath.Join(root, name), []byte("too large"), 0o600)
		}
		return nil
	}
	got, conflict, err := s.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
	if got != nil || conflict != nil {
		t.Fatalf("shape = %#v %#v", got, conflict)
	}
	o := assertRecoveryUnknown(t, err, TargetUnknown)
	if _, statErr := os.Stat(filepath.Join(root, o.RecoveryPath)); statErr != nil {
		t.Fatalf("recovery missing: %v", statErr)
	}
}

func TestRevisionReaderHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := RevisionReader(ctx, strings.NewReader(strings.Repeat("x", 1<<20))); !errors.Is(err, context.Canceled) {
		t.Fatalf("revision error = %v", err)
	}
}

func TestCreateAndEditRecheckCancellationAtPublicationBoundary(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		root := t.TempDir()
		s := writeService(t, root)
		ctx, cancel := context.WithCancel(context.Background())
		s.beforeCreatePublish = func() error { cancel(); return nil }
		_, err := s.CreateText(ctx, "notes", "note", "new", false)
		assertFSCode(t, err, "filesystem_unavailable")
		if _, statErr := os.Lstat(filepath.Join(root, "note")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("published after cancel: %v", statErr)
		}
	})
	t.Run("edit", func(t *testing.T) {
		root := t.TempDir()
		writeBytes(t, filepath.Join(root, "note"), []byte("old"))
		s := writeService(t, root)
		ctx, cancel := context.WithCancel(context.Background())
		s.beforeSwap = func() error { cancel(); return nil }
		_, _, err := s.EditText(ctx, "notes", "note", EditRequest{ExpectedRevision: Revision([]byte("old")), ProposedContent: "new"})
		assertFSCode(t, err, "filesystem_unavailable")
		assertWriteFileContent(t, root, "note", "old")
		assertNoTemporaryFiles(t, root)
	})
}

func TestEditReadHonorsCancellationDuringContentRead(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note"), []byte(strings.Repeat("x", 1024)))
	s := writeService(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	s.afterReadAtStat = func(string) error { cancel(); return nil }
	_, _, err := s.EditText(ctx, "notes", "note", EditRequest{ExpectedRevision: "stale", ProposedContent: "new"})
	assertFSCode(t, err, "filesystem_unavailable")
}

func TestSearchTextSkipsDELBinary(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "binary"), []byte("needle\x7fstill text\n"))
	got, err := queryService(t, root).SearchText(context.Background(), "notes", ".", "needle", SearchOptions{})
	if err != nil || len(got) != 0 {
		t.Fatalf("DEL match leaked = %#v %v", got, err)
	}
}

func TestSearchCappedSelectionIsGloballyOrderedAcrossBatches(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "z-last"), []byte("needle"))
	writeBytes(t, filepath.Join(root, "a-first"), []byte("needle"))
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]os.DirEntry{}
	for _, entry := range entries {
		byName[entry.Name()] = entry
	}
	s := queryService(t, root)
	calls := 0
	s.readDirectory = func(_ *os.File, _ int) ([]os.DirEntry, error) {
		calls++
		switch calls {
		case 1:
			return []os.DirEntry{byName["z-last"]}, nil
		case 2:
			return []os.DirEntry{byName["a-first"]}, nil
		default:
			return nil, io.EOF
		}
	}
	paths, err := s.SearchPaths(context.Background(), "notes", ".", "", SearchOptions{MaxResults: 1})
	if err != nil || len(paths) != 1 || paths[0].Path != "a-first" {
		t.Fatalf("paths = %#v %v", paths, err)
	}
	calls = 0
	texts, err := s.SearchText(context.Background(), "notes", ".", "needle", SearchOptions{MaxResults: 1})
	if err != nil || len(texts) != 1 || texts[0].Path != "a-first" {
		t.Fatalf("texts = %#v %v", texts, err)
	}
}

func TestTextMatchCollectorBoundsRetainedTextDuringGlobalSelection(t *testing.T) {
	// Each one-character path match costs 216 bytes under the response estimate:
	// one root byte + one path byte + 150 text bytes + 64 bytes of JSON overhead.
	collector := newTextMatchCollector(1000, 216)
	collector.add(TextMatch{Root: "r", Path: "z", Line: 1, Text: strings.Repeat("z", 150)})
	collector.add(TextMatch{Root: "r", Path: "zz", Line: 1, Text: strings.Repeat("q", 150)})
	collector.add(TextMatch{Root: "r", Path: "a", Line: 1, Text: strings.Repeat("a", 150)})
	collector.add(TextMatch{Root: "r", Path: "b", Line: 1, Text: strings.Repeat("b", 150)})

	if len(collector.matches) != 1 || collector.matches[0].Path != "a" || collector.usedBytes != 216 {
		t.Fatalf("retained = %#v bytes=%d", collector.matches, collector.usedBytes)
	}
	if collector.boundary == nil || collector.boundary.Path != "b" || collector.boundary.Line != 1 {
		t.Fatalf("boundary = %#v", collector.boundary)
	}
	// Truncated backing-array slots must be cleared so discarded large strings
	// are not kept alive by the collector's capacity.
	backing := collector.matches[:cap(collector.matches)]
	for i := len(collector.matches); i < len(backing); i++ {
		if backing[i].Text != "" {
			t.Fatalf("discarded text retained at backing slot %d", i)
		}
	}
}

func TestSearchTextMergesEarlierOverBudgetBoundaryAcrossFiles(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "a-first"), []byte("needle"+strings.Repeat("a", 50)))
	writeBytes(t, filepath.Join(root, "b-later"), []byte("needle"))
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]os.DirEntry{}
	for _, entry := range entries {
		byName[entry.Name()] = entry
	}
	s := queryService(t, root)
	s.maxReadBytes = 100
	calls := 0
	s.readDirectory = func(_ *os.File, _ int) ([]os.DirEntry, error) {
		calls++
		switch calls {
		case 1:
			return []os.DirEntry{byName["b-later"]}, nil
		case 2:
			return []os.DirEntry{byName["a-first"]}, nil
		default:
			return nil, io.EOF
		}
	}
	got, err := s.SearchText(context.Background(), "notes", ".", "needle", SearchOptions{})
	if err != nil || len(got) != 0 {
		t.Fatalf("matches after earlier over-budget boundary = %#v, %v", got, err)
	}
}

func TestEntryIncludesCreationAndModificationTimestamps(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note"), []byte("body"))
	entry, err := queryService(t, root).Stat(context.Background(), "notes", "note")
	if err != nil || entry.ModifiedAt == "" || entry.CreatedAt == "" {
		t.Fatalf("entry = %#v, %v", entry, err)
	}
}

func TestRollbackTargetReadFailureNeverClaimsUnverifiedProposal(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "note.md")
	writeBytes(t, target, []byte("old\n"))
	s := writeService(t, root)
	rolledBack := false
	s.beforeSwap = func() error { return os.WriteFile(target, []byte("second\n"), 0o600) }
	s.beforeRollback = func() error {
		if err := os.WriteFile(target, []byte("third\n"), 0o600); err != nil {
			return err
		}
		rolledBack = true
		return nil
	}
	s.beforeReadAt = func(name string) error {
		if rolledBack && name == "note.md" {
			return syscall.EIO
		}
		return nil
	}
	_, _, err := s.EditText(context.Background(), "notes", "note.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
	o := assertRecovery(t, err, TargetCurrentRestored, "third\n")
	if strings.Contains(o.RecoveryContent, "new") {
		t.Fatalf("reported unverified proposal: %#v", o)
	}
}

func TestFinalSymlinkEditReturnsOpenableCanonicalRecoveryPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "actual"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(root, "actual", "note.md"), []byte("old\n"))
	if err := os.Symlink("actual/note.md", filepath.Join(root, "alias.md")); err != nil {
		t.Fatal(err)
	}
	got, conflict, err := writeService(t, root).EditText(context.Background(), "notes", "alias.md", EditRequest{ExpectedRevision: Revision([]byte("old\n")), ProposedContent: "new\n"})
	if err != nil || conflict != nil || got == nil {
		t.Fatalf("edit = %#v %#v %v", got, conflict, err)
	}
	assertWriteFileContent(t, root, got.RecoveryPath, "old\n")
}

func TestPermanentDeleteUsesBatchesAndReportsPartialCompletion(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "tree")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 260; i++ {
		writeBytes(t, filepath.Join(directory, fmt.Sprintf("%03d", i)), []byte("x"))
	}
	s := mutationService(t, []mutationRoot{{name: "notes", path: root, allow: []config.Operation{config.OpPermanentDelete}}}, Options{})
	count := 0
	s.beforeDeleteEntry = func(string) error {
		count++
		if count == 130 {
			return context.Canceled
		}
		return nil
	}
	_, err := s.PermanentDelete(context.Background(), "notes", "tree")
	assertFSCode(t, err, "partial_delete")
	if count != 130 {
		t.Fatalf("visited = %d", count)
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil || len(entries) == 0 || len(entries) >= 260 {
		t.Fatalf("partial entries = %d, %v", len(entries), readErr)
	}
}
