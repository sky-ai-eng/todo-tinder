package delegate

import (
	"context"
	"sync/atomic"
	"testing"
)

// TestAwaitClassification_NilSafe guards the contract that callers
// (setupGitHub / setupJira) can invoke awaitClassification without
// first having to check whether SetWaitForClassification was called.
// Tests and pre-classifier configurations leave the hook unset.
func TestAwaitClassification_NilSafe(t *testing.T) {
	s := &Spawner{}
	s.awaitClassification(context.Background(), "entity-1") // must not panic
}

// TestAwaitClassification_InvokesHookWithEntityID verifies the hook
// is called with exactly the entityID the spawner passes through, so
// the projectclassify.WaitFor wired in main.go can poll the right
// row.
func TestAwaitClassification_InvokesHookWithEntityID(t *testing.T) {
	s := &Spawner{}
	var got atomic.Value
	var calls atomic.Int32
	s.SetWaitForClassification(func(_ context.Context, entityID string) {
		got.Store(entityID)
		calls.Add(1)
	})

	s.awaitClassification(context.Background(), "entity-42")

	if calls.Load() != 1 {
		t.Errorf("hook calls = %d, want 1", calls.Load())
	}
	if v, _ := got.Load().(string); v != "entity-42" {
		t.Errorf("hook called with %q, want entity-42", v)
	}
}

// TestAwaitClassification_ForwardsCtx verifies the spawner threads its
// own ctx into the hook, so a cancelled run breaks out of the wait
// instead of blocking the full classifier timeout.
func TestAwaitClassification_ForwardsCtx(t *testing.T) {
	s := &Spawner{}
	var sawCancel atomic.Bool
	s.SetWaitForClassification(func(ctx context.Context, _ string) {
		<-ctx.Done()
		sawCancel.Store(true)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.awaitClassification(ctx, "entity-7")

	if !sawCancel.Load() {
		t.Errorf("hook did not observe ctx cancellation")
	}
}
