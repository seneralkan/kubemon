package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"unicode"

	"github.com/seneralkan/kubemon/internal/logs"
	"github.com/seneralkan/kubemon/internal/model"
)

func TestJSONWritesLowerCamelNDJSON(t *testing.T) {
	snaps := make(chan model.Snapshot, 1)
	lines := make(chan logs.Line, 1)
	s := canarySnapshot(0)
	s.Pods[0] = crashingPod("checkout-abc12-x", "abc12", 2, at(-1))
	snaps <- s
	lines <- logs.Line{Time: at(1), Pod: "checkout-abc12-x", Container: "app", Text: "boom", Previous: true, Err: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	if err := Run(ctx, Options{Mode: ModeJSON, Out: &buf}, Input{Snapshots: snaps, Logs: lines}); err != nil {
		t.Fatal(err)
	}

	var types []string
	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("invalid JSON line %q: %v", sc.Text(), err)
		}
		checkLowerCamel(t, rec)
		types = append(types, rec["type"].(string))
		if rec["type"] == "log" && rec["log"].(map[string]any)["err"] != true {
			t.Errorf("log record should carry err: %v", rec)
		}
		if rec["type"] == "snapshot" {
			pod := rec["snapshot"].(map[string]any)["pods"].([]any)[0].(map[string]any)
			last := pod["containers"].([]any)[0].(map[string]any)["last"].(map[string]any)
			if last["reason"] != "OOMKilled" || last["exitCode"] != 137.0 {
				t.Errorf("last termination = %v", last)
			}
		}
	}
	if len(types) != 2 || types[0] != "log" || types[1] != "snapshot" {
		t.Errorf("record types = %v, want [log snapshot]", types)
	}
}

func checkLowerCamel(t *testing.T, v any) {
	t.Helper()
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if k == "" || !unicode.IsLower(rune(k[0])) {
				t.Errorf("key %q is not lowerCamel", k)
			}
			checkLowerCamel(t, child)
		}
	case []any:
		for _, child := range v {
			checkLowerCamel(t, child)
		}
	}
}
