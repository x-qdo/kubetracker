package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/x-qdo/kubetracker/internal/kube"
	"github.com/x-qdo/kubetracker/internal/track"
	"github.com/x-qdo/kubetracker/pkg/exitcode"
)

var (
	// version is injected via -ldflags, defaults to "dev".
	version = "dev"
)

type options struct {
	files              []string
	namespace          string
	kubeContext        string
	kubeconfig         string
	timeout            time.Duration
	failureGracePeriod time.Duration
	includeKinds       []string
	excludeKinds       []string
	crdConditions      []string
	showEvents         bool
	onlyErrorsLogs     bool
	recursive          bool
	quiet              bool
	verbose            bool
	printSummary       bool
	exitOnFirstFail    bool
}

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		var ee *exitcode.ExitError
		if errors.As(err, &ee) {
			if ee.Err != nil && ee.Code != 0 {
				_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", ee.Err)
			}
			os.Exit(ee.Code)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(exitcode.ExitTimedOut)
		}
		// Treat other cobra/validation errors as invalid input
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(exitcode.ExitInvalidInput)
	}
}

func newRootCmd() *cobra.Command {
	var (
		globalOpts options
	)

	cmd := &cobra.Command{
		Use:           "kubetracker",
		Short:         "Track Kubernetes resources defined in manifests using concise output",
		Long:          "kubetracker tracks readiness of Kubernetes resources (including CRDs) from manifests and streams only failure-related pod logs.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&globalOpts.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "Path to the kubeconfig file")
	cmd.PersistentFlags().StringVar(&globalOpts.kubeContext, "context", "", "Kubeconfig context to use")
	cmd.PersistentFlags().BoolVar(&globalOpts.quiet, "quiet", false, "Suppress non-essential output")
	cmd.PersistentFlags().BoolVar(&globalOpts.printSummary, "summary", true, "Print a summary of resources to be tracked before starting")
	cmd.PersistentFlags().BoolVar(&globalOpts.exitOnFirstFail, "exit-on-first-fail", false, "Exit immediately when a resource fails (useful in CI)")

	cmd.AddCommand(newVersionCmd())

	cmd.AddCommand(newTrackCmd(&globalOpts))

	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Printf("kubetracker %s\n", version)
		},
	}
}

func newTrackCmd(global *options) *cobra.Command {
	var (
		opts options
	)

	cmd := &cobra.Command{
		Use:   "track",
		Short: "Track Kubernetes resources from manifests",
		Long:  "Track Kubernetes resources from YAML manifests: files, directories, or stdin (-). Only failure-related pod logs are printed.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// merge global into local
			opts.kubeconfig = firstNonEmpty(opts.kubeconfig, global.kubeconfig)
			opts.kubeContext = firstNonEmpty(opts.kubeContext, global.kubeContext)
			opts.quiet = opts.quiet || global.quiet
			opts.verbose = opts.verbose || global.verbose
			opts.printSummary = opts.printSummary && global.printSummary
			opts.exitOnFirstFail = opts.exitOnFirstFail || global.exitOnFirstFail

			return runTrack(cmd, &opts)
		},
	}

	cmd.Flags().StringSliceVarP(&opts.files, "file", "f", nil, "Manifest file/dir path or '-' for stdin. Can be specified multiple times")
	cmd.Flags().BoolVarP(&opts.recursive, "recursive", "R", true, "Recursively traverse directories when reading manifests")
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "", "Default namespace for namespaceless resources")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 0, "Global tracking timeout (e.g. 10m, 1h). 0 means no timeout")
	cmd.Flags().DurationVar(&opts.failureGracePeriod, "failure-grace-period", 0, "Grace period to keep tracking and retry after a failure (e.g. 2m). 0 disables retry")

	cmd.Flags().StringSliceVar(&opts.includeKinds, "include-kind", nil, "Only track specified kinds (repeatable), e.g. Deployment, KafkaTopic")
	cmd.Flags().StringSliceVar(&opts.excludeKinds, "exclude-kind", nil, "Exclude specified kinds (repeatable), e.g. Job")
	cmd.Flags().StringSliceVar(&opts.crdConditions, "crd-condition", nil, "CRD readiness mapping: group/version,Kind=ConditionType=ExpectedValue (repeatable)")

	cmd.Flags().BoolVar(&opts.showEvents, "show-events", true, "Show Kubernetes events")
	cmd.Flags().BoolVar(&opts.onlyErrorsLogs, "only-errors-logs", true, "Only stream logs for failing containers")
	cmd.Flags().BoolVar(&opts.verbose, "verbose", global.verbose, "Enable verbose logging")

	// kube flags on subcommand too, for convenience
	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", global.kubeconfig, "Path to the kubeconfig file")
	cmd.Flags().StringVar(&opts.kubeContext, "context", global.kubeContext, "Kubeconfig context to use")

	return cmd
}

