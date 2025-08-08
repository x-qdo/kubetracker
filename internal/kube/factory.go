package kube

// Package kube provides a thin factory around Kubernetes client-go and kubedog needs.
// It centralizes kubeconfig/context resolution, and wires typed, dynamic,
// discovery, and RESTMapper clients.
//

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	memcache "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// Options controls how the Factory builds Kubernetes clients.
type Options struct {
	// Kubeconfig path to use. When empty, clientcmd default loading rules will be used:
	// - $KUBECONFIG (one or more paths)
	// - ~/.kube/config
	Kubeconfig string

	// Context name in kubeconfig to use. When empty, the current-context is used.
	Context string

	// Per-request QPS and Burst for rest.Config.
	// If QPS <= 0, a sane default may be applied by client-go; set explicitly for predictability.
	QPS   float32
	Burst int

	// Optional User-Agent for clients.
	UserAgent string

	// If set, overrides rest.Config.Timeout for all requests.
	Timeout time.Duration

	// InsecureSkipTLSVerify, when true, skips TLS verification. Prefer proper CAs instead.
	InsecureSkipTLSVerify bool
}

// Factory bundles commonly used Kubernetes clients and helpers.
type Factory struct {
	cfg             *rest.Config
	typed           *kubernetes.Clientset
	dyn             dynamic.Interface
	disco           discovery.DiscoveryInterface
	cachedDiscovery discovery.CachedDiscoveryInterface
	mapper          meta.RESTMapper
}

// NewFactory creates a new Factory by resolving kubeconfig/context and wiring clients.
// It does not start any informers or background routines.
func NewFactory(ctx context.Context, opts Options) (*Factory, error) {
	cfg, err := buildRESTConfig(opts)
	if err != nil {
		return nil, fmt.Errorf("build rest config: %w", err)
	}

	// Apply optional overrides
	if opts.UserAgent != "" {
		rest.AddUserAgent(cfg, opts.UserAgent)
	}
	if opts.QPS > 0 {
		cfg.QPS = opts.QPS
	}
	if opts.Burst > 0 {
		cfg.Burst = opts.Burst
	}
	if opts.Timeout > 0 {
		cfg.Timeout = opts.Timeout
	}
	if opts.InsecureSkipTLSVerify {
		cfg.Insecure = true
		// NOTE: consider also clearing / customizing TLSClientConfig as needed
	}

	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("construct typed clientset: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("construct dynamic client: %w", err)
	}

	disco := typed.Discovery()
	cachedDisco := memcache.NewMemCacheClient(disco)

	// Deferred discovery mapper backed by a cached discovery client.
	deferred := restmapper.NewDeferredDiscoveryRESTMapper(cachedDisco)
	mapper := restmapper.NewShortcutExpander(deferred, disco, func(string) {})

	return &Factory{
		cfg:             cfg,
		typed:           typed,
		dyn:             dynClient,
		disco:           disco,
		cachedDiscovery: cachedDisco,
		mapper:          mapper,
	}, nil
}

// buildRESTConfig resolves kubeconfig/context into a rest.Config.
// It prefers the provided Kubeconfig path if set; otherwise uses clientcmd defaults.
// If Context is set, it will be used to override the selected context.
func buildRESTConfig(opts Options) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.Kubeconfig != "" {
		// Explicit path has priority; multi-path resolution can be added later if needed.
		loadingRules = &clientcmd.ClientConfigLoadingRules{
			ExplicitPath: opts.Kubeconfig,
		}
	}

	overrides := &clientcmd.ConfigOverrides{
		ClusterInfo: clientcmdapi.Cluster{},
	}
	if opts.Context != "" {
		overrides.CurrentContext = opts.Context
	}

	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// Config returns the underlying rest.Config (do not mutate in-place in callers).
func (f *Factory) Config() *rest.Config {
	return rest.CopyConfig(f.cfg)
}

// KubeClient returns a typed Kubernetes clientset.
func (f *Factory) KubeClient() *kubernetes.Clientset {
	return f.typed
}

// Dynamic returns a dynamic client.
func (f *Factory) Dynamic() dynamic.Interface {
	return f.dyn
}

// Discovery returns a discovery client.
func (f *Factory) Discovery() discovery.DiscoveryInterface {
	return f.disco
}

// CachedDiscovery returns a cached discovery client (memory-backed).
func (f *Factory) CachedDiscovery() discovery.CachedDiscoveryInterface {
	return f.cachedDiscovery
}

// Mapper returns a RESTMapper backed by deferred discovery.
func (f *Factory) Mapper() meta.RESTMapper {
	return f.mapper
}

// RefreshRESTMapper resets the discovery cache and forces the mapper to re-discover resources.
// Useful when CRDs are installed during runtime and need to be recognized.
func (f *Factory) RefreshRESTMapper() {
	f.cachedDiscovery.Invalidate()
	// restmapper.DeferredDiscoveryRESTMapper lazily refreshes on misses;
	// invalidating the cache is typically sufficient.
}

// TODO: Add helpers as needed:
// - func (f *Factory) Namespace() (string, bool, error)  // resolve default namespace from kubeconfig
// - func (f *Factory) WithRateLimits(qps float32, burst int) *Factory
// - func (f *Factory) RESTClientFor(gvk schema.GroupVersionKind) (rest.Interface, error)
