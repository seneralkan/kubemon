//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// No traffic routing: with 2 replicas, setWeight 50 runs one canary pod.

func TestArgoCanarySucceeds(t *testing.T) {
	t.Parallel()
	requireArgo(t)
	e := newEnv(t)
	e.createRollout(imageOld, "setWeight: 50", "pause: {duration: 5s}")
	e.setRolloutImage(imageNew)

	r := e.kubemon(5*time.Minute, "rollout", "rollout/web", "-o", "plain", "--wait", "--timeout", "4m")

	expectExit(t, r, 0)
	expectContains(t, "stdout", r.stdout, "Rollout "+e.ns+"/web", "Canary", "step ", "1.28-alpine")
	if !hasLine(r.stdout, "final Rollout "+e.ns+"/web: Healthy") {
		t.Errorf("stdout has no final Healthy summary line")
	}
}

// TestArgoAbort aborts a canary paused indefinitely the way
// `kubectl argo rollouts abort` does: by setting status.abort.
func TestArgoAbort(t *testing.T) {
	t.Parallel()
	requireArgo(t)
	e := newEnv(t)
	e.createRollout(imageOld, "setWeight: 50", "pause: {}")
	e.setRolloutImage(imageNew)
	e.waitRolloutPhase("Paused", 3*time.Minute)

	p := e.startKubemon("rollout", "rollout/web", "-o", "plain", "--wait", "--timeout", "3m")
	header := "Rollout " + e.ns + "/web"
	waitFor(t, time.Minute, "kubemon to print its header", func() bool {
		return strings.Contains(p.stdout.String(), header)
	})
	e.kubectl("patch", "rollouts.argoproj.io/web", "--subresource=status", "--type", "merge",
		"-p", `{"status":{"abort":true}}`)

	r := p.wait(2 * time.Minute)

	expectExit(t, r, 1)
	expectContains(t, "stdout", r.stdout, "paused", "Aborted")
	expectContains(t, "stderr", r.stderr, "release failed")
}
