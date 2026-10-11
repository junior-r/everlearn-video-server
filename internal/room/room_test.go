package room

import "testing"

func TestRoomNamesCarryTheSessionID(t *testing.T) {
	const session = "0199d3c2-7b1e-7c3a-9f41-2f6f5e0c1a11"
	if got, ok := SessionID(RoomName(session)); !ok || got != session {
		t.Fatalf("SessionID = %q, %v", got, ok)
	}
	for _, name := range []string{"", "lobby", "class-", "class-not-a-uuid", session} {
		if _, ok := SessionID(name); ok {
			t.Errorf("%q was taken for a class room", name)
		}
	}
	if !IsMembership(session) || IsMembership("recorder-bot") {
		t.Error("IsMembership must accept only UUIDs")
	}
}
