package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// extractArgs drives the StasisStart dispatch: args[0] decides whether a channel
// is an inbound call, an outbound leg, or an attended-transfer leg. Asterisk
// sends `args` as an untyped JSON array, so every shape has to degrade to nil
// rather than panic — a panic here takes down the event loop for all calls.
func TestExtractArgs(t *testing.T) {
	tests := []struct {
		name  string
		event string
		want  []string
	}{
		{"no args key", `{"type":"StasisStart"}`, nil},
		{"empty args", `{"args":[]}`, []string{}},
		{"inbound call", `{"args":["outbound","42"]}`, []string{"outbound", "42"}},
		{"attended transfer leg", `{"args":["attended_transfer","1783.24"]}`, []string{"attended_transfer", "1783.24"}},
		{"non-array args are ignored", `{"args":"outbound"}`, nil},
		{"non-string entries are skipped", `{"args":["outbound",42,null,"keep"]}`, []string{"outbound", "keep"}},
		{"null args", `{"args":null}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(tt.event), &event); err != nil {
				t.Fatalf("bad test fixture: %v", err)
			}
			got := extractArgs(event)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractArgs(%s) = %#v, want %#v", tt.event, got, tt.want)
			}
		})
	}
}

// The poller only dials when no call is active, and reconcile() is what repairs
// the active set after an ARI reconnect: a StasisEnd missed during a WebSocket
// outage would otherwise leave a phantom active channel and wedge outbound calls
// forever (no dials, retries appearing to do nothing).
func TestCoordinatorReconcileDropsPhantomChannels(t *testing.T) {
	c := newCoordinator()
	c.addActive("live-1")
	c.addActive("ended-during-outage")

	if c.activeCount() != 2 {
		t.Fatalf("activeCount() = %d, want 2", c.activeCount())
	}
	c.reconcile(map[string]struct{}{"live-1": {}})
	if c.activeCount() != 1 {
		t.Errorf("after reconcile activeCount() = %d, want 1", c.activeCount())
	}

	// Asterisk knows about no channel at all → the poller must be free to dial.
	c.reconcile(map[string]struct{}{})
	if c.activeCount() != 0 {
		t.Errorf("after empty reconcile activeCount() = %d, want 0 (poller wedged)", c.activeCount())
	}
}

func TestCoordinatorPendingLifecycle(t *testing.T) {
	c := newCoordinator()
	if c.getPending("nope") != nil {
		t.Error("getPending on unknown channel should return nil")
	}
	p := &outboundPending{}
	c.addPending("out-1", p)
	if c.getPending("out-1") != p {
		t.Error("getPending should return the registered pending call")
	}
	c.removePending("out-1")
	if c.getPending("out-1") != nil {
		t.Error("getPending after removePending should return nil")
	}
}
