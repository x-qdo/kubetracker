package track

// Package track contains the tracking runner that wires kubedog dynamic tracker
// state stores (tasks, logs) together with CLI options, CRD readiness rules,
// and table printers.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samber/lo"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kddyntracker "github.com/werf/kubedog/pkg/trackers/dyntracker"
	kdlogstore "github.com/werf/kubedog/pkg/trackers/dyntracker/logstore"
	kdstatestore "github.com/werf/kubedog/pkg/trackers/dyntracker/statestore"
	kdutil "github.com/werf/kubedog/pkg/trackers/dyntracker/util"

	"github.com/x-qdo/kubetracker/internal/kube"
	objutil "github.com/x-qdo/kubetracker/internal/objects"
	"github.com/x-qdo/kubetracker/pkg/printer"
	"k8s.io/client-go/util/flowcontrol"
)

const podStatusMinRefresh = 10 * time.Second

// CRDConditionRule defines a readiness rule for a CRD based on status.conditions.
// Example: external-secrets.io/v1beta1,ExternalSecret -> Condition: Ready, Expected: True
type CRDConditionRule struct {
	Group     string
	Version   string
	Kind      string
	Condition string
	Expected  string
}

// RunnerOptions controls tracking behavior.
type RunnerOptions struct {
	// DefaultNamespace is used to fill in missing namespaces when the object is namespaced.
	DefaultNamespace string

	// ShowEvents controls whether to render K8s event tables per resource.
	ShowEvents bool

	// OnlyErrorLogs controls whether to suppress healthy pod logs and print logs only
	// when pods fail to provision (CrashLoopBackOff, ImagePullBackOff, OOMKilled, etc.).
	OnlyErrorLogs bool

	// Verbose enables debug logs to stdout.
	Verbose bool

	// ProgressPrintInterval controls how often progress/events/log tables are re-rendered.
	ProgressPrintInterval time.Duration

	// Global timeout for tracking. If 0, no global deadline is enforced here
	// (callers can pass a context with deadline).
	Timeout time.Duration

	// FailureGracePeriod defines how long to keep tracking after a builtin tracker reports a failure,
	// allowing transient errors (e.g., ImagePullBackOff) to recover. A second attempt will be made
	// within this period before marking the resource as failed.
	FailureGracePeriod time.Duration

	// ExitOnFirstFail makes the runner stop early as soon as a tracked resource fails.
	ExitOnFirstFail bool

	// CRDRules defines custom readiness rules for CRDs.
	CRDRules []CRDConditionRule
}

// Runner is the entry point for resource tracking. It is designed to be long-lived
// for a single tracking session. Create a new Runner for each invocation.
type Runner struct {
	factory   *kube.Factory
	opts      RunnerOptions
	taskStore *kdstatestore.TaskStore
	logStore  *kdutil.Concurrent[*kdlogstore.LogStore]

	// throttle pod status GETs during enrichment
	podStatusLast map[string]time.Time

	// shared request rate limiter for polling
	limiter flowcontrol.RateLimiter

	// rule index for quick lookups
	crdRulesIndex map[gvkKey]CRDConditionRule
}

// NewRunner constructs a new Runner. It does not start any background goroutines.
func NewRunner(factory *kube.Factory, opts RunnerOptions) *Runner {
	if opts.ProgressPrintInterval <= 0 {
		opts.ProgressPrintInterval = 2 * time.Second
	}

	r := &Runner{
		factory: factory,
		opts:    opts,
		// kubedog state stores
		taskStore:     kdstatestore.NewTaskStore(),
		logStore:      kdutil.NewConcurrent(kdlogstore.NewLogStore()),
		podStatusLast: make(map[string]time.Time),
	}
	r.buildCRDRulesIndex()

	// build shared request rate limiter from rest.Config
	cfg := factory.Config()
	if cfg != nil {
		if cfg.RateLimiter != nil {
			r.limiter = cfg.RateLimiter
		} else {
			qps := cfg.QPS
			if qps <= 0 {
				qps = 5
			}
			burst := cfg.Burst
			if burst <= 0 {
				burst = 10
			}
			r.limiter = flowcontrol.NewTokenBucketRateLimiter(qps, burst)
		}
	}

	return r
}

