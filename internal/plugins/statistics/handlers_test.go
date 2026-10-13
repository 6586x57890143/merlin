package statistics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
)

// The handlers are driven through core.Ix interactions decoded from
// Discord's JSON, answered through a REST client whose requests land on a
// paneltest.Wire. disgo's IDs are snowflakes, so the guild is
// paneltest.IxGuild and the operator and an admin are numbered.

const (
	guild = paneltest.IxGuild
	op    = "900"
	admin = "901"
)

// run is actor running /statistics sub with args.
func run(t *testing.T, actor, sub string, args ...map[string]any) (*core.Ix, *paneltest.Wire) {
	t.Helper()
	return paneltest.IxCommand(t, actor, "statistics", func(p map[string]any) {
		opts := []any{}
		for _, a := range args {
			opts = append(opts, a)
		}
		p["data"].(map[string]any)["options"] = []any{map[string]any{"name": sub, "type": 1, "options": opts}}
	})
}

func strArg(name, v string) map[string]any {
	return map[string]any{"name": name, "type": 3, "value": v}
}

func intArg(name string, v int) map[string]any {
	return map[string]any{"name": name, "type": 4, "value": v}
}

// unescape undoes JSON's escaping of < > and &, which every mention carries.
var unescape = strings.NewReplacer("\\u003c", "<", "\\u003e", ">", "\\u0026", "&")

func said(w *paneltest.Wire, text string) bool {
	return strings.Contains(unescape.Replace(w.All()), text)
}

// seeded is a plugin with a week of counted history for guild.
func seeded(t *testing.T) (*Plugin, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{channels: []ChannelSeen{textChannel("c1", "general")}, msgs: map[string][]post{}})
	p.privilege = fakePrivilege{operator: op}
	p.sched = newFakeScheduler()
	_ = store.MarkLive(context.Background(), guild, windowStart)
	_ = store.AddMessages(context.Background(), []Bucket{
		{GuildID: guild, ChannelID: "c1", UserID: "u1", Hour: windowStart, Messages: 5},
		{GuildID: guild, ChannelID: "c2", UserID: "u1", Hour: windowStart.Add(time.Hour), Messages: 1},
		{GuildID: guild, ChannelID: "c1", UserID: "u2", Hour: windowStart, Messages: 2},
	})
	_ = store.UpsertUsers(context.Background(), []UserSeen{{GuildID: guild, UserID: "u1", Name: "zoe", SeenAt: windowStart}})
	_ = store.UpsertChannels(context.Background(), []ChannelSeen{{GuildID: guild, ChannelID: "c1", Name: "general"}})
	_ = store.AddMembers(context.Background(), []MemberBucket{{GuildID: guild, Hour: windowStart, Joined: 3, Departed: 1}})
	return p, store
}

// TestInitRegistersAFullyWiredCommandTree: every leaf has a handler and a
// tier, which Finalize is what enforces.
func TestInitRegistersAFullyWiredCommandTree(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := core.NewCommandRouter(core.NewPermissions(nil, nil, ""), nil, log)
	p := New(newFakeStore(), nil, nil)
	if p.Name() != "statistics" {
		t.Fatal(p.Name())
	}
	if err := p.Init(core.Deps{Commands: router, Logger: log, Scheduler: newFakeScheduler()}); err != nil {
		t.Fatal(err)
	}
	if err := router.Finalize(); err != nil {
		t.Fatal(err)
	}
}

