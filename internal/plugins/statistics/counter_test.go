package statistics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func newTestPlugin(store *fakeStore, src *fakeSource) *Plugin {
	p := New(store, nil, func(_, channelID string) string { return "chan-" + channelID })
	p.source = src
	p.now = func() time.Time { return windowStart.Add(24 * time.Hour) }
	return p
}

func guildMsg(at time.Time, seq int, guild, channel, author, name string) *discordgo.Message {
	m := msgAt(at, seq, author, name)
	m.GuildID, m.ChannelID = guild, channel
	return m
}

// TestCounterBucketsByHourAndSkipsNonPeople: the live path counts into the
// hour of the message's own timestamp, and bots, webhooks and DMs are not
// people in a server.
func TestCounterBucketsByHourAndSkipsNonPeople(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})

	p.HandleMessage(guildMsg(windowStart, 1, "g1", "c1", "u1", "zoe"))
	p.HandleMessage(guildMsg(windowStart.Add(20*time.Minute), 2, "g1", "c1", "u1", "zoe"))
	p.HandleMessage(guildMsg(windowStart.Add(time.Hour), 3, "g1", "c2", "u1", "zoe"))
	p.HandleMessage(guildMsg(windowStart, 4, "g1", "c1", "u2", "abe"))
	bot := guildMsg(windowStart, 5, "g1", "c1", "bot", "bot")
	bot.Author.Bot = true
	p.HandleMessage(bot)
	hook := guildMsg(windowStart, 6, "g1", "c1", "u3", "hook")
	hook.WebhookID = "w"
	p.HandleMessage(hook)
	p.HandleMessage(msgAt(windowStart, 7, "u4", "dm")) // no guild
	p.HandleMessage(nil)

	p.flush(context.Background())

	if got := store.hourly[bucketKey{"g1", "c1", "u1", windowStart}]; got != 2 {
		t.Fatalf("two messages in one hour should be one cell of 2, got %d", got)
	}
	if got := store.hourly[bucketKey{"g1", "c2", "u1", windowStart.Add(time.Hour)}]; got != 1 {
		t.Fatalf("the next hour is its own cell, got %d", got)
	}
	if store.total("g1", "") != 4 {
		t.Fatalf("bots, webhooks and DMs must not be counted: total %d", store.total("g1", ""))
	}
	if u := store.users["g1:u1"]; u.Name != "zoe" || !u.SeenAt.Equal(windowStart.Add(time.Hour)) {
		t.Fatalf("newest sighting should win: %+v", u)
	}
	if store.channels["g1:c1"] != "chan-c1" {
		t.Fatalf("channel name not recorded: %v", store.channels)
	}
	// A second flush with nothing new writes nothing.
	p.flush(context.Background())
	if store.total("g1", "") != 4 {
		t.Fatal("an empty flush changed the counts")
	}
}

// TestCounterKeepsCountsAcrossAFailedFlush: a database outage costs a delay,
// not the messages counted during it, and nothing is counted twice once it
// recovers.
func TestCounterKeepsCountsAcrossAFailedFlush(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	p.HandleMessage(guildMsg(windowStart, 1, "g1", "c1", "u1", "zoe"))

	store.addErr = errors.New("db down")
	p.flush(context.Background())
	if store.total("g1", "") != 0 {
		t.Fatal("nothing should have landed")
	}
	p.HandleMessage(guildMsg(windowStart, 2, "g1", "c1", "u1", "zoe"))
	store.addErr = nil
	p.flush(context.Background())
	if got := store.total("g1", "u1"); got != 2 {
		t.Fatalf("expected both messages after recovery, got %d", got)
	}
}

// TestCounterMembers: joins and departures land in the hour they happen,
// with no user attached.
func TestCounterMembers(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	p.HandleMemberJoin("g1")
	p.HandleMemberJoin("g1")
	p.HandleMemberLeave("g1")
	p.HandleMemberJoin("")
	p.flush(context.Background())
	hour := p.now().Truncate(time.Hour)
	if b := store.members[memberKey{"g1", hour}]; b.Joined != 2 || b.Departed != 1 {
		t.Fatalf("members: %+v", b)
	}
	if len(store.members) != 1 {
		t.Fatal("a guildless event must not be counted")
	}
}

