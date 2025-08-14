package render

// Package render provides utilities for reading Kubernetes manifests from files,
// directories, or stdin and converting them into unstructured objects that can
// be tracked or applied. It focuses on being a thin, composable layer used by
// the CLI and higher-level orchestration code.
//
// Notes:
// - This package does NOT apply or validate the resources against the cluster.
// - It returns best-effort warnings for non-fatal issues (e.g., skipping non-YAML files).
// - It injects a default namespace for namespaceless, namespaced-scoped resources,
//   avoiding a static set of known cluster-scoped kinds.
//
// Roadmap:
// - Add JSONPath-based filtering or selective document extraction.
// - Add schema-aware namespace inference using RESTMapper when available.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	objutil "github.com/x-qdo/kubetracker/internal/objects"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Options configure manifest loading.
type Options struct {
	// DefaultNamespace is applied to objects missing a namespace when those objects
	// are namespaced-scoped (best-effort heuristic).
	DefaultNamespace string

	// Recursive controls whether directory traversal should recurse into subdirectories.
	Recursive bool

	// Stdin can be set to override the reader when paths contain "-".
	// If nil, os.Stdin will be used.
	Stdin io.Reader
}

// Load reads Kubernetes manifests from the provided paths (files, directories, or "-"
// for stdin), decodes them into unstructured objects, expands List kinds into items,
// applies best-effort default namespace for namespaced objects, and returns the
// resulting objects alongside non-fatal warnings.
//
// Behavior:
// - Directories are traversed; only .yaml/.yml/.json files are decoded.
// - Multi-document YAML is supported.
// - Kind: List is expanded into individual items.
// - Objects without apiVersion/kind are ignored.
//
// Returns:
// - objects: slice of decoded unstructured objects.
// - warnings: slice of non-fatal issues encountered (e.g., skipped files).
// - err: fatal error (e.g., read permissions, malformed YAML that cannot be recovered).
func Load(paths []string, opts Options) (objects []*unstructured.Unstructured, warnings []string, err error) {
	if len(paths) == 0 {
		paths = []string{"-"}
	}

	seen := make(map[string]struct{})
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}

	for _, p := range paths {
		if p == "-" {
			docs, w, err := decodeStream("stdin", stdin, opts.DefaultNamespace)
			if err != nil {
				return nil, warnings, err
			}
			objects = append(objects, docs...)
			warnings = append(warnings, w...)
			continue
		}

		info, err := os.Stat(p)
		if err != nil {
			return nil, warnings, fmt.Errorf("stat %q: %w", p, err)
		}

		if info.IsDir() {
			walkFn := func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if path != p && !opts.Recursive {
						return fs.SkipDir
					}
					return nil
				}
				if !isYAMLLike(d.Name()) {
					return nil
				}

				abs, _ := filepath.Abs(path)
				if _, ok := seen[abs]; ok {
					return nil
				}
				seen[abs] = struct{}{}

				f, err := os.Open(path)
				if err != nil {
					return fmt.Errorf("open %q: %w", path, err)
				}
				defer f.Close()

				docs, w, err := decodeStream(path, f, opts.DefaultNamespace)
				if err != nil {
					return fmt.Errorf("decode %q: %w", path, err)
				}
				objects = append(objects, docs...)
				warnings = append(warnings, w...)
				return nil
			}

			if err := filepath.WalkDir(p, walkFn); err != nil {
				return nil, warnings, err
			}
			continue
		}

		// Single file
		if !isYAMLLike(info.Name()) {
			warnings = append(warnings, fmt.Sprintf("skipping non-YAML file: %s", p))
			continue
		}

		abs, _ := filepath.Abs(p)
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}

		f, err := os.Open(p)
		if err != nil {
			return nil, warnings, fmt.Errorf("open %q: %w", p, err)
		}
		func() {
			defer f.Close()
			docs, w, err := decodeStream(p, f, opts.DefaultNamespace)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("failed decoding %s: %v", p, err))
				return
			}
			objects = append(objects, docs...)
			warnings = append(warnings, w...)
		}()
	}

	objects = expandLists(objects)

	// Drop empty/unrecognized entries and sort for deterministic order
	clean := make([]*unstructured.Unstructured, 0, len(objects))
	for _, u := range objects {
		if strings.TrimSpace(u.GetKind()) == "" || strings.TrimSpace(u.GetAPIVersion()) == "" {
			continue
		}
		clean = append(clean, u)
	}
	objects = sortObjects(clean)

	return objects, warnings, nil
}

// decodeStream reads a YAML/JSON stream and returns decoded unstructured objects.
// It applies default namespace to namespaced objects missing a namespace.
// Non-fatal warnings are returned separately.
func decodeStream(source string, r io.Reader, defaultNamespace string) ([]*unstructured.Unstructured, []string, error) {
	var warnings []string

	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	var out []*unstructured.Unstructured

	for {
		var raw map[string]any
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Some decoders return EOF-like errors; tolerate if we've read nothing meaningful.
			if err.Error() == "EOF" && len(raw) == 0 {
				break
			}
			return nil, warnings, fmt.Errorf("decode %s: %w", source, err)
		}
		if len(raw) == 0 {
			continue
		}

		u := &unstructured.Unstructured{Object: raw}

		// Best-effort default namespace injection for namespaced resources.
		if u.GetNamespace() == "" && defaultNamespace != "" && !isClusterScoped(u) {
			u.SetNamespace(defaultNamespace)
		}

		out = append(out, u)
	}

	return out, warnings, nil
}

// expandLists expands objects with kind=List into individual items.
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

// isYAMLLike returns true if filename suggests a YAML or JSON manifest.
func isYAMLLike(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".yaml") || strings.HasSuffix(l, ".yml") || strings.HasSuffix(l, ".json")
}

// isClusterScoped performs a best-effort heuristic to determine cluster-scoped kinds.
// This is NOT exhaustive; it's intended to avoid setting a namespace for well-known
// cluster-scoped resources when injecting a default namespace.
func isClusterScoped(u *unstructured.Unstructured) bool {
	gvk := objutil.ObjectGVK(u)
	kind := gvk.Kind
	group := gvk.Group

	switch kind {
	case "Namespace", "Node", "PersistentVolume", "StorageClass", "CustomResourceDefinition",
		"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration", "ClusterRole", "ClusterRoleBinding",
		"PriorityClass", "APIService", "ClusterIssuer":
		return true
	}

	// Heuristics for cluster-scoped kinds prefixed with "Cluster" in common groups.
	if group == "apiextensions.k8s.io" || group == "admissionregistration.k8s.io" || group == "rbac.authorization.k8s.io" {
		if strings.HasPrefix(kind, "Cluster") {
			return true
		}
	}

	return false
}

// sortObjects sorts objects by Group, Kind, Namespace, Name for determinism.
func sortObjects(objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	sort.SliceStable(objs, func(i, j int) bool {
		gi := objutil.ObjectGVK(objs[i])
		gj := objutil.ObjectGVK(objs[j])

		if gi.Group != gj.Group {
			return gi.Group < gj.Group
		}
		if gi.Kind != gj.Kind {
			return gi.Kind < gj.Kind
		}
		if objs[i].GetNamespace() != objs[j].GetNamespace() {
			return objs[i].GetNamespace() < objs[j].GetNamespace()
		}
		return objs[i].GetName() < objs[j].GetName()
	})
	return objs
}
