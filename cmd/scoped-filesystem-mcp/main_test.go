package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRunRequiresConfig(t *testing.T) {
	var stderr bytes.Buffer
	err := run(context.Background(), nil, &stderr)
	if err == nil || err.Error() != "--config is required" {
		t.Fatalf("run() error = %v, want --config is required", err)
	}
}

func TestRunExpandsHomeRelativeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	writeCommandConfig(t, filepath.Join(home, "config.yaml"), root)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	err := run(ctx, []string{"--config", "~/config.yaml"}, &stderr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run() error = %v, want context cancellation after loading expanded config", err)
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
		name string
		args []string
		want string
	}{
		{name: "missing config", want: "scoped-filesystem-mcp: --config is required"},
		{name: "invalid config", args: []string{"--config", invalidConfig}, want: "scoped-filesystem-mcp: decode config"},
		{name: "unknown flag", args: []string{"--unknown"}, want: "flag provided but not defined: -unknown"},
		{name: "extra argument", args: []string{"--config", validConfig, "unexpected"}, want: "scoped-filesystem-mcp: unexpected positional argument \"unexpected\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
	content := "version: 1\n\ndirectories:\n  - name: notes\n    path: " + root + "\n    allow:\n      - read\n    ask: []\n    on_conflict: ask\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
