# 02 — Developer-specific release insights

**Status:** todo. Starts after Phase 4 is finished. Uses the watch, model, logs and ui layers from Phase 1–4.

**Question:** During a deploy a developer asks "what did my change do, why is it stuck, is it worse than before". ArgoCD reports status ("Degraded"), stern streams logs, k9s shows everything but you have to find it all by hand. kubemon instead shows the **answer** to these questions.

Common principle: the informers and log follower already collect the data. Insights will be pure functions (`internal/insight`) running on top of this data, easy to test and used both in the UI and in report output.

Effort: S ≈ half a day, M ≈ 1–2 days, L ≈ 3+ days.

---

## Next (right after Phase 4, in priority order)

### I1. Crash card (S–M)
- **What it shows:** A single card when a container crashes. It contains the exit code and its meaning (137 = SIGKILL/OOM, 143 = SIGTERM, 1 = application error, 127/126 = command not found/not executable), the memory limit value if `OOMKilled`, the restart count and the last crash time. The last N log lines of the `previous` container are fetched automatically. The **first** line matching `panic|fatal|exception|error` is highlighted, and Go panics and Java stack traces are folded.
- **Why others don't show it:** stern does not fetch the `--previous` log on its own. ArgoCD only says "Degraded". In k9s you have to navigate to the pod, container and previous log by hand.
- **Source:** `pod.status.containerStatuses[].lastState.terminated{exitCode,reason,finishedAt}`, `state.waiting.reason`, `spec.containers[].resources.limits.memory`, `GetLogs(Previous:true, TailLines:N)`.

### I2. Noise filter (S)
- **What it shows:** By default shows only pods, events and logs belonging to the **new revision**. Repeated events are counted and collapsed into a single line as `×12`. Terminating noise from old pods is reduced to a single counter. One key toggles everything on.
- **Why others don't show it:** `kubectl get events` dumps everything in the namespace. stern filters by label, not by revision.
- **Source:** `pod-template-hash` / `rollouts-pod-template-hash` label, `event.involvedObject`, `event.count` and `series.count`.

### I3. "Why is it stuck?" diagnosis (M)
- **What it shows:** If the rollout does not progress for N seconds, a one-line verdict and its evidence:
  - The `FailedScheduling` message is parsed: Insufficient cpu/memory, taint, affinity, PVC unbound.
  - `ErrImagePull` is split: `not found` means wrong tag, `unauthorized` means a pull secret problem.
  - If the ReplicaSet gets `FailedCreate`, a ResourceQuota or LimitRange has been exceeded.
  - If `progressDeadlineSeconds` is about to expire, a countdown is shown.
  - If Argo is waiting for a manual promote, or the AnalysisRun is `Inconclusive`, this is stated.
  - If a PDB is blocking eviction, it is shown.
- **Why others don't show it:** They all show the raw message in different places; none of them ties these to a single cause.
- **Source:** Pod and RS events, `deployment.status.conditions` (`Progressing`/`ProgressDeadlineExceeded`), `rs.status.conditions` (`ReplicaFailure`), `resourcequotas.status`, `policy/v1 PodDisruptionBudget.status`, Rollout `status.pauseConditions`.

### I4. Revision diff: old vs new (M)
- **What it shows:** At the start of the rollout, shows the pod template difference between the stable RS and the new RS: image (tag **and** digest), env (added, removed, changed; including `valueFrom` references), `envFrom` ConfigMap/Secret names, requests/limits, probes, command/args, volumes. It also says when a referenced ConfigMap/Secret was changed right before the rollout ("ConfigMap X changed 3 min ago").
- **Why others don't show it:** ArgoCD compares git with the live state, not the running old revision with the new one. `kubectl rollout history` only gives a number.
- **Source:** `apps/v1 ReplicaSet.spec.template` (old and new RS), ConfigMap/Secret `metadata.managedFields[].time` (for Secrets only the time; the content is never shown).

### I5. Probe failure explanation (S)
- **What it shows:** From the `Unhealthy` event, the probe type (startup, readiness, liveness), target (path:port or exec) and last message ("503", "connection refused", "timeout") are extracted. Together with `initialDelaySeconds`, `periodSeconds` and `failureThreshold` from the spec, it computes how many seconds of tolerance the application gets. It also warns separately if restarts are caused by liveness.
- **Why others don't show it:** The event is just raw text and is not combined with the spec.
- **Source:** Event `reason=Unhealthy` message, `spec.containers[].{startup,readiness,liveness}Probe`.

### I6. Pod lifeline and "time to ready" (M)
- **What it shows:** For each new pod, these durations: created → scheduled → image pull (with duration) → init containers → started → ready. The new revision's median time-to-ready is compared with the old revision ("starts 2.3x slower than before").
- **Why others don't show it:** No tool breaks startup time down into its stages. Regressions caused by JVM, migrations or cache warmup go unnoticed.
- **Source:** `status.conditions[PodScheduled|Initialized|Ready].lastTransitionTime`, `containerStatuses[].state.running.startedAt`, `Pulling`/`Pulled` events (the message says "in 3.2s"), `creationTimestamp`.

