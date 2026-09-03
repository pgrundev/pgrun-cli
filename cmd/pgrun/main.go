// Command pgrun creates, uses, and deletes disposable PGRun database
// branches from the command line. See internal/cli for the implementation —
// main is deliberately thin so the CLI is testable without exec'ing a binary.
package main

import (
	"os"

	"github.com/pgrundev/pgrun-cli/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
