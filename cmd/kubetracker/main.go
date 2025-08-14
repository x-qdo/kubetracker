package main

import (
	"os"

	"github.com/go-logr/logr"
	"github.com/x-qdo/kubetracker/internal/cli"
	"k8s.io/klog/v2"
)

// version is injected via -ldflags "-X main.version=<value>", defaults to "dev".
var version = "dev"

func init() {
	klog.SetLogger(logr.Discard())
}

func main() {
	// Delegate to internal CLI. It returns a process exit code.
	os.Exit(cli.Execute(version))
}
