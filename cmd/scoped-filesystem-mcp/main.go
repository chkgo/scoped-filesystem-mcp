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
	pathpkg "path"
	"runtime"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chkgo/scoped-filesystem-mcp/internal/access"
	"github.com/chkgo/scoped-filesystem-mcp/internal/config"
	"github.com/chkgo/scoped-filesystem-mcp/internal/filesystem"
	"github.com/chkgo/scoped-filesystem-mcp/internal/server"
)

const configEnvironmentVariable = "SCOPED_FILESYSTEM_MCP_CONFIG"

type configEnvironment struct {
	goos          string
	getenv        func(string) string
	userHomeDir   func() (string, error)
	userConfigDir func() (string, error)
}

type configSelection struct {
	path   string
	source string
}

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
	return runWithEnvironment(ctx, args, stderr, configEnvironment{
		goos:          runtime.GOOS,
		getenv:        os.Getenv,
		userHomeDir:   os.UserHomeDir,
		userConfigDir: os.UserConfigDir,
	})
}

func runWithEnvironment(ctx context.Context, args []string, stderr io.Writer, env configEnvironment) error {
	flags := flag.NewFlagSet("scoped-filesystem-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the YAML configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}
	selection, err := resolveConfigPath(*configPath, env)
	if err != nil {
		return err
	}
	cfg, err := config.Load(selection.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("configuration file not found at %q (selected from %s); pass --config PATH or set %s; no configuration was created", selection.path, selection.source, configEnvironmentVariable)
		}
		return err
	}
	mcpServer, err := buildConfiguredServer(cfg)
	if err != nil {
		return err
	}
	return mcpServer.Run(ctx, &mcp.StdioTransport{})
}

func resolveConfigPath(explicit string, env configEnvironment) (configSelection, error) {
	if explicit != "" {
		return expandConfigPath(explicit, "--config", env)
	}
	if configured := env.getenv(configEnvironmentVariable); configured != "" {
		return expandConfigPath(configured, configEnvironmentVariable, env)
	}

	var base string
	var err error
	switch env.goos {
	case "windows":
		base, err = env.userConfigDir()
	case "linux":
		if xdg := env.getenv("XDG_CONFIG_HOME"); xdg != "" {
			if !pathpkg.IsAbs(xdg) {
				return configSelection{}, fmt.Errorf("XDG_CONFIG_HOME must be an absolute path")
			}
			base = xdg
		} else {
			base, err = env.userHomeDir()
			base = joinConfigPath(env.goos, base, ".config")
		}
	default:
		base, err = env.userHomeDir()
		base = joinConfigPath(env.goos, base, ".config")
	}
	if err != nil {
		return configSelection{}, fmt.Errorf("resolve platform configuration directory: %w", err)
	}
	return configSelection{path: joinConfigPath(env.goos, base, "scoped-filesystem-mcp", "config.yaml"), source: "platform default"}, nil
}

func expandConfigPath(value, source string, env configEnvironment) (configSelection, error) {
	if value != "~" && !strings.HasPrefix(value, "~/") && !(env.goos == "windows" && strings.HasPrefix(value, `~\`)) {
		return configSelection{path: value, source: source}, nil
	}
	home, err := env.userHomeDir()
	if err != nil {
		return configSelection{}, fmt.Errorf("expand home in %s: %w", source, err)
	}
	if value == "~" {
		return configSelection{path: home, source: source}, nil
	}
	return configSelection{path: joinConfigPath(env.goos, home, value[2:]), source: source}, nil
}

func joinConfigPath(goos string, elements ...string) string {
	if goos != "windows" {
		return pathpkg.Join(elements...)
	}
	joined := ""
	for _, element := range elements {
		element = strings.ReplaceAll(element, "/", `\`)
		if joined == "" {
			joined = strings.TrimRight(element, `\/`)
			continue
		}
		joined += `\` + strings.Trim(element, `\/`)
	}
	return joined
}

func buildServer(configPath string) (*mcp.Server, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	return buildConfiguredServer(cfg)
}

func buildConfiguredServer(cfg config.Config) (*mcp.Server, error) {
	accessManager, err := access.New(cfg.Directories)
	if err != nil {
		return nil, err
	}
	return server.New(filesystem.New(accessManager, filesystem.Options{}), accessManager), nil
}