// Run starts tracking for the provided objects and blocks until completion, failure,
// or context timeout/cancelled.
func (r *Runner) Run(ctx context.Context, objects []*unstructured.Unstructured) error {
	// Respect global timeout if specified on options, wrapping ctx provided by caller.
	if r.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.opts.Timeout)
		defer cancel()
	}

	r.vprintf("Runner starting with %d object(s); timeout=%s, showEvents=%v, onlyErrorLogs=%v",
		len(objects), r.opts.Timeout, r.opts.ShowEvents, r.opts.OnlyErrorLogs)

	// Prepare tracking specs from objects.
	specs := r.buildResourceSpecs(objects)
	r.vprintf("Built %d tracking spec(s)", len(specs))
	if len(specs) == 0 {
		return errors.New("no recognizable Kubernetes resources to track")
	}

	// Start dynamic trackers for workloads and CRD condition tasks and wait for completion.
	if err := r.startAndWaitTrackers(ctx, specs); err != nil {
		return err
	}
	// Explicit success marker to make it clear we finished successfully.
	fmt.Println("READY")
	r.vprintf("Runner completed")
	return nil
}

// startProgressPrinter starts a goroutine that periodically renders progress, events,
// and logs tables based on kubedog state stores. It returns a stop function.
func (r *Runner) startProgressPrinter(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)

	tb := printer.NewTablesBuilder(r.taskStore, r.logStore, printer.TablesBuilderOptions{
		DefaultNamespace: r.opts.DefaultNamespace,
		MaxTableWidth:    140, // a sensible default; can be made dynamic later
		OnlyErrorLogs:    r.opts.OnlyErrorLogs,
	})

	ticker := time.NewTicker(r.opts.ProgressPrintInterval)
	done := make(chan struct{})

	r.vprintf("Progress printer started (interval=%s)", r.opts.ProgressPrintInterval)

	go func() {
		defer close(done)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Enrich pod statuses before printing to avoid UNKNOWN without context.
				r.enrichPodStatuses(ctx)
				r.printTables(tb)
			}
		}
	}()

	return func() {
		cancel()
		<-done
		r.vprintf("Progress printer stopped")
	}
}

// printTables builds and prints tables for progress, events, and logs.
func (r *Runner) printTables(tb *printer.TablesBuilder) {
	// Render Progress
	if table, nonEmpty := tb.BuildProgressTable(); nonEmpty {
		fmt.Println(table.Render())
	}

	// Render Events
	if r.opts.ShowEvents {
		if tables, nonEmpty := tb.BuildEventTables(); nonEmpty {
			// deterministic order
			keys := lo.Keys(tables)
			sort.Strings(keys)
			for _, h := range keys {
				fmt.Println(h)
				fmt.Println(tables[h].Render())
			}
		}
	}

	// Render Logs (filtered if OnlyErrorLogs)
	if tables, nonEmpty := tb.BuildLogTables(); nonEmpty {
		keys := lo.Keys(tables)
		sort.Strings(keys)
		for _, h := range keys {
			fmt.Println(h)
			fmt.Println(tables[h].Render())
		}
	}
}

