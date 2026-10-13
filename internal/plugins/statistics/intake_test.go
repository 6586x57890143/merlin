package statistics

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
)

// HandleMessage and HandleVoiceState read Discord's JSON as the discordgo
// fixtures elsewhere are read: who, where, when, by which name, and that a
// mute is not a move.
func TestIntakeReadsDiscordsJSON(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	clock := windowStart.Add(30 * time.Minute)
	p.now = func() time.Time { return clock }

	var m, bot discord.Message
	if err := json.Unmarshal([]byte(`{"id":"1","channel_id":"2","guild_id":"3","timestamp":"2026-01-01T00:00:00Z",
		"author":{"id":"4","username":"u","global_name":"Zoe","avatar":"abc"},"member":{"nick":"zed"}}`), &m); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"id":"5","channel_id":"2","guild_id":"3","author":{"id":"6","username":"b","bot":true}}`), &bot); err != nil {
		t.Fatal(err)
	}
	p.HandleMessage(m)
	p.HandleMessage(bot)
	c := p.count
	if u := c.users["3:4"]; u.Name != "zed" || u.Avatar != "abc" || len(c.users) != 1 {
		t.Fatalf("users %+v", c.users)
	}
	if n := c.messages[bucketKey{"3", "2", "4", m.CreatedAt.UTC().Truncate(time.Hour)}]; n != 1 || len(c.messages) != 1 {
		t.Fatalf("messages %+v", c.messages)
	}

	var vs, muted discord.VoiceState
	_ = json.Unmarshal([]byte(`{"guild_id":"3","user_id":"4","channel_id":"7"}`), &vs)
	_ = json.Unmarshal([]byte(`{"guild_id":"3","user_id":"4","channel_id":"7","self_mute":true}`), &muted)
	p.HandleVoiceState(vs, discord.VoiceState{}, discord.Member{User: discord.User{ID: 4, Username: "u"}})
	clock = clock.Add(10 * time.Minute)
	p.HandleVoiceState(muted, vs, discord.Member{})
	if o := c.open["3:4"]; o.channelID != "7" || !o.since.Equal(windowStart.Add(30*time.Minute)) {
		t.Fatalf("a mute moved the cursor: %+v", o)
	}
	p.SyncVoice("3", []discord.VoiceState{muted})
	p.flush(context.Background())
}