func runTrack(cmd *cobra.Command, opts *options) error {
	if len(opts.files) == 0 {
		// default to stdin when no files provided
		opts.files = []string{"-"}
	}

	if opts.timeout < 0 {
		return exitcode.InvalidInput(errors.New("--timeout must be >= 0"))
	}

	ctx := cmd.Context()
	if opts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}

	if opts.verbose {
		cmd.Printf("Reading manifests from %d path(s): %s (recursive=%v, defaultNamespace=%q)\n", len(opts.files), strings.Join(opts.files, ", "), opts.recursive, opts.namespace)
	}
	objects, readWarnings, err := loadManifests(opts.files, opts.namespace, opts.recursive, opts.verbose)
	if err != nil {
		return exitcode.InvalidInput(fmt.Errorf("load manifests: %w", err))
	}
	for _, w := range readWarnings {
		if !opts.quiet {
			cmd.Printf("warn: %s\n", w)
		}
	}
	if opts.verbose {
		cmd.Printf("Loaded %d object(s) from manifests\n", len(objects))
	}
	objects = filterObjectsByKind(objects, opts.includeKinds, opts.excludeKinds)

	if opts.verbose {
		cmd.Printf("Tracking %d object(s) after filtering\n", len(objects))
	}
	if len(objects) == 0 {
		return exitcode.InvalidInput(errors.New("no Kubernetes objects found after filtering"))
	}

	if opts.printSummary && !opts.quiet {
		printObjectsSummary(cmd, objects)
	}

	crdRules, crdRuleErrs := parseCRDConditions(opts.crdConditions)
	for _, e := range crdRuleErrs {
		cmd.Printf("warn: %s\n", e.Error())
	}
	if opts.verbose {
		cmd.Printf("Parsed %d CRD rule(s), %d error(s)\n", len(crdRules), len(crdRuleErrs))
	}
	// Build kube clients and start tracker runner
	if opts.verbose {
		cmd.Printf("Initializing Kubernetes clients (context=%q, kubeconfig=%q)\n", opts.kubeContext, opts.kubeconfig)
	}
	factory, err := kube.NewFactory(ctx, kube.Options{
		Kubeconfig:            opts.kubeconfig,
		Context:               opts.kubeContext,
		UserAgent:             fmt.Sprintf("kubetracker/%s", version),
		InsecureSkipTLSVerify: false,
	})
	if err != nil {
		return exitcode.InvalidInput(fmt.Errorf("kube factory: %w", err))
	}
	if opts.verbose {
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
		DefaultNamespace:      opts.namespace,
		ShowEvents:            opts.showEvents,
		OnlyErrorLogs:         opts.onlyErrorsLogs,
		Verbose:               opts.verbose,
		ProgressPrintInterval: 2 * time.Second,
		Timeout:               opts.timeout,
		FailureGracePeriod:    opts.failureGracePeriod,
		ExitOnFirstFail:       opts.exitOnFirstFail,
		CRDRules:              rules,
	})
	if opts.printSummary && !opts.quiet {
		cmd.Println("Starting tracking...")
		printObjectsSummary(cmd, objects)
	}

	if err := runner.Run(ctx, objects); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return exitcode.Timeout(err)
		}
		return exitcode.New(exitcode.ExitFailed, err)
	}
	if opts.printSummary && !opts.quiet {
		cmd.Println("Tracking completed. Final summary:")
		printObjectsSummary(cmd, objects)
	}
	return nil
}