// Enrich pod ResourceStates with a concise "Status" attribute derived from Pod phase
// and container states (Waiting/Terminated reasons). Also, when a Pod exists but its
// state is Unknown, mark it as Created so the STATE column is more informative.
func (r *Runner) enrichPodStatuses(ctx context.Context) {
	for _, crts := range r.taskStore.ReadinessTasksStates() {
		crts.RTransaction(func(rts *kdstatestore.ReadinessTaskState) {
			for _, crs := range rts.ResourceStates() {
				var (
					kind string
					name string
					ns   string
				)
				crs.RTransaction(func(rs *kdstatestore.ResourceState) {
					kind = rs.GroupVersionKind().Kind
					name = rs.Name()
					ns = rs.Namespace()
				})

				if !strings.EqualFold(kind, "Pod") || name == "" {
					continue
				}

				// rate-limit enrichment GETs per pod to reduce API pressure
				key := ns + "/" + name
				if ts, ok := r.podStatusLast[key]; ok && time.Since(ts) < podStatusMinRefresh {
					continue
				}
				r.podStatusLast[key] = time.Now()

				if r.limiter != nil {
					r.limiter.Accept()
				}
				pod, err := r.factory.KubeClient().CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil || pod == nil {
					continue
				}

				// Build a concise status string
				status := string(pod.Status.Phase)
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
						status = "Waiting:" + cs.State.Waiting.Reason
						break
					}
					if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
						status = "Terminated:" + cs.State.Terminated.Reason
						break
					}
				}

				if status == "" {
					continue
				}

				crs.RWTransaction(func(rs *kdstatestore.ResourceState) {
					// Update or add Status attribute
					updated := false
					for _, attr := range rs.Attributes() {
						if fmt.Sprint(attr.Name()) == "Status" {
							if a, ok := attr.(*kdstatestore.Attribute[string]); ok {
								a.Value = status
								updated = true
							}
							break
						}
					}
					if !updated {
						rs.AddAttribute(kdstatestore.NewAttribute[string]("Status", status))
					}

					// If kubedog hasn't set a concrete state yet but the Pod exists, mark as Created.
					if rs.Status() == kdstatestore.ResourceStatusUnknown {
						rs.SetStatus(kdstatestore.ResourceStatusCreated)
					}
				})
			}
		})
	}
}

// ------------------------------
// Spec building and CRD rules
// ------------------------------

// readinessStrategy describes how to determine readiness for a resource.
type readinessStrategy struct {
	// Builtin indicates a known native workload strategy (e.g., Deployment/StatefulSet/Job/DaemonSet).
	// When set, ConditionType/Expected are ignored.
	Builtin string // e.g., "Deployment", "StatefulSet", "Job", "DaemonSet", "Pod"

	// For CRDs or generic resources, readiness can be determined by a condition.
	ConditionType string // e.g., "Ready", "Available", "Healthy"
	Expected      string // e.g., "True", "Succeeded"
}

// resourceSpec represents a single resource to track.
type resourceSpec struct {
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string

	Readiness readinessStrategy
}

// buildCRDRulesIndex constructs a map for case-insensitive lookups by group/version/kind.
func (r *Runner) buildCRDRulesIndex() {
	idx := make(map[gvkKey]CRDConditionRule, len(r.opts.CRDRules))
	for _, rule := range r.opts.CRDRules {
		k := gvkKey{
			Group:   strings.ToLower(strings.TrimSpace(rule.Group)),
			Version: strings.ToLower(strings.TrimSpace(rule.Version)),
			Kind:    strings.ToLower(strings.TrimSpace(rule.Kind)),
		}
		if k.Group == "" && rule.Version != "" {
			// Core group case: group may be "", keep as empty string
		}
		idx[k] = rule
	}
	r.crdRulesIndex = idx
	{
		// Built-in common CRD rules (only if not already provided by user)
		addRule := func(g, v, k, cond, exp string) {
			key := gvkKey{Group: strings.ToLower(g), Version: strings.ToLower(v), Kind: strings.ToLower(k)}
			if _, exists := idx[key]; !exists {
				idx[key] = CRDConditionRule{Group: g, Version: v, Kind: k, Condition: cond, Expected: exp}
			}
		}

		// Strimzi Kafka CRDs
		addRule("kafka.strimzi.io", "v1beta2", "KafkaTopic", "Ready", "True")
		addRule("kafka.strimzi.io", "v1beta2", "Kafka", "Ready", "True")
		addRule("kafka.strimzi.io", "v1beta2", "KafkaUser", "Ready", "True")

		// cert-manager (old and new API groups) Certificate/Issuer readiness
		addRule("certmanager.k8s.io", "v1alpha1", "Certificate", "Ready", "True")
		addRule("certmanager.k8s.io", "v1alpha1", "Issuer", "Ready", "True")
		addRule("cert-manager.io", "v1", "Certificate", "Ready", "True")
		addRule("cert-manager.io", "v1", "Issuer", "Ready", "True")

		// external-secrets
		addRule("external-secrets.io", "v1beta1", "ExternalSecret", "Ready", "True")

		// AWS Controllers for Kubernetes - DynamoDB Table readiness
		addRule("dynamodb.services.k8s.aws", "v1alpha1", "Table", "ACK.ResourceSynced", "True")

		// Apache APISIX ApisixRoute readiness
		addRule("apisix.apache.org", "v2", "ApisixRoute", "ResourcesAvailable", "True")
	}
	r.vprintf("Indexed %d CRD rule(s)", len(idx))
}

