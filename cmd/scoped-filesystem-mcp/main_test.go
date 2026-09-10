package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestResolveConfigPathUsesFlagThenEnvironmentThenPlatformDefault(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		env      configEnvironment
		want     string
		source   string
	}{
		{
			name:     "flag overrides environment",
			explicit: "~/flag.yaml",
			env:      testConfigEnvironment("linux", "/home/tester", "/ignored", "/xdg", "~/environment.yaml"),
			want:     "/home/tester/flag.yaml",
			source:   "--config",
		},
		{
			name:   "environment overrides default",
			env:    testConfigEnvironment("linux", "/home/tester", "/ignored", "/xdg", "~/environment.yaml"),
			want:   "/home/tester/environment.yaml",
			source: configEnvironmentVariable,
		},
		{
			name:   "darwin keeps legacy default",
			env:    testConfigEnvironment("darwin", "/Users/tester", "/Library/ignored", "", ""),
			want:   "/Users/tester/.config/scoped-filesystem-mcp/config.yaml",
			source: "platform default",
		},
		{
			name:   "linux uses XDG config home",
			env:    testConfigEnvironment("linux", "/home/tester", "/ignored", "/home/tester/xdg", ""),
			want:   "/home/tester/xdg/scoped-filesystem-mcp/config.yaml",
			source: "platform default",
		},
		{
			name:   "linux falls back to dot config",
			env:    testConfigEnvironment("linux", "/home/tester", "/ignored", "", ""),
			want:   "/home/tester/.config/scoped-filesystem-mcp/config.yaml",
			source: "platform default",
		},
		{
			name:   "windows uses user config directory",
			env:    testConfigEnvironment("windows", `C:\Users\tester`, `C:\Users\tester\AppData\Roaming`, "", ""),
			want:   `C:\Users\tester\AppData\Roaming\scoped-filesystem-mcp\config.yaml`,
			source: "platform default",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveConfigPath(tc.explicit, tc.env)
			if err != nil {
				t.Fatalf("resolveConfigPath() error = %v", err)
			}
			if got.path != tc.want || got.source != tc.source {
				t.Fatalf("resolveConfigPath() = %#v, want path %q from %q", got, tc.want, tc.source)
			}
		})
	}
}

func TestResolveConfigPathExpandsWindowsHomeSeparator(t *testing.T) {
	env := testConfigEnvironment("windows", `C:\Users\tester`, `C:\config`, "", "")
	got, err := resolveConfigPath(`~\policy.yaml`, env)
	if err != nil {
		t.Fatalf("resolveConfigPath() error = %v", err)
	}
	if got.path != `C:\Users\tester\policy.yaml` {
		t.Fatalf("resolved path = %q, want Windows home-relative path", got.path)
	}
}

func TestRunMissingDefaultConfigExplainsResolutionWithoutCreatingPolicy(t *testing.T) {
	home := t.TempDir()
	env := testConfigEnvironment(runtime.GOOS, home, filepath.Join(home, "config-root"), "", "")

	var stderr bytes.Buffer
	err := runWithEnvironment(context.Background(), nil, &stderr, env)
	if err == nil || !strings.Contains(err.Error(), "configuration file not found") || !strings.Contains(err.Error(), "--config") || !strings.Contains(err.Error(), configEnvironmentVariable) {
		t.Fatalf("runWithEnvironment() error = %v, want actionable missing-config diagnostic", err)
	}
	wantPath, resolveErr := resolveConfigPath("", env)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if _, statErr := os.Stat(wantPath.path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("default policy path stat error = %v, want not created", statErr)
	}
}

func TestRunDoesNotMisreportMissingConfiguredRootAsMissingConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "policy.yaml")
	writeCommandConfig(t, configPath, filepath.Join(t.TempDir(), "missing-root"))

	err := run(context.Background(), []string{"--config", configPath}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("run() error = nil, want missing configured root")
	}
	if strings.Contains(err.Error(), "configuration file not found") {
		t.Fatalf("run() error = %v, must identify the configured root failure", err)
	}
}

func TestRunExpandsHomeRelativeConfig(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	writeCommandConfig(t, filepath.Join(home, "config.yaml"), root)
	env := testConfigEnvironment(runtime.GOOS, home, filepath.Join(home, "config-root"), "", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	err := runWithEnvironment(ctx, []string{"--config", "~/config.yaml"}, &stderr, env)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run() error = %v, want context cancellation after loading expanded config", err)
	}
}

