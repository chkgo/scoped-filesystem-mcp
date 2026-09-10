// Command scoped-filesystem-mcp serves configured filesystem roots over MCP stdio.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
	"github.com/chkgo/scoped-filesystem-mcp/internal/server"
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Stderr))
}

func realMain(args []string, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, args, stderr); err != nil {
		fmt.Fprintln(stderr, "scoped-filesystem-mcp:", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("scoped-filesystem-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the YAML configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}
	if *configPath == "" {
		return errors.New("--config is required")
	}
	mcpServer, err := buildServer(expandHome(*configPath))
	if err != nil {
		return err
	}
	return mcpServer.Run(ctx, &mcp.StdioTransport{})
}

func buildServer(configPath string) (*mcp.Server, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	accessManager, err := access.New(cfg.Directories)
	if err != nil {
		return nil, err
	}
	return server.New(filesystem.New(accessManager, filesystem.Options{}), accessManager), nil
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}
