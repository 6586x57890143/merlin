package rapsheet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func withForum(h *harness) {
	h.ops.addForum("forum-1")
	cfg := defaultConfig(testGuild)
	cfg.ForumChannelID = "forum-1"
	_ = h.store.SetConfig(context.Background(), cfg)
}

func warn(h *harness, userID, reason string) {
	s, _ := stubSession()
	h.ops.addMember(userID)
	h.p.handleWarn(context.Background(), s, withResolved(
		interaction("warn", userOpt("user", userID), strOpt("category", "spam"), strOpt("reason", reason)),
		&discordgo.User{ID: userID, Username: "dana", GlobalName: "Dana"}))
}

func TestTheFirstEntryOpensACaseFileAndLaterOnesAppend(t *testing.T) {
	h := newHarness()
	withForum(h)

	warn(h, "u1", "first")
	if h.ops.threadsOpened != 1 {
		t.Fatalf("threads opened = %d, want 1", h.ops.threadsOpened)
	}
	th := h.ops.channels["t-1"]
	if th.Name != "Dana (u1)" || th.ParentID != "forum-1" {
		t.Errorf("thread = %+v", th)
	}
	cf, _, _ := h.store.CaseFile(context.Background(), testGuild, "u1")
	if cf.ThreadID != "t-1" {
		t.Errorf("case file thread = %q", cf.ThreadID)
	}
	if e, _ := h.store.Entry(context.Background(), testGuild, 1); e.ThreadMessageID != "t-1" {
		t.Errorf("starter message id should be the thread id, got %q", e.ThreadMessageID)
	}

	warn(h, "u1", "second")
	if h.ops.threadsOpened != 1 {
		t.Errorf("a second entry opened another thread")
	}
	if sent := h.ops.sentTo("t-1"); len(sent) != 2 {
		t.Errorf("messages in the case file = %d, want 2", len(sent))
	} else if !strings.Contains(sent[1].Embeds[0].Title, "#2") || sent[1].Embeds[0].Description != "second" {
		t.Errorf("mirrored embed = %+v", sent[1].Embeds[0])
	}
	if n, _ := h.store.CountUnmirrored(context.Background(), testGuild); n != 0 {
		t.Errorf("unmirrored = %d", n)
	}

	// A different member gets their own post.
	warn(h, "u2", "other")
	if h.ops.threadsOpened != 2 {
		t.Errorf("threads opened = %d, want 2", h.ops.threadsOpened)
	}
}

func TestNoForumMeansNoMirrorAndNoFailure(t *testing.T) {
	h := newHarness()
	warn(h, "u1", "r")
	if h.ops.threadsOpened != 0 || len(h.store.all()) != 1 {
		t.Errorf("threads %d entries %d", h.ops.threadsOpened, len(h.store.all()))
	}
	if n, _ := h.store.CountUnmirrored(context.Background(), testGuild); n != 1 {
		t.Errorf("unmirrored = %d, want 1", n)
	}
}

func TestAMirrorFailureNeverFailsTheEntry(t *testing.T) {
	h := newHarness()
	withForum(h)
	h.ops.threadErr = errors.New("rate limited")
	s, rt := stubSession()
	h.ops.addMember("u1")
	h.p.handleWarn(context.Background(), s, interaction("warn", userOpt("user", "u1"), strOpt("category", "spam"), strOpt("reason", "r")))
	if len(h.store.all()) != 1 || !strings.Contains(rt.said(), "Case #1") {
		t.Errorf("the warning should have landed regardless: %s", rt.said())
	}
	if e, _ := h.store.Entry(context.Background(), testGuild, 1); e.ThreadMessageID != "" {
		t.Errorf("a failed mirror recorded a message id %q", e.ThreadMessageID)
	}
	if cfg, _ := h.store.Config(context.Background(), testGuild); cfg.ForumChannelID == "" {
		t.Error("a transient failure unset the forum")
	}
}

func TestADeletedForumUnsetsItself(t *testing.T) {
	h := newHarness()
	withForum(h)
	delete(h.ops.channels, "forum-1")
	warn(h, "u1", "r")
	if cfg, _ := h.store.Config(context.Background(), testGuild); cfg.ForumChannelID != "" {
		t.Errorf("forum still configured after Discord said it is gone: %q", cfg.ForumChannelID)
	}
}

