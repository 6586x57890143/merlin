package core

import (
	"encoding/json"
	"testing"

	"github.com/disgoorg/disgo/gateway"
)

// A channel type Discord added after our disgo was cut must not fail the
// whole GUILD_CREATE: under disgo 0.19 type 20 did, and the gateway
// reconnected forever with merlin offline in every guild.
func TestGuildCreateWithAnUnknownChannelType(t *testing.T) {
	raw := `{"id":"100","name":"g","channels":[{"id":"1","type":0,"name":"general"},{"id":"2","type":20,"name":"new"}]}`
	var g gateway.EventGuildCreate
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatalf("GUILD_CREATE with a type 20 channel: %v", err)
	}
	if len(g.Channels) != 2 {
		t.Fatalf("got %d channels, want 2", len(g.Channels))
	}
}