// ------------------------------
// Manifest loading & filtering
// ------------------------------

func loadManifests(paths []string, defaultNamespace string, recursive bool, verbose bool) (out []*unstructured.Unstructured, warnings []string, _ error) {
	seen := make(map[string]struct{})

	if verbose {
		fmt.Fprintf(os.Stderr, "loader: starting with %d path(s), recursive=%v, defaultNamespace=%q\n", len(paths), recursive, defaultNamespace)
	}

	for _, p := range paths {
		if verbose {
			fmt.Fprintf(os.Stderr, "loader: processing path: %s\n", p)
		}

		if p == "-" {
			if verbose {
				fmt.Fprintln(os.Stderr, "loader: reading from stdin")
			}
			docs, w, err := decodeStream("stdin", os.Stdin, defaultNamespace, verbose)
			if err != nil {
				return nil, warnings, err
			}
			if verbose {
				fmt.Fprintf(os.Stderr, "loader: decoded %d object(s) from stdin\n", len(docs))
			}
			out = append(out, docs...)
			warnings = append(warnings, w...)
			continue
		}

		info, err := os.Stat(p)
		if err != nil {
			return nil, warnings, fmt.Errorf("stat %q: %w", p, err)
		}

		if info.IsDir() {
			if verbose {
				fmt.Fprintf(os.Stderr, "loader: walking directory %s (recursive=%v)\n", p, recursive)
			}
			walkFn := func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					if verbose {
						fmt.Fprintf(os.Stderr, "loader: error while walking %s: %v\n", path, err)
					}
					return err
				}
				if d.IsDir() {
					if path != p && !recursive {
						if verbose {
							fmt.Fprintf(os.Stderr, "loader: skipping subdir %s (recursive disabled)\n", path)
						}
						return fs.SkipDir
					}
					return nil
				}
				if !isYAMLLike(d.Name()) {
					if verbose {
						fmt.Fprintf(os.Stderr, "loader: skipping non-YAML file: %s\n", path)
					}
					return nil
				}

				abs, _ := filepath.Abs(path)
				if _, ok := seen[abs]; ok {
					if verbose {
						fmt.Fprintf(os.Stderr, "loader: already processed file: %s\n", path)
					}
					return nil
				}
				seen[abs] = struct{}{}

				if verbose {
					fmt.Fprintf(os.Stderr, "loader: decoding file %s\n", path)
				}
				f, err := os.Open(path)
				if err != nil {
					return fmt.Errorf("open %q: %w", path, err)
				}
				defer f.Close()

				docs, w, err := decodeStream(path, f, defaultNamespace, verbose)
				if err != nil {
					return err
				}
				if verbose {
					fmt.Fprintf(os.Stderr, "loader: decoded %d object(s) from file %s\n", len(docs), path)
				}
				out = append(out, docs...)
				warnings = append(warnings, w...)
				return nil
			}
			if err := filepath.WalkDir(p, walkFn); err != nil {
				return nil, warnings, err
			}
			continue
		}

		// single file
		if !isYAMLLike(info.Name()) {
			if verbose {
				fmt.Fprintf(os.Stderr, "loader: skipping non-YAML file: %s\n", p)
			}
			warnings = append(warnings, fmt.Sprintf("skipping non-YAML file: %s", p))
			continue
		}
		abs, _ := filepath.Abs(p)
		if _, ok := seen[abs]; ok {
			if verbose {
				fmt.Fprintf(os.Stderr, "loader: already processed file: %s\n", p)
			}
			continue
		}
		seen[abs] = struct{}{}

		if verbose {
			fmt.Fprintf(os.Stderr, "loader: decoding file %s\n", p)
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, warnings, fmt.Errorf("open %q: %w", p, err)
		}
		func() {
			defer f.Close()
			docs, w, err := decodeStream(p, f, defaultNamespace, verbose)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("failed decoding %s: %v", p, err))
				if verbose {
					fmt.Fprintf(os.Stderr, "loader: failed decoding %s: %v\n", p, err)
				}
				return
			}
			if verbose {
				fmt.Fprintf(os.Stderr, "loader: decoded %d object(s) from file %s\n", len(docs), p)
			}
			out = append(out, docs...)
			warnings = append(warnings, w...)
		}()
	}

	// Expand "List" objects into items
	out = expandLists(out)

	// Drop empty/unrecognized entries
	clean := make([]*unstructured.Unstructured, 0, len(out))
	for _, u := range out {
		if strings.TrimSpace(u.GetKind()) == "" || strings.TrimSpace(u.GetAPIVersion()) == "" {
			continue
		}
		clean = append(clean, u)
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "loader: total objects after cleaning=%d\n", len(clean))
	}

	return clean, warnings, nil
}

