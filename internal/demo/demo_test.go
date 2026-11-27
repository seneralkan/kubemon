package demo

import (
	"context"
	"testing"
	"time"

	"github.com/seneralkan/kubemon/internal/model"
)

func play(t *testing.T, fail bool) []model.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snaps, _ := Play(ctx, Options{Fail: fail, Step: time.Millisecond})
	var out []model.Snapshot
	for s := range snaps {
		out = append(out, s)
	}
	if ctx.Err() != nil {
		t.Fatal("script did not finish")
	}
	return out
}

func TestScriptSucceeds(t *testing.T) {
	snaps := play(t, false)
	last := snaps[len(snaps)-1]
	if !last.Rollout.Done() || last.Rollout.Weight != 100 || last.Rollout.StableHash != canaryHash {
		t.Fatalf("final rollout = %+v, want Healthy at 100%% on the new revision", last.Rollout)
	}
	sawCrash := false
	for _, s := range snaps {
		for _, p := range s.Pods {
			sawCrash = sawCrash || p.Status == "CrashLoopBackOff"
		}
	}
	if !sawCrash {
		t.Error("the script should show a CrashLoopBackOff pod")
	}
	for _, p := range last.Pods {
		if p.Hash != canaryHash || !p.Healthy() {
			t.Errorf("leftover pod %s (%s, %s)", p.Name, p.Hash, p.Status)
		}
	}
}

func TestScriptFails(t *testing.T) {
	last := play(t, true)
	end := last[len(last)-1]
	if !end.Rollout.Failed() || end.Rollout.Analysis[0].Phase != "Failed" {
		t.Fatalf("final rollout = %+v, want Aborted with a failed analysis", end.Rollout)
	}
}
