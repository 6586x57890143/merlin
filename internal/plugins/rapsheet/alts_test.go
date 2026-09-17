package rapsheet

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// snowflakeAt builds a Discord id whose creation time is t.
func snowflakeAt(t time.Time) string {
	const discordEpoch = 1420070400000
	return strconv.FormatInt((t.UnixMilli()-discordEpoch)<<22, 10)
}

func TestAltSignalsScoreEachCueSeparately(t *testing.T) {
	base := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	onFile := CaseFile{UserID: snowflakeAt(base), Username: "dana_k", GlobalName: "Dana", AvatarHash: "abc123"}
	join := testNow

	at := func(t time.Time) *Entry {
		ends := t.Add(7 * 24 * time.Hour)
		return &Entry{Kind: KindBan, Duration: 7 * 24 * time.Hour, EndsAt: &ends, CreatedAt: t}
	}
	cases := []struct {
		name    string
		joiner  *discordgo.User
		action  *Entry
		signals int
		score   int
	}{
		{"nothing in common", &discordgo.User{ID: snowflakeAt(base.Add(48 * time.Hour)), Username: "zed", Avatar: "zzz"}, nil, 0, 0},
		{"same avatar", &discordgo.User{ID: snowflakeAt(base.Add(48 * time.Hour)), Username: "zed", Avatar: "abc123"}, nil, 1, 3},
		{"same name exactly (letters only)", &discordgo.User{ID: snowflakeAt(base.Add(48 * time.Hour)), Username: "DANAK2"}, nil, 1, 2},
		{"name prefix", &discordgo.User{ID: snowflakeAt(base.Add(48 * time.Hour)), Username: "danakreturns"}, nil, 1, 1},
		{"short prefix does not count", &discordgo.User{ID: snowflakeAt(base.Add(48 * time.Hour)), Username: "dan"}, nil, 0, 0},
		{"created minutes apart", &discordgo.User{ID: snowflakeAt(base.Add(4 * time.Minute)), Username: "zed"}, nil, 1, 2},
		{"joined right after their ban", &discordgo.User{ID: snowflakeAt(base.Add(48 * time.Hour)), Username: "zed"}, at(join.Add(-5 * time.Minute)), 1, 2},
		{"joined long after their ban", &discordgo.User{ID: snowflakeAt(base.Add(48 * time.Hour)), Username: "zed"}, at(join.Add(-5 * time.Hour)), 0, 0},
		{"everything", &discordgo.User{ID: snowflakeAt(base.Add(time.Minute)), Username: "dana_k", Avatar: "abc123"}, at(join.Add(-time.Minute)), 4, 9},
		{"the same account", &discordgo.User{ID: onFile.UserID, Username: "dana_k", Avatar: "abc123"}, nil, 0, 0},
		{"no default avatar match", &discordgo.User{ID: snowflakeAt(base.Add(48 * time.Hour)), Username: "zed", Avatar: ""}, nil, 0, 0},
	}
	for _, tc := range cases {
		signals, score := altSignals(tc.joiner, join, onFile, tc.action)
		if len(signals) != tc.signals || score != tc.score {
			t.Errorf("%s: signals %v score %d, want %d signals scoring %d", tc.name, signals, score, tc.signals, tc.score)
		}
	}
	// The join-after signal names the sanction and the gap, never a template.
	if signals, _ := altSignals(&discordgo.User{ID: snowflakeAt(base.Add(48 * time.Hour)), Username: "zed"}, join, onFile, at(join.Add(-5*time.Minute))); len(signals) != 1 || signals[0] != "joined 5 minutes after being banned 7d" {
		t.Errorf("signal text = %v", signals)
	}
	// An empty avatar on file never matches an empty one on the joiner.
	if signals, _ := altSignals(&discordgo.User{ID: "1", Avatar: ""}, join, CaseFile{UserID: "2", AvatarHash: ""}, nil); strings.Contains(strings.Join(signals, ","), "avatar") {
		t.Error("two default avatars matched")
	}
}