func TestRunExplicitConfigOverridesEnvironment(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	explicit := filepath.Join(home, "explicit.yaml")
	writeCommandConfig(t, explicit, root)
	env := testConfigEnvironment(runtime.GOOS, home, filepath.Join(home, "config-root"), "", filepath.Join(home, "missing-environment.yaml"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runWithEnvironment(ctx, []string{"--config", explicit}, &bytes.Buffer{}, env)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runWithEnvironment() error = %v, want explicit config to load before context cancellation", err)
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(configPath, []byte("version: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	err := run(context.Background(), []string{"--config", configPath}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "decode config") {
		t.Fatalf("run() error = %v, want invalid-config error", err)
	}
}

func TestRealMainReturnsNonzeroAndWritesStartupFailuresToStderr(t *testing.T) {
	invalidConfig := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(invalidConfig, []byte("version: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	validConfig := filepath.Join(t.TempDir(), "config.yaml")
	writeCommandConfig(t, validConfig, t.TempDir())

	for _, tc := range []struct {
		name        string
		args        []string
		environment string
		want        string
	}{
		{name: "missing config", environment: filepath.Join(t.TempDir(), "missing.yaml"), want: "scoped-filesystem-mcp: configuration file not found"},
		{name: "invalid config", args: []string{"--config", invalidConfig}, want: "scoped-filesystem-mcp: decode config"},
		{name: "unknown flag", args: []string{"--unknown"}, want: "flag provided but not defined: -unknown"},
		{name: "extra argument", args: []string{"--config", validConfig, "unexpected"}, want: "scoped-filesystem-mcp: unexpected positional argument \"unexpected\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.environment != "" {
				t.Setenv(configEnvironmentVariable, tc.environment)
			}
			var stderr bytes.Buffer
			if code := realMain(tc.args, &stderr); code == 0 {
				t.Fatal("realMain() exit code = 0, want startup failure")
			}
			if got := stderr.String(); !strings.Contains(got, tc.want) {
				t.Fatalf("stderr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExecutableStdioLifecycle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("from subprocess"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "policy.yaml")
	writeCommandConfigWithAllow(t, configPath, root, "read", "create")

	binaryPath := stdioTestBinary(t)

	command := exec.Command(binaryPath)
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), configEnvironmentVariable+"="+configPath)
	var processStderr bytes.Buffer
	command.Stderr = &processStderr
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "1.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: 2 * time.Second}, nil)
	if err != nil {
		t.Fatalf("connect to executable: %v; stderr: %s", err, processStderr.String())
	}
	defer session.Close()

	initialized := session.InitializeResult()
	if initialized == nil || initialized.ServerInfo == nil || initialized.ServerInfo.Name != "scoped-filesystem-mcp" {
		t.Fatalf("initialize result = %#v", initialized)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) == 0 {
		t.Fatalf("tools/list = %#v, %v", tools, err)
	}
	read, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "read_text_file", Arguments: map[string]any{"root": "notes", "path": "existing.txt"}})
	if err != nil {
		t.Fatalf("read call: %v", err)
	}
	readOutput, _ := read.StructuredContent.(map[string]any)
	if read.IsError || readOutput["content"] != "from subprocess" {
		t.Fatalf("read result = %#v", read)
	}
	created, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create_file", Arguments: map[string]any{"root": "notes", "path": "created.txt", "content": "created over stdio"}})
	if err != nil {
		t.Fatalf("create call: %v", err)
	}
	if created.IsError {
		t.Fatalf("create result = %#v", created)
	}
	if got, readErr := os.ReadFile(filepath.Join(root, "created.txt")); readErr != nil || string(got) != "created over stdio" {
		t.Fatalf("created file = %q, %v", got, readErr)
	}
	rejected, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "read_text_file", Arguments: map[string]any{"root": "notes", "path": "../escape.txt"}})
	if err != nil {
		t.Fatalf("escape call: %v", err)
	}
	if !rejected.IsError {
		t.Fatalf("escape result = %#v, want tool rejection", rejected)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("clean shutdown: %v; stderr: %s", err, processStderr.String())
	}
}

func stdioTestBinary(t *testing.T) string {
	t.Helper()
	if configured := os.Getenv("SCOPEDFS_TEST_BINARY"); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			t.Fatalf("resolve SCOPEDFS_TEST_BINARY: %v", err)
		}
		if info, err := os.Stat(absolute); err != nil || info.IsDir() {
			t.Fatalf("SCOPEDFS_TEST_BINARY %q is not an executable file: %v", absolute, err)
		}
		return absolute
	}

	binaryName := "scoped-filesystem-mcp"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(t.TempDir(), "path with spaces", binaryName)
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	build.Env = append(os.Environ(), "GOCACHE="+filepath.Join(os.TempDir(), "scopedfs-portability-gocache"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v\n%s", err, output)
	}
	return binaryPath
}

func testConfigEnvironment(goos, home, userConfig, xdg, configured string) configEnvironment {
	values := map[string]string{
		configEnvironmentVariable: configured,
		"XDG_CONFIG_HOME":         xdg,
	}
	return configEnvironment{
		goos:          goos,
		getenv:        func(key string) string { return values[key] },
		userHomeDir:   func() (string, error) { return home, nil },
		userConfigDir: func() (string, error) { return userConfig, nil },
	}
}

func TestBuildServerCompletesMCPHandshake(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeCommandConfig(t, configPath, root)

	server, err := buildServer(configPath)
	if err != nil {
		t.Fatalf("buildServer() error = %v", err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	result := clientSession.InitializeResult()
	if result == nil || result.ServerInfo == nil {
		t.Fatalf("initialize result = %#v, want server information", result)
	}
	if result.ServerInfo.Name != "scoped-filesystem-mcp" || result.ServerInfo.Version != "0.1.0" {
		t.Fatalf("server identity = %#v, want scoped-filesystem-mcp 0.1.0", result.ServerInfo)
	}
}

func writeCommandConfig(t *testing.T, path, root string) {
	t.Helper()
	writeCommandConfigWithAllow(t, path, root, "read")
}

func writeCommandConfigWithAllow(t *testing.T, path, root string, allow ...string) {
	t.Helper()
	content := "version: 1\n\ndirectories:\n  - name: notes\n    path: " + root + "\n    allow:\n      - " + strings.Join(allow, "\n      - ") + "\n    ask: []\n    on_conflict: ask\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
