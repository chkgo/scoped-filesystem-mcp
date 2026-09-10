package server

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
)

const maximumSearchResults = 1000

func (s toolServer) register(server *mcp.Server) {
	mcp.AddTool(server, tool[ListRootsOutput]("list_roots", "List configured filesystem roots and their allowed operations."), s.listRoots)
	mcp.AddTool(server, tool[ListDirectoryOutput]("list_directory", "List immediate entries in one configured directory."), s.listDirectory)
	mcp.AddTool(server, tool[StatPathOutput]("stat_path", "Report metadata for one path in a configured root."), s.statPath)
	mcp.AddTool(server, tool[SearchPathsOutput]("search_paths", "Search relative paths below a configured directory; results are capped at 1000."), s.searchPaths)
	mcp.AddTool(server, tool[SearchTextOutput]("search_text", "Search UTF-8 text-file lines below a configured directory; results are capped at 1000."), s.searchText)
	mcp.AddTool(server, tool[ReadTextOutput]("read_text_file", "Read one UTF-8 text file and its revision."), s.readText)
	mcp.AddTool(server, tool[ReadBinaryOutput]("read_binary_file", "Read one binary file as native image, audio, or embedded resource content together with metadata."), s.readBinary)
}

func tool[Out any](name, description string) *mcp.Tool {
	successSchema, err := jsonschema.For[Out](nil)
	if err != nil {
		panic(fmt.Sprintf("derive output schema for %s: %v", name, err))
	}
	errorSchema, err := jsonschema.For[ToolError](nil)
	if err != nil {
		panic(fmt.Sprintf("derive error schema for %s: %v", name, err))
	}
	openWorld := false
	return &mcp.Tool{
		Name:         name,
		Description:  description,
		OutputSchema: &jsonschema.Schema{OneOf: []*jsonschema.Schema{successSchema, errorSchema}},
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld},
	}
}

func (s toolServer) listRoots(_ context.Context, req *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, any, error) {
	if _, _, result, err := s.gate(req, input, nil, false); result != nil || err != nil {
		return result, nil, err
	}
	roots := s.access.Roots()
	output := ListRootsOutput{Roots: make([]RootOutput, 0, len(roots))}
	for _, root := range roots {
		allow := make([]string, len(root.Allow))
		for i, operation := range root.Allow {
			allow[i] = string(operation)
		}
		output.Roots = append(output.Roots, RootOutput{Name: root.Name, Path: root.Path, Allow: allow})
	}
	return successResult(output, "listed configured roots"), nil, nil
}

func (s toolServer) listDirectory(ctx context.Context, req *mcp.CallToolRequest, input PathInput) (*mcp.CallToolResult, any, error) {
	if _, _, result, err := s.gate(req, input, []operationTarget{target(input, config.OpList, access.Existing)}, false); result != nil || err != nil {
		return result, nil, err
	}
	entries, err := s.filesystem.ListDirectory(ctx, input.Root, input.Path)
	if err != nil {
		return failed(err)
	}
	output := ListDirectoryOutput{Entries: make([]EntryOutput, len(entries))}
	for i, entry := range entries {
		output.Entries[i] = entryOutput(entry)
	}
	return successResult(output, "listed directory"), nil, nil
}

func (s toolServer) statPath(ctx context.Context, req *mcp.CallToolRequest, input PathInput) (*mcp.CallToolResult, any, error) {
	if _, _, result, err := s.gate(req, input, []operationTarget{target(input, config.OpList, access.Existing)}, false); result != nil || err != nil {
		return result, nil, err
	}
	entry, err := s.filesystem.Stat(ctx, input.Root, input.Path)
	if err != nil {
		return failed(err)
	}
	return successResult(StatPathOutput{Entry: entryOutput(entry)}, "reported path metadata"), nil, nil
}

