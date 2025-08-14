package cli

import (
	"fmt"
	"time"

	objutil "github.com/x-qdo/kubetracker/internal/objects"
)

// Options contains shared CLI flags and configuration used by kubetracker subcommands.
//
// This type is intended to be the single source-of-truth for CLI-level options
// (global and per-subcommand). It is kept in its own file so the root command can
// remain focused on wiring while options/validation helpers live independently.
type Options struct {
	// Common options
	Namespace   string // default namespace for namespaceless manifests / releases
	KubeContext string // kubeconfig context to use
	Kubeconfig  string // path to kubeconfig file

	// Kubernetes client rate limit overrides (passed to client-go rest.Config).
	// If zero, client-go defaults / server-side limits apply; set >0 to override.
	KubeQPS   float32 // per-client QPS for Kubernetes client (0 means use client-go default)
	KubeBurst int     // burst for Kubernetes client (0 means use client-go default)

	Timeout               time.Duration // global tracking timeout
	FailureGracePeriod    time.Duration // grace period to retry builtin trackers after failure
	ProgressPrintInterval time.Duration // interval between progress table re-renders (0 means use runner default)

	IncludeKinds  []string // only track these kinds (repeatable)
	ExcludeKinds  []string // exclude these kinds (repeatable)
	CRDConditions []string // custom CRD readiness mappings as provided by CLI

	ShowEvents      bool // show Kubernetes events tables
	OnlyErrorsLogs  bool // only stream logs for failing containers
	Quiet           bool // suppress non-essential output
	Verbose         bool // enable verbose logging
	PrintSummary    bool // print summary before/after tracking
	ExitOnFirstFail bool // exit immediately on first resource failure

	// Track-specific options
	Files     []string // input manifest paths (files, dirs, '-' for stdin)
	Recursive bool     // recursively traverse directories when loading manifests
}

// ApplyGlobal merges values from a global options set into the receiver according
// to the CLI precedence rules (local overrides global for strings unless empty,
// booleans are combined sensibly).
func (o *Options) ApplyGlobal(global *Options) {
	if global == nil {
		return
	}
	o.Kubeconfig = objutil.FirstNonEmpty(o.Kubeconfig, global.Kubeconfig)
	o.KubeContext = objutil.FirstNonEmpty(o.KubeContext, global.KubeContext)

	// Kube client QPS/Burst: prefer explicit local non-zero value, otherwise inherit global.
	if o.KubeQPS == 0 {
		o.KubeQPS = global.KubeQPS
	}
	if o.KubeBurst == 0 {
		o.KubeBurst = global.KubeBurst
	}

	// Duration merge: prefer explicit local non-zero value, otherwise inherit global.
	if o.ProgressPrintInterval == 0 {
		o.ProgressPrintInterval = global.ProgressPrintInterval
	}

	// boolean merges: prefer explicit local truthiness, but honor global flags as defaults.
	o.Quiet = o.Quiet || global.Quiet
	o.Verbose = o.Verbose || global.Verbose
	// PrintSummary should be true only if both local and global allow it.
	o.PrintSummary = o.PrintSummary && global.PrintSummary
	o.ExitOnFirstFail = o.ExitOnFirstFail || global.ExitOnFirstFail
}

// EnsureFilesDefault sets a sensible default for Files when none were provided
// (kubetracker treats absence of file paths as reading from stdin).
func (o *Options) EnsureFilesDefault() {
	if len(o.Files) == 0 {
		o.Files = []string{"-"}
	}
}

// Validate performs basic validation of option values and returns a helpful error
// when values are invalid (caller can wrap or convert into CLI exit codes).
func (o *Options) Validate() error {
	if o.Timeout < 0 {
		return fmt.Errorf("--timeout must be >= 0")
	}
	if o.ProgressPrintInterval < 0 {
		return fmt.Errorf("--progress-print-interval must be >= 0")
	}
	if o.KubeQPS < 0 {
		return fmt.Errorf("--kube-qps must be >= 0")
	}
	if o.KubeBurst < 0 {
		return fmt.Errorf("--kube-burst must be >= 0")
	}
	return nil
}
