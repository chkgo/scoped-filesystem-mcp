// Command package builds portable scoped-filesystem-mcp plugin archives.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const archiveRoot = "scoped-filesystem-mcp"

type target struct {
	goos   string
	goarch string
}

type archiveEntry struct {
	name string
	data []byte
	mode os.FileMode
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "package:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("package", flag.ContinueOnError)
	repository := flags.String("repo", ".", "repository root")
	output := flags.String("output", "dist", "archive output directory")
	version := flags.String("version", "", "artifact version; defaults to plugin metadata")
	goos := flags.String("goos", "", "build one operating system")
	goarch := flags.String("goarch", "", "build one architecture")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}
	repo, err := filepath.Abs(*repository)
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	artifactVersion := *version
	if artifactVersion == "" {
		artifactVersion, err = pluginVersion(repo)
		if err != nil {
			return err
		}
	}
	if !safeArtifactPart(artifactVersion) {
		return fmt.Errorf("version %q contains characters unsafe for an archive name", artifactVersion)
	}

	targets := []target{
		{goos: "linux", goarch: "amd64"},
		{goos: "linux", goarch: "arm64"},
		{goos: "darwin", goarch: "amd64"},
		{goos: "darwin", goarch: "arm64"},
		{goos: "windows", goarch: "amd64"},
	}
	if *goos != "" || *goarch != "" {
		if *goos == "" || *goarch == "" {
			return errors.New("-goos and -goarch must be provided together")
		}
		targets = []target{{goos: *goos, goarch: *goarch}}
	}
	for _, buildTarget := range targets {
		archivePath, err := buildAndPackage(repo, *output, buildTarget, artifactVersion)
		if err != nil {
			return err
		}
		fmt.Println(archivePath)
	}
	return nil
}

func pluginVersion(repository string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repository, ".codex-plugin", "plugin.json"))
	if err != nil {
		return "", fmt.Errorf("read plugin metadata: %w", err)
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("decode plugin metadata: %w", err)
	}
	if metadata.Version == "" {
		return "", errors.New("plugin metadata version is empty")
	}
	return metadata.Version, nil
}

func safeArtifactPart(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._+-", char) {
			continue
		}
		return false
	}
	return true
}