func TestAJoinThatLooksLikeAReturnIsHintedAndPosted(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	base := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	banned := snowflakeAt(base)
	h.ops.addMember(banned)
	h.ops.users[banned] = &discordgo.User{ID: banned, Username: "dana_k", GlobalName: "Dana", Avatar: "abc123"}
	s, _ := stubSession()
	h.p.handleBan(context.Background(), s, withResolved(banCmd(banned, strOpt("duration", "7d")), h.ops.users[banned]))

	joiner := &discordgo.Member{
		User:     &discordgo.User{ID: snowflakeAt(base.Add(48 * time.Hour)), Username: "danak2", Avatar: "abc123"},
		JoinedAt: testNow.Add(3 * time.Minute),
	}
	h.p.HandleMemberJoin(context.Background(), testGuild, joiner)

	hints, _ := h.store.Hints(context.Background(), testGuild, banned)
	if len(hints) != 1 || hints[0].Score != 7 || hints[0].UserID != joiner.User.ID {
		t.Fatalf("hints = %+v", hints)
	}
	posts := h.ops.sentTo("mods")
	if len(posts) != 1 || !strings.Contains(posts[0].Embeds[0].Description, "same avatar") || len(posts[0].Components) != 1 {
		t.Fatalf("mod channel = %+v", posts)
	}
	// The sheet shows it from either side.
	s2, rt := stubSession()
	h.p.handleView(context.Background(), s2, interaction("view", userOpt("user", joiner.User.ID)))
	if !strings.Contains(rt.said(), "Possible alts") {
		t.Errorf("got %s", rt.said())
	}
	// Nothing was linked.
	if g, _ := h.store.Group(context.Background(), testGuild, joiner.User.ID); len(g) != 1 {
		t.Error("a hint linked accounts by itself")
	}
}

func TestWeakOrDisabledHintsStayQuiet(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	h.ops.addMember("u1")
	h.ops.users["u1"] = &discordgo.User{ID: "u1", Username: "dana_k"}
	warnPts(h, "u1", 5)
	// Only a name prefix: score 1, below the hint threshold.
	h.p.HandleMemberJoin(context.Background(), testGuild, &discordgo.Member{User: &discordgo.User{ID: "u7", Username: "danakx"}, JoinedAt: testNow})
	if hints, _ := h.store.Hints(context.Background(), testGuild, "u1"); len(hints) != 0 {
		t.Errorf("a weak hint was stored: %+v", hints)
	}
	// A bot, a disabled guild, and the switch off all short-circuit.
	h.p.HandleMemberJoin(context.Background(), testGuild, &discordgo.Member{User: &discordgo.User{ID: "b1", Username: "dana_k", Bot: true}})
	cfg, _ := h.store.Config(context.Background(), testGuild)
	cfg.AltHints = false
	_ = h.store.SetConfig(context.Background(), cfg)
	h.p.HandleMemberJoin(context.Background(), testGuild, &discordgo.Member{User: &discordgo.User{ID: "u8", Username: "dana_k"}})
	if hints, _ := h.store.Hints(context.Background(), testGuild, "u1"); len(hints) != 0 {
		t.Errorf("hints with the switch off: %+v", hints)
	}
	if len(h.ops.sentTo("mods")) != 0 {
		t.Error("something was posted")
	}
}

