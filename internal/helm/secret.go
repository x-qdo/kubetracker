package helm

// Package helm provides helpers to locate and decode Helm v3 release manifests
// stored in Kubernetes Secrets, and utilities to parse CLI release references.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kblabels "k8s.io/apimachinery/pkg/labels"

	"github.com/x-qdo/kubetracker/internal/kube"
)

// ReleaseRef identifies a Helm release by namespace and name.
type ReleaseRef struct {
	Namespace string
	Name      string
}

// ParseReleaseArg parses a single release CLI argument into a ReleaseRef.
// The argument can be either "<namespace>/<name>" or "<name>" (in which case
// defaultNS must be non-empty).
func ParseReleaseArg(arg string, defaultNS string) (ReleaseRef, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ReleaseRef{}, fmt.Errorf("empty release")
	}
	if strings.Contains(arg, "/") {
		parts := strings.SplitN(arg, "/", 2)
		ns := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		if ns == "" || name == "" {
			return ReleaseRef{}, fmt.Errorf("invalid release %q, expected <namespace>/<name>", arg)
		}
		return ReleaseRef{Namespace: ns, Name: name}, nil
	}
	// No namespace in arg; require defaultNS
	if strings.TrimSpace(defaultNS) == "" {
		return ReleaseRef{}, fmt.Errorf("release %q missing namespace; provide --namespace or use <namespace>/<name>", arg)
	}
	return ReleaseRef{Namespace: defaultNS, Name: arg}, nil
}

// ParseReleaseArgs parses multiple CLI release arguments into ReleaseRefs.
// Each arg can be "<ns>/<name>" or "<name>" (requiring defaultNS).
func ParseReleaseArgs(args []string, defaultNS string) ([]ReleaseRef, error) {
	refs := make([]ReleaseRef, 0, len(args))
	for _, a := range args {
		r, err := ParseReleaseArg(a, defaultNS)
		if err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, nil
}

// GetLatestReleaseManifest finds the latest Helm v3 Secret for the given release
// (ns/name), decodes the embedded release payload, and returns the .manifest
// string. A non-empty warn may be returned for non-fatal issues (e.g. bad version label).
//
// This reads Secrets with labels owner=helm, name=<release>. Among those with
// Type "helm.sh/release.v1", it picks the highest numeric 'version' label, breaking
// ties by most recent creation timestamp.
func GetLatestReleaseManifest(ctx context.Context, factory *kube.Factory, ns, name string, verbose bool) (manifest string, warn string, _ error) {
	selector := kblabels.Set{
		"owner": "helm",
		"name":  name,
	}.AsSelector().String()

	seclist, err := factory.KubeClient().CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", warn, fmt.Errorf("list secrets in %s: %w", ns, err)
	}
	if len(seclist.Items) == 0 {
		return "", warn, fmt.Errorf("no Helm secrets found for release %s/%s", ns, name)
	}

	// Select best candidate: highest version, then most recent creation time.
	type candidate struct {
		version int
		created time.Time
		idx     int
	}
	var best *candidate
	for i := range seclist.Items {
		s := seclist.Items[i]
		if string(s.Type) != "helm.sh/release.v1" {
			continue
		}
		vstr := s.Labels["version"]
		v, convErr := strconv.Atoi(vstr)
		if convErr != nil {
			if verbose {
				warn = fmt.Sprintf("helm: secret %s/%s has non-integer version label %q", s.Namespace, s.Name, vstr)
			}
			v = -1
		}
		c := candidate{version: v, created: s.CreationTimestamp.Time, idx: i}
		if best == nil ||
			c.version > best.version ||
			(c.version == best.version && c.created.After(best.created)) {
			best = &c
		}
	}
	if best == nil {
		return "", warn, fmt.Errorf("no Helm release secrets of type helm.sh/release.v1 for %s/%s", ns, name)
	}

	sec := seclist.Items[best.idx]
	raw := sec.Data["release"]
	if len(raw) == 0 {
		return "", warn, fmt.Errorf("secret %s/%s has empty release payload", sec.Namespace, sec.Name)
	}

	manifest, err = DecodeReleaseManifest(raw)
	if err != nil {
		return "", warn, fmt.Errorf("decode release manifest from %s/%s: %w", sec.Namespace, sec.Name, err)
	}
	return manifest, warn, nil
}

// DecodeReleaseManifest decodes a Helm v3 Secret's "release" data payload,
// which is a base64-encoded, gzipped JSON of the Release proto, and extracts
// the embedded .manifest string.
func DecodeReleaseManifest(secretData []byte) (string, error) {
	enc := string(secretData)
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}

	// gzip magic header 1f 8b 08
	if len(b) > 3 && b[0] == 0x1f && b[1] == 0x8b && b[2] == 0x08 {
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return "", fmt.Errorf("gzip reader: %w", err)
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			return "", fmt.Errorf("gzip read: %w", err)
		}
		b = data
	}

	var payload struct {
		Manifest string `json:"manifest"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return "", fmt.Errorf("json unmarshal release: %w", err)
	}
	return payload.Manifest, nil
}
