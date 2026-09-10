package filesystem

import (
	"context"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsEditFailsWithoutChangingOrCreatingFiles(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "note"), []byte("original"))
	service := writeService(t, root)
	if service.OperationError(config.OpEdit) == nil {
		t.Fatal("unavailable edit advertised as supported")
	}
	_, _, err := service.EditText(context.Background(), "notes", "note", EditRequest{ExpectedRevision: Revision([]byte("original")), ProposedContent: "proposal"})
	assertFSCode(t, err, "atomic_replace_unsupported")
	data, err := os.ReadFile(filepath.Join(root, "note"))
	if err != nil || string(data) != "original" {
		t.Fatalf("source changed: %q %v", data, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected artifacts: %v %v", entries, err)
	}
}
