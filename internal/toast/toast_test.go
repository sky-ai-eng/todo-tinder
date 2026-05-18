package toast

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// fakeHub captures broadcast calls for assertion.
type fakeHub struct {
	events []websocket.Event
}

func (f *fakeHub) Broadcast(evt websocket.Event) {
	f.events = append(f.events, evt)
}

const testOrgID = "00000000-0000-0000-0000-00000000aaaa"

func TestFire_PublishesExpectedShape(t *testing.T) {
	hub := &fakeHub{}

	Error(hub, testOrgID, "Jira poll failed: auth error")

	if len(hub.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(hub.events))
	}
	evt := hub.events[0]
	if evt.Type != "toast" {
		t.Errorf("expected type 'toast', got %q", evt.Type)
	}
	if evt.OrgID != testOrgID {
		t.Errorf("expected OrgID %q stamped on the event, got %q", testOrgID, evt.OrgID)
	}
	payload, ok := evt.Data.(Payload)
	if !ok {
		t.Fatalf("expected Payload data, got %T", evt.Data)
	}
	if payload.Level != LevelError {
		t.Errorf("expected level 'error', got %q", payload.Level)
	}
	if payload.Body != "Jira poll failed: auth error" {
		t.Errorf("unexpected body: %q", payload.Body)
	}
	if payload.Title != "" {
		t.Errorf("expected empty title, got %q", payload.Title)
	}
	if payload.ID == "" {
		t.Error("expected non-empty ID for dedup")
	}
}

func TestFire_EmptyBody_NoOp(t *testing.T) {
	// Empty body should be dropped — a blank toast card is worse than no toast.
	hub := &fakeHub{}
	Info(hub, testOrgID, "")
	if len(hub.events) != 0 {
		t.Errorf("expected 0 events for empty body, got %d", len(hub.events))
	}
}

func TestFire_NilHub_NoOp(t *testing.T) {
	// Untyped nil must not panic — simplifies test setup and makes toast-fires
	// safe in code paths where WS setup was skipped.
	Error(nil, testOrgID, "anything")
	// no panic = pass
}

func TestFire_TypedNilHub_NoOp(t *testing.T) {
	// Typed-nil *websocket.Hub is the realistic pre-wiring state in packages
	// that hold a hub pointer field (spawner, server, etc.). The naive
	// `hub == nil` check on a Broadcaster interface does NOT catch this
	// because the interface's type descriptor is non-nil; only the wrapped
	// pointer is nil. Without the reflect guard, Broadcast would panic.
	var hub *websocket.Hub // nil pointer, assignable to Broadcaster
	Error(hub, testOrgID, "anything")
	// no panic = pass
}

func TestFire_TitledHelper(t *testing.T) {
	hub := &fakeHub{}
	WarningTitled(hub, testOrgID, "Scorer", "batch failed — 3 tasks skipped")

	if len(hub.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(hub.events))
	}
	payload := hub.events[0].Data.(Payload)
	if payload.Title != "Scorer" {
		t.Errorf("unexpected title: %q", payload.Title)
	}
	if payload.Level != LevelWarning {
		t.Errorf("unexpected level: %q", payload.Level)
	}
}

func TestFire_LevelsDistinct(t *testing.T) {
	// Each helper stamps its corresponding level — catches copy-paste errors
	// in the helper definitions where a future maintainer might accidentally
	// have Success() fire LevelInfo.
	hub := &fakeHub{}
	Info(hub, testOrgID, "a")
	Success(hub, testOrgID, "b")
	Warning(hub, testOrgID, "c")
	Error(hub, testOrgID, "d")

	want := []Level{LevelInfo, LevelSuccess, LevelWarning, LevelError}
	if len(hub.events) != len(want) {
		t.Fatalf("expected %d events, got %d", len(want), len(hub.events))
	}
	for i, evt := range hub.events {
		got := evt.Data.(Payload).Level
		if got != want[i] {
			t.Errorf("event %d: expected level %q, got %q", i, want[i], got)
		}
	}
}

// TestFire_EmptyOrg_Passthrough — the empty-OrgID case is the system-wide
// broadcast contract. The Fire helper accepts it without complaint; the
// hub's per-connection filter delivers empty-OrgID events to every client.
// Toasts fired from main.go startup paths legitimately use this shape.
func TestFire_EmptyOrg_Passthrough(t *testing.T) {
	hub := &fakeHub{}
	Info(hub, "", "system-wide announcement")
	if len(hub.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(hub.events))
	}
	if hub.events[0].OrgID != "" {
		t.Errorf("expected empty OrgID, got %q", hub.events[0].OrgID)
	}
}