func decodeStream(source string, r io.Reader, defaultNamespace string, verbose bool) ([]*unstructured.Unstructured, []string, error) {
	var warnings []string

	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	var out []*unstructured.Unstructured

	for {
		var raw map[string]any
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Some decoders return nil maps for separators; tolerate and continue
			if err.Error() == "EOF" && len(raw) == 0 {
				break
			}
			return nil, warnings, fmt.Errorf("decode %s: %w", source, err)
		}
		if len(raw) == 0 {
			if verbose {
				fmt.Fprintf(os.Stderr, "loader: %s: empty document (separator)\n", source)
			}
			continue
		}

		u := &unstructured.Unstructured{Object: raw}

		// Best-effort default namespace for namespaceless resources (skip known cluster-scoped kinds)
		if u.GetNamespace() == "" && defaultNamespace != "" && !isClusterScoped(u) {
			if verbose {
				fmt.Fprintf(os.Stderr, "loader: %s: injecting namespace %q for %s/%s %s\n", source, defaultNamespace, u.GetAPIVersion(), u.GetKind(), u.GetName())
			}
			u.SetNamespace(defaultNamespace)
		}

		if verbose {
			ns := u.GetNamespace()
			if ns == "" {
				ns = "<cluster>"
			}
			fmt.Fprintf(os.Stderr, "loader: %s: decoded %s %s/%s (apiVersion=%s)\n", source, u.GetKind(), ns, u.GetName(), u.GetAPIVersion())
		}

		out = append(out, u)
	}

	return out, warnings, nil
}

func isYAMLLike(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".yaml") || strings.HasSuffix(l, ".yml") || strings.HasSuffix(l, ".json")
}

func expandLists(list []*unstructured.Unstructured) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, u := range list {
		if strings.EqualFold(u.GetKind(), "List") {
			items, found, _ := unstructured.NestedSlice(u.Object, "items")
			if !found {
				continue
			}
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					out = append(out, &unstructured.Unstructured{Object: m})
				}
			}
			continue
		}
		out = append(out, u)
	}
	return out
}

func isClusterScoped(u *unstructured.Unstructured) bool {
	// Best-effort list of cluster-scoped kinds; not exhaustive.
	// We avoid forcibly setting a namespace for these.
	gvk := objectGVK(u)
	k := gvk.Kind
	g := gvk.Group

	switch k {
	case "Namespace", "Node", "PersistentVolume", "StorageClass", "CustomResourceDefinition",
		"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration", "ClusterRole", "ClusterRoleBinding",
		"PriorityClass", "APIService", "ClusterIssuer":
		return true
	}

	// OpenShift and others (best effort)
	if g == "apiextensions.k8s.io" || g == "admissionregistration.k8s.io" || g == "rbac.authorization.k8s.io" {
		if strings.HasPrefix(k, "Cluster") {
			return true
		}
	}

	return false
}

func objectGVK(u *unstructured.Unstructured) schema.GroupVersionKind {
	gv, err := schema.ParseGroupVersion(u.GetAPIVersion())
	if err != nil {
		return schema.GroupVersionKind{Group: "", Version: u.GetAPIVersion(), Kind: u.GetKind()}
	}
	return gv.WithKind(u.GetKind())
}