### I7. Argo step timeline and analysis values (M)
- **What it shows:** Canary steps are listed with start time, duration and weight, and the time spent waiting in pauses is visible. The **values** of AnalysisRun metrics are shown: "error-rate 0.8% (limit 1%), 3/5 measurements ok, failureLimit 2".
- **Why others don't show it:** The Argo plugin only shows the current step. Analysis values are buried in the YAML.
- **Source:** Rollout `status.currentStepIndex`, `spec.strategy.canary.steps`, `status.pauseConditions[].startTime`. AnalysisRun `status.metricResults[].measurements[].{value,phase,finishedAt}` and `spec.metrics[].{successCondition,failureLimit}`. Since step transition times are not kept in status, kubemon records the transitions it observes itself.

### I8. Release summary (S)
- **What it shows:** When the rollout finishes, or with `--report file.md|json`, produces a summary: target, old and new image with digest, total duration, step timeline, crashes and their causes, Warning event counts, result. It can be pasted directly into a PR or Slack.
- **Source:** Data collected by I1–I7. No new API calls needed.

### I9. Same tag, different digest, and version label (S)
- **What it shows:** Warns if the tag stayed the same but the digest changed (the `latest` trap). Catches some nodes running the old digest. Shows `app.kubernetes.io/version` and commit SHA annotations (for example a `git-sha` written by CI).
- **Source:** `containerStatuses[].imageID`, pod/RS labels and annotations.

### I10. Shutdown behavior of old pods (M)
- **What it shows:** For each old pod, the time from `deletionTimestamp` until the container terminates, and the exit code. Pods killed with SIGKILL (137) after `terminationGracePeriodSeconds` expired are flagged. This shows that the application does not handle SIGTERM properly and that requests are lost during the deploy.
- **Why others don't show it:** No trace remains once the pod is deleted. kubemon can capture informer updates before deletion.
- **Source:** Informer update and delete events: `metadata.deletionTimestamp`, `containerStatuses[].state.terminated`, `spec.terminationGracePeriodSeconds`.

### I11. Revision comparison table (M–L, depends on I2 and Phase 3)
- **What it shows:** Old and new pods side by side: ready ratio, restart count, readiness flap count (how many times the Ready condition changed), error log lines per minute and median time-to-ready.
- **Why others don't show it:** Argo analysis looks at Prometheus; there is no view comparing revisions at the pod level.
- **Source:** Ready transitions observed in the informer and regex counters in the log follower.

---

## Later

| # | Insight | Source | Effort |
|---|---|---|---|
| L1 | **Is traffic actually arriving:** Warns if a pod is Ready but not in the EndpointSlice (selector mismatch). In blue-green, shows which RS the active and preview services point to. | `discovery.k8s.io/v1 EndpointSlice` (`endpoints[].conditions.ready`, `targetRef`) | M |
| L2 | **HPA interaction:** Shows the HPA scaling during the rollout, the gap between desired and current, and the fluctuation that arises together with maxSurge. | `autoscaling/v2 HPA.status`, `SuccessfulRescale` events | S–M |
| L3 | **Resource usage new vs old:** CPU and memory per revision. Warns **before** OOM if memory is approaching the limit. | `metrics.k8s.io/v1beta1 PodMetrics` (requires metrics-server) | M |
| L4 | **Local release history:** I8 summaries are stored in `~/.kubemon/history`. Comparisons are made such as "this release is worse than the last 5 releases in restart count and startup time". | Local file | M |
| L5 | **Hook and migration visibility:** Logs and durations of ArgoCD PreSync and Sync hook Jobs and of init containers. Migrations are often the real cause. Done together with Phase 5. | `batch/v1 Job`, `argocd.argoproj.io/hook` annotation | M |
| L6 | **Notification:** Desktop notification or terminal bell when the rollout finishes, fails or waits for promote. | Local | S |
| L7 | **Node and zone distribution:** Are all new pods on a single node, are crashes happening on only one node? | `pod.spec.nodeName`, node `topology.kubernetes.io/zone` label | S |
| L8 | **Stale config:** If a referenced ConfigMap or Secret changed after the pod started, says the pod is running with the old config. | Comparing `managedFields[].time` with `startedAt` | S |
| L9 | **Image OCI labels:** Reads `org.opencontainers.image.revision` and `source` from the registry and shows a commit link. Deferred because it requires registry access and auth. | Registry API (`go-containerregistry`) | L |

## Implementation notes
- New package `internal/insight`: input is the model snapshot and the observed transition history, output is `[]Finding{Severity, Title, Evidence, Hint}`. The UI and the report use the same `Finding` type.
- Transition history (Ready flaps, step times, shutdown durations) accumulates only while kubemon is running. This limitation must be stated explicitly in the docs.
- Secret contents are never read or shown; only the name and change time are used.
