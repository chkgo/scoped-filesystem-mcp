package filesystem

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chkgo/scoped-filesystem-mcp/internal/platform"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
)

const (
	DefaultMaxResults = 100
	maxSearchResults  = 1000
)

type SearchOptions struct {
	CaseSensitive bool
	MaxResults    int
}

type Entry struct {
	Root       string
	Path       string
	Type       string
	Revision   string
	Size       int64
	CreatedAt  string
	ModifiedAt string
}

type PathMatch struct {
	Root string
	Path string
}

type TextMatch struct {
	Root string
	Path string
	Text string
	Line int
}

func (s *Service) ListDirectory(ctx context.Context, root, path string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, newError(root, path, config.OpList, "filesystem_unavailable", err)
	}

	p, err := s.access.Resolve(root, path, config.OpList, access.Existing)
	if err != nil {
		return nil, MapError(root, path, config.OpList, err)
	}
	directory, err := p.OpenDirectory()
	if err != nil {
		if errors.Is(err, platform.ErrNotDirectory) {
			return nil, newError(root, path, config.OpList, "unsupported_file_type", err)
		}
		return nil, MapError(root, path, config.OpList, err)
	}
	defer directory.Close()

	info, err := directory.Stat()
	if err != nil {
		return nil, MapError(root, path, config.OpList, err)
	}
	if !info.IsDir() {
		return nil, newError(root, path, config.OpList, "unsupported_file_type", nil)
	}

	items, err := directory.ReadDir(-1)
	if err != nil {
		return nil, MapError(root, path, config.OpList, err)
	}
	sortDirEntries(items)

	entries := make([]Entry, 0, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, newError(root, path, config.OpList, "filesystem_unavailable", err)
		}
		entryPath := joinRelative(path, item.Name())
		entry, err := s.directoryEntry(ctx, root, entryPath, directory, item)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (s *Service) Stat(ctx context.Context, root, path string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, newError(root, path, config.OpList, "filesystem_unavailable", err)
	}

	p, err := s.access.Resolve(root, path, config.OpList, access.Existing)
	if err != nil {
		return Entry{}, MapError(root, path, config.OpList, err)
	}
	file, err := p.Open(os.O_RDONLY, 0)
	if err != nil {
		return Entry{}, MapError(root, path, config.OpList, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return Entry{}, MapError(root, path, config.OpList, err)
	}
	return s.entryFromOpenFile(ctx, root, path, file, info, config.OpList)
}

