package filesystem

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

func TestRevisionUsesFileBytes(t *testing.T) {
	if got, want := Revision([]byte("hello")), "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"; got != want {
		t.Fatalf("Revision() = %q, want %q", got, want)
	}
}

func TestReadTextPreservesUTF8AndRevision(t *testing.T) {
	root := t.TempDir()
	content := "Привет, мир 🌍\n"
	writeBytes(t, filepath.Join(root, "note.md"), []byte(content))
	got, err := testService(t, root).ReadText(context.Background(), "notes", "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != content || got.Root != "notes" || got.Path != "note.md" || got.Size != int64(len([]byte(content))) || got.Revision != Revision([]byte(content)) {
		t.Fatalf("ReadText() = %#v", got)
	}
}

func TestReadTextRejectsInvalidUTF8(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "bad.txt"), []byte{0xff, 0xfe})
	_, err := testService(t, root).ReadText(context.Background(), "notes", "bad.txt")
	assertFSCode(t, err, "invalid_utf8")
}

func TestReadTextEnforcesTenMiBLimit(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "limit.txt"), bytes.Repeat([]byte{'x'}, int(DefaultMaxReadBytes)))
	got, err := testService(t, root).ReadText(context.Background(), "notes", "limit.txt")
	if err != nil || got.Size != DefaultMaxReadBytes {
		t.Fatalf("10 MiB text read = %#v, %v", got, err)
	}
	writeBytes(t, filepath.Join(root, "oversize.txt"), bytes.Repeat([]byte{'x'}, int(DefaultMaxReadBytes)+1))
	_, err = testService(t, root).ReadText(context.Background(), "notes", "oversize.txt")
	assertFSCode(t, err, "response_too_large")
}

func TestReadRejectsNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "folder")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	svc := testService(t, root)
	_, err := svc.ReadText(context.Background(), "notes", "folder")
	assertFSCode(t, err, "unsupported_file_type")
	_, err = svc.ReadBinary(context.Background(), "notes", "folder")
	assertFSCode(t, err, "unsupported_file_type")
}

func TestReadBinaryClassifiesMediaAndResource(t *testing.T) {
	root := t.TempDir()
	writeBytes(t, filepath.Join(root, "image.png"), []byte("\x89PNG\r\n\x1a\n"))
	writeBytes(t, filepath.Join(root, "sound.wav"), []byte("RIFF\x24\x00\x00\x00WAVEfmt "))
	writeBytes(t, filepath.Join(root, "doc.pdf"), []byte("%PDF-1.7\n"))
	writeBytes(t, filepath.Join(root, "data.unknown"), []byte{0, 1, 2, 3})
	svc := testService(t, root)
	for _, tc := range []struct {
		path string
		kind BinaryKind
		mime string
	}{{"image.png", BinaryImage, "image/png"}, {"sound.wav", BinaryAudio, "audio/wave"}, {"doc.pdf", BinaryResource, "application/pdf"}, {"data.unknown", BinaryResource, "application/octet-stream"}} {
		got, err := svc.ReadBinary(context.Background(), "notes", tc.path)
		if err != nil {
			t.Fatalf("ReadBinary(%s): %v", tc.path, err)
		}
		if got.Kind != tc.kind || got.MIMEType != tc.mime || got.Path != tc.path || got.Root != "notes" || !bytes.Equal(got.Data, mustRead(t, filepath.Join(root, tc.path))) {
			t.Fatalf("ReadBinary(%s) = %#v", tc.path, got)
		}
		if got.URI != "scopedfs://notes/"+tc.path {
			t.Fatalf("URI = %q", got.URI)
		}
	}
}

func TestReadBinaryEnforcesTenMiBBeforeAndDuringRead(t *testing.T) {
	root := t.TempDir()
	svc := testService(t, root)
	writeBytes(t, filepath.Join(root, "limit.bin"), bytes.Repeat([]byte{'x'}, int(DefaultMaxReadBytes)))
	got, err := svc.ReadBinary(context.Background(), "notes", "limit.bin")
	if err != nil || got.Size != DefaultMaxReadBytes || int64(len(got.Data)) != DefaultMaxReadBytes {
		t.Fatalf("10 MiB read = %#v, %v", got, err)
	}
	writeBytes(t, filepath.Join(root, "oversize.bin"), bytes.Repeat([]byte{'x'}, int(DefaultMaxReadBytes)+1))
	_, err = svc.ReadBinary(context.Background(), "notes", "oversize.bin")
	assertFSCode(t, err, "response_too_large")
}

func TestReadRejectsRegularFileGrowingDuringBoundedRead(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"growing.txt", "growing.bin"} {
		path := filepath.Join(root, name)
		writeBytes(t, path, bytes.Repeat([]byte{'x'}, 1024))
		svc := testServiceWithOptions(t, root, Options{MaxReadBytes: 1024})
		svc.beforeRead = func() error {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return err
			}
			_, err = f.Write([]byte{'y'})
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			return err
		}
		if name == "growing.txt" {
			_, err := svc.ReadText(context.Background(), "notes", name)
			assertFSCode(t, err, "response_too_large")
		} else {
			_, err := svc.ReadBinary(context.Background(), "notes", name)
			assertFSCode(t, err, "response_too_large")
		}
	}
}

func TestMapErrorESTALEDoesNotLeakAbsolutePath(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "secret", "file")
	err := MapError("notes", "file", config.OpRead, &os.PathError{Op: "open", Path: absolute, Err: syscall.ESTALE})
	assertFSCode(t, err, "filesystem_unavailable")
	if bytes.Contains([]byte(err.Error()), []byte(absolute)) {
		t.Fatalf("error leaks absolute path: %q", err)
	}
	if !errors.Is(err, syscall.ESTALE) {
		t.Fatalf("error does not unwrap ESTALE: %v", err)
	}
}

func testService(t *testing.T, root string) *Service {
	return testServiceWithOptions(t, root, Options{})
}
func testServiceWithOptions(t *testing.T, root string, options Options) *Service {
	t.Helper()
	m, err := access.New([]config.DirectoryRule{{Name: "notes", Path: root, Allow: []config.Operation{config.OpRead}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return New(m, options)
}
func writeBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func assertFSCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %q", want)
	}
	var fsErr *Error
	if !errors.As(err, &fsErr) || fsErr.Code != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}
