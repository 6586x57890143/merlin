package aimod

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/disgoorg/disgo/discord"
)

// gatewayMessage reads everything aimod judges by from Discord's JSON: the
// forward's snapshots, the reply, the member's roles, and the marks that
// skip a message (webhook, bot, an edit with no author).
func TestGatewayMessageReadsWhatAimodJudges(t *testing.T) {
	var m discord.Message
	if err := json.Unmarshal([]byte(`{"id":"1","channel_id":"2","guild_id":"3","content":"",
		"author":{"id":"4","username":"u"},"member":{"roles":["5","6"]},
		"edited_timestamp":"2026-01-01T00:00:00Z","webhook_id":"7",
		"message_snapshots":[{"message":{"content":"first"}},{"message":{"content":"second"}}],
		"referenced_message":{"id":"8","channel_id":"2","content":"before","author":{"id":"9","username":"v"}}}`), &m); err != nil {
		t.Fatal(err)
	}
	got := gatewayMessage(m)
	if got.ID != "1" || got.ChannelID != "2" || got.GuildID != "3" || got.AuthorID != "4" || !got.Webhook || !got.Edited || got.Bot {
		t.Fatalf("ids and marks: %+v", got)
	}
	if !slices.Equal(got.RoleIDs, []string{"5", "6"}) || got.ReplyTo != "before" {
		t.Fatalf("roles %v reply %q", got.RoleIDs, got.ReplyTo)
	}
	if text, fwd := messageText(got); text != "first\nsecond" || !fwd {
		t.Fatalf("forward read as %q %v", text, fwd)
	}

	var edit discord.Message
	if err := json.Unmarshal([]byte(`{"id":"1","channel_id":"2","guild_id":"3","content":"x","edited_timestamp":"2026-01-01T00:00:00Z"}`), &edit); err != nil {
		t.Fatal(err)
	}
	if got := gatewayMessage(edit); got.AuthorID != "" {
		t.Fatalf("an edit without its author read as %q", got.AuthorID)
	}
}
