package sandbox

import (
	"context"
	"testing"
	"time"
)

// The progress operation reads the stage prepare is in without waiting
// behind the worker lock prepare holds, carries the driver's detail, needs
// the chat's binding, and is empty once prepare returns.
func TestProgressReportsStagesWithoutTheWorkerLock(t *testing.T) {
	w, d, _, r := managedFixture(t)
	progress := r
	progress.Operation = "progress"
	if res, err := w.dispatch(context.Background(), progress); err != nil || res.Progress != nil {
		t.Fatalf("idle sandbox reported %+v, %v", res.Progress, err)
	}
	stranger := progress
	stranger.ChatID = "chat-two"
	if _, err := w.dispatch(context.Background(), stranger); err == nil {
		t.Fatal("unbound chat read progress")
	}
	d.createStarted = make(chan struct{})
	d.createBlock = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	prepare := r
	prepare.Operation = "prepare"
	go func() { _, err := w.dispatch(ctx, prepare); done <- err }()
	<-d.createStarted
	answered := make(chan Response, 1)
	go func() { res, _ := w.dispatch(context.Background(), progress); answered <- res }()
	select {
	case res := <-answered:
		if res.Progress == nil || res.Progress.Stage != StageCreating || res.Progress.Detail != "creating the VM" || res.Progress.Since.IsZero() {
			t.Fatalf("progress during creation: %+v", res.Progress)
		}
	case <-time.After(time.Second):
		t.Fatal("progress waited behind the creation")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled creation succeeded")
	}
	if p, ok := w.Progress(r.SandboxID); ok {
		t.Fatalf("progress left behind after prepare: %+v", p)
	}
}

// A stage keeps its start time across detail changes.
func TestProgressSinceIsTheStageStart(t *testing.T) {
	w, _, _, r := managedFixture(t)
	now := time.Unix(1000, 0)
	w.Now = func() time.Time { return now }
	w.setProgress(r.SandboxID, StageCreating, "one")
	now = now.Add(time.Second)
	w.setProgress(r.SandboxID, StageCreating, "two")
	p, _ := w.Progress(r.SandboxID)
	if p.Detail != "two" || !p.Since.Equal(time.Unix(1000, 0)) {
		t.Fatalf("stage start moved: %+v", p)
	}
	now = now.Add(time.Second)
	w.setProgress(r.SandboxID, StageProbing, "")
	p, _ = w.Progress(r.SandboxID)
	if p.Stage != StageProbing || !p.Since.Equal(time.Unix(1002, 0)) {
		t.Fatalf("new stage start: %+v", p)
	}
}

// The cluster operations answer unavailable on a shape without a cluster.
func TestClusterOperationsWithoutACluster(t *testing.T) {
	w, _, _, r := managedFixture(t)
	res, err := w.dispatch(context.Background(), Request{Version: 2, Operation: "cluster.status"})
	if err != nil || res.Cluster == nil || res.Cluster.Available {
		t.Fatalf("cluster.status = %+v, %v", res.Cluster, err)
	}
	if _, err = w.dispatch(context.Background(), Request{Version: 2, Operation: "cluster.logs", Pod: "x"}); err == nil {
		t.Fatal("logs answered without a cluster")
	}
	pod := r
	pod.Operation = "pod"
	if _, err = w.dispatch(context.Background(), pod); err == nil {
		t.Fatal("pod answered without a cluster")
	}
}
