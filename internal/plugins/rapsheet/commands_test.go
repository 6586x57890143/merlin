package rapsheet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

func TestEveryLeafIsRegisteredWithATier(t *testing.T) {
	router := core.NewCommandRouter(nil, nil, quietLog())
	h := newHarness()
	h.p.commands = router
	h.p.registerCommands()

	if err := router.Finalize(); err != nil {
		t.Fatalf("the command tree does not finalize, so the bot would not start: %v", err)
	}
	for _, want := range []string{actionView, actionMe, actionWarn, actionNote, actionVoid, actionEdit} {
		found := false
		for _, a := range router.Actions() {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("action %q is not registered, so /config permissions cannot name it", want)
		}
	}
	if !strings.Contains(strings.Join(router.Plugins(), ","), "rapsheet") {
		t.Error("plugin name not registered, so /config plugins set cannot toggle it")
	}
}

func TestPluginLifecycleIsInert(t *testing.T) {
	h := newHarness()
	if h.p.Name() != "rapsheet" {
		t.Errorf("Name = %q", h.p.Name())
	}
	if err := h.p.Start(context.Background()); err != nil {
		t.Errorf("Start: %v", err)
	}
	if err := h.p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	h.p.SyncGuild(context.Background(), testGuild)
	h.p.ForgetGuild(testGuild)
}

// --- warn -----------------------------------------------------------------------

func TestWarnRecordsDMsAuditsAndReportsTheScore(t *testing.T) {
	h := newHarness()
	h.ops.addMember("1001")
	rt := stubSession()
	target := &discord.User{ID: core.ID("1001"), Username: "dana", GlobalName: strp("Dana")}

	h.p.handleWarn(context.Background(), withResolved(interaction("warn",
		userOpt("user", "1001"), strOpt("category", "hate_speech"), strOpt("reason", "slur in #general")), target))

	entries := h.store.all()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Kind != KindWarn || e.Category != CategoryHateSpeech || e.Points != 50 || e.ActorID != modID ||
		e.Source != SourceCommand || e.Reason != "slur in #general" || e.UserID != "1001" {
		t.Errorf("entry = %+v", e)
	}
	if got := h.ops.sentTo(dmWith("1001")); len(got) != 1 {
		t.Errorf("DMs to the member = %d, want 1", len(got))
	} else if len(got[0].Embeds) == 0 || !strings.Contains(fieldValue(got[0].Embeds[0], "Reason given"), "slur") {
		t.Errorf("the DM should carry the reason as a field, got %+v", got[0].Embeds[0])
	}
	if len(h.voice.keys) != 1 || h.voice.keys[0] != voice.KeyWarnNotice {
		t.Errorf("voice keys = %v, want [moderation.warn]", h.voice.keys)
	}
	if !h.audit.has("rapsheet.warn") {
		t.Errorf("audit rows = %v, want rapsheet.warn", h.audit.all())
	}
	said := rt.said()
	if !strings.Contains(said, "Case #1") || !strings.Contains(said, "Score is now 50") || !strings.Contains(said, "jail 2h") {
		t.Errorf("follow-up should name the case, the score and the ladder, got %s", said)
	}
	cf, ok, _ := h.store.CaseFile(context.Background(), testGuild, "1001")
	if !ok || cf.Username != "dana" || cf.GlobalName != "Dana" {
		t.Errorf("case file should be opened with the resolved identity, got %+v %v", cf, ok)
	}
}

func TestWarnHonoursAnExplicitPointsOverride(t *testing.T) {
	h := newHarness()
	h.ops.addMember("1001")
	stubSession()
	h.p.handleWarn(context.Background(), interaction("warn",
		userOpt("user", "1001"), strOpt("category", "spam"), strOpt("reason", "r"), intOpt("points", 3)))
	if e := h.store.all(); len(e) != 1 || e[0].Points != 3 {
		t.Errorf("entries = %+v, want one with 3 points", e)
	}
}

