# 03 — Known gaps and next steps

**Status:** todo. Gaps remaining after Phase 0–4 (`feat/release-monitor`, awaiting commit) and the independent review.

Effort: S ≈ half a day, M ≈ 1–2 days, L ≈ 3+ days.

### O1. No smoke test on a real cluster (M)
- **What:** kind did not come up on OrbStack (kubeadm timeout, also with the k8s 1.37 and 1.34 node images).
- **Impact:** Phase 0–4 was verified only with `fake` client tests. Real API server, RBAC and Argo behavior were not tried.
- **Suggestion:** Enable OrbStack's built-in Kubernetes, or run kind in CI (GitHub Actions) and add an e2e test with Argo Rollouts installed.

### O2. Argo Rollout using `spec.workloadRef` without `spec.selector` (S)
- **What:** Resolve cannot find a selector and returns an error.
- **Impact:** Rollouts that reference a Deployment via workloadRef cannot be watched.
- **Suggestion:** Fall back to `status.selector`, and if that is missing too, to the selector of the referenced Deployment.

### O3. RBAC with `list` permission but no `watch` permission (S)
- **What:** The `list` probe passes but cache sync hangs.
- **Impact:** Waiting until `--timeout` expires instead of a clear error.
- **Suggestion:** Add `SetWatchErrorHandler` to the informers; on Forbidden, cancel the context and return a clear error.

### O4. `--logs=new` with a bare selector (S)
- **What:** The highest revision number is picked across ReplicaSets belonging to different owners.
- **Impact:** If the selector covers more than one workload, logs of the wrong "new" revision may be followed.
- **Suggestion:** Group revisions by controller owner and take the newest for each owner.

### O5. `pods --wait` right after `kubectl apply` (S, documented)
- **What:** Since there is no rollout object to look at, it can return exit 0 before the new ReplicaSet is created.
- **Impact:** False success in CI.
- **Suggestion:** Documented; `rollout --wait` is recommended in CI. If desired, a minimum observation period can be added for `pods --wait`.

### O6. Duplicate lines when the log follower reconnects (S)
- **What:** If a reconnect coincides with a restarted container, up to `--tail` lines may be printed again on the next fresh start.
- **Impact:** Duplicate lines in the log (no data loss).
- **Suggestion:** Track container ID / restart count and the last line's timestamp, and use `SinceTime` when reconnecting.

### O7. Local golangci-lint built with Go 1.26 (S)
- **What:** It cannot lint the go 1.27 module.
- **Impact:** Lint runs only in CI (the action uses the latest version).
- **Suggestion:** Reinstall golangci-lint with Go 1.27.

### O8. `internal/insight` package does not exist yet (S)
- **What:** 02-developer-insights proposes this package; it is not in the current layout.
- **Impact:** None, a plan discrepancy.
- **Suggestion:** Add the `internal/insight` package when starting I1 and update the package layout in 01.
