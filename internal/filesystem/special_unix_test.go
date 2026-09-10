//go:build darwin || linux

package filesystem

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDirectoryToolsRejectSocketWithoutOpeningIt(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "scopedfs-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	listener, err := net.Listen("unix", filepath.Join(root, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s := queryService(t, root)
	_, err = s.ListDirectory(context.Background(), "notes", "socket")
	assertFSCode(t, err, "unsupported_file_type")
	_, err = s.SearchPaths(context.Background(), "notes", "socket", "x", SearchOptions{})
	assertFSCode(t, err, "unsupported_file_type")
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
