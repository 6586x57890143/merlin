package rapsheet

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

type fakeJailer struct {
	mu    sync.Mutex
	calls []struct {
		userID string
		d      time.Duration
		reason string
	}
	err error
}

func (f *fakeJailer) JailAutomatic(_ context.Context, _, userID string, d time.Duration, reason string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, struct {
		userID string
		d      time.Duration
		reason string
	}{userID, d, reason})
	return nil
}

func withModChannel(h *harness, mode Mode) {
	h.ops.channels["mods"] = &discordgo.Channel{ID: "mods", Type: discordgo.ChannelTypeGuildText}
	cfg := defaultConfig(testGuild)
	cfg.ModChannelID = "mods"
	cfg.EscalationMode = mode
	_ = h.store.SetConfig(context.Background(), cfg)
}

// warnPts warns u1 with an explicit points override, the quickest way to
// walk a record up the ladder.
func warnPts(h *harness, userID string, pts int) {
	s, _ := stubSession()
	h.ops.addMember(userID)
	h.p.handleWarn(context.Background(), s, interaction("warn",
		userOpt("user", userID), strOpt("category", "spam"), strOpt("reason", "r"), intOpt("points", pts)))
}

func suggestions(h *harness) []Entry {
	var out []Entry
	for _, e := range h.store.all() {
		if e.Kind == KindSuggestion {
			out = append(out, e)
		}
	}
	return out
}

func TestCrossingABandSuggestsOnceAndPostsToTheModChannel(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)

	warnPts(h, "u1", 20) // 20: below notice
	if len(suggestions(h)) != 0 {
		t.Fatal("suggested below the first band")
	}
	warnPts(h, "u1", 10) // 30: notice band
	sug := suggestions(h)
	if len(sug) != 1 || sug[0].Band != 1 || sug[0].Points != 0 || sug[0].Source != SourceLadder {
		t.Fatalf("suggestions = %+v", sug)
	}
	posts := h.ops.sentTo("mods")
	if len(posts) != 1 || !strings.Contains(posts[0].Embeds[0].Title, "notice") || len(posts[0].Components) != 1 {
		t.Fatalf("mod channel posts = %+v", posts)
	}
	warnPts(h, "u1", 10) // 40: still notice band, no second suggestion
	if len(suggestions(h)) != 1 {
		t.Error("the same band was suggested twice")
	}
	warnPts(h, "u1", 20) // 60: jail 2h band
	sug = suggestions(h)
	if len(sug) != 2 || sug[1].Band != 2 || sug[1].Duration != 2*time.Hour {
		t.Errorf("suggestions = %+v", sug)
	}
	if len(h.ops.sentTo("mods")) != 2 {
		t.Error("second band not posted")
	}
}

func TestADismissedSuggestionCanBeMadeAgainLater(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	warnPts(h, "u1", 30)
	sug := suggestions(h)[0]

	id := suggestDismissPrefix + strconv.FormatInt(sug.ID, 10)
	s, rt := stubSession()
	h.p.handleSuggestion(context.Background(), s, componentClick(modID, id), id)
	if e, _ := h.store.Entry(context.Background(), testGuild, sug.ID); !e.Voided() || e.VoidReason != "dismissed" {
		t.Fatalf("suggestion after dismiss = %+v (id %d)", e, sug.ID)
	}
	if !strings.Contains(rt.said(), "Dismissed by") || strings.Contains(rt.said(), "custom_id") {
		t.Errorf("dismissed message should settle without buttons: %s", rt.said())
	}
	if !h.audit.has("rapsheet.suggestion_dismissed") {
		t.Error("dismiss not audited")
	}
	// Another offence at the same band: a different time, suggested again.
	warnPts(h, "u1", 5)
	if n := len(suggestions(h)); n != 2 {
		t.Errorf("suggestions after dismiss and re-cross = %d, want 2", n)
	}
}

