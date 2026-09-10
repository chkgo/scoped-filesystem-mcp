package server

type CreateDirectoryInput struct {
	PathInput
	Parents bool `json:"parents" jsonschema:"create missing parent directories when true"`
}

type CreateFileInput struct {
	PathInput
	Content string `json:"content" jsonschema:"UTF-8 text for the new file"`
}

type TextEditInput struct {
	OldText             string `json:"old_text"`
	NewText             string `json:"new_text"`
	ExpectedOccurrences int    `json:"expected_occurrences"`
}

type EditFileInput struct {
	PathInput
	ExpectedRevision string          `json:"expected_revision"`
	ProposedContent  string          `json:"proposed_content"`
	Edits            []TextEditInput `json:"edits,omitempty"`
}

type MovePathInput struct {
	SourceRoot      string `json:"source_root"`
	SourcePath      string `json:"source_path"`
	DestinationRoot string `json:"destination_root"`
	DestinationPath string `json:"destination_path"`
}

type WriteOutput struct {
	Root          string   `json:"root"`
	Path          string   `json:"path"`
	Revision      string   `json:"revision"`
	Size          int64    `json:"size"`
	RecoveryPath  string   `json:"recovery_path,omitempty" jsonschema:"retained root-relative prior version; inspect it and use trash_path for explicit cleanup"`
	RecoveryPaths []string `json:"recovery_paths,omitempty" jsonschema:"all retained root-relative versions that remain the caller's responsibility"`
}

type MutationOutput struct {
	Root string `json:"root"`
	Path string `json:"path"`
}

type PendingEditOutput struct {
	ExpectedRevision string          `json:"expected_revision"`
	ProposedContent  string          `json:"proposed_content"`
	Edits            []TextEditInput `json:"edits"`
}

type ConflictOutput struct {
	Status          string            `json:"status"`
	CurrentContent  string            `json:"current_content"`
	CurrentRevision string            `json:"current_revision"`
	Pending         PendingEditOutput `json:"pending"`
	RecoveryPath    string            `json:"recovery_path"`
	RecoveryPaths   []string          `json:"recovery_paths,omitempty"`
}

// PathInput is the common root-relative addressing shape for filesystem tools.
type PathInput struct {
	Root string `json:"root" jsonschema:"configured root name returned by list_roots"`
	Path string `json:"path" jsonschema:"relative path within root; absolute paths and .. traversal are rejected"`
}

type ListRootsOutput struct {
	Roots []RootOutput `json:"roots" jsonschema:"configured roots available to this server"`
}

type RootOutput struct {
	Unsupported []string `json:"unsupported,omitempty" jsonschema:"allowed operations unavailable in this build; runtime filesystem limits can also apply"`
	Name        string   `json:"name" jsonschema:"configured root name for tool calls"`
	Path        string   `json:"path" jsonschema:"configured user-facing root path"`
	Allow       []string `json:"allow" jsonschema:"operations allowed within this root"`
}

type ListDirectoryOutput struct {
	Entries []EntryOutput `json:"entries" jsonschema:"immediate entries in the requested directory"`
}

type StatPathOutput struct {
	Entry EntryOutput `json:"entry" jsonschema:"metadata for the requested path"`
}

type EntryOutput struct {
	Root       string `json:"root" jsonschema:"configured root name"`
	Path       string `json:"path" jsonschema:"relative path within the configured root"`
	Type       string `json:"type" jsonschema:"entry type: file, directory, symlink, or other"`
	Revision   string `json:"revision" jsonschema:"content revision for regular files when available"`
	Size       int64  `json:"size" jsonschema:"entry size in bytes"`
	CreatedAt  string `json:"created_at" jsonschema:"filesystem creation timestamp in RFC 3339 format"`
	ModifiedAt string `json:"modified_at" jsonschema:"filesystem modification timestamp in RFC 3339 format"`
}

type SearchInput struct {
	PathInput
	Query         string `json:"query" jsonschema:"text to match in paths or text-file lines"`
	CaseSensitive bool   `json:"case_sensitive,omitempty" jsonschema:"match case exactly when true; false uses case-insensitive matching"`
	MaxResults    int    `json:"max_results,omitempty" jsonschema:"maximum number of results; default 100 and values above 1000 are capped at 1000"`
}

type SearchPathsOutput struct {
	Matches    []PathMatchOutput `json:"matches" jsonschema:"matching relative paths"`
	MaxResults int               `json:"max_results" jsonschema:"effective result cap; at most 1000"`
}

type PathMatchOutput struct {
	Root string `json:"root" jsonschema:"configured root name"`
	Path string `json:"path" jsonschema:"matching relative path"`
}

type SearchTextOutput struct {
	Matches    []TextMatchOutput `json:"matches" jsonschema:"matching text-file lines"`
	MaxResults int               `json:"max_results" jsonschema:"effective result cap; at most 1000"`
}

type TextMatchOutput struct {
	Root string `json:"root" jsonschema:"configured root name"`
	Path string `json:"path" jsonschema:"relative path containing the match"`
	Text string `json:"text" jsonschema:"matching line text"`
	Line int    `json:"line" jsonschema:"one-based line number"`
}

type ReadTextOutput struct {
	Root     string `json:"root" jsonschema:"configured root name"`
	Path     string `json:"path" jsonschema:"relative path within the configured root"`
	Content  string `json:"content" jsonschema:"UTF-8 file content"`
	Revision string `json:"revision" jsonschema:"revision hash for later protected edits"`
	Size     int64  `json:"size" jsonschema:"content size in bytes"`
}

type ReadBinaryOutput struct {
	Root     string `json:"root" jsonschema:"configured root name"`
	Path     string `json:"path" jsonschema:"relative path within the configured root"`
	MIMEType string `json:"mime_type" jsonschema:"detected media type"`
	Size     int64  `json:"size" jsonschema:"content size in bytes"`
	Revision string `json:"revision" jsonschema:"revision hash for the returned bytes"`
	URI      string `json:"uri" jsonschema:"scopedfs resource URI for this binary file"`
	Kind     string `json:"kind" jsonschema:"MCP content kind: image, audio, or resource"`
}