func TestWarnRefusesStaffSelfAndBots(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *harness) *core.Ix
	}{
		{"an admin, by a mod", func(h *harness) *core.Ix {
			h.ops.addMember("7300006")
			h.ranker.admins["7300006"] = true
			return interaction("warn", userOpt("user", "7300006"), strOpt("category", "spam"), strOpt("reason", "r"))
		}},
		{"the bootstrap operator, by an admin", func(h *harness) *core.Ix {
			h.ops.addMember("boot")
			h.ranker.bootstrap = "boot"
			h.ranker.admins[modID] = true
			return interaction("warn", userOpt("user", "boot"), strOpt("category", "spam"), strOpt("reason", "r"))
		}},
		{"yourself", func(h *harness) *core.Ix {
			h.ops.addMember(modID)
			return interaction("warn", userOpt("user", modID), strOpt("category", "spam"), strOpt("reason", "r"))
		}},
		{"a bot", func(h *harness) *core.Ix {
			h.ops.addMember("1101")
			return withResolved(interaction("warn", userOpt("user", "1101"), strOpt("category", "spam"), strOpt("reason", "r")),
				&discord.User{ID: core.ID("1101"), Bot: true})
		}},
		{"an unresolvable target", func(h *harness) *core.Ix {
			h.ops.memberErr = errors.New("discord is down")
			return interaction("warn", userOpt("user", "1001"), strOpt("category", "spam"), strOpt("reason", "r"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			i := tc.setup(h)
			stubSession()
			h.p.handleWarn(context.Background(), i)
			if n := len(h.store.all()); n != 0 {
				t.Errorf("%d entries written, want 0", n)
			}
			if len(h.ops.sentTo("dm-"+i.Args()["user"].String())) != 0 {
				t.Error("a refused warning must not DM anybody")
			}
		})
	}
}

func TestWarnOfSomebodyWhoLeftStillRecords(t *testing.T) {
	// Not in the server, but not staff either: the rank check runs against
	// an empty role set and the entry goes on the sheet they will have if
	// they come back.
	h := newHarness()
	stubSession()
	h.p.handleWarn(context.Background(), interaction("warn", userOpt("user", "7300017"), strOpt("category", "spam"), strOpt("reason", "r")))
	if n := len(h.store.all()); n != 1 {
		t.Errorf("entries = %d, want 1", n)
	}
}

func TestAClosedDMNeverFailsTheWarning(t *testing.T) {
	h := newHarness()
	h.ops.addMember("1001")
	h.ops.dmErr = errors.New("cannot send messages to this user")
	rt := stubSession()
	h.p.handleWarn(context.Background(), interaction("warn", userOpt("user", "1001"), strOpt("category", "spam"), strOpt("reason", "r")))
	if n := len(h.store.all()); n != 1 {
		t.Errorf("entries = %d, want 1", n)
	}
	if !strings.Contains(rt.said(), "Case #1") {
		t.Errorf("the mod should still get the case number, got %s", rt.said())
	}
}

func TestAStoreFailureIsReportedNotSwallowed(t *testing.T) {
	h := newHarness()
	h.ops.addMember("1001")
	h.store.insertErr = errors.New("db down")
	rt := stubSession()
	h.p.handleWarn(context.Background(), interaction("warn", userOpt("user", "1001"), strOpt("category", "spam"), strOpt("reason", "r")))
	if len(h.ops.sentTo(dmWith("1001"))) != 0 {
		t.Error("nothing was recorded, so nobody should be told they were warned")
	}
	if !strings.Contains(rt.said(), "db down") {
		t.Errorf("the mod should see the failure, got %s", rt.said())
	}
}

// --- note -----------------------------------------------------------------------

func TestNoteCarriesNoPointsAndSendsNoDM(t *testing.T) {
	h := newHarness()
	h.ranker.admins["7300006"] = true // a note about an admin is fine
	stubSession()
	h.p.handleNote(context.Background(), interaction("note", userOpt("user", "7300006"), strOpt("reason", "asked for a rule change")))
	e := h.store.all()
	if len(e) != 1 || e[0].Kind != KindNote || e[0].Points != 0 {
		t.Fatalf("entries = %+v", e)
	}
	if h.ops.dmOpen != 0 {
		t.Error("a note must not DM its subject")
	}
	if !h.audit.has("rapsheet.note") {
		t.Error("note not audited")
	}
}

// --- view / me --------------------------------------------------------------------

func seedEntries(h *harness, userID string, n int) {
	for i := 0; i < n; i++ {
		_, _ = h.store.Insert(context.Background(), Entry{
			GuildID: testGuild, UserID: userID, Kind: KindWarn, Category: CategorySpam, Points: 10,
			ActorID: modID, Reason: "reason", Source: SourceCommand, CreatedAt: testNow.Add(-time.Duration(i) * time.Hour),
		})
	}
}

func TestViewRendersScoreEntriesAndPages(t *testing.T) {
	h := newHarness()
	seedEntries(h, "1001", 12)
	_, _ = h.store.Insert(context.Background(), Entry{
		GuildID: testGuild, UserID: "1001", Kind: KindNote, ActorID: "7300005", Reason: "internal", Source: SourceCommand, CreatedAt: testNow,
	})
	rt := stubSession()
	h.p.handleView(context.Background(), withResolved(interaction("view", userOpt("user", "1001")), &discord.User{ID: core.ID("1001"), Username: "dana"}))
	said := rt.said()
	// Twelve warnings at ten points, spread over the last eleven hours of a
	// thirty-day half-life, decay to just under 120.
	for _, want := range []string{"Rapsheet: dana", "**Score:** 119", "jail 1d", "internal", "@7300005", "Page 1/2", "#13 note"} {
		if !strings.Contains(said, want) {
			t.Errorf("view should contain %q, got %s", want, said)
		}
	}

	// Page two, through the button.
	rt2 := stubSession()
	h.p.handleViewPage(context.Background(), componentClick(modID, viewPagePrefix("1001")+"1"), viewPagePrefix("1001")+"1")
	if !strings.Contains(rt2.said(), "Page 2/2") || !strings.Contains(rt2.said(), "#12 warned") {
		t.Errorf("page two should show the oldest entries, got %s", rt2.said())
	}
}

func TestViewOfACleanMemberSaysSo(t *testing.T) {
	h := newHarness()
	rt := stubSession()
	h.p.handleView(context.Background(), interaction("view", userOpt("user", "1009")))
	if !strings.Contains(rt.said(), "Nothing on record") || !strings.Contains(rt.said(), "**Score:** 0") {
		t.Errorf("got %s", rt.said())
	}
}

func TestMeHidesNotesModeratorsAndVoids(t *testing.T) {
	h := newHarness()
	seedEntries(h, "1001", 2)
	_, _ = h.store.Insert(context.Background(), Entry{
		GuildID: testGuild, UserID: "1001", Kind: KindNote, ActorID: modID, Reason: "STAFF-ONLY", Source: SourceCommand, CreatedAt: testNow,
	})
	id, _ := h.store.Insert(context.Background(), Entry{
		GuildID: testGuild, UserID: "1001", Kind: KindWarn, Points: 10, ActorID: modID, Reason: "MISTAKE", Source: SourceCommand, CreatedAt: testNow,
	})
	_ = h.store.Void(context.Background(), testGuild, id, modID, "oops", testNow)
	rt := stubSession()
	h.p.handleMe(context.Background(), interactionBy("1001", "me"))
	said := rt.said()
	for _, leak := range []string{"STAFF-ONLY", "MISTAKE", "@" + modID, "half-life"} {
		if strings.Contains(said, leak) {
			t.Errorf("/rapsheet me leaked %q: %s", leak, said)
		}
	}
	if !strings.Contains(said, "a moderator") || !strings.Contains(said, "**Score:** 20") {
		t.Errorf("got %s", said)
	}
}

func TestPublicViewIsTheModViewWithoutAlts(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	seedEntries(h, "1001", 12)
	_, _ = h.store.Insert(ctx, Entry{
		GuildID: testGuild, UserID: "1001", Kind: KindNote, ActorID: modID, Reason: "STAFF-ONLY", Source: SourceCommand, CreatedAt: testNow,
	})
	id, _ := h.store.Insert(ctx, Entry{
		GuildID: testGuild, UserID: "1001", Kind: KindWarn, Points: 10, ActorID: modID, Reason: "TAKEN-BACK", Source: SourceCommand, CreatedAt: testNow,
	})
	_ = h.store.Void(ctx, testGuild, id, modID, "oops", testNow)
	_ = h.store.Link(ctx, Link{GuildID: testGuild, UserID: "1001", GroupID: "1001", LinkedBy: modID})
	_ = h.store.Link(ctx, Link{GuildID: testGuild, UserID: "7300024", GroupID: "1001", LinkedBy: modID})
	_ = h.store.UpsertHint(ctx, AltHint{GuildID: testGuild, UserID: "1001", CandidateID: "7300025", Signals: []string{"same avatar"}})

	rt := stubSession()
	h.p.handleView(ctx, withResolved(
		interaction("view", userOpt("user", "1001"), boolOpt("public", true)),
		&discord.User{ID: core.ID("1001"), Username: "dana"}))
	said := rt.said()
	// Ephemeral messages cannot be forwarded, so the flag must be absent,
	// and the buttons must not be the mod view's, whose next page would
	// put the linked accounts back.
	for _, leak := range []string{"7300024", "7300025", "Linked accounts", "Possible alts", `"flags":64`, viewPrefix} {
		if strings.Contains(said, leak) {
			t.Errorf("public view carried %q: %s", leak, said)
		}
	}
	// The mod view otherwise: notes, who acted, the voided case, the score,
	// the band, and pages.
	for _, want := range []string{"Rapsheet: dana", "STAFF-ONLY", "@" + modID, "~~#14 warned~~", "voided by", "14 entries", "half-life", "jail 1d", pubPagePrefix("1001"), "Page 1/2"} {
		if !strings.Contains(said, want) {
			t.Errorf("public view should contain %q, got %s", want, said)
		}
	}

	// Page two, through the public button, stays public.
	rt2 := stubSession()
	h.p.handlePubPage(ctx, componentClick(modID, pubPagePrefix("1001")+"1"), pubPagePrefix("1001")+"1")
	said2 := rt2.said()
	if !strings.Contains(said2, "Page 2/2") || strings.Contains(said2, "7300024") || strings.Contains(said2, "7300025") {
		t.Errorf("page two should stay the public view, got %s", said2)
	}
}

func TestViewTakesAMentionOrAnIDForSomebodyWhoLeft(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	const gone = "123456789012345678"
	seedEntries(h, gone, 2)
	// Not a member any more, but still a Discord account.
	h.ops.users[gone] = &discord.User{ID: core.ID(gone), Username: "departed"}

	for _, in := range []string{gone, " " + gone + " ", "<@" + gone + ">", "<@!" + gone + ">"} {
		rt := stubSession()
		h.p.handleView(ctx, interaction("view", strOpt("user", in)))
		if said := rt.said(); !strings.Contains(said, "Rapsheet: departed") || !strings.Contains(said, "**Score:** 20") {
			t.Errorf("input %q: got %s", in, said)
		}
	}

	for in, want := range map[string]string{
		"dana":               "is not a member or a user ID",
		"<@&" + gone + ">":   "is not a member or a user ID", // a role mention
		"999999999999999999": "no Discord account has the ID",
	} {
		rt := stubSession()
		h.p.handleView(ctx, interaction("view", strOpt("user", in)))
		if !strings.Contains(rt.said(), want) {
			t.Errorf("input %q: want %q, got %s", in, want, rt.said())
		}
	}
}

func TestMePageIgnoresTheCustomIDForIdentity(t *testing.T) {
	h := newHarness()
	seedEntries(h, "victim", 11)
	rt := stubSession()
	// A member paging their own (empty) sheet cannot reach somebody else's
	// by editing the button id: the subject is always the clicker.
	h.p.handleMePage(context.Background(), componentClick("1001", mePrefix+"0"), mePrefix+"0")
	if !strings.Contains(rt.said(), "Nothing on record") {
		t.Errorf("got %s", rt.said())
	}
}

// --- void / edit --------------------------------------------------------------------

func TestVoidStrikesAnEntryAndItStopsCounting(t *testing.T) {
	h := newHarness()
	h.ops.addMember(modID)
	seedEntries(h, "1001", 1)
	rt := stubSession()
	h.p.handleVoid(context.Background(), interaction("void", intOpt("case", 1), strOpt("reason", "wrong person")))
	e, _ := h.store.Entry(context.Background(), testGuild, 1)
	if !e.Voided() || e.VoidedBy != modID || e.VoidReason != "wrong person" {
		t.Fatalf("entry = %+v", e)
	}
	if !strings.Contains(rt.said(), "voided") || !h.audit.has("rapsheet.void") {
		t.Errorf("said %s, audit %v", rt.said(), h.audit.all())
	}
	cfg := defaultConfig(testGuild)
	sh, _ := h.p.loadSheet(context.Background(), cfg, testGuild, "1001")
	if sh.Score != 0 {
		t.Errorf("a voided entry still scores %v", sh.Score)
	}
	// Twice is refused, and says so.
	rt2 := stubSession()
	h.p.handleVoid(context.Background(), interaction("void", intOpt("case", 1), strOpt("reason", "again")))
	if !strings.Contains(rt2.said(), "already voided") {
		t.Errorf("got %s", rt2.said())
	}
}

func TestVoidRespectsHierarchy(t *testing.T) {
	h := newHarness()
	h.ops.addMember("7300006")
	h.ranker.admins["7300006"] = true
	_, _ = h.store.Insert(context.Background(), Entry{
		GuildID: testGuild, UserID: "1001", Kind: KindWarn, Points: 10, ActorID: "7300006", Source: SourceCommand, CreatedAt: testNow,
	})
	rt := stubSession()
	h.p.handleVoid(context.Background(), interaction("void", intOpt("case", 1), strOpt("reason", "nope")))
	if e, _ := h.store.Entry(context.Background(), testGuild, 1); e.Voided() {
		t.Fatal("a mod voided an admin's entry")
	}
	if !strings.Contains(rt.said(), "outranks you") {
		t.Errorf("got %s", rt.said())
	}

	// An admin may; so may anyone once the recorder has left; so may anyone
	// for an automatic entry.
	h.ranker.admins[modID] = true
	stubSession()
	h.p.handleVoid(context.Background(), interaction("void", intOpt("case", 1), strOpt("reason", "peer")))
	if e, _ := h.store.Entry(context.Background(), testGuild, 1); !e.Voided() {
		t.Error("an admin could not void a peer's entry")
	}

	h2 := newHarness()
	_, _ = h2.store.Insert(context.Background(), Entry{GuildID: testGuild, UserID: "1001", Kind: KindWarn, Points: 10, ActorID: "left-1", Source: SourceCommand, CreatedAt: testNow})
	_, _ = h2.store.Insert(context.Background(), Entry{GuildID: testGuild, UserID: "1001", Kind: KindRemoval, Points: 10, ActorID: core.ActorSystem, Source: SourceAIMod, Ref: "inc-1", CreatedAt: testNow})
	for id := int64(1); id <= 2; id++ {
		stubSession()
		h2.p.handleVoid(context.Background(), interaction("void", intOpt("case", int(id)), strOpt("reason", "ok")))
		if e, _ := h2.store.Entry(context.Background(), testGuild, id); !e.Voided() {
			t.Errorf("case #%d should be voidable", id)
		}
	}

	// An unresolvable recorder fails closed.
	h3 := newHarness()
	h3.ops.memberErr = errors.New("discord is down")
	_, _ = h3.store.Insert(context.Background(), Entry{GuildID: testGuild, UserID: "1001", Kind: KindWarn, Points: 10, ActorID: "other-mod", Source: SourceCommand, CreatedAt: testNow})
	stubSession()
	h3.p.handleVoid(context.Background(), interaction("void", intOpt("case", 1), strOpt("reason", "ok")))
	if e, _ := h3.store.Entry(context.Background(), testGuild, 1); e.Voided() {
		t.Error("voided an entry whose recorder could not be ranked")
	}
}

func TestVoidOfAStandingBanIsRefused(t *testing.T) {
	h := newHarness()
	ends := testNow.Add(time.Hour)
	_, _ = h.store.Insert(context.Background(), Entry{
		GuildID: testGuild, UserID: "1001", Kind: KindBan, ActorID: modID, Source: SourceCommand, EndsAt: &ends, Duration: time.Hour, CreatedAt: testNow,
	})
	rt := stubSession()
	h.p.handleVoid(context.Background(), interaction("void", intOpt("case", 1), strOpt("reason", "x")))
	if e, _ := h.store.Entry(context.Background(), testGuild, 1); e.Voided() {
		t.Fatal("voided the record of a ban still in force")
	}
	if !strings.Contains(rt.said(), "unban first") {
		t.Errorf("got %s", rt.said())
	}
}

func TestVoidOfAMissingCaseSaysSo(t *testing.T) {
	h := newHarness()
	rt := stubSession()
	h.p.handleVoid(context.Background(), interaction("void", intOpt("case", 404), strOpt("reason", "x")))
	if !strings.Contains(rt.said(), "no such case") {
		t.Errorf("got %s", rt.said())
	}
}

func TestEditRewritesTheReason(t *testing.T) {
	h := newHarness()
	seedEntries(h, "1001", 1)
	stubSession()
	h.p.handleEdit(context.Background(), interaction("edit", intOpt("case", 1), strOpt("reason", "clearer")))
	if e, _ := h.store.Entry(context.Background(), testGuild, 1); e.Reason != "clearer" {
		t.Errorf("reason = %q", e.Reason)
	}
	if !h.audit.has("rapsheet.edit") {
		t.Error("edit not audited")
	}
}

// --- list / status ------------------------------------------------------------------

func TestListCategoriesShowsTuning(t *testing.T) {
	h := newHarness()
	cfg := defaultConfig(testGuild)
	cfg.CategoryPoints[CategorySpam] = 42
	_ = h.store.SetConfig(context.Background(), cfg)
	rt := stubSession()
	h.p.handleListCategories(context.Background(), interaction("list/categories"))
	if !strings.Contains(rt.said(), "**spam** · 42 pts (tuned)") || !strings.Contains(rt.said(), "**child safety** · 100 pts") {
		t.Errorf("got %s", rt.said())
	}
}

func TestListBandsShowsTheDefaults(t *testing.T) {
	h := newHarness()
	rt := stubSession()
	h.p.handleListBands(context.Background(), interaction("list/bands"))
	for _, want := range []string{"50+: jail 2h", "400+: ban 30d", "These are the defaults", "**suggest**"} {
		if !strings.Contains(rt.said(), want) {
			t.Errorf("want %q in %s", want, rt.said())
		}
	}
}

func TestStatusWarnsWhenSuggestionsHaveNowhereToGo(t *testing.T) {
	h := newHarness()
	rt := stubSession()
	h.p.handleStatus(context.Background(), interaction("status"))
	if !strings.Contains(rt.said(), "nowhere to go") {
		t.Errorf("got %s", rt.said())
	}
	cfg := defaultConfig(testGuild)
	cfg.ModChannelID = "7300020"
	cfg.ForumChannelID = "forum"
	_ = h.store.SetConfig(context.Background(), cfg)
	seedEntries(h, "1001", 2)
	rt2 := stubSession()
	h.p.handleStatus(context.Background(), interaction("status"))
	if !strings.Contains(rt2.said(), "#7300020") || !strings.Contains(rt2.said(), "2 entries are not mirrored") {
		t.Errorf("got %s", rt2.said())
	}
}

func TestConfigReadFailureFallsBackToDefaults(t *testing.T) {
	h := newHarness()
	h.store.configErr = errors.New("db down")
	cfg := h.p.config(context.Background(), testGuild)
	if cfg.EscalationMode != ModeSuggest || cfg.HalfLife != defaultHalfLife {
		t.Errorf("cfg = %+v", cfg)
	}
}

// fieldValue is the value of the embed field named name, or "".
func fieldValue(e discord.Embed, name string) string {
	for _, f := range e.Fields {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}