// TestBuildHidesChannelNamesByDefault: without show-channels nobody's
// channels are named, in the image or the list, so a ticket or mod channel
// cannot end up in a shared report. The count still stands.
func TestBuildHidesChannelNamesByDefault(t *testing.T) {
	p, _ := seeded(t)
	rep, err := p.build(context.Background(), guild, options{from: windowStart, to: windowStart.Add(3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if rep.channels != 2 {
		t.Fatalf("channel count: %d", rep.channels)
	}
	for _, per := range rep.people {
		if len(per.channels) != 0 || len(per.rooms) != 0 {
			t.Fatalf("%s named channels: %v %v", per.name, per.channels, per.rooms)
		}
	}
	if md := markdown(rep, "g", windowStart, windowStart.Add(3*time.Hour), 0); strings.Contains(md, "#general") {
		t.Fatalf("list names a channel:\n%s", md)
	}
}

// TestBuildNamesPeopleAndChannelsFromTheBuckets: the report comes out of
// the buckets ranked, named where a name was seen and by id where not, with
// channels resolved to the names on record.
func TestBuildNamesPeopleAndChannelsFromTheBuckets(t *testing.T) {
	p, _ := seeded(t)
	rep, err := p.build(context.Background(), guild, options{from: windowStart, to: windowStart.Add(3 * time.Hour), showChannels: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.messages != 8 || rep.channels != 2 || len(rep.people) != 2 {
		t.Fatalf("report: messages=%d channels=%d people=%d", rep.messages, rep.channels, len(rep.people))
	}
	if rep.people[0].name != "zoe" || rep.people[0].count != 6 {
		t.Fatalf("first: %+v", rep.people[0])
	}
	if rep.people[1].name != "u2" {
		t.Fatalf("an unseen member is named by id: %+v", rep.people[1])
	}
	if got := channelList(rep.people[0].channels); got != "#c2 #general" {
		t.Fatalf("channels: %q", got)
	}
	if rep.partial() {
		t.Fatal("a window starting where counting began is whole")
	}
	early, _ := p.build(context.Background(), guild, options{from: windowStart.Add(-24 * time.Hour), to: windowStart.Add(time.Hour)})
	if !early.partial() || !early.coveredFrom.Equal(windowStart) {
		t.Fatalf("a window from before the oldest bucket is partial: %+v", early)
	}
}

func TestHandleReportIsOperatorOnly(t *testing.T) {
	p, _ := seeded(t)
	ix, w := run(t, admin, "report", strArg("from", "2026-09-01"))
	p.handleReport(context.Background(), ix)
	if !said(w, "Not yours to run") {
		t.Fatal("an admin must be refused")
	}
	ix, w = run(t, admin, "backfill", strArg("from", "2026-09-01"))
	p.handleBackfill(context.Background(), ix)
	if !said(w, "Not yours to run") || len(w.Bodies) != 1 {
		t.Fatal("backfill reads history and is the operator's too")
	}
}

func TestHandleReportRendersAndAttaches(t *testing.T) {
	p, store := seeded(t)
	ix, w := run(t, op, "report", strArg("from", "2026-09-01"), intArg("top", 1))
	p.handleReport(context.Background(), ix)
	if !said(w, "who was active in") || !said(w, "**zoe**") {
		t.Fatalf("expected the report, got %d bodies", len(w.Bodies))
	}
	if !said(w, `filename="activity.png"`) || !said(w, `filename="activity.md"`) {
		t.Fatal("expected the card and, with top=1 of 2, the full list attached")
	}

	ix, w = run(t, op, "report", strArg("from", "next week"))
	p.handleReport(context.Background(), ix)
	if !said(w, "That window doesn't work") || len(w.Bodies) != 1 {
		t.Fatal("bad input is refused before deferring")
	}
	store.reportErr = errors.New("db down")
	ix, w = run(t, op, "report", strArg("from", "2026-09-01"))
	p.handleReport(context.Background(), ix)
	if !said(w, "Could not read the statistics") {
		t.Fatal("a store failure is reported")
	}
}

func TestHandleChannelsAndMembers(t *testing.T) {
	p, _ := seeded(t)
	ix, w := run(t, admin, "channels", strArg("from", "2026-09-01"))
	p.handleChannels(context.Background(), ix)
	if !said(w, "Where the traffic went") || !said(w, "#general") || !said(w, "from `2` people") {
		t.Fatalf("channels: %v", w.Bodies)
	}
	ix, w = run(t, admin, "members", strArg("from", "2026-09-01"))
	p.handleMembers(context.Background(), ix)
	if !said(w, "`3` joined, `1` left, net `+2`") || !said(w, "`2026-09-01` `+3` `-1`") {
		t.Fatalf("members: %v", w.Bodies)
	}
	ix, w = run(t, admin, "channels", strArg("from", "2020-01-01"), strArg("to", "2020-01-02"))
	p.handleChannels(context.Background(), ix)
	if !said(w, "nothing was counted in that window") {
		t.Fatal("an empty window says so")
	}
	ix, w = run(t, admin, "members", strArg("from", "2020-01-01"), strArg("to", "2020-01-02"))
	p.handleMembers(context.Background(), ix)
	if !said(w, "no joins or departures") {
		t.Fatal("an empty member window says so")
	}
	ix, w = run(t, admin, "members", strArg("from", "soon"))
	p.handleMembers(context.Background(), ix)
	ix2, w2 := run(t, admin, "channels", strArg("from", "soon"))
	p.handleChannels(context.Background(), ix2)
	if !said(w, "That window doesn't work") || !said(w2, "That window doesn't work") {
		t.Fatalf("expected a refusal for each bad window: %v %v", w.Bodies, w2.Bodies)
	}
}

func TestHandleStatusAndRetention(t *testing.T) {
	p, store := seeded(t)
	ix, w := run(t, admin, "status")
	p.handleStatus(context.Background(), ix)
	if !said(w, "since 2026-09-01 14:00 utc") || !said(w, "90 days") {
		t.Fatalf("status: %v", w.Bodies)
	}

	ix, w = retentionForm(t, "30")
	p.handleForm(context.Background(), ix, formSet+"retention")
	if !said(w, "kept for 30 days") {
		t.Fatal("expected confirmation")
	}
	if cfg, _ := store.Config(context.Background(), guild); cfg.RetentionDays != 30 {
		t.Fatalf("retention not saved: %+v", cfg)
	}
	ix, w = retentionForm(t, "0")
	p.handleForm(context.Background(), ix, formSet+"retention")
	if !said(w, "must be between 1 and") {
		t.Fatal("zero days must be refused")
	}

	// A guild the bot has not seen yet.
	fresh := newTestPlugin(newFakeStore(), &fakeSource{})
	ix, w = run(t, admin, "status")
	fresh.handleStatus(context.Background(), ix)
	if !said(w, "not started yet") {
		t.Fatal("status before counting should say so")
	}
}

func TestHandleBackfillQueuesUpToTheLiveHour(t *testing.T) {
	p, store := seeded(t)
	ix, w := run(t, op, "backfill", strArg("from", "2026-08-01"))
	p.handleBackfill(context.Background(), ix)
	if !said(w, "Backfill queued") || !said(w, "`1` channels") {
		t.Fatalf("backfill: %v", w.Bodies)
	}
	pending, _ := store.PendingBackfill(context.Background(), guild)
	if len(pending) != 1 || !pending[0].Until.Equal(windowStart) {
		t.Fatalf("queued row should stop at the hour counting began: %+v", pending)
	}
	ix, w = run(t, admin, "status")
	p.handleStatus(context.Background(), ix)
	if !said(w, "1 to read, 0 done") {
		t.Fatal("status should show the queue")
	}

	ix, w = run(t, op, "backfill", strArg("from", "2026-09-10"))
	p.handleBackfill(context.Background(), ix)
	if !said(w, "Already counted") {
		t.Fatal("a start after live counting began has nothing to fill")
	}
	ix, w = run(t, op, "backfill", strArg("from", "whenever"))
	p.handleBackfill(context.Background(), ix)
	if !said(w, "That window doesn't work") {
		t.Fatal("bad date refused")
	}

	fresh := newTestPlugin(newFakeStore(), &fakeSource{})
	fresh.privilege = fakePrivilege{operator: op}
	ix, w = run(t, op, "backfill", strArg("from", "2026-08-01"))
	fresh.handleBackfill(context.Background(), ix)
	if !said(w, "Nothing to fill up to") {
		t.Fatal("no live_since means nothing to fill up to yet")
	}
}

// TestBuildFoldsVoiceIn: a member who only sat in voice is a person with
// no channels, voice adds to the totals, and the day listing carries both.
func TestBuildFoldsVoiceIn(t *testing.T) {
	p, store := seeded(t)
	_ = store.AddVoice(context.Background(), []VoiceBucket{
		{GuildID: guild, ChannelID: "v1", UserID: "u1", Hour: windowStart, Seconds: 1800},
		{GuildID: guild, ChannelID: "v1", UserID: "u9", Hour: windowStart.Add(time.Hour), Seconds: 7200},
	})
	rep, err := p.build(context.Background(), guild, options{from: windowStart, to: windowStart.Add(3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.people) != 3 || rep.voice != 150*time.Minute {
		t.Fatalf("people=%d voice=%s", len(rep.people), rep.voice)
	}
	if rep.people[0].name != "zoe" || rep.people[0].voice != 30*time.Minute {
		t.Fatalf("zoe should carry her voice time: %+v", rep.people[0])
	}
	last := rep.people[2]
	if last.id != "u9" || last.count != 0 || last.voice != 2*time.Hour || len(last.channels) != 0 {
		t.Fatalf("a voice-only member: %+v", last)
	}
	// A three hour window is hourly: three cells, not one day.
	if !rep.hourly || len(rep.days) != 2 || rep.days[1].VoiceSeconds != 7200 {
		t.Fatalf("hours: %v %+v", rep.hourly, rep.days)
	}
	long, err := p.build(context.Background(), guild, options{from: windowStart, to: windowStart.AddDate(0, 0, 30)})
	if err != nil || long.hourly || len(long.days) != 1 || long.days[0].Messages != 8 || long.days[0].VoiceSeconds != 9000 {
		t.Fatalf("days: %v %+v %v", long.hourly, long.days, err)
	}

	perDay, ok := p.MessagesPerDay(context.Background(), guild, 7)
	if !ok || perDay != 8 {
		t.Fatalf("messages per day = %v, %v; want 8", perDay, ok)
	}
	if _, ok := p.MessagesPerDay(context.Background(), "nowhere", 7); ok {
		t.Fatal("a guild with nothing counted has no rate")
	}
}

// retentionForm is an admin submitting the retention form.
func retentionForm(t *testing.T, days string) (*core.Ix, *paneltest.Wire) {
	t.Helper()
	return paneltest.IxSubmit(t, admin, formSet+"retention", map[string]string{"days": days})
}
