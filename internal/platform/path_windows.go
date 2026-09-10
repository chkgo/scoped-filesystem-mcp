package platform

import (
	"io/fs"
	"path"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// A bounded UTF-8 tool path keeps the shared recovery-path reservation valid.
// Native root paths are configured separately and never enter recovery results.
const maxWindowsRelativePathBytes = 2048

func CleanRelative(name string) (string, error) {
	if !utf8.ValidString(name) || len(name) > maxWindowsRelativePathBytes {
		return "", fs.ErrInvalid
	}
	name = strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(name, "/") {
		return "", fs.ErrInvalid
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." {
			continue
		}
		if err := windowsComponent(part); err != nil {
			return "", err
		}
	}
	return path.Clean(name), nil
}

func windowsComponent(name string) error {
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") || len(utf16.Encode([]rune(name))) > 255 {
		return fs.ErrInvalid
	}
	for _, r := range name {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return fs.ErrInvalid
		}
	}
	base, _, _ := strings.Cut(name, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return fs.ErrInvalid
	}
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		suffix := base[3:]
		if utf8.RuneCountInString(suffix) == 1 && strings.Contains("123456789¹²³", suffix) {
			return fs.ErrInvalid
		}
	}
	return nil
}