func filterObjectsByKind(objs []*unstructured.Unstructured, includeKinds, excludeKinds []string) []*unstructured.Unstructured {
	normalize := func(ss []string) []string {
		out := make([]string, 0, len(ss))
		for _, s := range ss {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			out = append(out, strings.ToLower(s))
		}
		return out
	}
	inc := normalize(includeKinds)
	exc := normalize(excludeKinds)
	incSet := make(map[string]struct{}, len(inc))
	excSet := make(map[string]struct{}, len(exc))
	for _, k := range inc {
		incSet[k] = struct{}{}
	}
	for _, k := range exc {
		excSet[k] = struct{}{}
	}

	matches := func(u *unstructured.Unstructured, set map[string]struct{}) bool {
		if len(set) == 0 {
			return false
		}
		gvk := objectGVK(u)
		kind := strings.ToLower(gvk.Kind)
		group := strings.ToLower(gvk.Group)
		groupKind := group + "/" + kind
		_, ok1 := set[kind]
		_, ok2 := set[groupKind]
		return ok1 || ok2
	}

	var filtered []*unstructured.Unstructured
	for _, u := range objs {
		// include filter (if present)
		if len(incSet) > 0 && !matches(u, incSet) {
			continue
		}
		// exclude filter
		if matches(u, excSet) {
			continue
		}
		filtered = append(filtered, u)
	}
	return filtered
}

func printObjectsSummary(cmd *cobra.Command, objs []*unstructured.Unstructured) {
	type key struct {
		Group string
		Kind  string
	}
	counts := make(map[key]int)
	nsSet := make(map[string]struct{})

	for _, u := range objs {
		gvk := objectGVK(u)
		counts[key{Group: gvk.Group, Kind: gvk.Kind}]++
		if ns := u.GetNamespace(); ns != "" {
			nsSet[ns] = struct{}{}
		}
	}

	var keys []key
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Group == keys[j].Group {
			return keys[i].Kind < keys[j].Kind
		}
		return keys[i].Group < keys[j].Group
	})

	cmd.Println("Summary of resources to track:")
	for _, k := range keys {
		g := k.Group
		if g == "" {
			g = "core"
		}
		cmd.Printf("  - %s/%s: %d\n", g, k.Kind, counts[k])
	}
	if len(nsSet) > 0 {
		var nss []string
		for ns := range nsSet {
			nss = append(nss, ns)
		}
		sort.Strings(nss)
		cmd.Printf("Namespaces: %s\n", strings.Join(nss, ", "))
	}
}

// ------------------------------
// CRD condition parsing
// ------------------------------

type crdRule struct {
	Group     string
	Version   string
	Kind      string
	Condition string
	Expected  string
}

func parseCRDConditions(rules []string) ([]crdRule, []error) {
	var out []crdRule
	var errs []error

	for _, r := range rules {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		// Format: group/version,Kind=ConditionType=ExpectedValue
		parts := strings.SplitN(r, "=", 3)
		if len(parts) != 3 {
			errs = append(errs, fmt.Errorf("invalid --crd-condition %q, expected group/version,Kind=ConditionType=ExpectedValue", r))
			continue
		}
		left := parts[0]
		cond := strings.TrimSpace(parts[1])
		exp := strings.TrimSpace(parts[2])

		gvk := strings.SplitN(left, ",", 2)
		if len(gvk) != 2 {
			errs = append(errs, fmt.Errorf("invalid GVK section %q in %q, expected group/version,Kind", left, r))
			continue
		}
		gv := strings.TrimSpace(gvk[0])
		kind := strings.TrimSpace(gvk[1])
		parsedGV, err := schema.ParseGroupVersion(gv)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid group/version %q: %v", gv, err))
			continue
		}
		if cond == "" || exp == "" || kind == "" {
			errs = append(errs, fmt.Errorf("invalid condition mapping %q: empty fields", r))
			continue
		}

		out = append(out, crdRule{
			Group:     parsedGV.Group,
			Version:   parsedGV.Version,
			Kind:      kind,
			Condition: cond,
			Expected:  exp,
		})
	}

	return out, errs
}

// ------------------------------
// Helpers
// ------------------------------

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
