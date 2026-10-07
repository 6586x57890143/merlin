package rapsheet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

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
	h.ops.addMember("u1")
	s, rt := stubSession()
	target := &discordgo.User{ID: "u1", Username: "dana", GlobalName: "Dana"}

	h.p.handleWarn(context.Background(), s, withResolved(interaction("warn",
		userOpt("user", "u1"), strOpt("category", "hate_speech"), strOpt("reason", "slur in #general")), target))

	entries := h.store.all()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Kind != KindWarn || e.Category != CategoryHateSpeech || e.Points != 50 || e.ActorID != modID ||
		e.Source != SourceCommand || e.Reason != "slur in #general" || e.UserID != "u1" {
		t.Errorf("entry = %+v", e)
	}
	if got := h.ops.sentTo("dm-u1"); len(got) != 1 {
		t.Errorf("DMs to the member = %d, want 1", len(got))
	} else if got[0].Embed == nil || !strings.Contains(fieldValue(got[0].Embed, "Reason given"), "slur") {
		t.Errorf("the DM should carry the reason as a field, got %+v", got[0].Embed)
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
	cf, ok, _ := h.store.CaseFile(context.Background(), testGuild, "u1")
	if !ok || cf.Username != "dana" || cf.GlobalName != "Dana" {
		t.Errorf("case file should be opened with the resolved identity, got %+v %v", cf, ok)
	}
}

func TestWarnHonoursAnExplicitPointsOverride(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	s, _ := stubSession()
	h.p.handleWarn(context.Background(), s, interaction("warn",
		userOpt("user", "u1"), strOpt("category", "spam"), strOpt("reason", "r"), intOpt("points", 3)))
	if e := h.store.all(); len(e) != 1 || e[0].Points != 3 {
		t.Errorf("entries = %+v, want one with 3 points", e)
	}
}

