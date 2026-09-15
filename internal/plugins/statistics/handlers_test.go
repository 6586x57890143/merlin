package statistics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// The handlers are driven against a discordgo session whose transport
// records every request body and answers with an empty success, the same
// shape roles' and aimod's handler tests use.

type recordingTransport struct {
	mu     sync.Mutex
	bodies []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	r.mu.Lock()
	// JSON escapes < > and &, which every mention carries.
	r.bodies = append(r.bodies, strings.NewReplacer("\\u003c", "<", "\\u003e", ">", "\\u0026", "&").Replace(string(body)))
	r.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

func (r *recordingTransport) said(text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.bodies {
		if strings.Contains(b, text) {
			return true
		}
	}
	return false
}

func handlerSession(t *testing.T) (*discordgo.Session, *recordingTransport) {
	t.Helper()
	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	rt := &recordingTransport{}
	s.Client = &http.Client{Transport: rt}
	return s, rt
}

func interaction(actor, group, sub string, args ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.InteractionCreate {
	leaf := &discordgo.ApplicationCommandInteractionDataOption{Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand, Options: args}
	opts := []*discordgo.ApplicationCommandInteractionDataOption{leaf}
	if group != "" {
		opts = []*discordgo.ApplicationCommandInteractionDataOption{{
			Name: group, Type: discordgo.ApplicationCommandOptionSubCommandGroup,
			Options: []*discordgo.ApplicationCommandInteractionDataOption{leaf},
		}}
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "i1", Token: "tok", GuildID: "g1", ChannelID: "chan",
		Type:   discordgo.InteractionApplicationCommand,
		Member: &discordgo.Member{User: &discordgo.User{ID: actor}},
		Data:   discordgo.ApplicationCommandInteractionData{Name: "statistics", Options: opts},
	}}
}

func strArg(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: v}
}

func intArg(name string, v int) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionInteger, Value: float64(v)}
}

