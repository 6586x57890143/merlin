package rapsheet

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
)

func withForum(h *harness) {
	h.ops.addForum("7300022")
	cfg := defaultConfig(testGuild)
	cfg.ForumChannelID = "7300022"
	_ = h.store.SetConfig(context.Background(), cfg)
}

func warn(h *harness, userID, reason string) {
	stubSession()
	h.ops.addMember(userID)
	h.p.handleWarn(context.Background(), withResolved(
		interaction("warn", userOpt("user", userID), strOpt("category", "spam"), strOpt("reason", reason)),
		&discord.User{ID: core.ID(userID), Username: "dana", GlobalName: strp("Dana")}))
}

func TestTheFirstEntryOpensACaseFileAndLaterOnesAppend(t *testing.T) {
	h := newHarness()
	withForum(h)

	warn(h, "1001", "first")
	if h.ops.threadsOpened != 1 {
		t.Fatalf("threads opened = %d, want 1", h.ops.threadsOpened)
	}
	th := h.ops.channels["7500001"]
	if th.Name != "Dana (1001)" || th.ParentID != "7300022" {
		t.Errorf("thread = %+v", th)
	}
	cf, _, _ := h.store.CaseFile(context.Background(), testGuild, "1001")
	if cf.ThreadID != "7500001" {
		t.Errorf("case file thread = %q", cf.ThreadID)
	}
	if e, _ := h.store.Entry(context.Background(), testGuild, 1); e.ThreadMessageID != "7500001" {
		t.Errorf("starter message id should be the thread id, got %q", e.ThreadMessageID)
	}

	warn(h, "1001", "second")
	if h.ops.threadsOpened != 1 {
		t.Errorf("a second entry opened another thread")
	}
	if sent := h.ops.sentTo("7500001"); len(sent) != 2 {
		t.Errorf("messages in the case file = %d, want 2", len(sent))
	} else if !strings.Contains(sent[1].Embeds[0].Title, "#2") || sent[1].Embeds[0].Description != "second" {
		t.Errorf("mirrored embed = %+v", sent[1].Embeds[0])
	}
	if n, _ := h.store.CountUnmirrored(context.Background(), testGuild); n != 0 {
		t.Errorf("unmirrored = %d", n)
	}

	// A different member gets their own post.
	warn(h, "1002", "other")
	if h.ops.threadsOpened != 2 {
		t.Errorf("threads opened = %d, want 2", h.ops.threadsOpened)
	}
}

func TestNoForumMeansNoMirrorAndNoFailure(t *testing.T) {
	h := newHarness()
	warn(h, "1001", "r")
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
	rt := stubSession()
	h.ops.addMember("1001")
	h.p.handleWarn(context.Background(), interaction("warn", userOpt("user", "1001"), strOpt("category", "spam"), strOpt("reason", "r")))
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
	delete(h.ops.channels, "7300022")
	warn(h, "1001", "r")
	if cfg, _ := h.store.Config(context.Background(), testGuild); cfg.ForumChannelID != "" {
		t.Errorf("forum still configured after Discord said it is gone: %q", cfg.ForumChannelID)
	}
}

func TestADeletedThreadIsReopenedOnce(t *testing.T) {
	h := newHarness()
	withForum(h)
	warn(h, "1001", "first")
	delete(h.ops.channels, "7500001")
	warn(h, "1001", "second")
	if h.ops.threadsOpened != 2 {
		t.Fatalf("threads opened = %d, want 2", h.ops.threadsOpened)
	}
	cf, _, _ := h.store.CaseFile(context.Background(), testGuild, "1001")
	if cf.ThreadID != "7500002" {
		t.Errorf("case file should point at the new thread, got %q", cf.ThreadID)
	}
}

