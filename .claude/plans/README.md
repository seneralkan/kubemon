# kubemon plans

kubemon is the developer's own tool: during a release it gives, from a software developer's point of view, the information that the ArgoCD UI, `kubectl rollout status`, `kubectl argo rollouts`, stern and k9s **don't** give. It does not try to replace these tools.

| Plan | Topic | Status |
|---|---|---|
| [01-roadmap.md](01-roadmap.md) | Phase 0–6 main roadmap, package layout, version upgrade | Phase 0–4 done (awaiting commit) · Phase 5–6 todo |
| [02-developer-insights.md](02-developer-insights.md) | Developer-specific insights at release time (ones other tools don't have) | todo |
| [03-open-items.md](03-open-items.md) | Known gaps and next steps (after Phase 0–4) | todo |

Statuses: `todo` (not started) · `in-progress` (being worked on) · `done (awaiting commit)` (code ready on the `feat/release-monitor` branch, awaiting user review) · `done` (finished, merged into main)

When adding a new plan, name the file `NN-topic.md` and add a row to this table.
