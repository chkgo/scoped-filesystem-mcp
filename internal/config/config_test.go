package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `version: 1
directories:
  - name: notes
    path: /tmp/notes
    allow: [list, search, read, create, edit, move, trash]
    ask: [trash]
    on_conflict: ask
  - name: archive
    path: /var/tmp/archive
    allow: [list, read]
    ask: []
    on_conflict: ask
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func configWith(body string) string {
	return "version: 1\ndirectories:\n  - name: notes\n    path: /tmp/notes\n    " + body + "\n    on_conflict: ask\n"
}

func TestLoadValidConfig(t *testing.T) {
	got, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(got.Directories) != 2 || got.Directories[0].Name != "notes" || got.Directories[1].Name != "archive" {
		t.Fatalf("Load() = %#v", got)
	}
}

func TestLoadRejectsUnknownYAMLField(t *testing.T) {
	_, err := Load(writeConfig(t, configWith("allow: [read]\n    unexpected: true")))
	if err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsVersionOtherThanOne(t *testing.T) {
	_, err := Load(writeConfig(t, strings.Replace(validYAML, "version: 1", "version: 2", 1)))
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsDuplicateOrEmptyNames(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"duplicate names", strings.Replace(validYAML, "- name: archive", "- name: notes", 1)},
		{"empty name", strings.Replace(validYAML, "- name: notes", "- name: ''", 1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), "name") {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadRejectsNonAbsolutePath(t *testing.T) {
	_, err := Load(writeConfig(t, strings.Replace(validYAML, "/tmp/notes", "notes", 1)))
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsUnknownOperation(t *testing.T) {
	_, err := Load(writeConfig(t, configWith("allow: [read, rename]")))
	if err == nil || !strings.Contains(err.Error(), "operation") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsAskWithoutAllow(t *testing.T) {
	_, err := Load(writeConfig(t, configWith("allow: [read]\n    ask: [edit]")))
	if err == nil || !strings.Contains(err.Error(), "ask operation edit is not allowed") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsDuplicateOperations(t *testing.T) {
	_, err := Load(writeConfig(t, configWith("allow: [read, read]")))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsInvalidConflictPolicy(t *testing.T) {
	_, err := Load(writeConfig(t, strings.Replace(validYAML, "on_conflict: ask", "on_conflict: overwrite", 1)))
	if err == nil || !strings.Contains(err.Error(), "on_conflict") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRequiresReadWhenEditAllowed(t *testing.T) {
	_, err := Load(writeConfig(t, configWith("allow: [edit]")))
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