// buildResourceSpecs converts unstructured objects into trackable specs with readiness strategies.
func (r *Runner) buildResourceSpecs(objects []*unstructured.Unstructured) []resourceSpec {
	var specs []resourceSpec
	mapper := r.factory.Mapper()

	for _, u := range objects {
		gvk := objutil.ObjectGVK(u)
		ns := u.GetNamespace()

		// If namespace is missing, use RESTMapper to check scope and apply DefaultNamespace
		if ns == "" && r.opts.DefaultNamespace != "" {
			if mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err == nil {
				if mapping.Scope.Name() == apimeta.RESTScopeNameNamespace {
					ns = r.opts.DefaultNamespace
				}
			}
		}

		specs = append(specs, resourceSpec{
			GVK:       gvk,
			Namespace: ns,
			Name:      u.GetName(),
			Readiness: r.inferReadinessStrategy(u),
		})
	}
	return specs
}

// inferReadinessStrategy returns a readiness strategy for the given object.
func (r *Runner) inferReadinessStrategy(u *unstructured.Unstructured) readinessStrategy {
	gvk := objutil.ObjectGVK(u)
	kindLower := strings.ToLower(gvk.Kind)

	// Known native workloads: use builtin tracking strategies (to be mapped to kubedog)
	switch kindLower {
	case "deployment":
		return readinessStrategy{Builtin: "Deployment"}
	case "statefulset":
		return readinessStrategy{Builtin: "StatefulSet"}
	case "daemonset":
		return readinessStrategy{Builtin: "DaemonSet"}
	case "job":
		return readinessStrategy{Builtin: "Job"}
	}

	// CRD readiness via rule
	if rule, ok := r.lookupCRDRule(gvk); ok {
		return readinessStrategy{
			ConditionType: rule.Condition,
			Expected:      rule.Expected,
		}
	}

	// Best-effort generic fallback:
	// If object has status.conditions with a commonly-used readiness type,
	// attempt to track that condition automatically.
	if conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions"); found {
		candidates := []string{"Ready", "Available", "Healthy", "Succeeded", "Complete", "Completed"}
		for _, want := range candidates {
			for _, it := range conds {
				if m, ok := it.(map[string]any); ok {
					t, _, _ := unstructured.NestedString(m, "type")
					if strings.EqualFold(t, want) {
						// Most conditions use status=True when ready/available/healthy/completed.
						return readinessStrategy{
							ConditionType: want,
							Expected:      "True",
						}
					}
				}
			}
		}
	}

	// Presence-only (no readiness strategy available).
	return readinessStrategy{}
}

// lookupCRDRule returns a CRD rule for an exact group/version/kind match (case-insensitive).
func (r *Runner) lookupCRDRule(gvk schema.GroupVersionKind) (CRDConditionRule, bool) {
	k := gvkKey{
		Group:   strings.ToLower(gvk.Group),
		Version: strings.ToLower(gvk.Version),
		Kind:    strings.ToLower(gvk.Kind),
	}
	rule, ok := r.crdRulesIndex[k]
	return rule, ok
}

// gvkKey is used to index CRD rules.
type gvkKey struct {
	Group   string
	Version string
	Kind    string
}

// ------------------------------
// Utilities
// ------------------------------

// vprintf prints when verbose is enabled.
func (r *Runner) vprintf(format string, args ...any) {
	if r.opts.Verbose {
		fmt.Printf("[kubetracker] "+format+"\n", args...)
	}
}

// ------------------------------
// Dynamic tracker wiring
// ------------------------------