func TestADeletedThreadIsReopenedOnce(t *testing.T) {
	h := newHarness()
	withForum(h)
	warn(h, "u1", "first")
	delete(h.ops.channels, "t-1")
	warn(h, "u1", "second")
	if h.ops.threadsOpened != 2 {
		t.Fatalf("threads opened = %d, want 2", h.ops.threadsOpened)
	}
	cf, _, _ := h.store.CaseFile(context.Background(), testGuild, "u1")
	if cf.ThreadID != "t-2" {
		t.Errorf("case file should point at the new thread, got %q", cf.ThreadID)
	}
}

func TestVoidAndEditRewriteTheMirroredMessage(t *testing.T) {
	h := newHarness()
	withForum(h)
	warn(h, "u1", "first")
	h.ops.addMember(modID)

	s, _ := stubSession()
	h.p.handleEdit(context.Background(), s, interaction("edit", intOpt("case", 1), strOpt("reason", "clearer")))
	s2, _ := stubSession()
	h.p.handleVoid(context.Background(), s2, interaction("void", intOpt("case", 1), strOpt("reason", "wrong person")))

	if len(h.ops.edits) != 2 {
		t.Fatalf("edits = %d, want 2 (edit, void)", len(h.ops.edits))
	}
	if sent := h.ops.sentTo("t-1"); len(sent) != 1 {
		t.Errorf("an amend posted again instead of editing: %d messages", len(sent))
	}
	edit, void := h.ops.edits[0], h.ops.edits[1]
	if edit.Channel != "t-1" || edit.ID != "t-1" || (*edit.Embeds)[0].Description != "clearer" {
		t.Errorf("edit = %+v", edit)
	}
	ve := (*void.Embeds)[0]
	if !strings.Contains(ve.Title, "voided") || !strings.Contains(fieldValue(ve, "Voided"), "wrong person") {
		t.Errorf("void embed = %+v", ve)
	}
	if void.Attachments == nil {
		t.Error("attachments must be replaced on edit, or the old mood icon stays alongside the new one")
	}
}

func TestAmendingAnUnmirroredEntryMirrorsIt(t *testing.T) {
	h := newHarness()
	seedEntries(h, "u1", 1)
	withForum(h)
	s, _ := stubSession()
	h.p.handleEdit(context.Background(), s, interaction("edit", intOpt("case", 1), strOpt("reason", "now with a forum")))
	if h.ops.threadsOpened != 1 || len(h.ops.edits) != 0 {
		t.Errorf("threads %d edits %d; want the entry posted, not edited", h.ops.threadsOpened, len(h.ops.edits))
	}
}

func TestEntryEmbedShapes(t *testing.T) {
	ends := testNow.Add(time.Hour)
	e := Entry{ID: 7, Kind: KindBan, Category: CategoryThreats, Points: 100, ActorID: modID, Reason: "r",
		Duration: time.Hour, EndsAt: &ends, Source: SourceCommand, UserID: "u1"}
	em := entryEmbed(e)
	if !strings.HasPrefix(em.Title, "#7 · banned 1h") || fieldValue(em, "Points") != "100" || fieldValue(em, "Until") == "" || fieldValue(em, "Source") != "" {
		t.Errorf("embed = %+v", em)
	}
	e.Source = SourceDiscord
	if fieldValue(entryEmbed(e), "Source") != "discord" {
		t.Error("a non-command source should be named")
	}
	note := entryEmbed(Entry{ID: 8, Kind: KindNote, ActorID: modID})
	if note.Description != "(no reason given)" || fieldValue(note, "Category") != "" || fieldValue(note, "Points") != "" {
		t.Errorf("note embed = %+v", note)
	}
	if threadName(CaseFile{Username: "dana"}, "u1") != "dana (u1)" || threadName(CaseFile{}, "u1") != "member (u1)" {
		t.Error("threadName")
	}
	if got := threadName(CaseFile{Username: strings.Repeat("x", 200)}, "u1"); len(got) != maxThreadName {
		t.Errorf("thread name not capped: %d", len(got))
	}
}

// --- configure forum ------------------------------------------------------------