func TestLinkingSharesTheScoreAndUnlinkingTakesItBack(t *testing.T) {
	h := newHarness()
	warnPts(h, "u1", 30)
	warnPts(h, "u2", 30)
	s, rt := stubSession()
	h.p.handleLink(context.Background(), s, interaction("link", userOpt("user", "u2"), userOpt("other", "u1"), strOpt("reason", "admitted it")))
	if !strings.Contains(rt.said(), "share one sheet") || !h.audit.has("rapsheet.linked") {
		t.Errorf("said %s audit %v", rt.said(), h.audit.all())
	}
	cfg := defaultConfig(testGuild)
	sh, _ := h.p.loadSheet(context.Background(), cfg, testGuild, "u2")
	if sh.Score != 60 || len(sh.Group) != 2 {
		t.Errorf("linked sheet: score %v group %v", sh.Score, sh.Group)
	}
	// A third account joins the same group.
	warnPts(h, "u3", 10)
	s2, _ := stubSession()
	h.p.handleLink(context.Background(), s2, interaction("link", userOpt("user", "u3"), userOpt("other", "u2")))
	if g, _ := h.store.Group(context.Background(), testGuild, "u1"); len(g) != 3 {
		t.Errorf("group after a third link = %v", g)
	}
	s3, rt3 := stubSession()
	h.p.handleView(context.Background(), s3, interaction("view", userOpt("user", "u1")))
	if !strings.Contains(rt3.said(), "Linked accounts") {
		t.Errorf("got %s", rt3.said())
	}

	s4, _ := stubSession()
	h.p.handleUnlink(context.Background(), s4, interaction("unlink", userOpt("user", "u2")))
	sh, _ = h.p.loadSheet(context.Background(), cfg, testGuild, "u2")
	if sh.Score != 30 || len(sh.Group) != 1 {
		t.Errorf("after unlink: score %v group %v", sh.Score, sh.Group)
	}
	s5, rt5 := stubSession()
	h.p.handleUnlink(context.Background(), s5, interaction("unlink", userOpt("user", "u9")))
	if !strings.Contains(rt5.said(), "not linked") {
		t.Errorf("got %s", rt5.said())
	}
	s6, rt6 := stubSession()
	h.p.handleLink(context.Background(), s6, interaction("link", userOpt("user", "u1"), userOpt("other", "u1")))
	if !strings.Contains(rt6.said(), "same account") {
		t.Errorf("got %s", rt6.said())
	}
}

func TestTheAltNoticeButtonsLinkOrDismiss(t *testing.T) {
	h := newHarness()
	_ = h.store.UpsertHint(context.Background(), AltHint{GuildID: testGuild, UserID: "new", CandidateID: "old", Signals: []string{"same avatar"}, Score: 3})
	warnPts(h, "old", 10)

	id := altDismissPrefix + "new:old"
	s, rt := stubSession()
	h.p.handleAltButton(context.Background(), s, componentClick(modID, id), id)
	if hints, _ := h.store.Hints(context.Background(), testGuild, "old"); len(hints) != 0 {
		t.Error("dismiss left the hint")
	}
	if !strings.Contains(rt.said(), "Dismissed by") {
		t.Errorf("got %s", rt.said())
	}

	_ = h.store.UpsertHint(context.Background(), AltHint{GuildID: testGuild, UserID: "new", CandidateID: "old", Signals: []string{"same avatar"}, Score: 3})
	id = altLinkPrefix + "new:old"
	s2, rt2 := stubSession()
	h.p.handleAltButton(context.Background(), s2, componentClick(modID, id), id)
	if g, _ := h.store.Group(context.Background(), testGuild, "new"); len(g) != 2 {
		t.Errorf("link button did not link: %v", g)
	}
	if !strings.Contains(rt2.said(), "Linked by") || strings.Contains(rt2.said(), "custom_id") {
		t.Errorf("got %s", rt2.said())
	}
	if hints, _ := h.store.Hints(context.Background(), testGuild, "old"); len(hints) != 0 {
		t.Error("linking left the hint behind")
	}
}

func TestConfigureAltHintsRoundTrips(t *testing.T) {
	h := newHarness()
	s, _ := stubSession()
	h.p.handleConfigureAltHints(context.Background(), s, interaction("configure/alt-hints", boolOpt("enabled", false)))
	if cfg, _ := h.store.Config(context.Background(), testGuild); cfg.AltHints {
		t.Error("still on")
	}
	s2, _ := stubSession()
	h.p.handleConfigureAltHints(context.Background(), s2, interaction("configure/alt-hints", boolOpt("enabled", true)))
	if cfg, _ := h.store.Config(context.Background(), testGuild); !cfg.AltHints {
		t.Error("still off")
	}
}