// seeded is a plugin with a week of counted history for g1.
func seeded(t *testing.T) (*Plugin, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	p := newTestPlugin(store, &fakeSource{channels: []*discordgo.Channel{textChannel("c1", "general")}, msgs: map[string][]*discordgo.Message{}})
	p.privilege = fakePrivilege{operator: "op"}
	p.sched = newFakeScheduler()
	_ = store.MarkLive(context.Background(), "g1", windowStart)
	_ = store.AddMessages(context.Background(), []Bucket{
		{GuildID: "g1", ChannelID: "c1", UserID: "u1", Hour: windowStart, Messages: 5},
		{GuildID: "g1", ChannelID: "c2", UserID: "u1", Hour: windowStart.Add(time.Hour), Messages: 1},
		{GuildID: "g1", ChannelID: "c1", UserID: "u2", Hour: windowStart, Messages: 2},
	})
	_ = store.UpsertUsers(context.Background(), []UserSeen{{GuildID: "g1", UserID: "u1", Name: "zoe", SeenAt: windowStart}})
	_ = store.UpsertChannels(context.Background(), []ChannelSeen{{GuildID: "g1", ChannelID: "c1", Name: "general"}})
	_ = store.AddMembers(context.Background(), []MemberBucket{{GuildID: "g1", Hour: windowStart, Joined: 3, Departed: 1}})
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

// TestBuildNamesPeopleAndChannelsFromTheBuckets: the report comes out of
// the buckets ranked, named where a name was seen and by id where not, with
// channels resolved to the names on record.
func TestBuildNamesPeopleAndChannelsFromTheBuckets(t *testing.T) {
	p, _ := seeded(t)
	rep, err := p.build(context.Background(), "g1", options{from: windowStart, to: windowStart.Add(3 * time.Hour)})
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
	early, _ := p.build(context.Background(), "g1", options{from: windowStart.Add(-24 * time.Hour), to: windowStart.Add(time.Hour)})
	if !early.partial() || !early.coveredFrom.Equal(windowStart) {
		t.Fatalf("a window from before the oldest bucket is partial: %+v", early)
	}
}

func TestHandleReportIsOperatorOnly(t *testing.T) {
	p, _ := seeded(t)
	s, rt := handlerSession(t)
	p.handleReport(context.Background(), s, interaction("admin", "", "report", strArg("from", "2026-09-01")))
	if !rt.said("Not yours to run") {
		t.Fatal("an admin must be refused")
	}
	p.handleBackfill(context.Background(), s, interaction("admin", "", "backfill", strArg("from", "2026-09-01")))
	if len(rt.bodies) != 2 || !strings.Contains(rt.bodies[1], "Not yours to run") {
		t.Fatal("backfill reads history and is the operator's too")
	}
}

func TestHandleReportRendersAndAttaches(t *testing.T) {
	p, store := seeded(t)
	s, rt := handlerSession(t)
	p.handleReport(context.Background(), s, interaction("op", "", "report", strArg("from", "2026-09-01"), intArg("top", 1)))
	if !rt.said("who was active in") || !rt.said("**zoe**") {
		t.Fatalf("expected the report, got %d bodies", len(rt.bodies))
	}
	if !rt.said(`filename="activity.png"`) || !rt.said(`filename="activity.md"`) {
		t.Fatal("expected the card and, with top=1 of 2, the full list attached")
	}

	p.handleReport(context.Background(), s, interaction("op", "", "report", strArg("from", "next week")))
	if !rt.said("That window doesn't work") {
		t.Fatal("bad input is refused before deferring")
	}
	store.reportErr = errors.New("db down")
	p.handleReport(context.Background(), s, interaction("op", "", "report", strArg("from", "2026-09-01")))
	if !rt.said("Could not read the statistics") {
		t.Fatal("a store failure is reported")
	}
}

func TestHandleChannelsAndMembers(t *testing.T) {
	p, _ := seeded(t)
	s, rt := handlerSession(t)
	p.handleChannels(context.Background(), s, interaction("admin", "", "channels", strArg("from", "2026-09-01")))
	if !rt.said("Where the traffic went") || !rt.said("#general") || !rt.said("from `2` people") {
		t.Fatalf("channels: %v", rt.bodies)
	}
	p.handleMembers(context.Background(), s, interaction("admin", "", "members", strArg("from", "2026-09-01")))
	if !rt.said("`3` joined, `1` left, net `+2`") || !rt.said("`2026-09-01` `+3` `-1`") {
		t.Fatalf("members: %v", rt.bodies)
	}
	p.handleChannels(context.Background(), s, interaction("admin", "", "channels", strArg("from", "2020-01-01"), strArg("to", "2020-01-02")))
	if !rt.said("nothing was counted in that window") {
		t.Fatal("an empty window says so")
	}
	p.handleMembers(context.Background(), s, interaction("admin", "", "members", strArg("from", "2020-01-01"), strArg("to", "2020-01-02")))
	if !rt.said("no joins or departures") {
		t.Fatal("an empty member window says so")
	}
	p.handleMembers(context.Background(), s, interaction("admin", "", "members", strArg("from", "soon")))
	p.handleChannels(context.Background(), s, interaction("admin", "", "channels", strArg("from", "soon")))
	if len(rt.bodies) != 6 {
		t.Fatalf("expected a refusal for each bad window, got %d bodies", len(rt.bodies))
	}
}

func TestHandleStatusAndRetention(t *testing.T) {
	p, store := seeded(t)
	s, rt := handlerSession(t)
	p.handleStatus(context.Background(), s, interaction("admin", "", "status"))
	if !rt.said("counting since `2026-09-01 14:00`") || !rt.said("retention: `90` days") {
		t.Fatalf("status: %v", rt.bodies)
	}

	p.handleRetention(context.Background(), s, interaction("admin", "configure", "retention", intArg("days", 30)))
	if !rt.said("Retention set") {
		t.Fatal("expected confirmation")
	}
	if cfg, _ := store.Config(context.Background(), "g1"); cfg.RetentionDays != 30 {
		t.Fatalf("retention not saved: %+v", cfg)
	}
	p.handleRetention(context.Background(), s, interaction("admin", "configure", "retention", intArg("days", 0)))
	if !rt.said("Out of range") {
		t.Fatal("zero days must be refused")
	}

	// A guild the bot has not seen yet.
	fresh := newTestPlugin(newFakeStore(), &fakeSource{})
	fresh.handleStatus(context.Background(), s, interaction("admin", "", "status"))
	if !rt.said("counting has not started") {
		t.Fatal("status before counting should say so")
	}
}

func TestHandleBackfillQueuesUpToTheLiveHour(t *testing.T) {
	p, store := seeded(t)
	s, rt := handlerSession(t)
	p.handleBackfill(context.Background(), s, interaction("op", "", "backfill", strArg("from", "2026-08-01")))
	if !rt.said("Backfill queued") || !rt.said("`1` channels") {
		t.Fatalf("backfill: %v", rt.bodies)
	}
	pending, _ := store.PendingBackfill(context.Background(), "g1")
	if len(pending) != 1 || !pending[0].Until.Equal(windowStart) {
		t.Fatalf("queued row should stop at the hour counting began: %+v", pending)
	}
	p.handleStatus(context.Background(), s, interaction("admin", "", "status"))
	if !rt.said("backfill: `1` channels still to read") {
		t.Fatal("status should show the queue")
	}

	p.handleBackfill(context.Background(), s, interaction("op", "", "backfill", strArg("from", "2026-09-10")))
	if !rt.said("Already counted") {
		t.Fatal("a start after live counting began has nothing to fill")
	}
	p.handleBackfill(context.Background(), s, interaction("op", "", "backfill", strArg("from", "whenever")))
	if !rt.said("That window doesn't work") {
		t.Fatal("bad date refused")
	}

	fresh := newTestPlugin(newFakeStore(), &fakeSource{})
	fresh.privilege = fakePrivilege{operator: "op"}
	fresh.handleBackfill(context.Background(), s, interaction("op", "", "backfill", strArg("from", "2026-08-01")))
	if !rt.said("Nothing to fill up to") {
		t.Fatal("no live_since means nothing to fill up to yet")
	}
}
