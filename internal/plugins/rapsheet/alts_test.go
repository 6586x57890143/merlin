package rapsheet

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/disgoorg/disgo/discord"
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
		joiner  *discord.User
		action  *Entry
		signals int
		score   int
	}{
		{"nothing in common", &discord.User{ID: core.ID(snowflakeAt(base.Add(48 * time.Hour))), Username: "zed", Avatar: strp("zzz")}, nil, 0, 0},
		{"same avatar", &discord.User{ID: core.ID(snowflakeAt(base.Add(48 * time.Hour))), Username: "zed", Avatar: strp("abc123")}, nil, 1, 3},
		{"same name exactly (letters only)", &discord.User{ID: core.ID(snowflakeAt(base.Add(48 * time.Hour))), Username: "DANAK2"}, nil, 1, 2},
		{"name prefix", &discord.User{ID: core.ID(snowflakeAt(base.Add(48 * time.Hour))), Username: "danakreturns"}, nil, 1, 1},
		{"short prefix does not count", &discord.User{ID: core.ID(snowflakeAt(base.Add(48 * time.Hour))), Username: "dan"}, nil, 0, 0},
		{"created minutes apart", &discord.User{ID: core.ID(snowflakeAt(base.Add(4 * time.Minute))), Username: "zed"}, nil, 1, 2},
		{"joined right after their ban", &discord.User{ID: core.ID(snowflakeAt(base.Add(48 * time.Hour))), Username: "zed"}, at(join.Add(-5 * time.Minute)), 1, 2},
		{"joined long after their ban", &discord.User{ID: core.ID(snowflakeAt(base.Add(48 * time.Hour))), Username: "zed"}, at(join.Add(-5 * time.Hour)), 0, 0},
		{"everything", &discord.User{ID: core.ID(snowflakeAt(base.Add(time.Minute))), Username: "dana_k", Avatar: strp("abc123")}, at(join.Add(-time.Minute)), 4, 9},
		{"the same account", &discord.User{ID: core.ID(onFile.UserID), Username: "dana_k", Avatar: strp("abc123")}, nil, 0, 0},
		{"no default avatar match", &discord.User{ID: core.ID(snowflakeAt(base.Add(48 * time.Hour))), Username: "zed", Avatar: strp("")}, nil, 0, 0},
	}
	for _, tc := range cases {
		signals, score := altSignals(tc.joiner, join, onFile, tc.action)
		if len(signals) != tc.signals || score != tc.score {
			t.Errorf("%s: signals %v score %d, want %d signals scoring %d", tc.name, signals, score, tc.signals, tc.score)
		}
	}
	// The join-after signal names the sanction and the gap, never a template.
	if signals, _ := altSignals(&discord.User{ID: core.ID(snowflakeAt(base.Add(48 * time.Hour))), Username: "zed"}, join, onFile, at(join.Add(-5*time.Minute))); len(signals) != 1 || signals[0] != "joined 5 minutes after <@"+onFile.UserID+"> was banned 7d" {
		t.Errorf("signal text = %v", signals)
	}
	// An empty avatar on file never matches an empty one on the joiner.
	if signals, _ := altSignals(&discord.User{ID: core.ID("1"), Avatar: strp("")}, join, CaseFile{UserID: "2", AvatarHash: ""}, nil); strings.Contains(strings.Join(signals, ","), "avatar") {
		t.Error("two default avatars matched")
	}
}

func TestAJoinThatLooksLikeAReturnIsHintedAndPosted(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	base := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	banned := snowflakeAt(base)
	h.ops.addMember(banned)
	h.ops.users[banned] = &discord.User{ID: core.ID(banned), Username: "dana_k", GlobalName: strp("Dana"), Avatar: strp("abc123")}
	stubSession()
	h.p.handleBan(context.Background(), withResolved(banCmd(banned, strOpt("duration", "7d")), h.ops.users[banned]))

	joiner := discord.Member{
		User:     discord.User{ID: core.ID(snowflakeAt(base.Add(48 * time.Hour))), Username: "danak2", Avatar: strp("abc123")},
		JoinedAt: ptrTime(testNow.Add(3 * time.Minute)),
	}
	h.p.HandleMemberJoin(context.Background(), testGuild, joiner)

	hints, _ := h.store.Hints(context.Background(), testGuild, banned)
	if len(hints) != 1 || hints[0].Score != 7 || hints[0].UserID != joiner.User.ID.String() {
		t.Fatalf("hints = %+v", hints)
	}
	posts := h.ops.sentTo("7300020")
	if len(posts) != 1 || !strings.Contains(posts[0].Embeds[0].Description, "same avatar") || len(posts[0].Components) != 1 {
		t.Fatalf("mod channel = %+v", posts)
	}
	// The sheet shows it from either side.
	rt := stubSession()
	h.p.handleView(context.Background(), interaction("view", userOpt("user", joiner.User.ID.String())))
	if !strings.Contains(rt.said(), "Possible alts") {
		t.Errorf("got %s", rt.said())
	}
	// Nothing was linked.
	if g, _ := h.store.Group(context.Background(), testGuild, joiner.User.ID.String()); len(g) != 1 {
		t.Error("a hint linked accounts by itself")
	}
}

