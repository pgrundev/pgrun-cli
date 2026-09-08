package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/pgrundev/pgrun-cli/internal/api"
	"github.com/pgrundev/pgrun-cli/internal/mcpserver"
)

// runMCP handles `pgrun mcp serve`. Unlike every other command, it reads
// its stdin directly (os.Stdin) rather than through Run's stdout/stderr
// parameters — an MCP stdio server inherently needs the process's real
// stdin, and internal/mcpserver has its own pipe-driven tests that don't go
// through Run at all.
func runMCP(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "serve" {
		return usageErrf(stderr, "mcp: expected subcommand \"serve\" (usage: pgrun mcp serve)")
	}

	fs := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if ok, code := parseOrExit(fs, args[1:]); !ok {
		return code
	}
	if len(fs.Args()) > 0 {
		return usageErrf(stderr, "mcp serve: unexpected argument %q", fs.Args()[0])
	}

	// Config via env only — an MCP host launches pgrun as a subprocess and
	// passes credentials through its env, never through flags or a config
	// file lookup on the host's filesystem.
	var client *api.Client
	if url, token := os.Getenv("PGRUN_API_URL"), os.Getenv("PGRUN_API_TOKEN"); url != "" && token != "" {
		client = api.New(url, token)
		client.Kind = "mcp" // branches created through MCP tools are labeled mcp, not cli
	}

	srv := mcpserver.New(client, Version)
	if err := srv.Serve(context.Background(), os.Stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "pgrun: mcp serve: %v\n", err)
		return exitFailure
	}
	return exitSuccess
}
