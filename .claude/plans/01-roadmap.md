# 01 — Roadmap (Phase 0–6)

**Product direction:** kubemon is a personal release monitoring tool. The goal is to answer a developer's questions during a deploy, "what is happening right now, why, did my change break it", **on a single screen and instantly**. ArgoCD, `kubectl argo rollouts`, stern and k9s should keep doing what they already do well. kubemon combines them and adds the layer they lack (see [02-developer-insights.md](02-developer-insights.md)).

## Starting state (commit cf2f961)

The code is 262 lines. `kubemon pods` sends a `List` every 2 seconds and prints pods by phase.

Critical issues:
- Polling instead of watch.
- Only `Phase` is checked, so a CrashLoopBackOff pod shows as "Running".
- `os.Exit(1)` is called when there are 0 pods.
- Library code contains `log.Fatalf`.
- The clientset is rebuilt on every tick.
- Because of a map, row order changes every time.
- No `--context` and `KUBECONFIG` support.
- `exec("clear")` makes the screen flicker.
- Ctrl+C does not shut down cleanly.
- No README, tests or CI.

## Version upgrade (verified)

| | Old | New |
|---|---|---|
| Go | 1.22.5 | 1.27.4 |
| k8s.io/client-go, apimachinery | v0.31.0 | v0.37.3 |
| cobra | v1.8.1 | v1.10.2 |

On a scratch copy, `go mod edit -go=1.27`, `go get` and `go mod tidy` were run. `go build ./...` and `go vet ./...` passed without code changes.

## Package layout (implemented, `feat/release-monitor`)

Identical to the layout in the roadmap, except that `internal/app` (the shared run flow of the commands) was added.

```
cmd/                 pods, rollout commands (cobra); run.go shared flow and
                     --wait exit codes: 0 healthy, 1 failed,
                     2 timeout or stopped, 3 error
internal/kube        client: cli-runtime genericclioptions.ConfigFlags
                     (--kubeconfig, --context, -n, KUBECONFIG, in-cluster)
internal/model       snapshot types: Pod, Revision, Event, RolloutStatus
internal/watch       SharedInformer-based watcher (pods, replicasets, events,
                     optional dynamic CRDs), debounced snapshot on change
internal/rollout     Deployment + Argo Rollout status (dynamic/unstructured;
                     the argo-rollouts Go module is NOT IMPORTED)
internal/logs        follows new revision pod logs, previous log on crash
internal/ui          bubbletea TUI + plain/CI renderer, -o json
internal/app         command flow: combines resolve, watch, logs, ui
```

`internal/insight` (see 02) does not exist yet; it will be added when starting I1.

## Phases

### Phase 0: Cleanup and upgrade — done (awaiting commit)
- Go, client-go and cobra upgrade. `.gitignore`, README, Makefile, golangci-lint, GitHub Actions CI.
- Returning errors instead of `Fatal`/`Exit`, using a single client, ordered output.

### Phase 1: Correct core — done (awaiting commit)
- `internal/kube`: same connection behavior as kubectl via `ConfigFlags`. Closes the old TODO.
- `internal/watch`: informer instead of polling. The 0-pod case is treated as normal.
- Pod row: Ready x/y, restart count, waiting or termination reason, age, node, revision (`pod-template-hash`).
- Clean shutdown with `signal.NotifyContext`. Tests with `kubernetes/fake`.

### Phase 2: Release view — done (awaiting commit)
- `kubemon rollout <deploy|rollout>`: pods are grouped by old and new ReplicaSet, progress status is shown.
- Target-related events stream live (`FailedScheduling`, `Unhealthy`, `BackOff`, `FailedMount`...).
- For Argo Rollouts canary: step, setWeight, pause, AnalysisRun. For blue-green: active and preview service, waiting for promote.

### Phase 3: Logs — done (awaiting commit)
- Automatically tail logs of new revision pods as they come up: `--logs`, `--container`, colored prefix.
- `previous` log for a crashing container.

### Phase 4: Output modes — done (awaiting commit)
- With a TTY, bubbletea TUI: rollout summary on top, pods in the middle, event and log panels at the bottom.
- Without a TTY or in CI: line stream, `--wait --timeout`, `-o json`. Exit codes: 0 healthy, 1 failed, 2 timeout or stopped, 3 error.

**Flags implemented in Phase 0–4:** TUI/plain/json output, `--logs[=new|all]` (at most 20 pods), `-c`, `--tail` (`-1` = all), `--events-since`, `--wait`, `--timeout`.

**Fixed after the independent review:**
- `pods --wait` settling semantics.
- Fast, clear error when RBAC is missing: a `list` probe before the informers.
- The event filter no longer matches sibling workloads by prefix.
- Events wake the stream only if they relate to the target.
- Log open errors are reported once and backoff is applied. Log streams stop on Forbidden.
- Dropped log streams reconnect automatically.
- The TUI draws only visible rows and processes log lines in batches.
- Escape sequence cleaning (sanitize).
- klog silenced in the TUI.
- `--timeout` also covers Resolve and cache sync.
- If a bare name is given and the Argo CRDs are Forbidden, it falls back to Deployment.
- A paused but completed Deployment counts as Healthy.
- `CurrentHash` is used only if the RS revision matches the Deployment's.
- `series.count` is taken into account in event counting.

No smoke test on a real cluster yet. Remaining gaps: [03-open-items.md](03-open-items.md).

### Phase 5: ArgoCD integration — todo
- `kubemon app <argocd-app>`: find the workloads via `Application.status.resources` and watch them all together.
- Sync and health status, phase and messages of the sync operation.

### Phase 6: Distribution — todo
- goreleaser and Homebrew tap.
- The binary name will be `kubectl-monitor` and a krew manifest will be written, so it runs as `kubectl monitor`.

## Rules
- Think twice before adding an external dependency. The dynamic client is enough for CRDs.
- Every package must remain testable with a `fake` client.
- The user interface (TUI or plain) is kept separate from data collection (watch/model).
