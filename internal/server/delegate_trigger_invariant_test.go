package server

// Pins the load-bearing invariant that delegate trigger_type is
// server-side provenance, never derived from caller input. If a
// future contributor adds a "trigger_type" / "triggerType" JSON tag
// to a delegate-request struct, a multi-tenant caller could set
// triggerType="event" and bypass resolvePrompt's app-pool branch,
// reading prompts they have no RLS visibility into. This test
// reflects on every delegate-related request struct and refuses
// any tag that would open that hole.
//
// New delegate-related request types must be added to the
// `delegateRequestTypes` list below so the assertion sees them.
// The DelegateOpts.TriggerType doc comment in internal/delegate
// names this file directly so the linkage is discoverable from
// both sides.

import (
	"reflect"
	"strings"
	"testing"
)

func TestDelegateRequestStructs_NoTriggerTypeFieldFromAPI(t *testing.T) {
	// Add every request body type a delegate-related HTTP handler
	// unmarshals into. The reflection check refuses any tag that
	// would let a caller drive Spawner.Delegate's triggerType branch.
	delegateRequestTypes := []any{
		factoryDelegateRequest{},
		swipeRequest{},
	}

	forbidden := map[string]bool{
		"trigger_type": true,
		"triggertype":  true,
	}

	for _, raw := range delegateRequestTypes {
		typ := reflect.TypeOf(raw)
		t.Run(typ.Name(), func(t *testing.T) {
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				// JSON tag is everything before the first comma in the
				// `json:"..."` value; lowercase to catch case-shifted
				// variants like "TriggerType".
				tag := field.Tag.Get("json")
				if tag == "" {
					continue
				}
				name := strings.ToLower(strings.SplitN(tag, ",", 2)[0])
				if forbidden[name] {
					t.Errorf("%s.%s has JSON tag %q — delegate trigger_type must be server-side provenance, never caller input (see internal/delegate/delegate.go DelegateOpts.TriggerType)", typ.Name(), field.Name, tag)
				}
			}
		})
	}
}
