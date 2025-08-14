package cli

// Package cli wires the Cobra root command and subcommands for kubetracker,
// delegating business logic to internal packages. It exposes Execute(version)
// which returns a process exit code for the caller to os.Exit with.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/x-qdo/kubetracker/internal/helm"
	"github.com/x-qdo/kubetracker/internal/kube"
	objutil "github.com/x-qdo/kubetracker/internal/objects"
	"github.com/x-qdo/kubetracker/internal/render"
	"github.com/x-qdo/kubetracker/internal/track"
	"github.com/x-qdo/kubetracker/pkg/exitcode"
)

// Execute builds the CLI and runs it. It returns a process exit code.
func Execute(version string) int {
	root := newRootCmd(version)
	if err := root.Execute(); err != nil {
		var ee *exitcode.ExitError
		if errors.As(err, &ee) {
			if ee.Err != nil && ee.Code != 0 {
				_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", ee.Err)
			}
			return ee.Code
		}
		if errors.Is(err, context.DeadlineExceeded) {
			_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return exitcode.ExitTimedOut
		}
		// Treat other cobra/validation errors as invalid input
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitcode.ExitInvalidInput
	}
	return 0
}

// ------------------------------
// Root & shared options
// ------------------------------

/* Options type moved to `internal/cli/options.go` — use that shared type.
   Root/command wiring in this file consumes the exported `Options` type. */

func newRootCmd(version string) *cobra.Command {
	var globalOpts Options

	cmd := &cobra.Command{
		Use:           "kubetracker",
		Short:         "Track Kubernetes resources defined in manifests using concise output",
		Long:          "kubetracker tracks readiness of Kubernetes resources (including CRDs) from manifests and streams only failure-related pod logs.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Global flags
	cmd.PersistentFlags().StringVar(&globalOpts.Kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "Path to the kubeconfig file")
	cmd.PersistentFlags().StringVar(&globalOpts.KubeContext, "context", "", "Kubeconfig context to use")
	cmd.PersistentFlags().BoolVar(&globalOpts.Quiet, "quiet", false, "Suppress non-essential output")
	cmd.PersistentFlags().BoolVar(&globalOpts.PrintSummary, "summary", true, "Print a summary of resources to be tracked before starting")
	cmd.PersistentFlags().BoolVar(&globalOpts.ExitOnFirstFail, "exit-on-first-fail", false, "Exit immediately when a resource fails (useful in CI)")
	cmd.PersistentFlags().BoolVar(&globalOpts.Verbose, "verbose", false, "Enable verbose logging")
	cmd.PersistentFlags().DurationVar(&globalOpts.ProgressPrintInterval, "progress-print-interval", 0, "Interval between progress table re-renders (0 uses runner default)")
	// Kubernetes client rate-limit overrides (passed to client-go rest.Config)
	cmd.PersistentFlags().Float32Var(&globalOpts.KubeQPS, "kube-qps", 0, "Per-client QPS for Kubernetes client (0 uses client-go default)")
	cmd.PersistentFlags().IntVar(&globalOpts.KubeBurst, "kube-burst", 0, "Burst for Kubernetes client (0 uses client-go default)")

	// Subcommands
	cmd.AddCommand(newVersionCmd(version))
	cmd.AddCommand(newTrackCmd(&globalOpts, version))
	cmd.AddCommand(newHelmCmd(&globalOpts, version))

	return cmd
}

func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Printf("kubetracker %s\n", version)
		},
	}
}

// ------------------------------
// track subcommand
// ------------------------------