func TestWarnRefusesStaffSelfAndBots(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *harness) *discordgo.InteractionCreate
	}{
		{"an admin, by a mod", func(h *harness) *discordgo.InteractionCreate {
			h.ops.addMember("admin-1")
			h.ranker.admins["admin-1"] = true
			return interaction("warn", userOpt("user", "admin-1"), strOpt("category", "spam"), strOpt("reason", "r"))
		}},
		{"the bootstrap operator, by an admin", func(h *harness) *discordgo.InteractionCreate {
			h.ops.addMember("boot")
			h.ranker.bootstrap = "boot"
			h.ranker.admins[modID] = true
			return interaction("warn", userOpt("user", "boot"), strOpt("category", "spam"), strOpt("reason", "r"))
		}},
		{"yourself", func(h *harness) *discordgo.InteractionCreate {
			h.ops.addMember(modID)
			return interaction("warn", userOpt("user", modID), strOpt("category", "spam"), strOpt("reason", "r"))
		}},
		{"a bot", func(h *harness) *discordgo.InteractionCreate {
			h.ops.addMember("bot-1")
			return withResolved(interaction("warn", userOpt("user", "bot-1"), strOpt("category", "spam"), strOpt("reason", "r")),
				&discordgo.User{ID: "bot-1", Bot: true})
		}},
		{"an unresolvable target", func(h *harness) *discordgo.InteractionCreate {
			h.ops.memberErr = errors.New("discord is down")
			return interaction("warn", userOpt("user", "u1"), strOpt("category", "spam"), strOpt("reason", "r"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			i := tc.setup(h)
			s, _ := stubSession()
			h.p.handleWarn(context.Background(), s, i)
			if n := len(h.store.all()); n != 0 {
				t.Errorf("%d entries written, want 0", n)
			}
			if len(h.ops.sentTo("dm-"+i.ApplicationCommandData().Options[0].Options[0].Value.(string))) != 0 {
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
	s, _ := stubSession()
	h.p.handleWarn(context.Background(), s, interaction("warn", userOpt("user", "gone"), strOpt("category", "spam"), strOpt("reason", "r")))
	if n := len(h.store.all()); n != 1 {
		t.Errorf("entries = %d, want 1", n)
	}
}

func TestAClosedDMNeverFailsTheWarning(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	h.ops.dmErr = errors.New("cannot send messages to this user")
	s, rt := stubSession()
	h.p.handleWarn(context.Background(), s, interaction("warn", userOpt("user", "u1"), strOpt("category", "spam"), strOpt("reason", "r")))
	if n := len(h.store.all()); n != 1 {
		t.Errorf("entries = %d, want 1", n)
	}
	if !strings.Contains(rt.said(), "Case #1") {
		t.Errorf("the mod should still get the case number, got %s", rt.said())
	}
}

func TestAStoreFailureIsReportedNotSwallowed(t *testing.T) {
	h := newHarness()
	h.ops.addMember("u1")
	h.store.insertErr = errors.New("db down")
	s, rt := stubSession()
	h.p.handleWarn(context.Background(), s, interaction("warn", userOpt("user", "u1"), strOpt("category", "spam"), strOpt("reason", "r")))
	if len(h.ops.sentTo("dm-u1")) != 0 {
		t.Error("nothing was recorded, so nobody should be told they were warned")
	}
	if !strings.Contains(rt.said(), "db down") {
		t.Errorf("the mod should see the failure, got %s", rt.said())
	}
}

// --- note -----------------------------------------------------------------------

func TestNoteCarriesNoPointsAndSendsNoDM(t *testing.T) {
	h := newHarness()
	h.ranker.admins["admin-1"] = true // a note about an admin is fine
	s, _ := stubSession()
	h.p.handleNote(context.Background(), s, interaction("note", userOpt("user", "admin-1"), strOpt("reason", "asked for a rule change")))
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
	seedEntries(h, "u1", 12)
	_, _ = h.store.Insert(context.Background(), Entry{
		GuildID: testGuild, UserID: "u1", Kind: KindNote, ActorID: "mod-2", Reason: "internal", Source: SourceCommand, CreatedAt: testNow,
	})
	s, rt := stubSession()
	h.p.handleView(context.Background(), s, withResolved(interaction("view", userOpt("user", "u1")), &discordgo.User{ID: "u1", Username: "dana"}))
	said := rt.said()
	// Twelve warnings at ten points, spread over the last eleven hours of a
	// thirty-day half-life, decay to just under 120.
	for _, want := range []string{"Rapsheet: dana", "**Score:** 119", "jail 1d", "internal", "@mod-2", "Page 1/2", "#13 note"} {
		if !strings.Contains(said, want) {
			t.Errorf("view should contain %q, got %s", want, said)
		}
	}

	// Page two, through the button.
	s2, rt2 := stubSession()
	h.p.handleViewPage(context.Background(), s2, componentClick(modID, viewPagePrefix("u1")+"1"), viewPagePrefix("u1")+"1")
	if !strings.Contains(rt2.said(), "Page 2/2") || !strings.Contains(rt2.said(), "#12 warned") {
		t.Errorf("page two should show the oldest entries, got %s", rt2.said())
	}
}

func TestViewOfACleanMemberSaysSo(t *testing.T) {
	h := newHarness()
	s, rt := stubSession()
	h.p.handleView(context.Background(), s, interaction("view", userOpt("user", "u9")))
	if !strings.Contains(rt.said(), "Nothing on record") || !strings.Contains(rt.said(), "**Score:** 0") {
		t.Errorf("got %s", rt.said())
	}
}

func TestMeHidesNotesModeratorsAndVoids(t *testing.T) {
	h := newHarness()
	seedEntries(h, "u1", 2)
	_, _ = h.store.Insert(context.Background(), Entry{
		GuildID: testGuild, UserID: "u1", Kind: KindNote, ActorID: modID, Reason: "STAFF-ONLY", Source: SourceCommand, CreatedAt: testNow,
	})
	id, _ := h.store.Insert(context.Background(), Entry{
		GuildID: testGuild, UserID: "u1", Kind: KindWarn, Points: 10, ActorID: modID, Reason: "MISTAKE", Source: SourceCommand, CreatedAt: testNow,
	})
	_ = h.store.Void(context.Background(), testGuild, id, modID, "oops", testNow)
	s, rt := stubSession()
	h.p.handleMe(context.Background(), s, interactionBy("u1", "me"))
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
	seedEntries(h, "u1", 12)
	_, _ = h.store.Insert(ctx, Entry{
		GuildID: testGuild, UserID: "u1", Kind: KindNote, ActorID: modID, Reason: "STAFF-ONLY", Source: SourceCommand, CreatedAt: testNow,
	})
	id, _ := h.store.Insert(ctx, Entry{
		GuildID: testGuild, UserID: "u1", Kind: KindWarn, Points: 10, ActorID: modID, Reason: "TAKEN-BACK", Source: SourceCommand, CreatedAt: testNow,
	})
	_ = h.store.Void(ctx, testGuild, id, modID, "oops", testNow)
	_ = h.store.Link(ctx, Link{GuildID: testGuild, UserID: "u1", GroupID: "u1", LinkedBy: modID})
	_ = h.store.Link(ctx, Link{GuildID: testGuild, UserID: "alt-account", GroupID: "u1", LinkedBy: modID})
	_ = h.store.UpsertHint(ctx, AltHint{GuildID: testGuild, UserID: "u1", CandidateID: "hint-account", Signals: []string{"same avatar"}})

	s, rt := stubSession()
	h.p.handleView(ctx, s, withResolved(
		interaction("view", userOpt("user", "u1"), boolOpt("public", true)),
		&discordgo.User{ID: "u1", Username: "dana"}))
	said := rt.said()
	// Ephemeral messages cannot be forwarded, so the flag must be absent,
	// and the buttons must not be the mod view's, whose next page would
	// put the linked accounts back.
	for _, leak := range []string{"alt-account", "hint-account", "Linked accounts", "Possible alts", `"flags":64`, viewPrefix} {
		if strings.Contains(said, leak) {
			t.Errorf("public view carried %q: %s", leak, said)
		}
	}
	// The mod view otherwise: notes, who acted, the voided case, the score,
	// the band, and pages.
	for _, want := range []string{"Rapsheet: dana", "STAFF-ONLY", "@" + modID, "~~#14 warned~~", "voided by", "14 entries", "half-life", "jail 1d", pubPagePrefix("u1"), "Page 1/2"} {
		if !strings.Contains(said, want) {
			t.Errorf("public view should contain %q, got %s", want, said)
		}
	}

	// Page two, through the public button, stays public.
	s2, rt2 := stubSession()
	h.p.handlePubPage(ctx, s2, componentClick(modID, pubPagePrefix("u1")+"1"), pubPagePrefix("u1")+"1")
	said2 := rt2.said()
	if !strings.Contains(said2, "Page 2/2") || strings.Contains(said2, "alt-account") || strings.Contains(said2, "hint-account") {
		t.Errorf("page two should stay the public view, got %s", said2)
	}
}

func TestViewTakesAMentionOrAnIDForSomebodyWhoLeft(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	const gone = "123456789012345678"
	seedEntries(h, gone, 2)
	// Not a member any more, but still a Discord account.
	h.ops.users[gone] = &discordgo.User{ID: gone, Username: "departed"}

	for _, in := range []string{gone, " " + gone + " ", "<@" + gone + ">", "<@!" + gone + ">"} {
		s, rt := stubSession()
		h.p.handleView(ctx, s, interaction("view", strOpt("user", in)))
		if said := rt.said(); !strings.Contains(said, "Rapsheet: departed") || !strings.Contains(said, "**Score:** 20") {
			t.Errorf("input %q: got %s", in, said)
		}
	}

	for in, want := range map[string]string{
		"dana":               "is not a member or a user ID",
		"<@&" + gone + ">":   "is not a member or a user ID", // a role mention
		"999999999999999999": "no Discord account has the ID",
	} {
		s, rt := stubSession()
		h.p.handleView(ctx, s, interaction("view", strOpt("user", in)))
		if !strings.Contains(rt.said(), want) {
			t.Errorf("input %q: want %q, got %s", in, want, rt.said())
		}
	}
}

func TestMePageIgnoresTheCustomIDForIdentity(t *testing.T) {
	h := newHarness()
	seedEntries(h, "victim", 11)
	s, rt := stubSession()
	// A member paging their own (empty) sheet cannot reach somebody else's
	// by editing the button id: the subject is always the clicker.
	h.p.handleMePage(context.Background(), s, componentClick("u1", mePrefix+"0"), mePrefix+"0")
	if !strings.Contains(rt.said(), "Nothing on record") {
		t.Errorf("got %s", rt.said())
	}
}

// --- void / edit --------------------------------------------------------------------

func TestVoidStrikesAnEntryAndItStopsCounting(t *testing.T) {
	h := newHarness()
	h.ops.addMember(modID)
	seedEntries(h, "u1", 1)
	s, rt := stubSession()
	h.p.handleVoid(context.Background(), s, interaction("void", intOpt("case", 1), strOpt("reason", "wrong person")))
	e, _ := h.store.Entry(context.Background(), testGuild, 1)
	if !e.Voided() || e.VoidedBy != modID || e.VoidReason != "wrong person" {
		t.Fatalf("entry = %+v", e)
	}
	if !strings.Contains(rt.said(), "voided") || !h.audit.has("rapsheet.void") {
		t.Errorf("said %s, audit %v", rt.said(), h.audit.all())
	}
	cfg := defaultConfig(testGuild)
	sh, _ := h.p.loadSheet(context.Background(), cfg, testGuild, "u1")
	if sh.Score != 0 {
		t.Errorf("a voided entry still scores %v", sh.Score)
	}
	// Twice is refused, and says so.
	s2, rt2 := stubSession()
	h.p.handleVoid(context.Background(), s2, interaction("void", intOpt("case", 1), strOpt("reason", "again")))
	if !strings.Contains(rt2.said(), "already voided") {
		t.Errorf("got %s", rt2.said())
	}
}

func TestVoidRespectsHierarchy(t *testing.T) {
	h := newHarness()
	h.ops.addMember("admin-1")
	h.ranker.admins["admin-1"] = true
	_, _ = h.store.Insert(context.Background(), Entry{
		GuildID: testGuild, UserID: "u1", Kind: KindWarn, Points: 10, ActorID: "admin-1", Source: SourceCommand, CreatedAt: testNow,
	})
	s, rt := stubSession()
	h.p.handleVoid(context.Background(), s, interaction("void", intOpt("case", 1), strOpt("reason", "nope")))
	if e, _ := h.store.Entry(context.Background(), testGuild, 1); e.Voided() {
		t.Fatal("a mod voided an admin's entry")
	}
	if !strings.Contains(rt.said(), "outranks you") {
		t.Errorf("got %s", rt.said())
	}

	// An admin may; so may anyone once the recorder has left; so may anyone
	// for an automatic entry.
	h.ranker.admins[modID] = true
	s2, _ := stubSession()
	h.p.handleVoid(context.Background(), s2, interaction("void", intOpt("case", 1), strOpt("reason", "peer")))
	if e, _ := h.store.Entry(context.Background(), testGuild, 1); !e.Voided() {
		t.Error("an admin could not void a peer's entry")
	}

	h2 := newHarness()
	_, _ = h2.store.Insert(context.Background(), Entry{GuildID: testGuild, UserID: "u1", Kind: KindWarn, Points: 10, ActorID: "left-1", Source: SourceCommand, CreatedAt: testNow})
	_, _ = h2.store.Insert(context.Background(), Entry{GuildID: testGuild, UserID: "u1", Kind: KindRemoval, Points: 10, ActorID: core.ActorSystem, Source: SourceAIMod, Ref: "inc-1", CreatedAt: testNow})
	for id := int64(1); id <= 2; id++ {
		s3, _ := stubSession()
		h2.p.handleVoid(context.Background(), s3, interaction("void", intOpt("case", int(id)), strOpt("reason", "ok")))
		if e, _ := h2.store.Entry(context.Background(), testGuild, id); !e.Voided() {
			t.Errorf("case #%d should be voidable", id)
		}
	}

	// An unresolvable recorder fails closed.
	h3 := newHarness()
	h3.ops.memberErr = errors.New("discord is down")
	_, _ = h3.store.Insert(context.Background(), Entry{GuildID: testGuild, UserID: "u1", Kind: KindWarn, Points: 10, ActorID: "other-mod", Source: SourceCommand, CreatedAt: testNow})
	s4, _ := stubSession()
	h3.p.handleVoid(context.Background(), s4, interaction("void", intOpt("case", 1), strOpt("reason", "ok")))
	if e, _ := h3.store.Entry(context.Background(), testGuild, 1); e.Voided() {
		t.Error("voided an entry whose recorder could not be ranked")
	}
}

func TestVoidOfAStandingBanIsRefused(t *testing.T) {
	h := newHarness()
	ends := testNow.Add(time.Hour)
	_, _ = h.store.Insert(context.Background(), Entry{
		GuildID: testGuild, UserID: "u1", Kind: KindBan, ActorID: modID, Source: SourceCommand, EndsAt: &ends, Duration: time.Hour, CreatedAt: testNow,
	})
	s, rt := stubSession()
	h.p.handleVoid(context.Background(), s, interaction("void", intOpt("case", 1), strOpt("reason", "x")))
	if e, _ := h.store.Entry(context.Background(), testGuild, 1); e.Voided() {
		t.Fatal("voided the record of a ban still in force")
	}
	if !strings.Contains(rt.said(), "unban first") {
		t.Errorf("got %s", rt.said())
	}
}

func TestVoidOfAMissingCaseSaysSo(t *testing.T) {
	h := newHarness()
	s, rt := stubSession()
	h.p.handleVoid(context.Background(), s, interaction("void", intOpt("case", 404), strOpt("reason", "x")))
	if !strings.Contains(rt.said(), "no such case") {
		t.Errorf("got %s", rt.said())
	}
}

func TestEditRewritesTheReason(t *testing.T) {
	h := newHarness()
	seedEntries(h, "u1", 1)
	s, _ := stubSession()
	h.p.handleEdit(context.Background(), s, interaction("edit", intOpt("case", 1), strOpt("reason", "clearer")))
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
	s, rt := stubSession()
	h.p.handleListCategories(context.Background(), s, interaction("list/categories"))
	if !strings.Contains(rt.said(), "**spam** · 42 pts (tuned)") || !strings.Contains(rt.said(), "**child safety** · 100 pts") {
		t.Errorf("got %s", rt.said())
	}
}

func TestListBandsShowsTheDefaults(t *testing.T) {
	h := newHarness()
	s, rt := stubSession()
	h.p.handleListBands(context.Background(), s, interaction("list/bands"))
	for _, want := range []string{"50+: jail 2h", "400+: ban 30d", "These are the defaults", "**suggest**"} {
		if !strings.Contains(rt.said(), want) {
			t.Errorf("want %q in %s", want, rt.said())
		}
	}
}

func TestStatusWarnsWhenSuggestionsHaveNowhereToGo(t *testing.T) {
	h := newHarness()
	s, rt := stubSession()
	h.p.handleStatus(context.Background(), s, interaction("status"))
	if !strings.Contains(rt.said(), "nowhere to go") {
		t.Errorf("got %s", rt.said())
	}
	cfg := defaultConfig(testGuild)
	cfg.ModChannelID = "mods"
	cfg.ForumChannelID = "forum"
	_ = h.store.SetConfig(context.Background(), cfg)
	seedEntries(h, "u1", 2)
	s2, rt2 := stubSession()
	h.p.handleStatus(context.Background(), s2, interaction("status"))
	if !strings.Contains(rt2.said(), "#mods") || !strings.Contains(rt2.said(), "2 entries are not mirrored") {
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
func fieldValue(e *discordgo.MessageEmbed, name string) string {
	for _, f := range e.Fields {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}
