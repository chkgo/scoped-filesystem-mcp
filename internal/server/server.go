// Package server exposes the scoped filesystem through MCP tools.
package server

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
)

type toolServer struct {
	filesystem *filesystem.Service
	access     *access.Manager
	pending    *pendingStore
}

// New constructs an MCP server with the version-one scoped filesystem tools.
func New(filesystemService *filesystem.Service, accessManager *access.Manager) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "scoped-filesystem-mcp", Version: "0.1.0"}, nil)
	tools := toolServer{filesystem: filesystemService, access: accessManager, pending: &pendingStore{}}
	tools.register(server)
	tools.registerWrites(server)
	return server
}