func TestApplyRunsTheConsequenceThroughTheJailerAndRecordsItAtZeroPoints(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	jailer := &fakeJailer{}
	h.p.jailer = jailer
	warnPts(h, "u1", 60) // straight to jail 2h
	sug := suggestions(h)[0]
	id := suggestApplyPrefix + strconv.FormatInt(sug.ID, 10)

	s, rt := stubSession()
	h.p.handleSuggestion(context.Background(), s, componentClick(modID, id), id)

	if len(jailer.calls) != 1 || jailer.calls[0].userID != "u1" || jailer.calls[0].d != 2*time.Hour ||
		!strings.HasPrefix(jailer.calls[0].reason, ladderReasonPrefix) {
		t.Fatalf("jailer calls = %+v", jailer.calls)
	}
	var cons *Entry
	for _, e := range h.store.all() {
		if e.Kind == KindJail && e.Source == SourceLadder {
			cons = &e
		}
	}
	if cons == nil || cons.Points != 0 || cons.Band != 2 || cons.ActorID != modID || cons.Voided() {
		t.Errorf("consequence = %+v", cons)
	}
	if !strings.Contains(rt.said(), "Applied by") || !h.audit.has("rapsheet.escalated") {
		t.Errorf("said %s audit %v", rt.said(), h.audit.all())
	}
	// A second click is refused as already applied.
	s2, rt2 := stubSession()
	h.p.handleSuggestion(context.Background(), s2, componentClick("mod-2", id), id)
	if len(jailer.calls) != 1 || !strings.Contains(rt2.said(), "Already applied") {
		t.Errorf("second click: calls %d said %s", len(jailer.calls), rt2.said())
	}
	// And the sheet is not walked further by its own consequence.
	if n := len(suggestions(h)); n != 1 {
		t.Errorf("the ladder suggested again off its own row: %d", n)
	}
}

func TestAFailedApplyVoidsTheConsequenceAndLeavesTheSuggestionOpen(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	h.p.jailer = &fakeJailer{err: errors.New("jail role missing")}
	warnPts(h, "u1", 60)
	id := suggestApplyPrefix + strconv.FormatInt(suggestions(h)[0].ID, 10)

	s, rt := stubSession()
	h.p.handleSuggestion(context.Background(), s, componentClick(modID, id), id)
	for _, e := range h.store.all() {
		if e.Kind == KindJail && !e.Voided() {
			t.Errorf("a failed jail left a live consequence: %+v", e)
		}
	}
	if !strings.Contains(rt.said(), "it failed") || !strings.Contains(rt.said(), "custom_id") {
		t.Errorf("a failed apply should say so and keep the buttons: %s", rt.said())
	}
}

func TestApplyingABanNeedsAnAdminHoweverTheButtonWasRegistered(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	warnPts(h, "u1", 200) // ban 7d
	id := suggestApplyPrefix + strconv.FormatInt(suggestions(h)[0].ID, 10)

	h.ranker.authErr = core.ErrForbidden{Reason: "requires admin"}
	s, rt := stubSession()
	h.p.handleSuggestion(context.Background(), s, componentClick(modID, id), id)
	if len(h.ops.bans) != 0 || !strings.Contains(rt.said(), "needs an admin") || !strings.Contains(rt.said(), "custom_id") {
		t.Errorf("bans %v said %s", h.ops.bans, rt.said())
	}

	h.ranker.authErr = nil
	s2, _ := stubSession()
	h.p.handleSuggestion(context.Background(), s2, componentClick("admin-1", id), id)
	if _, ok := h.ops.bans["u1"]; !ok {
		t.Fatal("an admin could not apply the ban")
	}
	if !h.sched.has(sweepKey(testGuild)) {
		t.Error("a ladder ban should arm the sweep")
	}
	if h.voice.keys[len(h.voice.keys)-1] != voice.KeyBanNotice {
		t.Errorf("member should have been told: %v", h.voice.keys)
	}
}