func TestWeakOrDisabledHintsStayQuiet(t *testing.T) {
	h := newHarness()
	withModChannel(h, ModeSuggest)
	h.ops.addMember("7300007")
	h.ops.users["7300007"] = &discord.User{ID: core.ID("7300007"), Username: "dana_k"}
	warnPts(h, "7300007", 5)
	// Only a name prefix: score 1, below the hint threshold.
	h.p.HandleMemberJoin(context.Background(), testGuild, discord.Member{User: discord.User{ID: core.ID(snowflakeAt(testNow.Add(time.Hour))), Username: "danakx"}, JoinedAt: ptrTime(testNow)})
	if hints, _ := h.store.Hints(context.Background(), testGuild, "7300007"); len(hints) != 0 {
		t.Errorf("a weak hint was stored: %+v", hints)
	}
	// A bot, a disabled guild, and the switch off all short-circuit.
	h.p.HandleMemberJoin(context.Background(), testGuild, discord.Member{User: discord.User{ID: core.ID(snowflakeAt(testNow.Add(2 * time.Hour))), Username: "dana_k", Bot: true}})
	cfg, _ := h.store.Config(context.Background(), testGuild)
	cfg.AltHints = false
	_ = h.store.SetConfig(context.Background(), cfg)
	h.p.HandleMemberJoin(context.Background(), testGuild, discord.Member{User: discord.User{ID: core.ID(snowflakeAt(testNow.Add(3 * time.Hour))), Username: "dana_k"}})
	if hints, _ := h.store.Hints(context.Background(), testGuild, "7300007"); len(hints) != 0 {
		t.Errorf("hints with the switch off: %+v", hints)
	}
	if len(h.ops.sentTo("7300020")) != 0 {
		t.Error("something was posted")
	}
}

func TestLinkingSharesTheScoreAndUnlinkingTakesItBack(t *testing.T) {
	h := newHarness()
	warnPts(h, "7300007", 30)
	warnPts(h, "7300008", 30)
	rt := stubSession()
	h.p.handleLink(context.Background(), interaction("link", userOpt("user", "7300008"), userOpt("other", "7300007"), strOpt("reason", "admitted it")))
	if !strings.Contains(rt.said(), "share one sheet") || !h.audit.has("rapsheet.linked") {
		t.Errorf("said %s audit %v", rt.said(), h.audit.all())
	}
	cfg := defaultConfig(testGuild)
	sh, _ := h.p.loadSheet(context.Background(), cfg, testGuild, "7300008")
	if sh.Score != 60 || len(sh.Group) != 2 {
		t.Errorf("linked sheet: score %v group %v", sh.Score, sh.Group)
	}
	// A third account joins the same group.
	warnPts(h, "7300009", 10)
	stubSession()
	h.p.handleLink(context.Background(), interaction("link", userOpt("user", "7300009"), userOpt("other", "7300008")))
	if g, _ := h.store.Group(context.Background(), testGuild, "7300007"); len(g) != 3 {
		t.Errorf("group after a third link = %v", g)
	}
	rt3 := stubSession()
	h.p.handleView(context.Background(), interaction("view", userOpt("user", "7300007")))
	if !strings.Contains(rt3.said(), "Linked accounts") {
		t.Errorf("got %s", rt3.said())
	}

	stubSession()
	h.p.handleUnlink(context.Background(), interaction("unlink", userOpt("user", "7300008")))
	sh, _ = h.p.loadSheet(context.Background(), cfg, testGuild, "7300008")
	if sh.Score != 30 || len(sh.Group) != 1 {
		t.Errorf("after unlink: score %v group %v", sh.Score, sh.Group)
	}
	rt5 := stubSession()
	h.p.handleUnlink(context.Background(), interaction("unlink", userOpt("user", "7300015")))
	if !strings.Contains(rt5.said(), "not linked") {
		t.Errorf("got %s", rt5.said())
	}
	rt6 := stubSession()
	h.p.handleLink(context.Background(), interaction("link", userOpt("user", "7300007"), userOpt("other", "7300007")))
	if !strings.Contains(rt6.said(), "same account") {
		t.Errorf("got %s", rt6.said())
	}
}

func TestTheAltNoticeButtonsLinkOrDismiss(t *testing.T) {
	h := newHarness()
	_ = h.store.UpsertHint(context.Background(), AltHint{GuildID: testGuild, UserID: "7300901", CandidateID: "7300902", Signals: []string{"same avatar"}, Score: 3})
	warnPts(h, "7300902", 10)

	id := altDismissPrefix + "7300901:7300902"
	rt := stubSession()
	h.p.handleAltButton(context.Background(), componentClick(modID, id), id)
	if hints, _ := h.store.Hints(context.Background(), testGuild, "7300902"); len(hints) != 0 {
		t.Error("dismiss left the hint")
	}
	if !strings.Contains(rt.said(), "Dismissed by") {
		t.Errorf("got %s", rt.said())
	}

	_ = h.store.UpsertHint(context.Background(), AltHint{GuildID: testGuild, UserID: "7300901", CandidateID: "7300902", Signals: []string{"same avatar"}, Score: 3})
	id = altLinkPrefix + "7300901:7300902"
	rt2 := stubSession()
	h.p.handleAltButton(context.Background(), componentClick(modID, id), id)
	if g, _ := h.store.Group(context.Background(), testGuild, "7300901"); len(g) != 2 {
		t.Errorf("link button did not link: %v", g)
	}
	if !strings.Contains(rt2.said(), "Linked by") || strings.Contains(rt2.said(), "custom_id") {
		t.Errorf("got %s", rt2.said())
	}
	if hints, _ := h.store.Hints(context.Background(), testGuild, "7300902"); len(hints) != 0 {
		t.Error("linking left the hint behind")
	}
}

func TestConfigureAltHintsRoundTrips(t *testing.T) {
	h := newHarness()
	stubSession()
	panelClick(h.p, panelSet+"alts:off")
	if cfg, _ := h.store.Config(context.Background(), testGuild); cfg.AltHints {
		t.Error("still on")
	}
	stubSession()
	panelClick(h.p, panelSet+"alts:on")
	if cfg, _ := h.store.Config(context.Background(), testGuild); !cfg.AltHints {
		t.Error("still off")
	}
}