func TestVoidAndEditRewriteTheMirroredMessage(t *testing.T) {
	h := newHarness()
	withForum(h)
	warn(h, "1001", "first")
	h.ops.addMember(modID)

	stubSession()
	h.p.handleEdit(context.Background(), interaction("edit", intOpt("case", 1), strOpt("reason", "clearer")))
	stubSession()
	h.p.handleVoid(context.Background(), interaction("void", intOpt("case", 1), strOpt("reason", "wrong person")))

	if len(h.ops.edits) != 2 {
		t.Fatalf("edits = %d, want 2 (edit, void)", len(h.ops.edits))
	}
	if sent := h.ops.sentTo("7500001"); len(sent) != 1 {
		t.Errorf("an amend posted again instead of editing: %d messages", len(sent))
	}
	edit, void := h.ops.edits[0], h.ops.edits[1]
	if edit.channelID != "7500001" || edit.messageID != "7500001" || (*edit.u.Embeds)[0].Description != "clearer" {
		t.Errorf("edit = %+v", edit)
	}
	ve := (*void.u.Embeds)[0]
	if !strings.Contains(ve.Title, "voided") || !strings.Contains(fieldValue(ve, "Voided"), "wrong person") {
		t.Errorf("void embed = %+v", ve)
	}
	if void.u.Attachments == nil {
		t.Error("attachments must be replaced on edit, or the old mood icon stays alongside the new one")
	}
}

func TestAmendingAnUnmirroredEntryMirrorsIt(t *testing.T) {
	h := newHarness()
	seedEntries(h, "1001", 1)
	withForum(h)
	stubSession()
	h.p.handleEdit(context.Background(), interaction("edit", intOpt("case", 1), strOpt("reason", "now with a forum")))
	if h.ops.threadsOpened != 1 || len(h.ops.edits) != 0 {
		t.Errorf("threads %d edits %d; want the entry posted, not edited", h.ops.threadsOpened, len(h.ops.edits))
	}
}

func TestEntryEmbedShapes(t *testing.T) {
	ends := testNow.Add(time.Hour)
	e := Entry{ID: 7, Kind: KindBan, Category: CategoryThreats, Points: 100, ActorID: modID, Reason: "r",
		Duration: time.Hour, EndsAt: &ends, Source: SourceCommand, UserID: "1001"}
	em := entryEmbed(e)
	if !strings.HasPrefix(em.Title, "#7 · banned 1h") || fieldValue(em, "Offence") != "threats · 100 pts" || fieldValue(em, "Until") == "" || fieldValue(em, "Source") != "" {
		t.Errorf("embed = %+v", em)
	}
	// The forum previews the post's image: it must be the mood icon, the
	// only file uploaded, never the divider.
	if files := core.EmbedUploads(em); em.Image != nil || len(files) != 1 || !strings.HasPrefix(files[0].Name, "merlin_") {
		t.Errorf("case-file post image = %+v, files = %d; want only the mood icon", em.Image, len(files))
	}
	e.Source = SourceDiscord
	if fieldValue(entryEmbed(e), "Source") != "discord" {
		t.Error("a non-command source should be named")
	}
	note := entryEmbed(Entry{ID: 8, Kind: KindNote, ActorID: modID})
	if note.Description != "(no reason given)" || fieldValue(note, "Offence") != "" {
		t.Errorf("note embed = %+v", note)
	}
	if threadName(CaseFile{Username: "dana"}, "1001") != "dana (1001)" || threadName(CaseFile{}, "1001") != "member (1001)" {
		t.Error("threadName")
	}
	if got := threadName(CaseFile{Username: strings.Repeat("x", 200)}, "1001"); len(got) != maxThreadName {
		t.Errorf("thread name not capped: %d", len(got))
	}
}

// --- configure forum ------------------------------------------------------------

