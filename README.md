# kubetracker

A small Go CLI that tracks Kubernetes resources defined in manifests, showing concise progress and only the logs that matter (pods that fail to start or crashloop). It leverages werf/kubedog under the hood and borrows presentation concepts from werf/nelm while simplifying output.

Highlights:
- Tracks readiness of Deployments, StatefulSets, Jobs, DaemonSets, and arbitrary CRDs.
- Prints progress and events; pod logs are only shown when containers fail (ImagePullBackOff, CrashLoopBackOff, OOMKilled, etc.).
- Works with files, directories, or stdin (great for kubectl, helm, or kustomize pipelines).
- Extensible readiness strategy for CRDs via condition mappings.

Note: This tool does not apply resources; it tracks resources you’re applying via your deployment tooling (kubectl/helm/Argo/etc.). You can run it concurrently with your apply/upgrade and it will follow the resources defined in your manifests.

## Status

- Tracking implemented: readiness for Deployments, StatefulSets, DaemonSets, Jobs; CRDs via conditions (auto-inferred and custom mappings).
- Progress/events/logs wired; healthy pod logs suppressed by default; pod status enrichment avoids UNKNOWN; built-in CRD rules (cert-manager Certificate Ready=True, Apache APISIX ApisixRoute ResourcesAvailable=True).
- Prints READY on success; supports --exit-on-first-fail and global --timeout; optional --failure-grace-period keeps tracking and retries builtin trackers for transient failures (e.g., ImagePullBackOff).

What’s next:
- More per-CRD defaults and condition heuristics.
- Structured JSON output mode for CI.
- Optional --apply mode and additional QoL improvements.


## Installation

- Requirements: Go 1.22+

Build locally:
```/dev/null/example.sh#L1-4
# from repo root
cd jenkins-toolbox/files/kubetracker
go build -o ./bin/kubetracker ./cmd/kubetracker
./bin/kubetracker --help
```

Alternatively, install into GOPATH/bin:
```/dev/null/example.sh#L1-2
cd jenkins-toolbox/files/kubetracker
go install ./cmd/kubetracker
```

## Usage

Basic tracking from a folder:
```/dev/null/example.sh#L1-2
# Track resources defined in ./manifests
kubetracker track -f ./manifests
```

Track from stdin (kustomize):
```/dev/null/example.sh#L1-2
kustomize build ./overlays/prod | kubetracker track -f -
```

Track a Helm release template stream:
```/dev/null/example.sh#L1-2
helm template myapp ./chart --namespace prod | kubetracker track -f - -n prod
```

Common flags:
- `-f, --file` Path to a file, directory, or “-” for stdin. Can be repeated.
- `-n, --namespace` Default namespace for namespaceless manifests.
- `--context, --kubeconfig` Standard kube client overrides.
- `--timeout` Global tracking deadline (e.g. 10m).
- `--failure-grace-period` Grace window to keep tracking and retry builtin trackers after failures (e.g. 2m). 0 disables retry.
- `--include-kind / --exclude-kind` Filter kinds by name (e.g. Deployment, KafkaTopic).
- `--crd-condition` Custom condition mapping for CRDs (see below).
- `--show-events` Show K8s events table along with progress.
- `--only-errors-logs` Only stream pod logs for failing containers (default: true).
- `--exit-on-first-fail` Exit immediately when a resource fails (useful in CI).

Exit codes:
- 0: All tracked resources reached the desired state.
- 1: One or more resources failed.
- 2: Timed out.
- 3: Invalid input (bad YAML, missing kube context, etc.).

## What is tracked

- Native workload kinds: Deployment, StatefulSet, DaemonSet, Job (readiness).
- Pods under the hood for status and logs (but logs are shown only for failing states).
- Arbitrary CRDs: readiness determined by conditions (configurable), or presence-only fallback.

### CRDs readiness strategy

By default, kubetracker tries to infer readiness via status.conditions:
- If a resource exposes `status.conditions` with a recognizable “Ready”, “Available”, “Healthy”, or similar positive condition, kubetracker waits for that condition to be True.
- If such conditions are absent, kubetracker falls back to presence (created) which is often sufficient for controller-managed resources but may be too permissive.

You can customize CRD readiness via `--crd-condition`:
```/dev/null/example.sh#L1-5
# Format: group/version,Kind=ConditionType=ExpectedValue
# Multiple mappings allowed.
kubetracker track -f ./manifests \
  --crd-condition "external-secrets.io/v1beta1,ExternalSecret=Ready=True" \
  --crd-condition "kafka.strimzi.io/v1beta2,KafkaTopic=Ready=True"
```

Examples:
- ExternalSecret (external-secrets.io): `Ready=True`
- KafkaTopic (Strimzi): `Ready=True`
- Certificate (cert-manager.io): `Ready=True` (built-in default)
- ApisixRoute (apisix.apache.org): `ResourcesAvailable=True` (built-in default)

Built-in mappings are applied unless overridden by `--crd-condition`.

If your CRD uses a different condition type or value, point kubetracker to it with another mapping.

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

The log filtering rule:
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
- v0.1: Better CRD support
  - More per-kind defaults (ExternalSecret, KafkaTopic, PrometheusRule, etc.) and improved heuristics.
- v0.2: Quality-of-life
  - Structured JSON output mode for CI consumption.
  - --apply flag (optional) to apply manifests inline before tracking.
  - Namespace discovery from manifests when not provided.

## Best practices and notes

- Run kubetracker alongside your deploy step (kubectl/helm). If you must serialize, consider `--apply` in a future version.
- Keep CRD readiness explicit via `--crd-condition` whenever your controller doesn’t expose a standard “Ready=True”.
- If your CI needs machine readable output, prefer the future JSON mode. For now, you can grep by headers and statuses.

## License

MIT (same as the repository unless stated otherwise).
