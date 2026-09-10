package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteTargetArchiveContainsPortablePluginLayout(t *testing.T) {
	repository := packageFixture(t)
	binary := filepath.Join(t.TempDir(), "scoped-filesystem-mcp")
	if err := os.WriteFile(binary, []byte("executable"), 0o755); err != nil {
		t.Fatal(err)
	}

	output := filepath.Join(t.TempDir(), "output path with spaces")
	archivePath, err := writeTargetArchive(repository, output, target{goos: "linux", goarch: "arm64"}, "1.2.3", binary)
	if err != nil {
		t.Fatalf("writeTargetArchive() error = %v", err)
	}
	entries := readTarGzip(t, archivePath)
	assertArchiveEntries(t, entries, []string{
		"scoped-filesystem-mcp/.codex-plugin/plugin.json",
		"scoped-filesystem-mcp/.mcp.json",
		"scoped-filesystem-mcp/bin/scoped-filesystem-mcp",
		"scoped-filesystem-mcp/config.example.yaml",
		"scoped-filesystem-mcp/config.example.windows.yaml",
		"scoped-filesystem-mcp/docs/windows-filesystem-semantics.md",
		"scoped-filesystem-mcp/README.md",
		"scoped-filesystem-mcp/scripts/launch-scoped-filesystem-mcp",
	})
	if entries["scoped-filesystem-mcp/bin/scoped-filesystem-mcp"].mode&0o111 == 0 {
		t.Fatal("packaged Unix executable is not executable")
	}
	assertManifestCommand(t, entries["scoped-filesystem-mcp/.mcp.json"].data, "./bin/scoped-filesystem-mcp")
}

func TestWriteWindowsTargetUsesZipAndExeManifest(t *testing.T) {
	repository := packageFixture(t)
	binary := filepath.Join(t.TempDir(), "scoped-filesystem-mcp.exe")
	if err := os.WriteFile(binary, []byte("executable"), 0o755); err != nil {
		t.Fatal(err)
	}

	archivePath, err := writeTargetArchive(repository, t.TempDir(), target{goos: "windows", goarch: "amd64"}, "1.2.3", binary)
	if err != nil {
		t.Fatalf("writeTargetArchive() error = %v", err)
	}
	if !strings.HasSuffix(archivePath, ".zip") {
		t.Fatalf("archive path = %q, want .zip", archivePath)
	}
	entries := readZip(t, archivePath)
	assertArchiveEntries(t, entries, []string{
		"scoped-filesystem-mcp/.codex-plugin/plugin.json",
		"scoped-filesystem-mcp/.mcp.json",
		"scoped-filesystem-mcp/bin/scoped-filesystem-mcp.exe",
		"scoped-filesystem-mcp/config.example.yaml",
		"scoped-filesystem-mcp/config.example.windows.yaml",
		"scoped-filesystem-mcp/docs/windows-filesystem-semantics.md",
		"scoped-filesystem-mcp/README.md",
	})
	if _, found := entries["scoped-filesystem-mcp/scripts/launch-scoped-filesystem-mcp"]; found {
		t.Fatal("Windows archive unexpectedly depends on the Unix launcher")
	}
	assertManifestCommand(t, entries["scoped-filesystem-mcp/.mcp.json"].data, "./bin/scoped-filesystem-mcp.exe")
}

type archivedFile struct {
	data []byte
	mode os.FileMode
}

func packageFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		".codex-plugin/plugin.json":            `{"name":"scoped-filesystem-mcp","version":"1.2.3","mcpServers":"./.mcp.json"}`,
		"README.md":                            "# Scoped Filesystem MCP\n",
		"config.example.yaml":                  "version: 1\n",
		"config.example.windows.yaml":          "version: 1\n",
		"docs/windows-filesystem-semantics.md": "# Windows filesystem semantics\n",
		"scripts/launch-scoped-filesystem-mcp": "#!/bin/sh\nexit 0\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "scripts", "launch-scoped-filesystem-mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func readTarGzip(t *testing.T, path string) map[string]archivedFile {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	entries := make(map[string]archivedFile)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = archivedFile{data: data, mode: header.FileInfo().Mode()}
	}
	return entries
}

func readZip(t *testing.T, path string) map[string]archivedFile {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	entries := make(map[string]archivedFile)
	for _, file := range reader.File {
		opened, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(opened)
		_ = opened.Close()
		if err != nil {
			t.Fatal(err)
		}
		entries[file.Name] = archivedFile{data: data, mode: file.Mode()}
	}
	return entries
}

func assertArchiveEntries(t *testing.T, entries map[string]archivedFile, want []string) {
	t.Helper()
	if len(entries) != len(want) {
		t.Fatalf("archive entries = %#v, want exactly %#v", entries, want)
	}
	for _, name := range want {
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			t.Fatalf("unsafe expected archive path %q", name)
		}
		if _, found := entries[name]; !found {
			t.Errorf("archive is missing %q", name)
		}
	}
}

func assertManifestCommand(t *testing.T, data []byte, want string) {
	t.Helper()
	var manifest struct {
		Servers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if got := manifest.Servers["scoped_filesystem"].Command; got != want {
		t.Fatalf("manifest command = %q, want %q", got, want)
	}
}