func (s *Service) directoryEntry(ctx context.Context, root, path string, directory *os.File, item os.DirEntry) (Entry, error) {
	// The parent is already authorized and pinned. Inspect the entry itself so
	// dangling or out-of-root symlinks can be listed without following targets.
	stat, err := platform.LstatAt(directory, item.Name())
	if err != nil {
		return Entry{}, MapError(root, path, config.OpList, err)
	}
	entry := entryFromStat(root, path, stat)
	if entry.Type != "file" {
		return entry, nil
	}

	p, err := s.access.Resolve(root, path, config.OpList, access.Existing)
	if err != nil {
		return Entry{}, MapError(root, path, config.OpList, err)
	}
	file, err := p.Open(os.O_RDONLY, 0)
	if err != nil {
		return Entry{}, MapError(root, path, config.OpList, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Entry{}, MapError(root, path, config.OpList, err)
	}
	return s.entryFromOpenFile(ctx, root, path, file, info, config.OpList)
}

func (s *Service) entryFromOpenFile(ctx context.Context, root, path string, file *os.File, info os.FileInfo, operation config.Operation) (Entry, error) {
	entry := Entry{Root: root, Path: path, Type: entryType(info.Mode()), Size: info.Size(), ModifiedAt: info.ModTime().UTC().Format(time.RFC3339Nano)}
	if stat, err := platform.MetadataForFile(file); err == nil && stat.CreatedAt != nil {
		entry.CreatedAt = stat.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if !info.Mode().IsRegular() {
		return entry, nil
	}
	revision, size, err := RevisionReader(ctx, file)
	if err != nil {
		return Entry{}, MapError(root, path, operation, err)
	}
	entry.Revision = revision
	entry.Size = size
	return entry, nil
}

func entryFromStat(root, path string, stat platform.Metadata) Entry {
	entry := Entry{Root: root, Path: path, Type: entryType(stat.Mode), Size: stat.Size, ModifiedAt: stat.ModifiedAt.UTC().Format(time.RFC3339Nano)}
	if stat.CreatedAt != nil {
		entry.CreatedAt = stat.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	return entry
}

func entryType(mode os.FileMode) string {
	if mode&os.ModeSymlink != 0 {
		return "symlink"
	}
	if mode.IsDir() {
		return "directory"
	}
	if mode.IsRegular() {
		return "file"
	}
	return "other"
}

func (s *Service) SearchPaths(ctx context.Context, root, path, query string, options SearchOptions) ([]PathMatch, error) {
	needle := normalizedQuery(query, options.CaseSensitive)
	limit := resultLimit(options.MaxResults)
	results := make([]PathMatch, 0, limit)

	err := s.walk(ctx, root, path, func(relative string, _ os.FileMode) error {
		if strings.Contains(normalizedQuery(relative, options.CaseSensitive), needle) {
			results = keepPathMatch(results, PathMatch{Root: root, Path: relative}, limit)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return trimPathMatches(results, s.maxReadBytes), nil
}

func (s *Service) SearchText(ctx context.Context, root, path, query string, options SearchOptions) ([]TextMatch, error) {
	needle := normalizedQuery(query, options.CaseSensitive)
	limit := resultLimit(options.MaxResults)
	results := newTextMatchCollector(limit, s.maxReadBytes)

	err := s.walk(ctx, root, path, func(relative string, mode os.FileMode) error {
		if entryType(mode) == "symlink" {
			return nil
		}

		p, err := s.access.Resolve(root, relative, config.OpRead, access.Existing)
		if err != nil {
			return MapError(root, relative, config.OpRead, err)
		}
		file, err := p.Open(os.O_RDONLY, 0)
		if err != nil {
			return MapError(root, relative, config.OpRead, err)
		}
		defer file.Close()

		info, err := file.Stat()
		if err != nil {
			return MapError(root, relative, config.OpRead, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if s.beforeRead != nil {
			if err := s.beforeRead(); err != nil {
				return MapError(root, relative, config.OpRead, err)
			}
		}
		// Collect per-file matches only after the entire bounded file has been
		// classified as text, so growth or a late binary byte cannot leak a
		// partial result.
		limited := &countingReader{reader: io.LimitReader(&contextReader{ctx: ctx, reader: file}, s.maxReadBytes+1)}
		scanner := bufio.NewScanner(limited)
		scanner.Buffer(make([]byte, 64*1024), int(s.maxReadBytes)+1)
		line := 0
		fileMatches := newTextMatchCollector(limit, s.maxReadBytes)
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				return newError(root, relative, config.OpRead, "filesystem_unavailable", err)
			}
			line++
			text := scanner.Text()
			if !searchableText(scanner.Bytes()) {
				return nil
			}
			if strings.Contains(normalizedQuery(text, options.CaseSensitive), needle) {
				fileMatches.add(TextMatch{Root: root, Path: relative, Line: line, Text: text})
			}
		}
		if err := scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) || limited.count > s.maxReadBytes {
				return nil
			}
			return MapError(root, relative, config.OpRead, err)
		}
		if limited.count > s.maxReadBytes {
			return nil
		}
		for _, match := range fileMatches.matches {
			results.add(match)
		}
		results.mergeBoundary(fileMatches.boundary)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results.matches, nil
}

func (s *Service) walk(ctx context.Context, root, path string, visit func(string, os.FileMode) error) error {
	if err := ctx.Err(); err != nil {
		return newError(root, path, config.OpSearch, "filesystem_unavailable", err)
	}
	p, err := s.access.Resolve(root, path, config.OpSearch, access.Existing)
	if err != nil {
		return MapError(root, path, config.OpSearch, err)
	}
	directory, err := p.OpenDirectory()
	if err != nil {
		if errors.Is(err, platform.ErrNotDirectory) {
			return newError(root, path, config.OpSearch, "unsupported_file_type", err)
		}
		return MapError(root, path, config.OpSearch, err)
	}
	defer directory.Close()

	info, err := directory.Stat()
	if err != nil {
		return MapError(root, path, config.OpSearch, err)
	}
	if !info.IsDir() {
		return newError(root, path, config.OpSearch, "unsupported_file_type", nil)
	}
	return s.walkDirectory(ctx, root, path, directory, visit)
}

func (s *Service) walkDirectory(ctx context.Context, root, path string, directory *os.File, visit func(string, os.FileMode) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return newError(root, path, config.OpSearch, "filesystem_unavailable", err)
		}
		var items []os.DirEntry
		var readErr error
		if s.readDirectory != nil {
			items, readErr = s.readDirectory(directory, 128)
		} else {
			items, readErr = directory.ReadDir(128)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return MapError(root, path, config.OpSearch, readErr)
		}
		sortDirEntries(items)
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return newError(root, path, config.OpSearch, "filesystem_unavailable", err)
			}
			relative := joinRelative(path, item.Name())
			mode, err := directoryEntryMode(directory, item.Name())
			if err != nil {
				return MapError(root, relative, config.OpSearch, err)
			}
			if err := visit(relative, mode); err != nil {
				return err
			}
			if entryType(mode) != "directory" {
				continue
			}
			child, err := openDirectoryAt(directory, item.Name())
			if err != nil {
				return MapError(root, relative, config.OpSearch, err)
			}
			err = s.walkDirectory(ctx, root, relative, child, visit)
			closeErr := child.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return MapError(root, relative, config.OpSearch, closeErr)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func directoryEntryMode(directory *os.File, name string) (os.FileMode, error) {
	stat, err := platform.LstatAt(directory, name)
	return stat.Mode, err
}
func openDirectoryAt(parent *os.File, name string) (*os.File, error) {
	return platform.OpenDirectoryAt(parent, name)
}

func sortDirEntries(entries []os.DirEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
}

func joinRelative(parent, name string) string {
	if parent == "." {
		return name
	}
	return filepath.ToSlash(filepath.Join(parent, name))
}

func normalizedQuery(value string, caseSensitive bool) string {
	if caseSensitive {
		return value
	}
	return strings.ToLower(value)
}

func searchableText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for _, b := range data {
		if b == 0 || b == 0x7f || (b < 0x20 && b != '\t' && b != '\n' && b != '\r') {
			return false
		}
	}
	return true
}

func keepPathMatch(matches []PathMatch, candidate PathMatch, limit int) []PathMatch {
	index := sort.Search(len(matches), func(i int) bool { return matches[i].Path >= candidate.Path })
	matches = append(matches, PathMatch{})
	copy(matches[index+1:], matches[index:])
	matches[index] = candidate
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

func textMatchLess(left, right TextMatch) bool {
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	return left.Line < right.Line
}

type textMatchBoundary struct {
	Path string
	Line int
}

type textMatchCollector struct {
	matches   []TextMatch
	usedBytes int64
	maxBytes  int64
	limit     int
	boundary  *textMatchBoundary
}

func newTextMatchCollector(limit int, maxBytes int64) *textMatchCollector {
	return &textMatchCollector{matches: make([]TextMatch, 0, limit), maxBytes: maxBytes, limit: limit}
}

func (c *textMatchCollector) add(candidate TextMatch) {
	if c.limit <= 0 || c.maxBytes <= 0 {
		return
	}
	if c.boundary != nil && !textMatchBeforeBoundary(candidate, *c.boundary) {
		return
	}
	index := sort.Search(len(c.matches), func(i int) bool { return !textMatchLess(c.matches[i], candidate) })
	c.matches = append(c.matches, TextMatch{})
	copy(c.matches[index+1:], c.matches[index:])
	c.matches[index] = candidate

	used := int64(0)
	cut := 0
	for cut < len(c.matches) && cut < c.limit {
		size := textMatchSize(c.matches[cut])
		if size > c.maxBytes-used {
			break
		}
		used += size
		cut++
	}
	if cut < len(c.matches) {
		removed := textMatchBoundary{Path: c.matches[cut].Path, Line: c.matches[cut].Line}
		if c.boundary == nil || boundaryLess(removed, *c.boundary) {
			c.boundary = &removed
		}
		for i := cut; i < len(c.matches); i++ {
			c.matches[i] = TextMatch{}
		}
		c.matches = c.matches[:cut]
	}
	c.usedBytes = used
}

func (c *textMatchCollector) mergeBoundary(boundary *textMatchBoundary) {
	if boundary == nil || (c.boundary != nil && !boundaryLess(*boundary, *c.boundary)) {
		return
	}
	merged := *boundary
	c.boundary = &merged
	cut := sort.Search(len(c.matches), func(i int) bool {
		return !textMatchBeforeBoundary(c.matches[i], merged)
	})
	if cut == len(c.matches) {
		return
	}
	for i := cut; i < len(c.matches); i++ {
		c.matches[i] = TextMatch{}
	}
	c.matches = c.matches[:cut]
	c.usedBytes = 0
	for _, match := range c.matches {
		c.usedBytes += textMatchSize(match)
	}
}

func textMatchBeforeBoundary(match TextMatch, boundary textMatchBoundary) bool {
	return match.Path < boundary.Path || (match.Path == boundary.Path && match.Line < boundary.Line)
}

func boundaryLess(left, right textMatchBoundary) bool {
	return left.Path < right.Path || (left.Path == right.Path && left.Line < right.Line)
}

func textMatchSize(match TextMatch) int64 {
	return int64(len(match.Root)+len(match.Path)+len(match.Text)) + 64
}

func trimPathMatches(matches []PathMatch, maxBytes int64) []PathMatch {
	used := int64(0)
	for i, match := range matches {
		used += int64(len(match.Root)+len(match.Path)) + 64
		if used > maxBytes {
			return matches[:i]
		}
	}
	return matches
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.count += int64(n)
	return n, err
}

func resultLimit(max int) int {
	if max <= 0 {
		return DefaultMaxResults
	}
	if max > maxSearchResults {
		return maxSearchResults
	}
	return max
}