func (s toolServer) searchPaths(ctx context.Context, req *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, any, error) {
	if _, _, result, err := s.gate(req, input, []operationTarget{target(input.PathInput, config.OpSearch, access.Existing)}, false); result != nil || err != nil {
		return result, nil, err
	}
	matches, err := s.filesystem.SearchPaths(ctx, input.Root, input.Path, input.Query, searchOptions(input))
	if err != nil {
		return failed(err)
	}
	output := SearchPathsOutput{Matches: make([]PathMatchOutput, len(matches)), MaxResults: effectiveMaxResults(input.MaxResults)}
	for i, match := range matches {
		output.Matches[i] = PathMatchOutput{Root: match.Root, Path: match.Path}
	}
	return successResult(output, "searched paths"), nil, nil
}

func (s toolServer) searchText(ctx context.Context, req *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, any, error) {
	if _, _, result, err := s.gate(req, input, []operationTarget{target(input.PathInput, config.OpSearch, access.Existing), target(input.PathInput, config.OpRead, access.Existing)}, false); result != nil || err != nil {
		return result, nil, err
	}
	matches, err := s.filesystem.SearchText(ctx, input.Root, input.Path, input.Query, searchOptions(input))
	if err != nil {
		return failed(err)
	}
	output := SearchTextOutput{Matches: make([]TextMatchOutput, len(matches)), MaxResults: effectiveMaxResults(input.MaxResults)}
	for i, match := range matches {
		output.Matches[i] = TextMatchOutput{Root: match.Root, Path: match.Path, Text: match.Text, Line: match.Line}
	}
	return successResult(output, "searched text"), nil, nil
}

func (s toolServer) readText(ctx context.Context, req *mcp.CallToolRequest, input PathInput) (*mcp.CallToolResult, any, error) {
	if _, _, result, err := s.gate(req, input, []operationTarget{target(input, config.OpRead, access.Existing)}, false); result != nil || err != nil {
		return result, nil, err
	}
	file, err := s.filesystem.ReadText(ctx, input.Root, input.Path)
	if err != nil {
		return failed(err)
	}
	return successResult(ReadTextOutput{Root: file.Root, Path: file.Path, Content: file.Content, Revision: file.Revision, Size: file.Size}, "read text file"), nil, nil
}

func (s toolServer) readBinary(ctx context.Context, req *mcp.CallToolRequest, input PathInput) (*mcp.CallToolResult, any, error) {
	if _, _, result, err := s.gate(req, input, []operationTarget{target(input, config.OpRead, access.Existing)}, false); result != nil || err != nil {
		return result, nil, err
	}
	file, err := s.filesystem.ReadBinary(ctx, input.Root, input.Path)
	if err != nil {
		return failed(err)
	}
	output := ReadBinaryOutput{Root: file.Root, Path: file.Path, MIMEType: file.MIMEType, Size: file.Size, Revision: file.Revision, URI: file.URI, Kind: string(file.Kind)}
	result := successResult(output, "read binary file")
	switch file.Kind {
	case filesystem.BinaryImage:
		result.Content = []mcp.Content{&mcp.ImageContent{Data: file.Data, MIMEType: file.MIMEType}}
	case filesystem.BinaryAudio:
		result.Content = []mcp.Content{&mcp.AudioContent{Data: file.Data, MIMEType: file.MIMEType}}
	default:
		result.Content = []mcp.Content{&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: file.URI, MIMEType: file.MIMEType, Blob: file.Data}}}
	}
	return result, nil, nil
}

func failed(err error) (*mcp.CallToolResult, any, error) {
	result, unexpected := failureResult(err)
	return result, nil, unexpected
}

func successResult(output any, message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: message}}, StructuredContent: output}
}

func entryOutput(entry filesystem.Entry) EntryOutput {
	return EntryOutput{Root: entry.Root, Path: entry.Path, Type: entry.Type, Revision: entry.Revision, Size: entry.Size, CreatedAt: entry.CreatedAt, ModifiedAt: entry.ModifiedAt}
}

func searchOptions(input SearchInput) filesystem.SearchOptions {
	return filesystem.SearchOptions{CaseSensitive: input.CaseSensitive, MaxResults: input.MaxResults}
}

func effectiveMaxResults(value int) int {
	if value <= 0 {
		return filesystem.DefaultMaxResults
	}
	if value > maximumSearchResults {
		return maximumSearchResults
	}
	return value
}