func newTrackCmd(global *Options, version string) *cobra.Command {
	var opts Options

	cmd := &cobra.Command{
		Use:   "track",
		Short: "Track Kubernetes resources from manifests",
		Long:  "Track Kubernetes resources from YAML manifests: files, directories, or stdin (-). Only failure-related pod logs are printed.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// merge global into local
			opts.ApplyGlobal(global)

			if len(opts.Files) == 0 {
				opts.Files = []string{"-"}
			}
			if opts.Timeout < 0 {
				return exitcode.InvalidInput(errors.New("--timeout must be >= 0"))
			}

			ctx := cmd.Context()
			if opts.Timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
				defer cancel()
			}

			// Load manifests
			if opts.Verbose {
				cmd.Printf("Reading manifests from %d path(s): %s (recursive=%v, defaultNamespace=%q)\n", len(opts.Files), strings.Join(opts.Files, ", "), opts.Recursive, opts.Namespace)
			}
			objects, readWarnings, err := render.Load(opts.Files, render.Options{
				DefaultNamespace: opts.Namespace,
				Recursive:        opts.Recursive,
				Stdin:            os.Stdin,
			})
			if err != nil {
				return exitcode.InvalidInput(fmt.Errorf("load manifests: %w", err))
			}
			for _, w := range readWarnings {
				if !opts.Quiet {
					cmd.Printf("warn: %s\n", w)
				}
			}
			if opts.Verbose {
				cmd.Printf("Loaded %d object(s) from manifests\n", len(objects))
			}

			objects = objutil.FilterObjectsByKind(objects, opts.IncludeKinds, opts.ExcludeKinds)
			if opts.Verbose {
				cmd.Printf("Tracking %d object(s) after filtering\n", len(objects))
			}
			if len(objects) == 0 {
				return exitcode.InvalidInput(errors.New("no Kubernetes objects found after filtering"))
			}

			if opts.PrintSummary && !opts.Quiet {
				objutil.PrintObjectsSummary(cmd, objects)
			}

			crdRules, crdRuleErrs := objutil.ParseCRDConditions(opts.CRDConditions)
			for _, e := range crdRuleErrs {
				cmd.Printf("warn: %s\n", e.Error())
			}
			if opts.Verbose {
				cmd.Printf("Parsed %d CRD rule(s), %d error(s)\n", len(crdRules), len(crdRuleErrs))
			}

			// Build kube clients and start tracker runner
			if opts.Verbose {
				cmd.Printf("Initializing Kubernetes clients (context=%q, kubeconfig=%q)\n", opts.KubeContext, opts.Kubeconfig)
			}
			factory, err := kube.NewFactory(ctx, kube.Options{
				Kubeconfig:            opts.Kubeconfig,
				Context:               opts.KubeContext,
				QPS:                   opts.KubeQPS,
				Burst:                 opts.KubeBurst,
				UserAgent:             fmt.Sprintf("kubetracker/%s", version),
				InsecureSkipTLSVerify: false,
			})
			if err != nil {
				return exitcode.InvalidInput(fmt.Errorf("kube factory: %w", err))
			}
			if opts.Verbose {
				cmd.Printf("Kubernetes clients initialized\n")
			}

			// Convert CRD rules to track runner format
			var rules []track.CRDConditionRule
			for _, r := range crdRules {
				rules = append(rules, track.CRDConditionRule{
					Group:     r.Group,
					Version:   r.Version,
					Kind:      r.Kind,
					Condition: r.Condition,
					Expected:  r.Expected,
				})
			}

			runner := track.NewRunner(factory, track.RunnerOptions{
				DefaultNamespace:      opts.Namespace,
				ShowEvents:            opts.ShowEvents,
				OnlyErrorLogs:         opts.OnlyErrorsLogs,
				Verbose:               opts.Verbose,
				ProgressPrintInterval: opts.ProgressPrintInterval,
				Timeout:               opts.Timeout,
				FailureGracePeriod:    opts.FailureGracePeriod,
				ExitOnFirstFail:       opts.ExitOnFirstFail,
				CRDRules:              rules,
			})

			if opts.PrintSummary && !opts.Quiet {
				cmd.Println("Starting tracking...")
				objutil.PrintObjectsSummary(cmd, objects)
			}

			if err := runner.Run(ctx, objects); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return exitcode.Timeout(err)
				}
				return exitcode.New(exitcode.ExitFailed, err)
			}
			if opts.PrintSummary && !opts.Quiet {
				cmd.Println("Tracking completed. Final summary:")
				objutil.PrintObjectsSummary(cmd, objects)
			}
			return nil
		},
	}

	// Flags
	cmd.Flags().StringSliceVarP(&opts.Files, "file", "f", nil, "Manifest file/dir path or '-' for stdin. Can be specified multiple times")
	cmd.Flags().BoolVarP(&opts.Recursive, "recursive", "R", true, "Recursively traverse directories when reading manifests")
	cmd.Flags().StringVarP(&opts.Namespace, "namespace", "n", "", "Default namespace for namespaceless resources")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 0, "Global tracking timeout (e.g. 10m, 1h). 0 means no timeout")
	cmd.Flags().DurationVar(&opts.FailureGracePeriod, "failure-grace-period", 0, "Grace period to keep tracking and retry after a failure (e.g. 2m). 0 disables retry")
	cmd.Flags().DurationVar(&opts.ProgressPrintInterval, "progress-print-interval", 0, "Interval between progress table re-renders (e.g. 15s). 0 uses runner default")

	cmd.Flags().StringSliceVar(&opts.IncludeKinds, "include-kind", nil, "Only track specified kinds (repeatable), e.g. Deployment, KafkaTopic")
	cmd.Flags().StringSliceVar(&opts.ExcludeKinds, "exclude-kind", nil, "Exclude specified kinds (repeatable), e.g. Job")
	cmd.Flags().StringArrayVar(&opts.CRDConditions, "crd-condition", nil, "CRD readiness mapping: group/version,Kind=ConditionType=ExpectedValue (repeatable)")

	cmd.Flags().BoolVar(&opts.ShowEvents, "show-events", true, "Show Kubernetes events")
	cmd.Flags().BoolVar(&opts.OnlyErrorsLogs, "only-errors-logs", true, "Only stream logs for failing containers")
	cmd.Flags().BoolVar(&opts.Verbose, "verbose", global.Verbose, "Enable verbose logging")
	cmd.Flags().BoolVar(&opts.PrintSummary, "summary", global.PrintSummary, "Print a summary of resources to be tracked before starting")
	cmd.Flags().BoolVar(&opts.ExitOnFirstFail, "exit-on-first-fail", global.ExitOnFirstFail, "Exit immediately when a resource fails (useful in CI)")

	// kube flags on subcommand too, for convenience
	cmd.Flags().StringVar(&opts.Kubeconfig, "kubeconfig", global.Kubeconfig, "Path to the kubeconfig file")
	cmd.Flags().StringVar(&opts.KubeContext, "context", global.KubeContext, "Kubeconfig context to use")
	cmd.Flags().Float32Var(&opts.KubeQPS, "kube-qps", global.KubeQPS, "Per-client QPS for Kubernetes client (0 uses client-go default)")
	cmd.Flags().IntVar(&opts.KubeBurst, "kube-burst", global.KubeBurst, "Burst for Kubernetes client (0 uses client-go default)")

	return cmd
}

