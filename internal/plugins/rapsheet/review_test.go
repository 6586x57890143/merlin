package rapsheet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/disgoorg/disgo/discord"
)

type fakeReviewer struct {
	out     string
	err     error
	calls   int
	lastSys string
	lastUsr string
}

func (f *fakeReviewer) Complete(_ context.Context, _, system, user string) (string, error) {
	f.calls++
	f.lastSys, f.lastUsr = system, user
	return f.out, f.err
}

type noModel struct{ msg string }

func (e noModel) Error() string     { return e.msg }
func (e noModel) Unavailable() bool { return true }

func TestSummaryAsksTheModelWithLedgerTextOnly(t *testing.T) {
	h := newHarness()
	rev := &fakeReviewer{out: "Two spam warnings a month apart and one automatic removal; recent, but not escalating."}
	h.p.reviewer = rev
	seedEntries(h, "1001", 2)
	_, _ = h.store.Insert(context.Background(), Entry{GuildID: testGuild, UserID: "1001", Kind: KindRemoval, Category: CategoryHateSpeech, Points: 50,
		ActorID: "system", Reason: "message removed automatically: a slur", Source: SourceAIMod, Ref: "9", CreatedAt: testNow.Add(-48 * time.Hour)})

	rt := stubSession()
	h.p.handleSummary(context.Background(), withResolved(interaction("summary", userOpt("user", "1001")), &discord.User{ID: core.ID("1001"), Username: "dana"}))
	if rev.calls != 1 {
		t.Fatalf("model calls = %d", rev.calls)
	}
	if strings.Contains(rev.lastUsr, "1001") || strings.Contains(rev.lastUsr, modID) || strings.Contains(rev.lastUsr, "<@") {
		t.Errorf("the prompt carried an id or a mention: %s", rev.lastUsr)
	}
	for _, want := range []string{"by a moderator", "by the bot (AI moderation)", "hate speech", "50 points", "Server ladder", "Current score: 68"} {
		if !strings.Contains(rev.lastUsr, want) {
			t.Errorf("prompt should contain %q: %s", want, rev.lastUsr)
		}
	}
	if !strings.Contains(rt.said(), "not escalating") || !strings.Contains(rt.said(), "Summary: dana") || !strings.Contains(rt.said(), "Written by") {
		t.Errorf("got %s", rt.said())
	}
}

func TestSummaryDegradesToTheSheetWithoutAModel(t *testing.T) {
	h := newHarness()
	seedEntries(h, "1001", 1)
	rt := stubSession()
	h.p.handleSummary(context.Background(), interaction("summary", userOpt("user", "1001")))
	if !strings.Contains(rt.said(), "Summary unavailable") || !strings.Contains(rt.said(), "No model is wired") || !strings.Contains(rt.said(), "#1 warned") {
		t.Errorf("got %s", rt.said())
	}

	h.p.reviewer = &fakeReviewer{err: noModel{"aimod: no gateway key configured for this guild"}}
	rt2 := stubSession()
	h.p.handleSummary(context.Background(), interaction("summary", userOpt("user", "1001")))
	if !strings.Contains(rt2.said(), "no model key is configured") || strings.Contains(rt2.said(), "aimod:") || !strings.Contains(rt2.said(), "#1 warned") {
		t.Errorf("got %s", rt2.said())
	}

	h.p.reviewer = &fakeReviewer{err: errors.New("gateway exploded")}
	rt3 := stubSession()
	h.p.handleSummary(context.Background(), interaction("summary", userOpt("user", "1001")))
	if !strings.Contains(rt3.said(), "did not answer") {
		t.Errorf("a real failure should say so: %s", rt3.said())
	}

	rt4 := stubSession()
	h.p.handleSummary(context.Background(), interaction("summary", userOpt("user", "clean")))
	if !strings.Contains(rt4.said(), "nothing to summarise") {
		t.Errorf("got %s", rt4.said())
	}
}

