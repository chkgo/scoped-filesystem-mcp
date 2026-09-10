// Package access authorizes paths beneath configured filesystem roots.
package access

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"

	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

// Error is an authorization error with a stable category.
type Error struct {
	Code string
	err  error
}

func (e *Error) Error() string {
	return e.Code
}

func (e *Error) Unwrap() error {
	return e.err
}

// RootInfo describes a configured root without exposing its canonical path.
type RootInfo struct {
	Name  string
	Path  string
	Allow []config.Operation
}

// Allows reports whether an operation is enabled for this root.
func (r RootInfo) Allows(op config.Operation) bool {
	for _, allowed := range r.Allow {
		if allowed == op {
			return true
		}
	}
	return false
}

type root struct {
	rule       config.DirectoryRule
	configured string
	canonical  string
	identity   platform.Identity
	file       *os.File
}

// Manager holds the startup-validated roots used to authorize requests.
type Manager struct {
	roots map[string]*root
	order []string
}

// New registers directory rules and records each root's canonical target.
func New(rules []config.DirectoryRule) (*Manager, error) {
	m := &Manager{roots: make(map[string]*root, len(rules))}
	for _, rule := range rules {
		if rule.Name == "" {
			m.Close()
			return nil, fmt.Errorf("invalid configuration: root name is empty")
		}
		if _, exists := m.roots[rule.Name]; exists {
			m.Close()
			return nil, fmt.Errorf("invalid configuration: duplicate root %q", rule.Name)
		}
		if !filepath.IsAbs(rule.Path) {
			m.Close()
			return nil, fmt.Errorf("invalid configuration: root %q path is not absolute", rule.Name)
		}

		configured := filepath.Clean(rule.Path)
		canonical, err := filepath.EvalSymlinks(configured)
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("invalid configuration: resolve root %q: %w", rule.Name, err)
		}
		file, err := platform.OpenRoot(canonical)
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("invalid configuration: open root %q: %w", rule.Name, err)
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			m.Close()
			return nil, fmt.Errorf("invalid configuration: stat root %q: %w", rule.Name, err)
		}
		if !info.IsDir() {
			file.Close()
			m.Close()
			return nil, fmt.Errorf("invalid configuration: root %q is not a directory", rule.Name)
		}

		metadata, err := platform.MetadataForFile(file)
		if err != nil {
			file.Close()
			m.Close()
			return nil, fmt.Errorf("invalid configuration: identify root %q: %w", rule.Name, err)
		}
		rule.Allow = append([]config.Operation(nil), rule.Allow...)
		rule.Ask = append([]config.Operation(nil), rule.Ask...)
		m.roots[rule.Name] = &root{rule: rule, configured: configured, canonical: canonical, identity: metadata.Identity, file: file}
		m.order = append(m.order, rule.Name)
	}
	return m, nil
}

// Close releases the directory descriptors pinned for configured roots.
func (m *Manager) Close() error {
	var first error
	for _, r := range m.roots {
		if r.file == nil {
			continue
		}
		if err := r.file.Close(); err != nil && first == nil {
			first = err
		}
		r.file = nil
	}
	return first
}

// Roots returns the configured user-facing roots and their enabled operations.
func (m *Manager) Roots() []RootInfo {
	roots := make([]RootInfo, 0, len(m.order))
	for _, name := range m.order {
		r := m.roots[name]
		allow := append([]config.Operation(nil), r.rule.Allow...)
		roots = append(roots, RootInfo{Name: r.rule.Name, Path: r.configured, Allow: allow})
	}
	return roots
}

// Asks reports whether an allowed operation needs user confirmation.
func (m *Manager) Asks(rootName string, op config.Operation) bool {
	r, exists := m.roots[rootName]
	return exists && r.rule.Asks(op)
}

func accessError(code string, err error) error {
	return &Error{Code: code, err: err}
}
