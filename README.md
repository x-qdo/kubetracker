# kubetracker

A small Go CLI that tracks Kubernetes resources defined in manifests, showing concise progress and only the logs that matter (pods that fail to start or crashloop). It leverages werf/kubedog under the hood and borrows presentation concepts from werf/nelm while simplifying output.

Highlights:
- Tracks readiness of Deployments, StatefulSets, Jobs, DaemonSets, and arbitrary CRDs.
- Prints progress and events; pod logs are only shown when containers fail (ImagePullBackOff, CrashLoopBackOff, OOMKilled, etc.).
- Works with files, directories, stdin, and Helm releases (via `kubetracker helm`).
- Extensible readiness strategy for CRDs via condition mappings.
- Helm release tracking implemented: `kubetracker helm` reads Helm v3 release Secrets, decodes the rendered manifest, and reuses the same tracker as `track`.

Note: This tool does not apply resources; it tracks resources you’re applying via your deployment tooling (kubectl/helm/Argo/etc.). You can run it concurrently with your apply/upgrade and it will follow the resources defined in your manifests.

## Status

- Tracking implemented: readiness for Deployments, StatefulSets, DaemonSets, Jobs; CRDs via conditions (auto-inferred and custom mappings).
- Progress/events/logs wired; healthy pod logs suppressed by default; pod status enrichment avoids UNKNOWN; built-in CRD rules (cert-manager `Certificate` `Ready=True`, Apache APISIX `ApisixRoute` `ResourcesAvailable=True`).
- Helm release tracking implemented:
  - `kubetracker helm` supports tracking one or more Helm releases directly by reading the cluster Secret for each release and decoding the embedded release manifest.
  - Supports release refs as `<namespace>/<name>` or `<name>` with `--namespace` set.
  - Picks the latest Helm Secret (type `helm.sh/release.v1`) by numeric `version` label, breaking ties by most recent creation timestamp.
  - Emits non-fatal warnings for things like non-integer `version` labels; decode errors surface as invalid input.
- Prints READY on success; supports `--exit-on-first-fail` and global `--timeout`; optional `--failure-grace-period` keeps tracking and retries builtin trackers for transient failures (e.g., ImagePullBackOff).

What’s next:
- More per-CRD defaults and condition heuristics.
- Structured JSON output mode for CI.
- Optional `--apply` mode and additional QoL improvements.

## Installation

- Requirements: Go 1.22+

Build locally:
```
    go build -o ./bin/kubetracker ./cmd/kubetracker
    ./bin/kubetracker --help
```

Alternatively, install into GOPATH/bin:
```
    go install ./cmd/kubetracker
```

## Usage

Basic tracking from a folder:
```
    # Track resources defined in ./manifests
    kubetracker track -f ./manifests
```

Track from stdin (kustomize):
```
    kustomize build ./overlays/prod | kubetracker track -f -
```

Track a Helm release template stream (render locally with `helm template`):
```
    helm template myapp ./chart --namespace prod | kubetracker track -f - -n prod
```

Track Helm release(s) directly (reads latest Secret in-cluster):
```
    # Single release specified as <ns>/<name>
    kubetracker helm prod/myapp

    # Multiple releases (different namespaces allowed)
    kubetracker helm prod/myapp staging/otherapp
```

You can also omit the namespace per release and use `--namespace`:
```
    # Using --namespace for names without <ns>/
    kubetracker helm myapp -n prod
    kubetracker helm myapp otherapp -n prod
```

Common flags (both `track` and `helm` reuse many options):
- `-n, --namespace` Default namespace for namespaceless manifests or to resolve names when release arg omits namespace.
- `--timeout` Global tracking deadline (e.g. 10m).
- `--failure-grace-period` Grace window to keep tracking and retry builtin trackers after failures (e.g. 2m). 0 disables retry.
- `--include-kind / --exclude-kind` Filter kinds by name (e.g. Deployment, KafkaTopic).
- `--crd-condition` Custom condition mapping for CRDs.
- `--show-events` Show K8s events table along with progress.
- `--only-errors-logs` Only stream pod logs for failing containers (default: true).
- `--exit-on-first-fail` Exit immediately when a resource fails (useful in CI).
- `--kubeconfig / --context` Standard kube client overrides.
- `--verbose` Print additional debug/operational details (including Helm secret selection & decode warnings).
- `--summary` Print a summary of resources being tracked.

Exit codes:
- 0: All tracked resources reached the desired state.
- 1: One or more resources failed.
- 2: Timed out.
- 3: Invalid input (bad YAML, missing kube context, failed to decode Helm release, etc.).

## Helm subcommand details

The `helm` subcommand provides two convenient ways to track Helm-managed resources:

1. Track rendered manifests produced by `helm template` piped to `kubetracker track -f -`.
2. Track releases directly with `kubetracker helm <RELEASE...>` which reads the in-cluster Helm v3 Secret for each release and decodes the `.manifest` embedded in the release payload.

How `kubetracker helm` works:
- Each CLI release arg can be specified as `<namespace>/<name>` or just `<name>` (requires `--namespace`).
- The subcommand lists Secrets in the release namespace labeled `owner=helm` and `name=<release>`.
- It filters Secrets of Type `helm.sh/release.v1` and selects the single best candidate:
  - Prefers the highest numeric `version` label.
  - If multiple Secrets share the same numeric `version`, picks the most recently created one.
  - If a Secret has a non-integer `version` label, kubetracker may emit a non-fatal warning and treat that version as `-1` for selection purposes (verbose mode will show the warning).