func TestWeeklyReviewRunsOnlyWhereItCanAndPostsToTheModChannel(t *testing.T) {
	h := newHarness()
	h.p.SyncGuild(context.Background(), testGuild)
	if h.sched.has(reviewKey(testGuild)) {
		t.Fatal("registered with no model, no mod channel")
	}
	rev := &fakeReviewer{out: "Spam was warned at 10 points twice and jailed once for the same thing (#1, #2, #4). Otherwise consistent."}
	h.p.reviewer = rev
	h.p.SyncGuild(context.Background(), testGuild)
	if h.sched.has(reviewKey(testGuild)) {
		t.Fatal("registered with a model but no mod channel")
	}
	withModChannel(h, ModeSuggest)
	h.p.SyncGuild(context.Background(), testGuild)
	if !h.sched.has(reviewKey(testGuild)) {
		t.Fatal("not registered with a model and a mod channel")
	}

	// Nothing on record: nothing posted, no model call.
	if err := h.sched.RunNow(context.Background(), reviewKey(testGuild)); err != nil || rev.calls != 0 || len(h.ops.sentTo("7300020")) != 0 {
		t.Errorf("empty month: err %v calls %d posts %d", err, rev.calls, len(h.ops.sentTo("7300020")))
	}
	seedEntries(h, "1001", 3)
	seedEntries(h, "1002", 2)
	if err := h.sched.RunNow(context.Background(), reviewKey(testGuild)); err != nil {
		t.Fatalf("review: %v", err)
	}
	posts := h.ops.sentTo("7300020")
	if rev.calls != 1 || len(posts) != 1 || !strings.Contains(posts[0].Embeds[0].Description, "Otherwise consistent") || !strings.Contains(posts[0].Embeds[0].Description, "5 entries") {
		t.Errorf("calls %d posts %+v", rev.calls, posts)
	}
	if strings.Contains(rev.lastUsr, "<@") || strings.Contains(rev.lastUsr, "1001") {
		t.Errorf("review prompt carried ids: %s", rev.lastUsr)
	}

	// Unavailable is a quiet skip; a failure is returned for the Scheduler.
	rev.err = noModel{"budget spent"}
	if err := h.sched.RunNow(context.Background(), reviewKey(testGuild)); err != nil {
		t.Errorf("unavailable model should not fail the job: %v", err)
	}
	rev.err = errors.New("gateway exploded")
	if err := h.sched.RunNow(context.Background(), reviewKey(testGuild)); err == nil {
		t.Error("a failed model call should be returned so the Scheduler backs off")
	}

	// Turning the ladder off unregisters it.
	cfg, _ := h.store.Config(context.Background(), testGuild)
	cfg.EscalationMode = ModeOff
	_ = h.store.SetConfig(context.Background(), cfg)
	h.p.SyncGuild(context.Background(), testGuild)
	if h.sched.has(reviewKey(testGuild)) {
		t.Error("still registered with the ladder off")
	}
}

func TestAltNoticeCarriesASecondOpinionWhenThereIsAModel(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	rev := &fakeReviewer{out: "Strong: identical avatar and a join minutes after the ban. Worth checking whether they greet the same people."}
	h.p.reviewer = rev
	base := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	banned := snowflakeAt(base)
	h.ops.addMember(banned)
	h.ops.users[banned] = &discord.User{ID: core.ID(banned), Username: "dana_k", Avatar: strp("abc123")}
	stubSession()
	h.p.handleBan(context.Background(), withResolved(banCmd(banned, strOpt("duration", "7d")), h.ops.users[banned]))

	h.p.HandleMemberJoin(context.Background(), testGuild, discord.Member{
		User: discord.User{ID: core.ID(snowflakeAt(base.Add(48 * time.Hour))), Username: "danak2", Avatar: strp("abc123")}, JoinedAt: ptrTime(testNow.Add(2 * time.Minute))})
	posts := h.ops.sentTo("7300020")
	if len(posts) != 1 || !strings.Contains(posts[0].Embeds[0].Description, "A model's read") || !strings.Contains(posts[0].Embeds[0].Description, "Strong:") {
		t.Fatalf("posts = %+v", posts)
	}
	if strings.Contains(rev.lastUsr, "<@") {
		t.Errorf("opinion prompt carried a mention: %s", rev.lastUsr)
	}
	hints, _ := h.store.Hints(context.Background(), testGuild, banned)
	if len(hints) != 1 || !strings.HasPrefix(hints[0].Opinion, "Strong") {
		t.Errorf("hints = %+v", hints)
	}
	// A failing model costs the opinion and nothing else.
	rev.err = errors.New("exploded")
	h.p.HandleMemberJoin(context.Background(), testGuild, discord.Member{
		User: discord.User{ID: core.ID(snowflakeAt(base.Add(72 * time.Hour))), Username: "danak3", Avatar: strp("abc123")}, JoinedAt: ptrTime(testNow.Add(3 * time.Minute))})
	if posts := h.ops.sentTo("7300020"); len(posts) != 2 || strings.Contains(posts[1].Embeds[0].Description, "A model's read") {
		t.Errorf("second notice = %+v", posts)
	}
}

func TestStatusNamesTheModelState(t *testing.T) {
	h := newHarness()
	rt := stubSession()
	h.p.handleStatus(context.Background(), interaction("status"))
	if !strings.Contains(rt.said(), "none in this build") {
		t.Errorf("got %s", rt.said())
	}
	h.p.reviewer = &fakeReviewer{}
	withModChannel(h, ModeSuggest)
	h.p.SyncGuild(context.Background(), testGuild)
	rt2 := stubSession()
	h.p.handleStatus(context.Background(), interaction("status"))
	if !strings.Contains(rt2.said(), "Mondays 09:00 UTC") {
		t.Errorf("got %s", rt2.said())
	}
}