func TestConfigureForumCreatesAHiddenForumForTheModRoles(t *testing.T) {
	h := newHarness()
	h.p.botID = "merlin-1"
	s, rt := stubSession()
	h.p.handleConfigureForum(context.Background(), s, interaction("configure/forum"))
	if len(h.ops.created) != 1 {
		t.Fatalf("created %d channels", len(h.ops.created))
	}
	data := h.ops.created[0]
	if data.Type != discordgo.ChannelTypeGuildForum || data.Name != caseFileForumName {
		t.Errorf("created %+v", data)
	}
	var everyoneDenied, botAllowed, modAllowed bool
	for _, ow := range data.PermissionOverwrites {
		switch {
		case ow.ID == testGuild && ow.Deny&discordgo.PermissionViewChannel != 0:
			everyoneDenied = true
		case ow.ID == "merlin-1" && ow.Allow&discordgo.PermissionCreatePublicThreads != 0:
			botAllowed = true
		case ow.ID == "mod-role" && ow.Allow&discordgo.PermissionViewChannel != 0 && ow.Allow&discordgo.PermissionCreatePublicThreads == 0:
			modAllowed = true
		}
	}
	if !everyoneDenied || !botAllowed || !modAllowed {
		t.Errorf("overwrites = %+v", data.PermissionOverwrites)
	}
	cfg, _ := h.store.Config(context.Background(), testGuild)
	if cfg.ForumChannelID != "ch-1" {
		t.Errorf("config forum = %q", cfg.ForumChannelID)
	}
	if !strings.Contains(rt.said(), "hidden from @everyone") || !h.audit.has("rapsheet.configured") {
		t.Errorf("said %s audit %v", rt.said(), h.audit.all())
	}
}

func TestConfigureForumAcceptsAnExistingForumAndWarnsAboutItsPermissions(t *testing.T) {
	h := newHarness()
	h.ops.addForum("their-forum")
	h.ops.channels["text"] = &discordgo.Channel{ID: "text", Type: discordgo.ChannelTypeGuildText}
	seedEntries(h, "u1", 3)

	s, rt := stubSession()
	h.p.handleConfigureForum(context.Background(), s, interaction("configure/forum", &discordgo.ApplicationCommandInteractionDataOption{
		Name: "channel", Type: discordgo.ApplicationCommandOptionChannel, Value: "their-forum"}))
	if cfg, _ := h.store.Config(context.Background(), testGuild); cfg.ForumChannelID != "their-forum" {
		t.Errorf("config forum = %q", cfg.ForumChannelID)
	}
	if len(h.ops.created) != 0 || !strings.Contains(rt.said(), "left as they are") || !strings.Contains(rt.said(), "3 existing entries") {
		t.Errorf("created %d, said %s", len(h.ops.created), rt.said())
	}

	s2, rt2 := stubSession()
	h.p.handleConfigureForum(context.Background(), s2, interaction("configure/forum", &discordgo.ApplicationCommandInteractionDataOption{
		Name: "channel", Type: discordgo.ApplicationCommandOptionChannel, Value: "text"}))
	if !strings.Contains(rt2.said(), "not a forum") {
		t.Errorf("got %s", rt2.said())
	}
}

func TestConfigureForumRefusesNearTheChannelCap(t *testing.T) {
	h := newHarness()
	h.p.botID = "merlin-1"
	for i := 0; i < 500-channelCapHeadroom; i++ {
		h.ops.channels[strings.Repeat("c", i+1)] = &discordgo.Channel{}
	}
	s, rt := stubSession()
	h.p.handleConfigureForum(context.Background(), s, interaction("configure/forum"))
	if len(h.ops.created) != 0 || !strings.Contains(rt.said(), "500 cap") {
		t.Errorf("created %d, said %s", len(h.ops.created), rt.said())
	}
}

func TestConfigureShowListsTheConfiguration(t *testing.T) {
	h := newHarness()
	cfg := defaultConfig(testGuild)
	cfg.ModChannelID = "mods"
	cfg.CategoryPoints[CategorySpam] = 3
	_ = h.store.SetConfig(context.Background(), cfg)
	s, rt := stubSession()
	h.p.handleConfigureShow(context.Background(), s, interaction("configure/show"))
	for _, want := range []string{"#mods", "not set", "spam=3", "defaults"} {
		if !strings.Contains(rt.said(), want) {
			t.Errorf("want %q in %s", want, rt.said())
		}
	}
}