- The Secret's `data["release"]` payload is decoded:
  - Base64 decode
  - If gzipped, gunzip the payload
  - Unmarshal JSON and extract the `manifest` field
- The extracted manifest (multi-document YAML) is fed to the same manifest loader used by `track`, then tracked normally.

Caveats and limitations:
- Only Helm v3 release Secrets of Type `helm.sh/release.v1` are supported.
- If your Helm installation uses a different storage backend (e.g., secrets encrypted by external tooling, or a different secret type), `kubetracker helm` may not be able to decode the manifest.
- RBAC: listing Secrets in the release namespace requires permissions; lacking access will produce an error.
- If the release Secret is missing, empty, or cannot be decoded, `kubetracker helm` will surface an invalid input error.

## What is tracked

- Native workload kinds: `Deployment`, `StatefulSet`, `DaemonSet`, `Job` (readiness).
- Pods under the hood for status and logs (but logs are shown only for failing states).
- Arbitrary CRDs: readiness determined by `status.conditions` (configurable), or presence-only fallback.

### CRDs readiness strategy

By default, kubetracker tries to infer readiness via `status.conditions`:
- If a resource exposes `status.conditions` with a recognizable `Ready`, `Available`, `Healthy`, or similar positive condition, kubetracker waits for that condition to be `True`.
- If such conditions are absent, kubetracker falls back to presence (created) which is often sufficient for controller-managed resources but may be too permissive.

You can customize CRD readiness via `--crd-condition`:
    # Format: group/version,Kind=ConditionType=ExpectedValue
    # Multiple mappings allowed.
    kubetracker track -f ./manifests \
      --crd-condition "external-secrets.io/v1beta1,ExternalSecret=Ready=True" \
      --crd-condition "kafka.strimzi.io/v1beta2,KafkaTopic=Ready=True"

Built-in mappings are applied unless overridden by `--crd-condition`.

## How it works (high level)

- Parse input manifests into `unstructured.Unstructured` objects.
- Build a set of resource identities (Group/Version/Kind + Namespace + Name).
- For each of them, construct kubedog dynamic tracker tasks:
  - Readiness tasks for workloads and CRDs (with condition rules when provided).
  - Presence tasks when applicable.
- Subscribe to cluster events and logs:
  - Events table shows new events.
  - Logs are collected but only printed for failing pods/containers.
- Render a concise progress table, event table and filtered logs until:
  - All resources are ready; or
  - Any resource fails; or
  - Timeout occurs.

Log filtering:
- For each container in a pod:
  - If waiting reason is problematic (ImagePullBackOff, ErrImagePull, CrashLoopBackOff, CreateContainerConfigError, etc.) OR
  - If terminated with non-zero exit code OR
  - If repeated restarts exceed a threshold (CrashLooping)
  - THEN stream logs for that container.
- Healthy pods remain silent to keep signal high.

## Project structure

Golang standard layout with clear separation of concerns:

- `cmd/kubetracker/`
  - CLI entrypoint (Cobra), flag parsing, wiring.
- `internal/kube/`
  - Kubeconfig, client-go/kubedog client factories, RESTMapper/discovery wiring.
- `internal/render/`
  - Manifest parsing: YAML/JSON decoding into `unstructured.Unstructured`.
  - File/dir/stdin loaders; multi-document handling; default namespace injection.
- `internal/track/`
  - Dynamic tracker task building for readiness/presence.
  - CRD condition mapping, resource classification, timeouts.
  - Pod log selector/filtering logic.
- `internal/helm/`
  - Helpers to locate and decode Helm v3 release Secrets and extract the rendered manifest used by `kubetracker helm`.
- `pkg/printer/`
  - Progress/events/log tables (inspired by nelm’s printer).
  - Adjusted to suppress healthy pod logs; render only failure logs.

This separation keeps the CLI thin and the tracking logic testable and reusable.

## Development roadmap

- v0: Implemented
  - Parse manifests (files/dir/stdin).
  - Readiness tasks for workloads (Deployments/StatefulSets/DaemonSets/Jobs).
  - CRD readiness via common conditions + custom mapping flag; built-in defaults for common CRDs.
  - Progress/events; logs only for failures; pod status enrichment; READY banner on success.
  - Failure grace period for transient errors on builtin trackers; exit-on-first-fail support.
  - Helm release tracking (`kubetracker helm`) to read and decode Helm v3 Secrets and track rendered manifests.
- v0.1: Better CRD support
  - More per-kind defaults (ExternalSecret, KafkaTopic, PrometheusRule, etc.) and improved heuristics.
- v0.2: Quality-of-life
  - Structured JSON output mode for CI consumption.
  - `--apply` flag (optional) to apply manifests inline before tracking.
  - Namespace discovery from manifests when not provided.

## Best practices and notes

- Run kubetracker alongside your deploy step (kubectl/helm). If you must serialize, consider `--apply` in a future version.
- For Helm releases, prefer `kubetracker helm <ns>/<name>` when you want in-cluster rendered manifests to be tracked, or pipe `helm template` to `kubetracker track` if you render locally.
- Keep CRD readiness explicit via `--crd-condition` whenever your controller doesn’t expose a standard `Ready=True`.
- If your CI needs machine readable output, prefer the future JSON mode. For now, you can grep by headers and statuses.

## License

MIT (same as the repository unless stated otherwise).