func TestAutoModeActsOnAutomaticEntriesAndSuggestsOnAModsOwn(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeAuto)
	jailer := &fakeJailer{}
	h.p.jailer = jailer
	h.ops.addMember("u1")

	// A mod's warn that crosses into jail: suggested, not applied.
	warnPts(h, "u1", 60)
	if len(jailer.calls) != 0 || len(suggestions(h)) != 1 {
		t.Fatalf("auto mode acted on a mod's own command: jails %d suggestions %d", len(jailer.calls), len(suggestions(h)))
	}

	// An aimod removal on somebody else that crosses: applied.
	h.ops.addMember("u2")
	for i := 0; i < 2; i++ {
		publishAction(h, testGuild, core.ModerationActionPayload{
			UserID: "u2", Kind: "removal", Category: "hate_speech", ActorID: core.ActorSystem, Source: "aimod", Ref: "inc-" + strconv.Itoa(i),
		})
	}
	// The first removal (50) crossed into jail 2h and was applied; the
	// second (100) crossed into jail 1d, which the standing 2h jail does not
	// cover, so it was applied too (roles extends, never shortens).
	if len(jailer.calls) != 2 || jailer.calls[0].d != 2*time.Hour || jailer.calls[1].userID != "u2" || jailer.calls[1].d != 24*time.Hour {
		t.Fatalf("auto mode did not act on aimod entries: %+v", jailer.calls)
	}
	var cons *Entry
	for _, e := range h.store.all() {
		if e.UserID == "u2" && e.Kind == KindJail {
			cons = &e
		}
	}
	if cons == nil || cons.ActorID != core.ActorSystem || cons.Source != SourceLadder || cons.Band != 3 {
		t.Errorf("consequence = %+v", cons)
	}
}

func TestTheLadderNeverActsOnStaffOrTheOperator(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeAuto)
	jailer := &fakeJailer{}
	h.p.jailer = jailer
	h.ops.addMember("admin-1")
	h.ranker.admins["admin-1"] = true
	h.ops.addMember("boot")
	h.ranker.bootstrap = "boot"

	for _, u := range []string{"admin-1", "boot"} {
		// An admin cannot be warned by a mod, so the points arrive over the
		// bus (a Discord-side ban by another admin, say).
		publishAction(h, testGuild, core.ModerationActionPayload{UserID: u, Kind: "kick", ActorID: "owner", Source: "discord", Ref: "k-" + u})
		publishAction(h, testGuild, core.ModerationActionPayload{UserID: u, Kind: "kick", ActorID: "owner", Source: "discord", Ref: "k2-" + u})
		publishAction(h, testGuild, core.ModerationActionPayload{UserID: u, Kind: "kick", ActorID: "owner", Source: "discord", Ref: "k3-" + u})
	}
	if len(jailer.calls) != 0 {
		t.Fatalf("the ladder jailed staff: %+v", jailer.calls)
	}
	for _, e := range h.store.all() {
		if e.Source == SourceLadder && e.Kind != KindSuggestion && !e.Voided() {
			t.Errorf("a live ladder consequence against staff: %+v", e)
		}
	}
}

func TestAStandingConsequenceSuppressesAWeakerSuggestion(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	// aimod jailed them for a day already (arrives as a zero-point jail),
	// and the offence carried 100 points: jail 24h band.
	ends := testNow.Add(24 * time.Hour)
	publishAction(h, testGuild, core.ModerationActionPayload{UserID: "u1", Kind: "jail", ActorID: core.ActorSystem, Duration: 24 * time.Hour, EndsAt: &ends, Source: "roles"})
	publishAction(h, testGuild, core.ModerationActionPayload{UserID: "u1", Kind: "removal", Category: "threats", ActorID: core.ActorSystem, Source: "aimod", Ref: "1"})
	if n := len(suggestions(h)); n != 0 {
		t.Errorf("suggested a 24h jail over a standing 24h jail: %d", n)
	}
	// A second critical offence reaches the ban band, which the jail does
	// not cover.
	publishAction(h, testGuild, core.ModerationActionPayload{UserID: "u1", Kind: "removal", Category: "threats", ActorID: core.ActorSystem, Source: "aimod", Ref: "2"})
	sug := suggestions(h)
	if len(sug) != 1 || sug[0].Band != 4 {
		t.Errorf("suggestions = %+v", sug)
	}
}

func TestNoModChannelMeansNothingIsRecordedOrPosted(t *testing.T) {
	h := newHarness()
	warnPts(h, "u1", 60)
	if len(suggestions(h)) != 0 {
		t.Error("a suggestion was recorded with nowhere to post it, silencing the band")
	}
}

