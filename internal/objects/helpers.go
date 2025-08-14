package objects

// Package objects contains shared helpers for operating on Kubernetes objects
// used across the CLI subcommands: parsing CRD readiness mappings, basic
// object filtering by kind, human-friendly summaries and small GVK helpers.
//
// These are intentionally small, focused utilities to keep the `cli` package
// constrained to command wiring only.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CRDConditionRule describes a readiness rule for a CRD based on status.conditions.
// Example: Group="external-secrets.io", Version="v1beta1", Kind="ExternalSecret",
// Condition="Ready", Expected="True".
type CRDConditionRule struct {
	Group     string
	Version   string
	Kind      string
	Condition string
	Expected  string
}

// ParseCRDConditions parses --crd-condition style mappings.
//
// Expected format for each entry:
//
//	group/version,Kind=ConditionType=ExpectedValue
//
// Returns a slice of CRDConditionRule and a slice of non-fatal parse errors.
func ParseCRDConditions(rules []string) ([]CRDConditionRule, []error) {
	var out []CRDConditionRule
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

		out = append(out, CRDConditionRule{
			Group:     parsedGV.Group,
			Version:   parsedGV.Version,
			Kind:      kind,
			Condition: cond,
			Expected:  exp,
		})
	}

	return out, errs
}

// FirstNonEmpty returns the first non-empty string (after TrimSpace), or "".
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// FilterObjectsByKind filters the provided unstructured objects by include/exclude
// kind lists. Both include and exclude accept either plain Kind (e.g. "Deployment")
// or "group/kind" (e.g. "apps/deployment"). Matching is case-insensitive.
// If includeKinds is empty, all kinds are considered included unless excluded.
func FilterObjectsByKind(objs []*unstructured.Unstructured, includeKinds, excludeKinds []string) []*unstructured.Unstructured {
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
		gvk := ObjectGVK(u)
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

// PrintObjectsSummary prints a brief summary of objects to the provided cobra command
// (so output follows the same writer/format helpers used by CLI). It prints counts
// grouped by group/kind and the list of namespaces present.
func PrintObjectsSummary(cmd *cobra.Command, objs []*unstructured.Unstructured) {
	type key struct {
		Group string
		Kind  string
	}
	counts := make(map[key]int)
	nsSet := make(map[string]struct{})

	for _, u := range objs {
		gvk := ObjectGVK(u)
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

// ObjectGVK returns the parsed GroupVersionKind for the provided unstructured.
// It tolerates simple/malformed apiVersion values by placing the whole version
// into Version when parsing fails (same behaviour used elsewhere).
func ObjectGVK(u *unstructured.Unstructured) schema.GroupVersionKind {
	gv, err := schema.ParseGroupVersion(u.GetAPIVersion())
	if err != nil {
		return schema.GroupVersionKind{Group: "", Version: u.GetAPIVersion(), Kind: u.GetKind()}
	}
	return gv.WithKind(u.GetKind())
}
