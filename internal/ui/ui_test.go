package ui

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/seneralkan/kubemon/internal/logs"
	"github.com/seneralkan/kubemon/internal/model"
)

func TestPumpBatchesBufferedLines(t *testing.T) {
	lines := make(chan logs.Line, 10)
	for i := range 5 {
		lines <- logs.Line{Text: fmt.Sprint(i)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var batches [][]logs.Line
	pump(ctx, Input{Snapshots: make(chan model.Snapshot), Logs: lines}, func(model.Snapshot) {},
		func(b []logs.Line) { batches = append(batches, b); cancel() }, 20*time.Millisecond)
	if len(batches) != 1 || len(batches[0]) != 5 || batches[0][4].Text != "4" {
		t.Errorf("batches = %v, want one batch of 5 lines", batches)
	}
}

func TestPumpLingerCollectsLateLines(t *testing.T) {
	lines := make(chan logs.Line)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := range 3 {
			lines <- logs.Line{Text: fmt.Sprint(i)}
			time.Sleep(time.Millisecond)
		}
		close(lines) // ends the linger early
	}()
	var batches [][]logs.Line
	pump(ctx, Input{Snapshots: make(chan model.Snapshot), Logs: lines}, func(model.Snapshot) {},
		func(b []logs.Line) { batches = append(batches, b); cancel() }, 10*time.Second)
	if len(batches) != 1 || len(batches[0]) != 3 {
		t.Errorf("batches = %v, want one batch of 3 lines", batches)
	}
}

func TestPumpDrainsInBatches(t *testing.T) {
	lines := make(chan logs.Line, maxDrain+50)
	for i := range maxDrain + 50 {
		lines <- logs.Line{Text: fmt.Sprint(i)}
	}
	snaps := make(chan model.Snapshot, 1)
	snaps <- canarySnapshot(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var sizes []int
	var order []string
	pump(ctx, Input{Snapshots: snaps, Logs: lines},
		func(model.Snapshot) { order = append(order, "snap") },
		func(b []logs.Line) { sizes = append(sizes, len(b)); order = append(order, "logs") }, logLinger)
	if fmt.Sprint(sizes) != "[200 200 200 200 200]" {
		t.Errorf("drain batch sizes = %v, want 5 x %d (capped at maxDrain)", sizes, maxBatch)
	}
	if order[len(order)-1] != "snap" {
		t.Errorf("final snapshot should come last: %v", order)
	}
}
