package rapsheet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// The preview dump: every user-facing surface this plugin renders, driven
// through the real handlers with the real voice catalogue, written as JSON
// for a Discord-style renderer to draw and a reviewer to look at.
//
// Not a test of anything on its own. It is the input to the visual review
// pass (scripts/rapsheet-preview/), and skips unless RAPSHEET_PREVIEW_DIR
// says where to write, so `go test ./...` is unaffected.

type previewScene struct {
	Title      string                       `json:"title"`
	Surface    string                       `json:"surface"` // interaction | dm | channel | thread
	Content    string                       `json:"content,omitempty"`
	Embeds     []*discordgo.MessageEmbed    `json:"embeds"`
	Components []discordgo.MessageComponent `json:"components,omitempty"`
	Ephemeral  bool                         `json:"ephemeral"`
}

// lastResponse pulls the embeds and components out of what a handler put
// on the wire: an interaction response ({"type":4,"data":{...}}), an
// update ({"type":7,...}) or a follow-up (a bare webhook body).
func lastResponse(rt *recordingTransport) (embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent, content string, ephemeral bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	// A follow-up inherits the ephemerality of the deferral it replaces,
	// which is on the earlier call, not on the follow-up's own body.
	deferredEphemeral := false
	for _, c := range rt.calls {
		var d struct {
			Type int `json:"type"`
			Data struct {
				Flags int `json:"flags"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(c.body), &d) == nil && d.Type == 5 && d.Data.Flags&64 != 0 {
			deferredEphemeral = true
		}
	}
	for i := len(rt.calls) - 1; i >= 0; i-- {
		body := rt.calls[i].body
		if body == "" {
			continue
		}
		var outer struct {
			Type int             `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		payload := []byte(body)
		if json.Unmarshal(payload, &outer) == nil && len(outer.Data) > 0 {
			if outer.Type == 5 || outer.Type == 6 {
				// A deferral: nothing to show. Keep looking earlier? No:
				// the follow-up is later in the list, so this is only hit
				// when nothing followed it.
				continue
			}
			payload = outer.Data
		}
		var data struct {
			Content    string                    `json:"content"`
			Embeds     []*discordgo.MessageEmbed `json:"embeds"`
			Components []json.RawMessage         `json:"components"`
			Flags      int                       `json:"flags"`
		}
		if err := json.Unmarshal(payload, &data); err != nil || (len(data.Embeds) == 0 && data.Content == "") {
			continue
		}
		var comps []discordgo.MessageComponent
		for _, raw := range data.Components {
			var row discordgo.ActionsRow
			if err := row.UnmarshalJSON(raw); err == nil {
				comps = append(comps, row)
			}
		}
		return data.Embeds, comps, data.Content, data.Flags&64 != 0 || deferredEphemeral
	}
	return nil, nil, "", false
}

func TestWritePreviews(t *testing.T) {
	dir := os.Getenv("RAPSHEET_PREVIEW_DIR")
	if dir == "" {
		t.Skip("RAPSHEET_PREVIEW_DIR not set")
	}
	speaker, err := voice.New(quietLog(), voice.WithRand(func(n int) int { return 0 }))
	if err != nil {
		t.Fatal(err)
	}

	var scenes []previewScene
	add := func(title, surface string, embeds []*discordgo.MessageEmbed, comps []discordgo.MessageComponent, content string, ephemeral bool) {
		scenes = append(scenes, previewScene{Title: title, Surface: surface, Content: content, Embeds: embeds, Components: comps, Ephemeral: ephemeral})
	}
	fromRT := func(title string, rt *recordingTransport) {
		e, c, content, eph := lastResponse(rt)
		add(title, "interaction", e, c, content, eph)
	}
	fromSend := func(title, surface string, sends []*discordgo.MessageSend) {
		if len(sends) == 0 {
			add(title, surface, nil, nil, "(nothing was sent)", false)
			return
		}
		m := sends[len(sends)-1]
		embeds := m.Embeds
		if m.Embed != nil {
			embeds = append([]*discordgo.MessageEmbed{m.Embed}, embeds...)
		}
		add(title, surface, embeds, m.Components, m.Content, false)
	}

	h := newHarness()
	h.p.speaker = speaker
	withForum(h)
	h.ops.channels["mods"] = &discordgo.Channel{ID: "mods", Type: discordgo.ChannelTypeGuildText}
	cfg, _ := h.store.Config(context.Background(), testGuild)
	cfg.ModChannelID = "mods"
	_ = h.store.SetConfig(context.Background(), cfg)
	h.ops.addMember(modID)
	dana := &discordgo.User{ID: "u1", Username: "dana_k", GlobalName: "Dana"}
	h.ops.addMember("u1")
	h.ops.users["u1"] = dana
	ctx := context.Background()

	// A record with some history, spread over time.
	old := h.p.now
	seed := func(ago time.Duration, in newEntry) Entry {
		h.p.now = func() time.Time { return testNow.Add(-ago) }
		e, _, err := h.p.record(ctx, cfg, in)
		if err != nil {
			t.Fatal(err)
		}
		h.p.now = old
		return e
	}
	seed(40*24*time.Hour, newEntry{GuildID: testGuild, UserID: "u1", Kind: KindWarn, Category: CategorySpam, ActorID: "mod-2", Reason: "posting the same link in six channels", Source: SourceCommand, Identity: dana})
	seed(12*24*time.Hour, newEntry{GuildID: testGuild, UserID: "u1", Kind: KindRemoval, Category: CategoryHateSpeech, ActorID: core.ActorSystem, Reason: "message removed automatically: a slur aimed at another member", Source: SourceAIMod, Ref: "inc-411"})
	ends := testNow.Add(-12*24*time.Hour + 8*time.Hour)
	seed(12*24*time.Hour, newEntry{GuildID: testGuild, UserID: "u1", Kind: KindJail, Category: CategoryServerRule, ActorID: core.ActorSystem, Reason: "automatic: hate speech (Discord hate speech policy)", Duration: 8 * time.Hour, EndsAt: &ends, Source: SourceRoles})
	seed(5*24*time.Hour, newEntry{GuildID: testGuild, UserID: "u1", Kind: KindNote, ActorID: modID, Reason: "apologised in modmail, seemed genuine", Source: SourceCommand})
	wrong := seed(3*24*time.Hour, newEntry{GuildID: testGuild, UserID: "u1", Kind: KindWarn, Category: CategoryThreats, ActorID: "mod-2", Reason: "threatened to find someone's address", Source: SourceCommand})
	_ = h.store.Void(ctx, testGuild, wrong.ID, modID, "misread; they were quoting a film", testNow.Add(-2*24*time.Hour))

	// 1. A warning by command, and everything it produces.
	s, rt := stubSession()
	h.p.handleWarn(ctx, s, withResolved(interaction("warn", userOpt("user", "u1"), strOpt("category", "server_rule"), strOpt("reason", "arguing with mods in #general after being asked to stop")), dana))
	fromRT("/rapsheet warn: the moderator's confirmation", rt)
	fromSend("DM to the member: warned", "dm", h.ops.sentTo("dm-u1"))
	fromSend("Case file: the mirrored entry", "thread", h.ops.sentTo("t-1"))

	_ = h.store.UpsertHint(ctx, AltHint{GuildID: testGuild, UserID: "u6", CandidateID: "u1", Signals: []string{"same avatar", "joined 12 minutes after being jailed 8h"}, Score: 5})
	_ = h.store.Link(ctx, Link{GuildID: testGuild, UserID: "u1", GroupID: "u1", LinkedBy: modID})
	_ = h.store.Link(ctx, Link{GuildID: testGuild, UserID: "u5", GroupID: "u1", LinkedBy: modID, Reason: "admitted it"})

	// 2. The sheet, both audiences.
	s, rt = stubSession()
	h.p.handleView(ctx, s, withResolved(interaction("view", userOpt("user", "u1")), dana))
	fromRT("/rapsheet view: the moderator's view", rt)
	s, rt = stubSession()
	me := interactionBy("u1", "me")
	me.Member.User = dana
	h.p.handleMe(ctx, s, me)
	fromRT("/rapsheet me: the member's own view", rt)

	// 3. A clean member.
	s, rt = stubSession()
	h.p.handleView(ctx, s, withResolved(interaction("view", userOpt("user", "u9")), &discordgo.User{ID: "u9", Username: "quiet_one"}))
	fromRT("/rapsheet view: a member with nothing on record", rt)

	// 4. The ladder suggests (score crossed jail 2h), then a mod applies it.
	h.p.jailer = &fakeJailer{}
	posts := h.ops.sentTo("mods")
	if len(posts) == 0 {
		t.Fatal("expected the warning above to have crossed a band and posted a suggestion")
	}
	fromSend("Mod channel: a ladder suggestion, open", "channel", posts)
	sug := suggestions(h)[len(suggestions(h))-1]
	applyID := suggestApplyPrefix + strconv.FormatInt(sug.ID, 10)
	s, rt = stubSession()
	h.p.handleSuggestion(ctx, s, componentClick(modID, applyID), applyID)
	fromRT("Mod channel: that suggestion clicked after #6 was voided (withdrawn)", rt)
	// A clean apply, on a member whose sheet was not amended underneath it.
	h.ops.addMember("u7")
	h.ops.users["u7"] = &discordgo.User{ID: "u7", Username: "rowdy", GlobalName: "Rowdy"}
	s, _ = stubSession()
	h.p.handleWarn(ctx, s, withResolved(interaction("warn", userOpt("user", "u7"), strOpt("category", "hate_speech"), strOpt("reason", "slur in voice chat, several people heard it"), intOpt("points", 60)), h.ops.users["u7"]))
	fromSend("Mod channel: a suggestion for a second member, open", "channel", h.ops.sentTo("mods"))
	sug = suggestions(h)[len(suggestions(h))-1]
	applyID = suggestApplyPrefix + strconv.FormatInt(sug.ID, 10)
	s, rt = stubSession()
	h.p.handleSuggestion(ctx, s, componentClick(modID, applyID), applyID)
	fromRT("Mod channel: the same suggestion after Apply", rt)

	// 5. Consequences by command.
	h.ops.addMember("u2")
	u2 := &discordgo.User{ID: "u2", Username: "loud_guy", GlobalName: "Loud"}
	h.ops.users["u2"] = u2
	s, rt = stubSession()
	h.p.handleTimeout(ctx, s, withResolved(interaction("timeout", userOpt("user", "u2"), strOpt("duration", "6h"), strOpt("category", "spam"), strOpt("reason", "wall of emoji in every channel")), u2))
	fromRT("/rapsheet timeout: confirmation", rt)
	fromSend("DM to the member: timed out", "dm", h.ops.sentTo("dm-u2"))
	s, rt = stubSession()
	h.p.handleBan(ctx, s, withResolved(interaction("ban", userOpt("user", "u2"), strOpt("category", "threats"), strOpt("reason", "doxxing threat against a moderator"), strOpt("duration", "7d")), u2))
	fromRT("/rapsheet ban: confirmation", rt)
	fromSend("DM to the member: banned for 7d", "dm", h.ops.sentTo("dm-u2"))
	h.ops.addMember("u3")
	s, _ = stubSession()
	h.p.handleBan(ctx, s, interaction("ban", userOpt("user", "u3"), strOpt("category", "child_safety"), strOpt("reason", "repeated after a final warning; details in the case file"), boolOpt("permanent", true)))
	fromSend("DM to the member: banned permanently", "dm", h.ops.sentTo("dm-u3"))
	h.ops.addMember("u4")
	s, _ = stubSession()
	h.p.handleKick(ctx, s, interaction("kick", userOpt("user", "u4"), strOpt("category", "other"), strOpt("reason", "advertising, after two warnings")))
	fromSend("DM to the member: kicked", "dm", h.ops.sentTo("dm-u4"))
	h.p.dm(ctx, testGuild, "u1", voice.KeyStrikeNotice, "A note about your record", core.ColorWarning, map[string]string{"guild": "Test Guild"})
	fromSend("DM to the member: the ladder's notice band", "dm", h.ops.sentTo("dm-u1"))

	// 6. Void, edit, and their refusals.
	s, rt = stubSession()
	h.p.handleVoid(ctx, s, interaction("void", intOpt("case", 1), strOpt("reason", "duplicate of an older case")))
	fromRT("/rapsheet void: confirmation", rt)
	fromSend("Case file: an entry after it was voided", "thread", h.ops.edits2(h))
	h.ops.addMember("admin-1")
	h.ranker.admins["admin-1"] = true
	adminEntry, _, _ := h.p.record(ctx, cfg, newEntry{GuildID: testGuild, UserID: "u1", Kind: KindWarn, Category: CategoryGore, ActorID: "admin-1", Reason: "gore in #art", Source: SourceCommand})
	s, rt = stubSession()
	h.p.handleVoid(ctx, s, interaction("void", intOpt("case", int(adminEntry.ID)), strOpt("reason", "nope")))
	fromRT("/rapsheet void: refused by hierarchy", rt)
	s, rt = stubSession()
	h.p.handleBan(ctx, s, interaction("ban", userOpt("user", "u5"), strOpt("category", "spam"), strOpt("reason", "r")))
	fromRT("/rapsheet ban: neither duration nor permanent", rt)
	s, rt = stubSession()
	h.p.handleWarn(ctx, s, interaction("warn", userOpt("user", "admin-1"), strOpt("category", "spam"), strOpt("reason", "r")))
	fromRT("/rapsheet warn: refused, target outranks the mod", rt)

	// 7. Configuration and status.
	s, rt = stubSession()
	h.p.handleStatus(ctx, s, interaction("status"))
	fromRT("/rapsheet status", rt)
	s, rt = stubSession()
	h.p.handleListBands(ctx, s, interaction("list/bands"))
	fromRT("/rapsheet list bands", rt)
	s, rt = stubSession()
	h.p.handleListCategories(ctx, s, interaction("list/categories"))
	fromRT("/rapsheet list categories", rt)
	s, rt = stubSession()
	h.p.handleConfigureShow(ctx, s, interaction("configure/show"))
	fromRT("/rapsheet configure show", rt)
	s, rt = stubSession()
	h.p.handleConfigureMode(ctx, s, interaction("configure/mode", strOpt("mode", "auto")))
	fromRT("/rapsheet configure mode auto", rt)
	s, rt = stubSession()
	h.p.handleConfigureBands(ctx, s, interaction("configure/bands", strOpt("ladder", "25 notice, 50 jail 2h, 100 jail 1d, 200 ban 7d, 400 ban 30d")))
	fromRT("/rapsheet configure bands", rt)
	s, rt = stubSession()
	h.p.handleConfigureHalfLife(ctx, s, interaction("configure/half-life", strOpt("duration", "14d")))
	fromRT("/rapsheet configure half-life 14d", rt)
	fresh := newHarness()
	fresh.p.botID = "merlin-1"
	s, rt = stubSession()
	fresh.p.handleConfigureForum(ctx, s, interaction("configure/forum"))
	fromRT("/rapsheet configure forum (created)", rt)

	// 8. Alt hints and links.
	h.ops.addMember("u6")
	h.ops.users["u6"] = &discordgo.User{ID: "u6", Username: "danak2", Avatar: "abc123"}
	hint := AltHint{GuildID: testGuild, UserID: "u6", CandidateID: "u1", Signals: []string{"same avatar", "joined 12 minutes after being jailed 8h"}, Score: 5}
	embed, comps := altNoticeEmbed(h.ops.users["u6"], hint, h.p.altContext(ctx, cfg, hint), "", false)
	add("Mod channel: a possible alt on join", "channel", []*discordgo.MessageEmbed{embed}, comps, "", false)
	embed, comps = altNoticeEmbed(h.ops.users["u6"], hint, "", "Linked by "+core.MentionUser(modID)+". They now share one sheet.", true)
	add("Mod channel: the same notice after Link", "channel", []*discordgo.MessageEmbed{embed}, comps, "", false)
	s, rt = stubSession()
	h.p.handleLink(ctx, s, interaction("link", userOpt("user", "u6"), userOpt("other", "u1"), strOpt("reason", "same avatar, rejoined minutes after the ban")))
	fromRT("/rapsheet link: confirmation", rt)
	s, rt = stubSession()
	h.p.handleUnlink(ctx, s, interaction("unlink", userOpt("user", "u6")))
	fromRT("/rapsheet unlink: confirmation", rt)
	s, rt = stubSession()
	h.p.handleConfigureAltHints(ctx, s, interaction("configure/alt-hints", boolOpt("enabled", false)))
	fromRT("/rapsheet configure alt-hints off", rt)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(scenes, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scenes.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d scenes to %s", len(scenes), dir)
}

// edits2 turns the last mirrored edit into a MessageSend so the same
// renderer can draw it.
func (f *fakeOps) edits2(_ *harness) []*discordgo.MessageSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edits) == 0 {
		return nil
	}
	last := f.edits[len(f.edits)-1]
	var embeds []*discordgo.MessageEmbed
	if last.Embeds != nil {
		embeds = *last.Embeds
	}
	return []*discordgo.MessageSend{{Embeds: embeds}}
}
