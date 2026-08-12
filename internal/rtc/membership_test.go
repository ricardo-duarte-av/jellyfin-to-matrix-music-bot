package rtc

import (
	"testing"

	"maunium.net/go/mautrix/event"
)

// The class is load-bearing. mautrix keys both its sync listener map and its
// room state map by the whole event.Type struct, and stamps StateEventType onto
// incoming state events. A type whose class is UnknownEventType — which is what
// event.NewEventType returns for any unstable type — silently matches nothing:
// sync handlers never fire and state lookups return empty.
func TestCallMemberEventTypeIsAStateEvent(t *testing.T) {
	if got := CallMemberEventType.Class; got != event.StateEventType {
		t.Errorf("CallMemberEventType.Class = %v; want StateEventType", got)
	}
	if got := CallMemberEventType.Type; got != "org.matrix.msc3401.call.member" {
		t.Errorf("CallMemberEventType.Type = %q; unexpected", got)
	}

	// Guard the specific trap: constructing it the obvious way is wrong, and
	// the two must not be interchangeable.
	guessed := event.NewEventType("org.matrix.msc3401.call.member")
	if guessed == CallMemberEventType {
		t.Skip("mautrix now recognises this type; the explicit class is redundant")
	}
	if guessed.Class == event.StateEventType {
		t.Error("NewEventType now infers the state class; this test needs revisiting")
	}
}

// A membership keyed the way mautrix keys incoming state events must be found
// by a lookup using our type.
func TestCallMemberEventTypeMatchesStateMapKey(t *testing.T) {
	incoming := event.Type{Type: "org.matrix.msc3401.call.member"}
	incoming.Class = event.StateEventType // what mautrix does on receipt

	state := map[event.Type]map[string]*event.Event{
		incoming: {"_@someone:example.org_DEV": {}},
	}
	if _, ok := state[CallMemberEventType]; !ok {
		t.Error("lookup with CallMemberEventType missed a state map keyed as mautrix keys it")
	}
}

// Ejecting someone means retracting every membership they hold, so the key
// formats in the wild all have to be recognised — without one user's ID
// swallowing another whose ID starts the same way.
func TestMembershipBelongsTo(t *testing.T) {
	const bob = "@bob:example.org"
	for _, key := range []string{
		"_@bob:example.org_DEVICE",
		"@bob:example.org_DEVICE",
		"@bob:example.org",
	} {
		if !MembershipBelongsTo(key, bob) {
			t.Errorf("MembershipBelongsTo(%q, %s) = false; want true", key, bob)
		}
	}
	for _, key := range []string{
		"_@bobby:example.org_DEVICE",
		"@bobby:example.org",
		"_@alice:example.org_DEVICE",
		"",
	} {
		if MembershipBelongsTo(key, bob) {
			t.Errorf("MembershipBelongsTo(%q, %s) = true; want false", key, bob)
		}
	}
}