func TestModeOffRecordsAndScoresOnly(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeOff)
	warnPts(h, "u1", 200)
	if len(suggestions(h)) != 0 || len(h.ops.sentTo("mods")) != 0 {
		t.Error("mode off suggested")
	}
}

func TestPriorsCountScoredOffencesAcrossTheGroupAndDeferWhenDisabled(t *testing.T) {
	h := newHarness()
	warnPts(h, "u1", 10)
	warnPts(h, "u2", 10)
	_ = h.store.Link(context.Background(), Link{GuildID: testGuild, UserID: "u1", GroupID: "u1", LinkedBy: modID})
	_ = h.store.Link(context.Background(), Link{GuildID: testGuild, UserID: "u2", GroupID: "u1", LinkedBy: modID})
	_, _ = h.store.Insert(context.Background(), Entry{GuildID: testGuild, UserID: "u1", Kind: KindNote, ActorID: modID, Source: SourceCommand, CreatedAt: testNow})
	n, ok, err := h.p.Priors(context.Background(), testGuild, "u1", testNow.Add(-time.Hour))
	if err != nil || !ok || n != 2 {
		t.Errorf("Priors = %d %v %v, want 2 (both accounts, note excluded)", n, ok, err)
	}
	h.p.gate = fakeGate{disabled: map[string]bool{testGuild: true}}
	if _, ok, _ := h.p.Priors(context.Background(), testGuild, "u1", testNow.Add(-time.Hour)); ok {
		t.Error("a disabled rapsheet should have no opinion")
	}
}

// --- configure ---------------------------------------------------------------------------

func TestConfigureModeHalfLifePointsAndBands(t *testing.T) {
	h := newHarness()
	s, rt := stubSession()
	h.p.handleConfigureMode(context.Background(), s, interaction("configure/mode", strOpt("mode", "auto")))
	if cfg, _ := h.store.Config(context.Background(), testGuild); cfg.EscalationMode != ModeAuto {
		t.Errorf("mode = %s", cfg.EscalationMode)
	}
	if !strings.Contains(rt.said(), "No mod channel is set") {
		t.Errorf("auto with no mod channel should warn: %s", rt.said())
	}

	s2, _ := stubSession()
	h.p.handleConfigureModChannel(context.Background(), s2, interaction("configure/mod-channel",
		&discordgo.ApplicationCommandInteractionDataOption{Name: "channel", Type: discordgo.ApplicationCommandOptionChannel, Value: "mods"}))

	s3, rt3 := stubSession()
	h.p.handleConfigureHalfLife(context.Background(), s3, interaction("configure/half-life", strOpt("duration", "14d")))
	s4, rt4 := stubSession()
	h.p.handleConfigureHalfLife(context.Background(), s4, interaction("configure/half-life", strOpt("duration", "2h")))
	if cfg, _ := h.store.Config(context.Background(), testGuild); cfg.HalfLife != 14*24*time.Hour || cfg.ModChannelID != "mods" {
		t.Errorf("cfg = %+v", cfg)
	}
	if !strings.Contains(rt3.said(), "count half after 14d") || !strings.Contains(rt4.said(), "between") {
		t.Errorf("said %s / %s", rt3.said(), rt4.said())
	}

	s5, _ := stubSession()
	h.p.handleConfigurePoints(context.Background(), s5, interaction("configure/points", strOpt("category", "spam"), intOpt("points", 3)))
	s6, _ := stubSession()
	h.p.handleConfigurePoints(context.Background(), s6, interaction("configure/points", strOpt("category", "gore")))
	cfg, _ := h.store.Config(context.Background(), testGuild)
	if cfg.CategoryPoints[CategorySpam] != 3 {
		t.Errorf("points = %v", cfg.CategoryPoints)
	}
	if _, tuned := cfg.CategoryPoints[CategoryGore]; tuned {
		t.Error("omitting points should reset the category")
	}

	s7, rt7 := stubSession()
	h.p.handleConfigureBands(context.Background(), s7, interaction("configure/bands", strOpt("ladder", "20 notice, 40 timeout 1h, 80 jail 2h, 160 ban 24h")))
	cfg, _ = h.store.Config(context.Background(), testGuild)
	if len(cfg.Bands) != 5 || cfg.Bands[2].Action != ActionTimeout || cfg.Bands[4].Duration != 24*time.Hour {
		t.Errorf("bands = %+v", cfg.Bands)
	}
	if !strings.Contains(rt7.said(), "40+: timeout 1h") {
		t.Errorf("got %s", rt7.said())
	}
	s8, rt8 := stubSession()
	h.p.handleConfigureBands(context.Background(), s8, interaction("configure/bands", strOpt("ladder", "50 ban")))
	if !strings.Contains(rt8.said(), "never bans permanently") {
		t.Errorf("a permanent ban band was accepted: %s", rt8.said())
	}
	s9, _ := stubSession()
	h.p.handleConfigureBands(context.Background(), s9, interaction("configure/bands", strOpt("ladder", "default")))
	if cfg, _ := h.store.Config(context.Background(), testGuild); len(cfg.Bands) != 0 {
		t.Error("default did not clear the bands")
	}
	if !h.audit.has("rapsheet.configured") {
		t.Error("configuration changes not audited")
	}
}

