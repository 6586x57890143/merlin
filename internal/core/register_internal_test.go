package core

import "testing"

// The comparison that guards registration catches a field lost on the way,
// and ignores only zeros one library writes and the other leaves out.
func TestSameJSONCatchesWhatIsLost(t *testing.T) {
	if !sameJSON([]byte(`{"name":"x","options":null,"nsfw":false}`), []byte(`{"name":"x"}`)) {
		t.Error("zeros written by one side and omitted by the other read as a difference")
	}
	if sameJSON([]byte(`{"name":"x","options":[{"name":"a","required":true}]}`), []byte(`{"name":"x","options":[{"name":"a"}]}`)) {
		t.Error("a lost required flag passed")
	}
	if sameJSON([]byte(`{"name":"x","channel_types":[0]}`), []byte(`{"name":"x"}`)) {
		t.Error("a lost channel type passed")
	}
}