// TestFlushLoopFlushesOnShutdown: Shutdown's final flush is what makes a
// redeploy lose nothing.
func TestFlushLoopFlushesOnShutdown(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.HandleMessage(guildMsg(windowStart, 1, "g1", "c1", "u1", "zoe"))
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.total("g1", "") != 1 {
		t.Fatal("the message counted just before shutdown was lost")
	}
}

// TestPruneHonoursEachGuildsRetention pins the direction that has to work:
// a shortened retention drops rows that already exist.
func TestPruneHonoursEachGuildsRetention(t *testing.T) {
	store := newFakeStore()
	now := windowStart.Add(200 * 24 * time.Hour)
	_ = store.AddMessages(context.Background(), []Bucket{
		{GuildID: "long", ChannelID: "c", UserID: "u", Hour: windowStart, Messages: 1},
		{GuildID: "short", ChannelID: "c", UserID: "u", Hour: windowStart, Messages: 1},
		{GuildID: "short", ChannelID: "c", UserID: "u", Hour: now.Add(-time.Hour), Messages: 1},
	})
	_ = store.SetRetention(context.Background(), "long", 365)
	_ = store.SetRetention(context.Background(), "short", 7)
	n, err := store.Prune(context.Background(), now)
	if err != nil || n != 1 {
		t.Fatalf("prune: n=%d err=%v", n, err)
	}
	if store.total("long", "") != 1 || store.total("short", "") != 1 {
		t.Fatalf("wrong rows pruned: long=%d short=%d", store.total("long", ""), store.total("short", ""))
	}
}

type fakeGate map[string]bool

func (g fakeGate) PluginEnabled(guildID, _ string) bool { return g[guildID] }

// TestCounterHonoursThePluginGate: a guild that switched the plugin off is
// not counted behind its back.
func TestCounterHonoursThePluginGate(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	p.gate = fakeGate{"on": true}
	p.HandleMessage(guildMsg(windowStart, 1, "on", "c", "u", "u"))
	p.HandleMessage(guildMsg(windowStart, 2, "off", "c", "u", "u"))
	p.HandleMemberJoin("off")
	p.flush(context.Background())
	if store.total("on", "") != 1 || store.total("off", "") != 0 || len(store.members) != 0 {
		t.Fatalf("gate ignored: on=%d off=%d members=%d", store.total("on", ""), store.total("off", ""), len(store.members))
	}
}

func voiceState(guild, user, channel string) *discordgo.VoiceStateUpdate {
	return &discordgo.VoiceStateUpdate{VoiceState: &discordgo.VoiceState{
		GuildID: guild, UserID: user, ChannelID: channel,
		Member: &discordgo.Member{User: &discordgo.User{ID: user, Username: user}},
	}}
}

// TestVoiceIsBookedByTheHour: a sitting is split at hour boundaries into
// the same cell shape messages use, a room switch closes one run and opens
// the next, a mute toggle changes nothing, a flush books whoever is still
// in voice up to now, and a leave stops the clock.
func TestVoiceIsBookedByTheHour(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	clock := windowStart.Add(30 * time.Minute) // 14:30
	p.now = func() time.Time { return clock }

	p.HandleVoiceState(voiceState("g1", "u1", "v1"))
	clock = clock.Add(time.Hour) // 15:30, one hour in v1 across two buckets
	muted := voiceState("g1", "u1", "v1")
	muted.SelfMute = true
	muted.BeforeUpdate = &discordgo.VoiceState{ChannelID: "v1"}
	p.HandleVoiceState(muted)
	p.HandleVoiceState(voiceState("g1", "u1", "v2")) // switch rooms
	clock = clock.Add(10 * time.Minute)              // 15:40
	p.flush(context.Background())
	clock = clock.Add(5 * time.Minute)             // 15:45
	p.HandleVoiceState(voiceState("g1", "u1", "")) // leave
	clock = clock.Add(time.Hour)
	p.flush(context.Background())

	want := map[bucketKey]int{
		{"g1", "v1", "u1", windowStart}:                1800,
		{"g1", "v1", "u1", windowStart.Add(time.Hour)}: 1800,
		{"g1", "v2", "u1", windowStart.Add(time.Hour)}: 900,
	}
	for k, secs := range want {
		if got := store.voice[k]; got != secs {
			t.Fatalf("%s in %s: %ds, want %ds", k.hour.Format("15:04"), k.channelID, got, secs)
		}
	}
	if total := store.voiceTotal("g1", "u1"); total != 4500 {
		t.Fatalf("booked %ds in all, want 4500", total)
	}
	if _, open := p.count.open["g1:u1"]; open {
		t.Fatal("a member who left is still on the clock")
	}
	if u := store.users["g1:u1"]; u.Name != "u1" {
		t.Fatalf("a voice sighting should record the name: %+v", u)
	}

	// Bots are not people here either, and a guild that turned the plugin
	// off is not watched.
	bot := voiceState("g1", "b1", "v1")
	bot.Member.User.Bot = true
	p.HandleVoiceState(bot)
	if _, open := p.count.open["g1:b1"]; open {
		t.Fatal("a bot is on the clock")
	}
}

