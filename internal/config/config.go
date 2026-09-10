// Package config loads and validates the server's directory permissions.
package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Operation string

const (
	OpList            Operation = "list"
	OpSearch          Operation = "search"
	OpRead            Operation = "read"
	OpCreate          Operation = "create"
	OpEdit            Operation = "edit"
	OpMove            Operation = "move"
	OpTrash           Operation = "trash"
	OpPermanentDelete Operation = "permanent_delete"
)

var validOperations = map[Operation]struct{}{
	OpList: {}, OpSearch: {}, OpRead: {}, OpCreate: {},
	OpEdit: {}, OpMove: {}, OpTrash: {}, OpPermanentDelete: {},
}

type DirectoryRule struct {
	Name       string      `yaml:"name"`
	Path       string      `yaml:"path"`
	Allow      []Operation `yaml:"allow"`
	Ask        []Operation `yaml:"ask"`
	OnConflict string      `yaml:"on_conflict"`
}

type Config struct {
	Version     int             `yaml:"version"`
	Directories []DirectoryRule `yaml:"directories"`
}

func (r DirectoryRule) Allows(op Operation) bool { return contains(r.Allow, op) }
func (r DirectoryRule) Asks(op Operation) bool   { return contains(r.Ask, op) }

func contains(operations []Operation, want Operation) bool {
	for _, operation := range operations {
		if operation == want {
			return true
		}
	}
	return false
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	var cfg Config
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("config contains multiple YAML documents")
		}
		return Config{}, fmt.Errorf("decode trailing config document: %w", err)
	}
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validate(cfg Config) error {
	if cfg.Version != 1 {
		return fmt.Errorf("version must be 1")
	}
	if len(cfg.Directories) == 0 {
		return fmt.Errorf("directories must not be empty")
	}
	names := make(map[string]struct{}, len(cfg.Directories))
	for i, rule := range cfg.Directories {
		if rule.Name == "" {
			return fmt.Errorf("directory %d name must not be empty", i)
		}
		if _, exists := names[rule.Name]; exists {
			return fmt.Errorf("duplicate directory name %q", rule.Name)
		}
		names[rule.Name] = struct{}{}
		if !filepath.IsAbs(rule.Path) {
			return fmt.Errorf("directory %q path must be absolute", rule.Name)
		}
		if rule.OnConflict != "ask" {
			return fmt.Errorf("directory %q on_conflict must be ask", rule.Name)
		}
		if err := validateOperations(rule.Name, "allow", rule.Allow); err != nil {
			return err
		}
		if err := validateOperations(rule.Name, "ask", rule.Ask); err != nil {
			return err
		}
		if contains(rule.Allow, OpEdit) && !contains(rule.Allow, OpRead) {
			return fmt.Errorf("directory %q allow edit requires read operation", rule.Name)
		}
		for _, operation := range rule.Ask {
			if !contains(rule.Allow, operation) {
				return fmt.Errorf("ask operation %s is not allowed in directory %q", operation, rule.Name)
			}
		}
	}
	return nil
}

func validateOperations(name, field string, operations []Operation) error {
	seen := make(map[Operation]struct{}, len(operations))
	for _, operation := range operations {
		if _, ok := validOperations[operation]; !ok {
			return fmt.Errorf("directory %q has unknown operation %q in %s", name, operation, field)
		}
		if _, ok := seen[operation]; ok {
			return fmt.Errorf("directory %q has duplicate operation %q in %s", name, operation, field)
		}
		seen[operation] = struct{}{}
	}
	return nil
}
