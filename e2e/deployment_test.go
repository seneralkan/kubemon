//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDeploymentRolloutSucceeds(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createDeployment(imageOld)
	e.kubectl("set", "image", "deploy/web", "web="+imageNew)

	r := e.kubemon(4*time.Minute, "rollout", "deploy/web", "-o", "plain", "--wait", "--timeout", "3m")

	expectExit(t, r, 0)
	expectContains(t, "stdout", r.stdout, "Deployment "+e.ns+"/web", "1.28-alpine")
	if !hasLine(r.stdout, "final Deployment "+e.ns+"/web: Healthy") {
		t.Errorf("stdout has no final Healthy summary line")
	}
}

func TestDeploymentRolloutFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createDeployment(imageOld)
	// The short deadline is set together with the bad image, so a slow first
	// image pull cannot fail the initial rollout.
	e.kubectl("patch", "deploy/web", "--type", "strategic", "-p", fmt.Sprintf(
		`{"spec":{"progressDeadlineSeconds":20,"template":{"spec":{"containers":[{"name":"web","image":%q}]}}}}`, imageBad))

	r := e.kubemon(4*time.Minute, "rollout", "deploy/web", "-o", "plain", "--wait", "--timeout", "3m")

	expectExit(t, r, 1)
	expectAny(t, "stdout", r.stdout, "ImagePullBackOff", "ErrImagePull")
	if !hasLine(r.stdout, "final Deployment "+e.ns+"/web: Degraded") {
		t.Errorf("stdout has no final Degraded summary line")
	}
	expectContains(t, "stderr", r.stderr, "release failed")
}

func TestDeploymentRolloutTimesOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createDeployment(imageOld)
	e.kubectl("set", "image", "deploy/web", "web="+imageBad) // default 600s progress deadline

	start := time.Now()
	r := e.kubemon(2*time.Minute, "rollout", "deploy/web", "-o", "plain", "--wait", "--timeout", "20s")

	expectExit(t, r, 2)
	expectContains(t, "stderr", r.stderr, "timed out after 20s")
	if d := time.Since(start); d < 19*time.Second {
		t.Errorf("exited after %s, before the 20s timeout", d.Round(time.Second))
	}
}

func TestRolloutTargetNotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, tc := range []struct{ ref, msg string }{
		{"nope", "no Rollout or Deployment named nope in namespace " + e.ns},
		{"deploy/nope", "no Deployment named nope in namespace " + e.ns},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			r := e.kubemon(time.Minute, "rollout", tc.ref, "-o", "plain", "--wait", "--timeout", "30s")
			expectExit(t, r, 3)
			expectContains(t, "stderr", r.stderr, tc.msg)
		})
	}
}

// TestSettledDeployment watches a Deployment that is already rolled out: both
// the selector mode and the rollout mode must report it done straight away.
func TestSettledDeployment(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.createDeployment(imageOld)

	t.Run("pods", func(t *testing.T) {
		r := e.kubemon(2*time.Minute, "pods", "-l", "app=web", "-o", "plain", "--wait", "--timeout", "1m")
		expectExit(t, r, 0)
		expectContains(t, "stdout", r.stdout, "selector app=web in "+e.ns)
		if !hasLine(r.stdout, "final selector app=web in "+e.ns, "2/2 ready") {
			t.Errorf("stdout has no final 2/2 ready summary line")
		}
	})

	t.Run("json", func(t *testing.T) {
		r := e.kubemon(2*time.Minute, "rollout", "deploy/web", "-o", "json", "--wait", "--timeout", "1m")
		expectExit(t, r, 0)
		snapshots, phase := 0, ""
		for line := range strings.Lines(r.stdout) {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec struct {
				Type     string `json:"type"`
				Snapshot *struct {
					Target struct {
						Kind string `json:"kind"`
						Name string `json:"name"`
					} `json:"target"`
					Rollout *struct {
						Phase string `json:"phase"`
					} `json:"rollout"`
				} `json:"snapshot"`
			}
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("stdout line is not JSON: %v\n%s", err, line)
			}
			if rec.Type != "snapshot" {
				continue
			}
			snapshots++
			if s := rec.Snapshot; s == nil || s.Target.Kind != "Deployment" || s.Target.Name != "web" {
				t.Errorf("snapshot without Deployment web target: %s", line)
			} else if s.Rollout != nil {
				phase = s.Rollout.Phase
			}
		}
		if snapshots == 0 {
			t.Fatalf("no snapshot records in stdout")
		}
		if phase != "Healthy" {
			t.Errorf("last snapshot phase %q, want Healthy", phase)
		}
	})
}