// TestSyncVoiceSeedsWhoIsAlreadyThere: on GuildCreate everybody sitting in
// voice starts their clock now, and a member already known keeps theirs.
func TestSyncVoiceSeedsWhoIsAlreadyThere(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	clock := windowStart
	p.now = func() time.Time { return clock }

	p.HandleVoiceState(voiceState("g1", "u1", "v1"))
	clock = clock.Add(10 * time.Minute)
	p.SyncVoice("g1", []*discordgo.VoiceState{
		{UserID: "u1", ChannelID: "v1"},
		{UserID: "u2", ChannelID: "v2"},
		{UserID: "u3"}, // not in a channel
	})
	clock = clock.Add(5 * time.Minute)
	p.flush(context.Background())

	if got := store.voiceTotal("g1", "u1"); got != 900 {
		t.Fatalf("a known member's clock was reset by sync: %ds", got)
	}
	if got := store.voiceTotal("g1", "u2"); got != 300 {
		t.Fatalf("a member found in voice should count from the sync: %ds", got)
	}
	if got := store.voiceTotal("g1", "u3"); got != 0 {
		t.Fatalf("a state with no channel is not in voice: %ds", got)
	}
}

// TestVoiceStopsWhenTheGuildTurnsThePluginOff: a cursor left open by a
// guild that disabled counting must not go on booking for ever, since the
// leave that would close it is no longer heard.
func TestVoiceStopsWhenTheGuildTurnsThePluginOff(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	gate := fakeGate{"g1": true}
	p.gate = gate
	clock := windowStart
	p.now = func() time.Time { return clock }

	p.HandleVoiceState(voiceState("g1", "u1", "v1"))
	clock = clock.Add(time.Minute)
	gate["g1"] = false
	p.HandleVoiceState(voiceState("g1", "u1", "")) // not heard
	p.flush(context.Background())
	clock = clock.Add(time.Hour)
	p.flush(context.Background())
	if got := store.voiceTotal("g1", "u1"); got != 0 {
		t.Fatalf("a disabled guild booked %ds", got)
	}
	if _, open := p.count.open["g1:u1"]; open {
		t.Fatal("the cursor outlived the switch")
	}
}

// TestVoiceSurvivesAFailedFlush: like messages, seconds a write could not
// land go back into the tally for the next one.
func TestVoiceSurvivesAFailedFlush(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	clock := windowStart
	p.now = func() time.Time { return clock }

	p.HandleVoiceState(voiceState("g1", "u1", "v1"))
	clock = clock.Add(time.Minute)
	store.addErr = errors.New("down")
	p.flush(context.Background())
	if store.voiceTotal("g1", "") != 0 {
		t.Fatal("a failed write stored something")
	}
	store.addErr = nil
	clock = clock.Add(time.Minute)
	p.flush(context.Background())
	if got := store.voiceTotal("g1", "u1"); got != 120 {
		t.Fatalf("carried over %ds, want 120", got)
	}
}

func TestHourSlices(t *testing.T) {
	from := windowStart.Add(50 * time.Minute)
	got := hourSlices(from, from.Add(2*time.Hour+15*time.Minute))
	want := []hourSlice{
		{windowStart, 600},
		{windowStart.Add(time.Hour), 3600},
		{windowStart.Add(2 * time.Hour), 3600},
		{windowStart.Add(3 * time.Hour), 300},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].hour.Equal(want[i].hour) || got[i].seconds != want[i].seconds {
			t.Fatalf("slice %d: got %v, want %v", i, got[i], want[i])
		}
	}
	if len(hourSlices(from, from)) != 0 || len(hourSlices(from, from.Add(-time.Hour))) != 0 {
		t.Fatal("an empty or backwards span has no slices")
	}
}