func TestParseBandsErrors(t *testing.T) {
	for _, spec := range []string{"", "x", "50 jail", "abc jail 1h", "50 jail 1h 2h", "50 jail 1h, 25 notice", "50 smite 1h"} {
		if _, err := parseBands(spec); err == nil {
			t.Errorf("parseBands(%q) accepted", spec)
		}
	}
	b, err := parseBands("25 notice, 50 jail 2h")
	if err != nil || len(b) != 3 || b[0].Action != ActionNone || b[2].Duration != 2*time.Hour {
		t.Errorf("parseBands = %+v, %v", b, err)
	}
}

func TestVoidingTheOffenceWithdrawsTheSuggestionItCaused(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	h.ops.addMember(modID)
	warnPts(h, "u1", 30)  // notice band, suggested
	warnPts(h, "u1", 100) // jail 1d band, suggested
	sug := suggestions(h)
	if len(sug) != 2 {
		t.Fatalf("suggestions = %+v", sug)
	}
	// The 100-point warning was a mistake.
	s, _ := stubSession()
	h.p.handleVoid(context.Background(), s, interaction("void", intOpt("case", 3), strOpt("reason", "misread")))
	sug = suggestions(h)
	if sug[0].Voided() || !sug[1].Voided() || !strings.Contains(sug[1].VoidReason, "no longer reaches") {
		t.Errorf("after void: notice suggestion %v, jail suggestion %v (%q)", sug[0].Voided(), sug[1].Voided(), sug[1].VoidReason)
	}
	// A click on the withdrawn one says so and applies nothing.
	h.p.jailer = &fakeJailer{}
	id := suggestApplyPrefix + strconv.FormatInt(sug[1].ID, 10)
	s2, rt := stubSession()
	h.p.handleSuggestion(context.Background(), s2, componentClick(modID, id), id)
	if len(h.p.jailer.(*fakeJailer).calls) != 0 || !strings.Contains(rt.said(), "dismissed") && !strings.Contains(rt.said(), "Withdrawn") && !strings.Contains(rt.said(), "withdrawn") {
		t.Errorf("said %s", rt.said())
	}
}

func TestApplyRefusesWhenTheRecordNoLongerReachesTheBand(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	h.p.jailer = &fakeJailer{}
	warnPts(h, "u1", 60)
	sug := suggestions(h)[0]
	// The warning is voided directly in the store, bypassing the command's
	// own withdrawal, so the click is what finds out.
	_ = h.store.Void(context.Background(), testGuild, 1, "mod-2", "x", testNow)
	id := suggestApplyPrefix + strconv.FormatInt(sug.ID, 10)
	s, rt := stubSession()
	h.p.handleSuggestion(context.Background(), s, componentClick(modID, id), id)
	if len(h.p.jailer.(*fakeJailer).calls) != 0 || !strings.Contains(rt.said(), "no longer reaches") {
		t.Errorf("calls %d said %s", len(h.p.jailer.(*fakeJailer).calls), rt.said())
	}
	if e, _ := h.store.Entry(context.Background(), testGuild, sug.ID); !e.Voided() {
		t.Error("the stale suggestion should be withdrawn on the click")
	}
}
