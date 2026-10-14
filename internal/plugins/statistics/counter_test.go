package statistics

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	snow "github.com/disgoorg/snowflake/v2"
)

func newTestPlugin(store *fakeStore, src *fakeSource) *Plugin {
	p := New(store, nil, func(_, channelID snow.ID) string { return "chan-" + channelID.String() })
	p.source = src
	p.now = func() time.Time { return windowStart.Add(24 * time.Hour) }
	return p
}

func guildMsg(at time.Time, _ int, guild, channel, author, name string) heard {
	return heard{guild: sid(guild), channel: sid(channel), author: sid(author), name: name, at: at}
}

// sid is a test ID; "" is no ID, as on the wire.
func sid(s string) snow.ID {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		panic(err)
	}
	return snow.ID(n)
}

// TestCounterBucketsByHourAndSkipsNonPeople: the live path counts into the
// hour of the message's own timestamp, and bots, webhooks and DMs are not
// people in a server.
func TestCounterBucketsByHourAndSkipsNonPeople(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})

	p.count1(guildMsg(windowStart, 1, "1", "11", "21", "zoe"))
	p.count1(guildMsg(windowStart.Add(20*time.Minute), 2, "1", "11", "21", "zoe"))
	p.count1(guildMsg(windowStart.Add(time.Hour), 3, "1", "12", "21", "zoe"))
	p.count1(guildMsg(windowStart, 4, "1", "11", "22", "abe"))
	bot := guildMsg(windowStart, 5, "1", "11", "29", "29")
	bot.skip = true
	p.count1(bot)
	hook := guildMsg(windowStart, 6, "1", "11", "23", "hook")
	hook.skip = true
	p.count1(hook)
	p.count1(guildMsg(windowStart, 7, "0", "11", "24", "dm")) // no guild

	p.flush(context.Background())

	if got := store.hourly[bucketKey{"1", "11", "21", windowStart}]; got != 2 {
		t.Fatalf("two messages in one hour should be one cell of 2, got %d", got)
	}
	if got := store.hourly[bucketKey{"1", "12", "21", windowStart.Add(time.Hour)}]; got != 1 {
		t.Fatalf("the next hour is its own cell, got %d", got)
	}
	if store.total("1", "") != 4 {
		t.Fatalf("bots, webhooks and DMs must not be counted: total %d", store.total("1", ""))
	}
	if u := store.users["1:21"]; u.Name != "zoe" || !u.SeenAt.Equal(windowStart.Add(time.Hour)) {
		t.Fatalf("newest sighting should win: %+v", u)
	}
	if store.channels["1:11"] != "chan-11" {
		t.Fatalf("channel name not recorded: %v", store.channels)
	}
	// A second flush with nothing new writes nothing.
	p.flush(context.Background())
	if store.total("1", "") != 4 {
		t.Fatal("an empty flush changed the counts")
	}
}

// TestCounterKeepsCountsAcrossAFailedFlush: a database outage costs a delay,
// not the messages counted during it, and nothing is counted twice once it
// recovers.
func TestCounterKeepsCountsAcrossAFailedFlush(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	p.count1(guildMsg(windowStart, 1, "1", "11", "21", "zoe"))

	store.addErr = errors.New("db down")
	p.flush(context.Background())
	if store.total("1", "") != 0 {
		t.Fatal("nothing should have landed")
	}
	p.count1(guildMsg(windowStart, 2, "1", "11", "21", "zoe"))
	store.addErr = nil
	p.flush(context.Background())
	if got := store.total("1", "21"); got != 2 {
		t.Fatalf("expected both messages after recovery, got %d", got)
	}
}