func TestConfigureForumCreatesAHiddenForumForTheModRoles(t *testing.T) {
	h := newHarness()
	h.p.botID = core.ID("7300003")
	rt := stubSession()
	panelClick(h.p, panelSet+"forumnew")
	if len(h.ops.created) != 1 {
		t.Fatalf("created %d channels", len(h.ops.created))
	}
	data := h.ops.created[0]
	if data.Type() != discord.ChannelTypeGuildForum || data.Name != caseFileForumName {
		t.Errorf("created %+v", data)
	}
	var everyoneDenied, botAllowed, modAllowed bool
	for _, ow := range core.Overwrites(data.PermissionOverwrites) {
		switch {
		case ow.ID.String() == testGuild && ow.Deny&discord.PermissionViewChannel != 0:
			everyoneDenied = true
		case ow.ID.String() == "7300003" && ow.Allow&discord.PermissionCreatePublicThreads != 0:
			botAllowed = true
		case ow.ID.String() == "7300018" && ow.Allow&discord.PermissionViewChannel != 0 && ow.Allow&discord.PermissionCreatePublicThreads == 0:
			modAllowed = true
		}
	}
	if !everyoneDenied || !botAllowed || !modAllowed {
		t.Errorf("overwrites = %+v", data.PermissionOverwrites)
	}
	cfg, _ := h.store.Config(context.Background(), testGuild)
	if cfg.ForumChannelID != "7400001" {
		t.Errorf("config forum = %q", cfg.ForumChannelID)
	}
	if !strings.Contains(rt.said(), "hidden from @everyone") || !h.audit.has("rapsheet.configured") {
		t.Errorf("said %s audit %v", rt.said(), h.audit.all())
	}
}

func TestConfigureForumAcceptsAnExistingForumAndWarnsAboutItsPermissions(t *testing.T) {
	h := newHarness()
	h.ops.addForum("7300023")
	h.ops.channels["7300021"] = &fch{ID: "7300021", Type: discord.ChannelTypeGuildText}
	seedEntries(h, "1001", 3)

	rt := stubSession()
	panelClick(h.p, panelSet+"forum", "7300023")
	if cfg, _ := h.store.Config(context.Background(), testGuild); cfg.ForumChannelID != "7300023" {
		t.Errorf("config forum = %q", cfg.ForumChannelID)
	}
	if len(h.ops.created) != 0 || !strings.Contains(rt.said(), "left as they are") || !strings.Contains(rt.said(), "3 existing entries") {
		t.Errorf("created %d, said %s", len(h.ops.created), rt.said())
	}

	rt2 := stubSession()
	panelClick(h.p, panelSet+"forum", "7300021")
	if !strings.Contains(rt2.said(), "not a forum") {
		t.Errorf("got %s", rt2.said())
	}
}

func TestConfigureForumRefusesNearTheChannelCap(t *testing.T) {
	h := newHarness()
	h.p.botID = core.ID("7300003")
	for i := 0; i < 500-channelCapHeadroom; i++ {
		h.ops.channels[strconv.Itoa(60000+i)] = &fch{}
	}
	rt := stubSession()
	panelClick(h.p, panelSet+"forumnew")
	if len(h.ops.created) != 0 || !strings.Contains(rt.said(), "500 cap") {
		t.Errorf("created %d, said %s", len(h.ops.created), rt.said())
	}
}

func TestConfigureShowListsTheConfiguration(t *testing.T) {
	h := newHarness()
	cfg := defaultConfig(testGuild)
	cfg.ModChannelID = "7300020"
	cfg.CategoryPoints[CategorySpam] = 3
	_ = h.store.SetConfig(context.Background(), cfg)
	rt := stubSession()
	h.p.handleSettings(context.Background(), interaction("settings"))
	for _, want := range []string{"#7300020", "not set", "spam=3", "defaults"} {
		if !strings.Contains(rt.said(), want) {
			t.Errorf("want %q in %s", want, rt.said())
		}
	}
}

// Two entries for one member mirror on their own goroutines. Both used to
// read "no thread yet" and each open a post, which is the pair of identical
// case files a guild found in its forum after two aimod removals.
func TestConcurrentEntriesShareOneCaseFile(t *testing.T) {
	h := newHarness()
	withForum(h)
	ctx := context.Background()
	cfg, _ := h.store.Config(ctx, testGuild)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		id, err := h.store.Insert(ctx, Entry{GuildID: testGuild, UserID: "1001", Kind: KindWarn, Category: CategorySpam, ActorID: core.ActorSystem})
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if err := h.p.mirrorNow(ctx, cfg, Entry{ID: id, GuildID: testGuild, UserID: "1001", Kind: KindWarn, Category: CategorySpam}); err != nil {
				t.Errorf("mirrorNow: %v", err)
			}
		}(id)
	}
	wg.Wait()
	if h.ops.threadsOpened != 1 {
		t.Errorf("threads opened = %d, want 1", h.ops.threadsOpened)
	}
}