// ------------------------------
// helm subcommand
// ------------------------------

func newHelmCmd(global *Options, version string) *cobra.Command {
	var opts Options

	cmd := &cobra.Command{
		Use:   "helm [RELEASE...]", // RELEASE can be <ns>/<name> or <name> with -n
		Short: "Track Kubernetes resources rendered by one or more Helm releases",
		Long: "Track Kubernetes resources for Helm releases by reading the latest Helm Secret for each release and reusing kubetracker's tracker. " +
			"Each RELEASE can be specified as <namespace>/<name> or just <name> with --namespace set.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// merge global into local
			opts.ApplyGlobal(global)

			if opts.Timeout < 0 {
				return exitcode.InvalidInput(errors.New("--timeout must be >= 0"))
			}

			if opts.Verbose {
				cmd.Printf("Preparing to track %d Helm release(s): %s\n", len(args), strings.Join(args, ", "))
			}

			// Parse release refs
			refs, err := helm.ParseReleaseArgs(args, opts.Namespace)
			if err != nil {
				return exitcode.InvalidInput(fmt.Errorf("parse releases: %w", err))
			}

			// Build context with optional timeout
			ctx := cmd.Context()
			if opts.Timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
				defer cancel()
			}

			// Kube clients
			if opts.Verbose {
				cmd.Printf("Initializing Kubernetes clients (context=%q, kubeconfig=%q)\n", opts.KubeContext, opts.Kubeconfig)
			}
			factory, err := kube.NewFactory(ctx, kube.Options{
				Kubeconfig:            opts.Kubeconfig,
				Context:               opts.KubeContext,
				QPS:                   opts.KubeQPS,
				Burst:                 opts.KubeBurst,
				UserAgent:             fmt.Sprintf("kubetracker/%s", version),
				InsecureSkipTLSVerify: false,
			})
			if err != nil {
				return exitcode.InvalidInput(fmt.Errorf("kube factory: %w", err))
			}
			if opts.Verbose {
				cmd.Printf("Kubernetes clients initialized\n")
			}

			// Load objects from Helm release secrets
			var objects []*unstructured.Unstructured
			var warnings []string
			for _, r := range refs {
				if opts.Verbose {
					cmd.Printf("Reading latest Helm secret for release %s/%s\n", r.Namespace, r.Name)
				}
				manifest, warn, err := helm.GetLatestReleaseManifest(ctx, factory, r.Namespace, r.Name, opts.Verbose)
				if warn != "" {
					warnings = append(warnings, warn)
				}
				if err != nil {
					return exitcode.InvalidInput(fmt.Errorf("release %s/%s: %w", r.Namespace, r.Name, err))
				}
				if opts.Verbose {
					cmd.Printf("Decoding manifest for release %s/%s\n", r.Namespace, r.Name)
				}
				docs, w, err := render.Load([]string{"-"}, render.Options{
					DefaultNamespace: "",
					Recursive:        false,
					Stdin:            strings.NewReader(manifest),
				})
				if err != nil {
					return exitcode.InvalidInput(fmt.Errorf("decode manifest for %s/%s: %w", r.Namespace, r.Name, err))
				}
				objects = append(objects, docs...)
				warnings = append(warnings, w...)
			}

			objects = objutil.FilterObjectsByKind(objects, opts.IncludeKinds, opts.ExcludeKinds)

			for _, w := range warnings {
				if !opts.Quiet {
					cmd.Printf("warn: %s\n", w)
				}
			}
			if len(objects) == 0 {
				return exitcode.InvalidInput(errors.New("no Kubernetes objects found in Helm releases after filtering"))
			}

			if opts.PrintSummary && !opts.Quiet {
				objutil.PrintObjectsSummary(cmd, objects)
			}

			crdRules, crdRuleErrs := objutil.ParseCRDConditions(opts.CRDConditions)
			for _, e := range crdRuleErrs {
				cmd.Printf("warn: %s\n", e.Error())
			}
			if opts.Verbose {
				cmd.Printf("Parsed %d CRD rule(s), %d error(s)\n", len(crdRules), len(crdRuleErrs))
			}

			// Convert CRD rules to track runner format
			var rules []track.CRDConditionRule
			for _, r := range crdRules {
				rules = append(rules, track.CRDConditionRule{
					Group:     r.Group,
					Version:   r.Version,
					Kind:      r.Kind,
					Condition: r.Condition,
					Expected:  r.Expected,
				})
			}

			runner := track.NewRunner(factory, track.RunnerOptions{
				DefaultNamespace:      opts.Namespace,
				ShowEvents:            opts.ShowEvents,
				OnlyErrorLogs:         opts.OnlyErrorsLogs,
				Verbose:               opts.Verbose,
				ProgressPrintInterval: opts.ProgressPrintInterval,
				Timeout:               opts.Timeout,
				FailureGracePeriod:    opts.FailureGracePeriod,
				ExitOnFirstFail:       opts.ExitOnFirstFail,
				CRDRules:              rules,
			})

			if opts.PrintSummary && !opts.Quiet {
				cmd.Println("Starting tracking...")
				objutil.PrintObjectsSummary(cmd, objects)
			}
			if err := runner.Run(ctx, objects); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return exitcode.Timeout(err)
				}
				return exitcode.New(exitcode.ExitFailed, err)
			}
			if opts.PrintSummary && !opts.Quiet {
				cmd.Println("Tracking completed. Final summary:")
				objutil.PrintObjectsSummary(cmd, objects)
			}
			return nil
		},
	}

	// Reuse most flags from "track"
	cmd.Flags().StringVarP(&opts.Namespace, "namespace", "n", "", "Namespace to resolve names when RELEASE is specified as <name>")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 0, "Global tracking timeout (e.g. 10m, 1h). 0 means no timeout")
	cmd.Flags().DurationVar(&opts.FailureGracePeriod, "failure-grace-period", 0, "Grace period to keep tracking and retry after a failure (e.g. 2m). 0 disables retry")
	cmd.Flags().DurationVar(&opts.ProgressPrintInterval, "progress-print-interval", 0, "Interval between progress table re-renders (e.g. 15s). 0 uses runner default")
	cmd.Flags().StringSliceVar(&opts.IncludeKinds, "include-kind", nil, "Only track specified kinds (repeatable), e.g. Deployment, KafkaTopic")
	cmd.Flags().StringSliceVar(&opts.ExcludeKinds, "exclude-kind", nil, "Exclude specified kinds (repeatable), e.g. Job")
	cmd.Flags().StringArrayVar(&opts.CRDConditions, "crd-condition", nil, "CRD readiness mapping: group/version,Kind=ConditionType=ExpectedValue (repeatable)")
	cmd.Flags().BoolVar(&opts.ShowEvents, "show-events", true, "Show Kubernetes events")
	cmd.Flags().BoolVar(&opts.OnlyErrorsLogs, "only-errors-logs", true, "Only stream logs for failing containers")
	cmd.Flags().BoolVar(&opts.Verbose, "verbose", global.Verbose, "Enable verbose logging")
	cmd.Flags().BoolVar(&opts.PrintSummary, "summary", global.PrintSummary, "Print a summary of resources to be tracked before starting")
	cmd.Flags().BoolVar(&opts.ExitOnFirstFail, "exit-on-first-fail", global.ExitOnFirstFail, "Exit immediately when a resource fails (useful in CI)")

	// kube flags on subcommand too, for convenience
	cmd.Flags().StringVar(&opts.Kubeconfig, "kubeconfig", global.Kubeconfig, "Path to the kubeconfig file")
	cmd.Flags().StringVar(&opts.KubeContext, "context", global.KubeContext, "Kubeconfig context to use")

	return cmd
}