func buildAndPackage(repository, output string, buildTarget target, version string) (string, error) {
	if !safeArtifactPart(buildTarget.goos) || !safeArtifactPart(buildTarget.goarch) {
		return "", errors.New("target contains characters unsafe for an archive name")
	}
	temporary, err := os.MkdirTemp("", "scoped-filesystem-mcp-package-")
	if err != nil {
		return "", fmt.Errorf("create build directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	binaryName := "scoped-filesystem-mcp"
	if buildTarget.goos == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(temporary, binaryName)
	command := exec.Command("go", "build", "-trimpath", "-o", binaryPath, "./cmd/scoped-filesystem-mcp")
	command.Dir = repository
	command.Env = environmentWith(os.Environ(), map[string]string{
		"CGO_ENABLED": "0",
		"GOARCH":      buildTarget.goarch,
		"GOOS":        buildTarget.goos,
	})
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build %s/%s: %w\n%s", buildTarget.goos, buildTarget.goarch, err, output)
	}
	return writeTargetArchive(repository, output, buildTarget, version, binaryPath)
}

func environmentWith(current []string, replacements map[string]string) []string {
	result := make([]string, 0, len(current)+len(replacements))
	for _, item := range current {
		key, _, found := strings.Cut(item, "=")
		if _, replace := replacements[key]; found && replace {
			continue
		}
		result = append(result, item)
	}
	for key, value := range replacements {
		result = append(result, key+"="+value)
	}
	return result
}

func writeTargetArchive(repository, output string, buildTarget target, version, binaryPath string) (string, error) {
	entries, err := targetEntries(repository, buildTarget, binaryPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	extension := ".tar.gz"
	if buildTarget.goos == "windows" {
		extension = ".zip"
	}
	archivePath := filepath.Join(output, fmt.Sprintf("scoped-filesystem-mcp-%s-%s-%s%s", version, buildTarget.goos, buildTarget.goarch, extension))
	temporary, err := os.CreateTemp(output, ".scoped-filesystem-mcp-archive-")
	if err != nil {
		return "", fmt.Errorf("create archive: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if buildTarget.goos == "windows" {
		err = writeZip(temporary, entries)
	} else {
		err = writeTarGzip(temporary, entries)
	}
	closeErr := temporary.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", fmt.Errorf("close archive: %w", closeErr)
	}
	if err := os.Remove(archivePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("replace archive: %w", err)
	}
	if err := os.Rename(temporaryPath, archivePath); err != nil {
		return "", fmt.Errorf("publish archive: %w", err)
	}
	return archivePath, nil
}

func targetEntries(repository string, buildTarget target, binaryPath string) ([]archiveEntry, error) {
	binaryName := "scoped-filesystem-mcp"
	if buildTarget.goos == "windows" {
		binaryName += ".exe"
	}
	manifest, err := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"scoped_filesystem": map[string]any{
				"command":                     "./bin/" + binaryName,
				"cwd":                         ".",
				"enabled":                     true,
				"default_tools_approval_mode": "approve",
				"startup_timeout_sec":         10,
				"tool_timeout_sec":            120,
			},
		},
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode MCP manifest: %w", err)
	}
	manifest = append(manifest, '\n')

	sources := []struct {
		archivePath string
		sourcePath  string
		mode        os.FileMode
	}{
		{archivePath: ".codex-plugin/plugin.json", sourcePath: ".codex-plugin/plugin.json", mode: 0o644},
		{archivePath: "README.md", sourcePath: "README.md", mode: 0o644},
		{archivePath: "config.example.yaml", sourcePath: "config.example.yaml", mode: 0o644},
		{archivePath: "config.example.windows.yaml", sourcePath: "config.example.windows.yaml", mode: 0o644},
		{archivePath: "docs/windows-filesystem-semantics.md", sourcePath: "docs/windows-filesystem-semantics.md", mode: 0o644},
	}
	if buildTarget.goos != "windows" {
		sources = append(sources, struct {
			archivePath string
			sourcePath  string
			mode        os.FileMode
		}{archivePath: "scripts/launch-scoped-filesystem-mcp", sourcePath: "scripts/launch-scoped-filesystem-mcp", mode: 0o755})
	}
	entries := []archiveEntry{{name: archiveRoot + "/.mcp.json", data: manifest, mode: 0o644}}
	for _, source := range sources {
		data, err := os.ReadFile(filepath.Join(repository, filepath.FromSlash(source.sourcePath)))
		if err != nil {
			return nil, fmt.Errorf("read package file %s: %w", source.sourcePath, err)
		}
		entries = append(entries, archiveEntry{name: archiveRoot + "/" + source.archivePath, data: data, mode: source.mode})
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("read built executable: %w", err)
	}
	entries = append(entries, archiveEntry{name: archiveRoot + "/bin/" + binaryName, data: binary, mode: 0o755})
	return entries, nil
}

func writeTarGzip(destination io.Writer, entries []archiveEntry) error {
	gzipWriter := gzip.NewWriter(destination)
	gzipWriter.Header.ModTime = time.Unix(0, 0)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: int64(entry.mode.Perm()), Size: int64(len(entry.data)), ModTime: time.Unix(0, 0)}
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("write tar header %s: %w", entry.name, err)
		}
		if _, err := tarWriter.Write(entry.data); err != nil {
			return fmt.Errorf("write tar entry %s: %w", entry.name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("finish tar archive: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return fmt.Errorf("finish gzip archive: %w", err)
	}
	return nil
}

func writeZip(destination io.Writer, entries []archiveEntry) error {
	writer := zip.NewWriter(destination)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(entry.mode)
		header.SetModTime(time.Unix(0, 0))
		file, err := writer.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("write zip header %s: %w", entry.name, err)
		}
		if _, err := file.Write(entry.data); err != nil {
			return fmt.Errorf("write zip entry %s: %w", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish zip archive: %w", err)
	}
	return nil
}
