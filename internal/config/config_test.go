package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func validConfig(t *testing.T) Config {
	t.Helper()
	return Config{Version: 1, Directories: []DirectoryRule{
		{Name: "notes", Path: filepath.Join(t.TempDir(), "notes"), Allow: []Operation{OpList, OpSearch, OpRead, OpCreate, OpEdit, OpMove, OpTrash}, Ask: []Operation{OpTrash}, OnConflict: "ask"},
		{Name: "archive", Path: filepath.Join(t.TempDir(), "archive"), Allow: []Operation{OpList, OpRead}, OnConflict: "ask"},
	}}
}

func writeConfigValue(t *testing.T, value any) string {
	t.Helper()
	content, err := yaml.Marshal(value)
	if err != nil {
		t.Fatalf("marshal config fixture: %v", err)
	}
	return writeConfig(t, string(content))
}

func configWith(t *testing.T, body string) string {
	t.Helper()
	return "version: 1\ndirectories:\n  - name: notes\n    path: " + strconv.Quote(filepath.Join(t.TempDir(), "notes")) + "\n    " + body + "\n    on_conflict: ask\n"
}

func TestLoadValidConfig(t *testing.T) {
	got, err := Load(writeConfigValue(t, validConfig(t)))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(got.Directories) != 2 || got.Directories[0].Name != "notes" || got.Directories[1].Name != "archive" {
		t.Fatalf("Load() = %#v", got)
	}
}

func TestLoadRejectsUnknownYAMLField(t *testing.T) {
	_, err := Load(writeConfig(t, configWith(t, "allow: [read]\n    unexpected: true")))
	if err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsVersionOtherThanOne(t *testing.T) {
	cfg := validConfig(t)
	cfg.Version = 2
	_, err := Load(writeConfigValue(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsDuplicateOrEmptyNames(t *testing.T) {
	duplicate := validConfig(t)
	duplicate.Directories[1].Name = "notes"
	empty := validConfig(t)
	empty.Directories[0].Name = ""
	tests := []struct {
		name string
		cfg  Config
	}{
		{"duplicate names", duplicate},
		{"empty name", empty},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigValue(t, tc.cfg))
			if err == nil || !strings.Contains(err.Error(), "name") {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadRejectsNonAbsolutePath(t *testing.T) {
	cfg := validConfig(t)
	cfg.Directories[0].Path = "notes"
	_, err := Load(writeConfigValue(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsUnknownOperation(t *testing.T) {
	_, err := Load(writeConfig(t, configWith(t, "allow: [read, rename]")))
	if err == nil || !strings.Contains(err.Error(), "operation") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsAskWithoutAllow(t *testing.T) {
	_, err := Load(writeConfig(t, configWith(t, "allow: [read]\n    ask: [edit]")))
	if err == nil || !strings.Contains(err.Error(), "ask operation edit is not allowed") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsDuplicateOperations(t *testing.T) {
	_, err := Load(writeConfig(t, configWith(t, "allow: [read, read]")))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsInvalidConflictPolicy(t *testing.T) {
	cfg := validConfig(t)
	cfg.Directories[0].OnConflict = "overwrite"
	_, err := Load(writeConfigValue(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "on_conflict") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRequiresReadWhenEditAllowed(t *testing.T) {
	_, err := Load(writeConfig(t, configWith(t, "allow: [edit]")))
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestDirectoryRuleAllowsAndAsks(t *testing.T) {
	rule := DirectoryRule{Allow: []Operation{OpRead}, Ask: []Operation{OpRead}}
	if !rule.Allows(OpRead) || rule.Allows(OpEdit) || !rule.Asks(OpRead) || rule.Asks(OpEdit) {
		t.Fatalf("rule lookup methods returned incorrect results")
	}
}