// TestCounterMembers: joins and departures land in the hour they happen,
// with no user attached.
func TestCounterMembers(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	p.HandleMemberJoin("1")
	p.HandleMemberJoin("1")
	p.HandleMemberLeave("1")
	p.HandleMemberJoin("")
	p.flush(context.Background())
	hour := p.now().Truncate(time.Hour)
	if b := store.members[memberKey{"1", hour}]; b.Joined != 2 || b.Departed != 1 {
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
	p.count1(guildMsg(windowStart, 1, "1", "11", "21", "zoe"))
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.total("1", "") != 1 {
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
func (g fakeGate) PluginEnabledID(guildID snow.ID, _ string) bool {
	return g[guildID.String()]
}

// TestCounterHonoursThePluginGate: a guild that switched the plugin off is
// not counted behind its back.
func TestCounterHonoursThePluginGate(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	p.gate = fakeGate{"101": true}
	p.count1(guildMsg(windowStart, 1, "101", "11", "21", "u"))
	p.count1(guildMsg(windowStart, 2, "102", "11", "21", "u"))
	p.HandleMemberJoin("102")
	p.flush(context.Background())
	if store.total("101", "") != 1 || store.total("102", "") != 0 || len(store.members) != 0 {
		t.Fatalf("gate ignored: on=%d off=%d members=%d", store.total("101", ""), store.total("102", ""), len(store.members))
	}
}

func voiceState(guild, user, channel string) move {
	return move{guild: sid(guild), user: sid(user), channel: sid(channel), named: true, name: user}
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

	p.voice1(voiceState("1", "21", "31"))
	clock = clock.Add(time.Hour) // 15:30, one hour in v1 across two buckets
	muted := voiceState("1", "21", "31")
	was := sid("31")
	muted.before = &was
	p.voice1(muted)
	p.voice1(voiceState("1", "21", "32")) // switch rooms
	clock = clock.Add(10 * time.Minute)    // 15:40
	p.flush(context.Background())
	clock = clock.Add(5 * time.Minute)   // 15:45
	p.voice1(voiceState("1", "21", "")) // leave
	clock = clock.Add(time.Hour)
	p.flush(context.Background())

	want := map[bucketKey]int{
		{"1", "31", "21", windowStart}:                1800,
		{"1", "31", "21", windowStart.Add(time.Hour)}: 1800,
		{"1", "32", "21", windowStart.Add(time.Hour)}: 900,
	}
	for k, secs := range want {
		if got := store.voice[k]; got != secs {
			t.Fatalf("%s in %s: %ds, want %ds", k.hour.Format("15:04"), k.channelID, got, secs)
		}
	}
	if total := store.voiceTotal("1", "21"); total != 4500 {
		t.Fatalf("booked %ds in all, want 4500", total)
	}
	if _, open := p.count.open[seat{1, 21}]; open {
		t.Fatal("a member who left is still on the clock")
	}
	if u := store.users["1:21"]; u.Name != "21" {
		t.Fatalf("a voice sighting should record the name: %+v", u)
	}

	// Bots are not people here either, and a guild that turned the plugin
	// off is not watched.
	bot := voiceState("1", "39", "31")
	bot.bot = true
	p.voice1(bot)
	if _, open := p.count.open[seat{1, 39}]; open {
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

	p.voice1(voiceState("1", "21", "31"))
	clock = clock.Add(10 * time.Minute)
	p.syncVoice(1, [][2]snow.ID{
		{21, 31},
		{22, 32},
		{23, 0}, // not in a channel
	})
	clock = clock.Add(5 * time.Minute)
	p.flush(context.Background())

	if got := store.voiceTotal("1", "21"); got != 900 {
		t.Fatalf("a known member's clock was reset by sync: %ds", got)
	}
	if got := store.voiceTotal("1", "22"); got != 300 {
		t.Fatalf("a member found in voice should count from the sync: %ds", got)
	}
	if got := store.voiceTotal("1", "23"); got != 0 {
		t.Fatalf("a state with no channel is not in voice: %ds", got)
	}
}

// TestVoiceStopsWhenTheGuildTurnsThePluginOff: a cursor left open by a
// guild that disabled counting must not go on booking for ever, since the
// leave that would close it is no longer heard.
func TestVoiceStopsWhenTheGuildTurnsThePluginOff(t *testing.T) {
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{})
	gate := fakeGate{"1": true}
	p.gate = gate
	clock := windowStart
	p.now = func() time.Time { return clock }

	p.voice1(voiceState("1", "21", "31"))
	clock = clock.Add(time.Minute)
	gate["1"] = false
	p.voice1(voiceState("1", "21", "")) // not heard
	p.flush(context.Background())
	clock = clock.Add(time.Hour)
	p.flush(context.Background())
	if got := store.voiceTotal("1", "21"); got != 0 {
		t.Fatalf("a disabled guild booked %ds", got)
	}
	if _, open := p.count.open[seat{1, 21}]; open {
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

	p.voice1(voiceState("1", "21", "31"))
	clock = clock.Add(time.Minute)
	store.addErr = errors.New("down")
	p.flush(context.Background())
	if store.voiceTotal("1", "") != 0 {
		t.Fatal("a failed write stored something")
	}
	store.addErr = nil
	clock = clock.Add(time.Minute)
	p.flush(context.Background())
	if got := store.voiceTotal("1", "21"); got != 120 {
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
