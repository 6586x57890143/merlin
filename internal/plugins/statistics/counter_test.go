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
