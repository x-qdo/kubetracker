package main

import (
	"os"

	"github.com/x-qdo/kubetracker/internal/cli"
)

// version is injected via -ldflags "-X main.version=<value>", defaults to "dev".
var version = "dev"

func main() {
	// Delegate to internal CLI. It returns a process exit code.
	os.Exit(cli.Execute(version))
}
