# kubemon

A command line tool for monitoring kubernetes resources while deploying releases.

kubemon shows one release live, in a single view:

- **Rollout progress.** Works for Deployments and Argo Rollouts. Canary shows the current step, its weight and pause state. Blue-green shows the active and preview revisions. AnalysisRun metric results appear alongside.
- **Pods grouped by revision**, so you can see which pods are new and which are old. Each pod shows a kubectl-accurate status such as `CrashLoopBackOff` or `Init:0/1`, its ready count, restarts with the last termination reason (`OOMKilled, exit 137`), and why it is not ready.
- **Events of the target only**: probe failures, scheduling, back-off, image pulls.
- **Logs of the new revision's pods**, followed as they start. A crashed container's previous logs are fetched automatically.

It uses watches (informers), not polling, so changes show up as they happen.

## Install

```sh
brew tap seneralkan/tap
brew install --cask kubemon
```

Shell completions for bash, zsh and fish are installed too. Update with `brew upgrade --cask kubemon`.

To build from source (Go 1.27+):

```sh
go install github.com/seneralkan/kubemon@latest
```

## Releasing

Push a tag. GoReleaser then builds macOS and Linux binaries for amd64 and arm64, publishes a GitHub release, and updates `Casks/kubemon.rb` in [seneralkan/homebrew-tap](https://github.com/seneralkan/homebrew-tap):

```sh
git tag v0.1.0 && git push origin v0.1.0
```

One-time setup:

1. `ssh-keygen -t ed25519 -f kubemon-tap -N ""`
2. Add `kubemon-tap.pub` to homebrew-tap as a deploy key with write access.
3. Add `kubemon-tap` (the private key) to this repo as the Actions secret `HOMEBREW_TAP_DEPLOY_KEY`.

Run `make release-snapshot` for a local dry run.

## Usage

```sh
# Argo Rollout or Deployment named checkout, in the current context/namespace
kubemon rollout checkout

# be explicit about the kind, follow the new pods' logs
kubemon rollout deploy/checkout -n payments --logs
kubemon rollout rollout/checkout --context prod-eu --logs -c app

# any label selector
kubemon pods -l app=checkout

# CI: plain output, block until done; exit 0 healthy, 1 failed/aborted, 2 timeout
kubemon rollout rollout/checkout -o plain --wait --timeout 20m

# machine-readable NDJSON stream
kubemon rollout checkout -o json
```

All kubectl connection flags work: `--kubeconfig`, `--context`, `-n`, `--as`, and so on. The `KUBECONFIG` variable and in-cluster config work too.

| Flag | Meaning |
|---|---|
| `-o, --output` | `tui` (default on a terminal), `plain` (default otherwise), `json` |
| `--logs[=new\|all]` | follow container logs; bare `--logs` means newest revision only |
| `-c, --container` | only this container's logs |
| `--tail` | lines to show when a log stream starts (20) |
| `-w, --wait`, `--timeout` | exit when the release finishes |
| `--events-since` | also show events from before start (10m) |

TUI keys: `tab` switches panel, `↑/↓` and `pgup/pgdn` scroll, `e` toggles warnings-only events, `q` quits.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | healthy, or not waiting |
| 1 | release degraded, aborted, or target deleted (with `--wait`) |
| 2 | timed out or stopped before the release finished (with `--wait`) |
| 3 | error, e.g. kubeconfig or target not found |

## Development

### Build from source

You need Go 1.27 or newer. An older `go` also works: it downloads the toolchain named in `go.mod` (`GOTOOLCHAIN=auto`). If `GOROOT` is set in your shell, it must point to a Go install that exists, or be unset.

```sh
git clone https://github.com/seneralkan/kubemon.git && cd kubemon
make build              # → ./bin/kubemon, with version info from git
./bin/kubemon version
./bin/kubemon rollout <name> -n <namespace> --context <context>
make install            # optional: installs kubemon into $(go env GOPATH)/bin
```

`bin/`, `dist/` and `completions/` are build output and are git-ignored. Never commit binaries.

### Common tasks

```sh
make help               # list all targets
make test               # unit tests with the race detector
make lint               # golangci-lint (the binary must be built with Go >= 1.27)
make e2e                # end-to-end tests; needs a throwaway kind cluster with Argo Rollouts
make release-snapshot   # dry-run the GoReleaser build into dist/, publishes nothing
make clean
```

CI (`.github/workflows/ci.yml`) runs on every push to `main` and on every pull request:

- vet, unit tests with coverage, and golangci-lint
- an e2e job that creates a kind cluster, installs Argo Rollouts, and runs `./e2e` against the real binary

Layout: `cmd/` holds the CLI. Under `internal/`:

- `kube`: clients
- `model`: plain types
- `watch`: informers and pod status
- `rollout`: Deployment and Argo status, read via the dynamic client
- `logs`: log follower
- `ui`: tui, plain and json output
- `app`: snapshot stream

Plans and the roadmap live in [.claude/plans](.claude/plans/README.md).