// startAndWaitTrackers creates kubedog dynamic trackers for native workloads and
// starts condition-based polling for CRDs with configured rules. It streams states
// into task/log stores and waits for completion. Honors ExitOnFirstFail.
func (r *Runner) startAndWaitTrackers(ctx context.Context, specs []resourceSpec) error {
	// Mapper must be ResettableRESTMapper for kubedog and util helpers
	mapper, ok := r.factory.Mapper().(apimeta.ResettableRESTMapper)
	if !ok {
		return errors.New("kube RESTMapper does not implement ResettableRESTMapper")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		errCh = make(chan error, len(specs))
	)

	r.vprintf("Starting trackers for %d resource(s)", len(specs))

	// Build and start trackers
	registered := 0
	for _, s := range specs {
		switch strings.ToLower(s.Readiness.Builtin) {
		case "deployment", "statefulset", "daemonset", "job":
			// Native workload: use kubedog dynamic readiness tracker
			r.vprintf("Registering builtin tracker: %s %s/%s", s.GVK.Kind, s.Namespace, s.Name)
			rts := kdstatestore.NewReadinessTaskState(s.Name, s.Namespace, s.GVK, kdstatestore.ReadinessTaskStateOptions{})
			r.taskStore.AddReadinessTaskState(kdutil.NewConcurrent(rts))

			opts := kddyntracker.DynamicReadinessTrackerOptions{
				Timeout:    r.opts.Timeout,
				SaveEvents: r.opts.ShowEvents,
				// We always collect logs; printing is filtered by TablesBuilder (OnlyErrorLogs)
				IgnoreLogs: false,
			}

			registered++

			wg.Add(1)
			go func(spec resourceSpec, task *kdstatestore.ReadinessTaskState) {
				defer wg.Done()

				start := time.Now()
				var lastErr error

				for {
					// Build a fresh tracker each attempt to re-establish informers after failures.
					tr, err := kddyntracker.NewDynamicReadinessTracker(
						ctx,
						kdutil.NewConcurrent(task),
						r.logStore,
						r.factory.KubeClient(),
						r.factory.Dynamic(),
						r.factory.CachedDiscovery(),
						mapper,
						opts,
					)
					if err != nil {
						lastErr = fmt.Errorf("create readiness tracker for %s/%s: %w", spec.GVK.Kind, spec.Name, err)
						break
					}

					if err := tr.Track(ctx); err == nil {
						// Success
						return
					} else {
						lastErr = err
					}

					// No grace period configured -> bail
					if r.opts.FailureGracePeriod <= 0 {
						break
					}

					// Grace period expired -> bail
					elapsed := time.Since(start)
					if elapsed >= r.opts.FailureGracePeriod {
						break
					}

					// Retry with a small backoff within the remaining grace window
					remaining := r.opts.FailureGracePeriod - elapsed
					delay := 5 * time.Second
					if remaining < delay {
						delay = remaining
					}

					r.vprintf("Tracker for %s/%s failed (%v); retrying in %s (remaining %s)", spec.GVK.Kind, spec.Name, lastErr, delay, remaining)

					stopRetry := false
					select {
					case <-ctx.Done():
						lastErr = context.Cause(ctx)
						stopRetry = true
					case <-time.After(delay):
					}

					if stopRetry || ctx.Err() != nil {
						break
					}
					// continue loop for another attempt
				}

				if lastErr != nil {
					errCh <- fmt.Errorf("%s/%s: %v", spec.GVK.Kind, spec.Name, lastErr)
					if r.opts.ExitOnFirstFail {
						cancel()
					}
				}
			}(s, rts)

		default:
			// CRD condition rule or auto-detected condition
			if s.Readiness.ConditionType != "" && s.Readiness.Expected != "" {
				r.vprintf("Registering condition tracker: %s %s/%s (condition=%s expected=%s)",
					s.GVK.Kind, s.Namespace, s.Name, s.Readiness.ConditionType, s.Readiness.Expected)

				rts := kdstatestore.NewReadinessTaskState(s.Name, s.Namespace, s.GVK, kdstatestore.ReadinessTaskStateOptions{})
				// Root resource state for printer
				rts.AddResourceState(s.Name, s.Namespace, s.GVK)
				r.taskStore.AddReadinessTaskState(kdutil.NewConcurrent(rts))
				registered++

				wg.Add(1)
				go func(spec resourceSpec, task *kdstatestore.ReadinessTaskState) {
					defer wg.Done()
					if err := r.trackCRDCondition(ctx, mapper, spec, task); err != nil {
						errCh <- fmt.Errorf("%s/%s: %v", spec.GVK.Kind, spec.Name, err)
						if r.opts.ExitOnFirstFail {
							cancel()
						}
					}
				}(s, rts)
			} else {
				// Presence tracker: wait until the resource exists, then mark ready.
				r.vprintf("Registering presence tracker: %s %s/%s", s.GVK.Kind, s.Namespace, s.Name)

				rts := kdstatestore.NewReadinessTaskState(s.Name, s.Namespace, s.GVK, kdstatestore.ReadinessTaskStateOptions{})
				// Root resource state for printer
				rts.AddResourceState(s.Name, s.Namespace, s.GVK)
				r.taskStore.AddReadinessTaskState(kdutil.NewConcurrent(rts))
				registered++

				wg.Add(1)
				go func(spec resourceSpec, task *kdstatestore.ReadinessTaskState) {
					defer wg.Done()

					gvr, err := kdutil.GVRFromGVK(spec.GVK, mapper)
					if err != nil {
						errCh <- fmt.Errorf("map GVK to GVR for presence: %w", err)
						if r.opts.ExitOnFirstFail {
							cancel()
						}
						return
					}

					crs := task.ResourceState(spec.Name, spec.Namespace, spec.GVK)
					ticker := time.NewTicker(3 * time.Second)
					defer ticker.Stop()
					// jitter start to spread load across many resources
					time.Sleep(time.Duration(rand.Int63n(int64(time.Second))))

					for {
						select {
						case <-ctx.Done():
							crs.RWTransaction(func(rs *kdstatestore.ResourceState) {
								rs.SetStatus(kdstatestore.ResourceStatusFailed)
							})
							task.SetStatus(kdstatestore.ReadinessTaskStatusFailed)
							errCh <- context.Cause(ctx)
							return

						case <-ticker.C:
							var _u *unstructured.Unstructured
							_ = _u
							if r.limiter != nil {
								r.limiter.Accept()
							}
							if spec.Namespace != "" {
								_u, err = r.factory.Dynamic().Resource(gvr).Namespace(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
							} else {
								_u, err = r.factory.Dynamic().Resource(gvr).Get(ctx, spec.Name, metav1.GetOptions{})
							}
							if err != nil {
								// keep waiting for resource presence
								crs.RWTransaction(func(rs *kdstatestore.ResourceState) {
									rs.SetStatus(kdstatestore.ResourceStatusUnknown)
								})
								task.SetStatus(kdstatestore.ReadinessTaskStatusProgressing)
								r.vprintf("Waiting: %s %s/%s not found yet (%v)", spec.GVK.Kind, spec.Namespace, spec.Name, err)
								continue
							}

							// Found: mark ready
							crs.RWTransaction(func(rs *kdstatestore.ResourceState) {
								rs.SetStatus(kdstatestore.ResourceStatusReady)
							})
							task.SetStatus(kdstatestore.ReadinessTaskStatusReady)
							r.vprintf("%s %s/%s present", spec.GVK.Kind, spec.Namespace, spec.Name)
							return
						}
					}
				}(s, rts)
			}
		}
	}

	// Start progress printer after tasks are registered
	stopPrint := r.startProgressPrinter(ctx)
	defer stopPrint()
	r.vprintf("All trackers registered: %d", registered)

	// Wait for all trackers, aggregate errors
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	var errs []error
loop:
	for {
		select {
		case <-done:
			break loop
		case e := <-errCh:
			if e != nil {
				errs = append(errs, e)
			}
		case <-ctx.Done():
			if cause := context.Cause(ctx); cause != nil && cause != context.Canceled {
				errs = append(errs, cause)
			}
			break loop
		}
	}

	if len(errs) > 0 {
		// Return deadline exceeded as-is for proper timeout detection by callers.
		for _, e := range errs {
			if errors.Is(e, context.DeadlineExceeded) {
				return context.DeadlineExceeded
			}
		}
		// If only one error, return it directly.
		if len(errs) == 1 {
			return errs[0]
		}
		// Join multiple errors and wrap to keep original messages while preserving error causes.
		return fmt.Errorf("tracking failed: %w", errors.Join(errs...))
	}

	return nil
}

// trackCRDCondition polls a CRD resource and sets readiness when a target condition
// reaches the expected value. Updates task and resource states for the printer.
func (r *Runner) trackCRDCondition(ctx context.Context, mapper apimeta.ResettableRESTMapper, spec resourceSpec, rts *kdstatestore.ReadinessTaskState) error {
	gvr, err := kdutil.GVRFromGVK(spec.GVK, mapper)
	if err != nil {
		return fmt.Errorf("map GVK to GVR: %w", err)
	}

	r.vprintf("Tracking %s %s/%s by condition %q==%q (GVR=%s/%s, Resource=%s)",
		spec.GVK.Kind, spec.Namespace, spec.Name, spec.Readiness.ConditionType, spec.Readiness.Expected, gvr.Group, gvr.Version, gvr.Resource)

	// Initialize attributes
	crs := rts.ResourceState(spec.Name, spec.Namespace, spec.GVK)
	crs.RWTransaction(func(rs *kdstatestore.ResourceState) {
		// Show what we're tracking in the printer
		rs.AddAttribute(kdstatestore.NewAttribute[string]("ConditionTarget", spec.Readiness.ConditionType))
	})

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	// jitter start to spread load across many resources
	time.Sleep(time.Duration(rand.Int63n(int64(time.Second))))

	var last string

	for {
		select {
		case <-ctx.Done():
			crs.RWTransaction(func(rs *kdstatestore.ResourceState) {
				rs.SetStatus(kdstatestore.ResourceStatusFailed)
			})
			rts.SetStatus(kdstatestore.ReadinessTaskStatusFailed)
			r.vprintf("Context done while tracking %s %s/%s: %v", spec.GVK.Kind, spec.Namespace, spec.Name, context.Cause(ctx))
			return context.Cause(ctx)

		case <-ticker.C:
			if r.limiter != nil {
				r.limiter.Accept()
			}
			var u *unstructured.Unstructured
			if spec.Namespace != "" {
				u, err = r.factory.Dynamic().Resource(gvr).Namespace(spec.Namespace).Get(ctx, spec.Name, metav1.GetOptions{})
			} else {
				u, err = r.factory.Dynamic().Resource(gvr).Get(ctx, spec.Name, metav1.GetOptions{})
			}
			if err != nil {
				// Keep progressing until present
				crs.RWTransaction(func(rs *kdstatestore.ResourceState) {
					rs.SetStatus(kdstatestore.ResourceStatusUnknown)
				})
				rts.SetStatus(kdstatestore.ReadinessTaskStatusProgressing)
				r.vprintf("Waiting: %s %s/%s not found yet (%v)", spec.GVK.Kind, spec.Namespace, spec.Name, err)
				continue
			}

			// Found object: check condition
			current := ""
			if conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions"); found {
				for _, it := range conds {
					if m, ok := it.(map[string]any); ok {
						t, _, _ := unstructured.NestedString(m, "type")
						if strings.EqualFold(t, spec.Readiness.ConditionType) {
							v, _, _ := unstructured.NestedString(m, "status")
							current = v
							break
						}
					}
				}
			}

			if current != last {
				r.vprintf("%s %s/%s condition %q -> %q", spec.GVK.Kind, spec.Namespace, spec.Name, spec.Readiness.ConditionType, current)
				last = current
			}

			crs.RWTransaction(func(rs *kdstatestore.ResourceState) {
				rs.AddAttribute(kdstatestore.NewAttribute[string]("ConditionCurrentValue", current))
				switch {
				case current == "":
					rs.SetStatus(kdstatestore.ResourceStatusCreated)
				case strings.EqualFold(current, spec.Readiness.Expected):
					rs.SetStatus(kdstatestore.ResourceStatusReady)
				default:
					rs.SetStatus(kdstatestore.ResourceStatusCreated)
				}
			})

			if current != "" && strings.EqualFold(current, spec.Readiness.Expected) {
				rts.SetStatus(kdstatestore.ReadinessTaskStatusReady)
				r.vprintf("Ready: %s %s/%s met condition %q==%q", spec.GVK.Kind, spec.Namespace, spec.Name, spec.Readiness.ConditionType, spec.Readiness.Expected)
				return nil
			}

			rts.SetStatus(kdstatestore.ReadinessTaskStatusProgressing)
		}
	}
}
