package audit

import (
	"slices"
	"testing"
	"time"

	"github.com/6586x57890143/cygnet/ledger"

	"github.com/6586x57890143/merlin/internal/core"
)

var at = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// What real call sites write, each of which must come out as a valid
// ledger entry: the adapter is how 55 call sites read well without each
// being rewritten, so a shape that fails Validate here would be a broken
// card in somebody's log channel.
var callSites = []struct{ action, old, new string }{
	{"roles.jail", "", `user=<@1001> duration=2h release_at=2026-10-17T14:00:00Z unmanageable_roles=[<@&501>, <@&502>] reason="spam, links"`},
	{"roles.release", "", "user=<@1001> from=jail restored=[<@&501> <@&503>]"},
	{"roles.grant", "", `user=<@1001> role=<@&501> reason="event"`},
	{"roles.supporter_grant", "", "user=<@1001> role=<@&501> until=2026-11-25T14:00:00Z"},
	{"rotation.archive_perms_fixed", "<#300>", "category overwrites corrected, 2 channel(s) resynced"},
	{"channel.rotated", "<#300>", "<#301>"},
	{"archive.deleted", "<#300>", ""},
	{"config.admin_added", "", "<@99>"},
	{"config.mod_role_removed", "<@&501>", ""},
	{"aimod.funding_cleared", "", "tip jar removed"},
	{"contest.configured", "starts=monday", "starts=friday"},
	{"rapsheet.edit", "spam", "case #4: `raid` alt"},
	{"rotation.edit", "sticky:\nline one", "sticky:\nline two"},
	{"config.setup", "", ""},
	{"aimod.rewrite", "", "user=<@1001> message=[Deleted](https://discord.com/channels/100/300/400) → [Reposted](https://discord.com/channels/100/300/401) policy=harassment"},
	{"nodot", "", ""},
	{"roles.jail_reapplied", "", `user=<@1001> release_at=2026-10-17T14:00:00Z unmanageable_roles=[<@&501>] reason="joined while a sentence was in force"`},
	{"roles.jail_bulk", "", `role=<@&7> duration=2h reason="raid" jailed=3 protected=1 users=[<@1> <@2> <@3>]`},
	{"archive.dryrun", "", "channel=[<#300> <#301>]"},
}

// The automatic re-jail names the member as its subject, never as what it
// "was", and the roles it could not strip as kept; a bulk jail's members
// are its subjects, unlabelled, every one of them.
func TestReappliedAndBulkJailsReadAsFields(t *testing.T) {
	e := toEntry("100", core.ActorSystem, "roles.jail_reapplied", "", callSites[len(callSites)-3].new, at)
	if !slices.Equal(e.Subjects, []ledger.Ref{ledger.User("1001"), ledger.Role("501").As("kept")}) || e.Reason == "" {
		t.Fatalf("subjects %+v reason %q", e.Subjects, e.Reason)
	}
	bulk := toEntry("100", "418", "roles.jail_bulk", "", callSites[len(callSites)-2].new, at)
	if want := []ledger.Ref{ledger.Role("7"), ledger.User("1"), ledger.User("2"), ledger.User("3")}; !slices.Equal(bulk.Subjects, want) {
		t.Fatalf("bulk subjects %+v", bulk.Subjects)
	}
}

func TestEveryCallSiteShapeIsAValidEntry(t *testing.T) {
	for _, c := range callSites {
		for _, actor := range []string{"418", core.ActorSystem} {
			e := toEntry("100", actor, c.action, c.old, c.new, at)
			if err := ledger.Validate(e); err != nil {
				t.Errorf("%s by %s: %v\n%+v", c.action, actor, err, e)
			}
		}
	}
}

func TestJailReadsAsFields(t *testing.T) {
	e := toEntry("100", "418", callSites[0].action, "", callSites[0].new, at)
	if e.Summary != "member jailed" || e.Mood != ledger.MoodWarn || e.Reason != "spam, links" {
		t.Fatalf("summary %q mood %q reason %q", e.Summary, e.Mood, e.Reason)
	}
	want := []ledger.Ref{ledger.User("1001"), ledger.Role("501").As("kept"), ledger.Role("502").As("kept")}
	if !slices.Equal(e.Subjects, want) {
		t.Fatalf("subjects %+v, want %+v", e.Subjects, want)
	}
	if !slices.Equal(e.Facts, []ledger.Fact{{Label: "for", Value: "2h"}, {Label: "until", Value: "17 oct 14:00 utc"}}) {
		t.Fatalf("facts %+v", e.Facts)
	}
	if e.Actor != ledger.Member("418") {
		t.Fatalf("actor %+v", e.Actor)
	}
}

func TestAutomatedEntriesAreAutomatic(t *testing.T) {
	if a := toEntry("100", core.ActorSystem, "channel.rotated", "", "", at).Actor; a.Kind != ledger.ActorAuto {
		t.Fatalf("the system actor must be automatic, got %+v", a)
	}
}

func TestOldAndNewWithoutPairsAreAChange(t *testing.T) {
	e := toEntry("100", "418", "contest.configured", "starts monday", "starts friday", at)
	if !slices.Equal(e.Changes, []ledger.Change{{Field: "value", From: "starts monday", To: "starts friday"}}) {
		t.Fatalf("changes %+v", e.Changes)
	}
	moved := toEntry("100", core.ActorSystem, "channel.rotated", "<#300>", "<#301>", at)
	if !slices.Equal(moved.Subjects, []ledger.Ref{ledger.Channel("300").As("was"), ledger.Channel("301")}) || len(moved.Changes) != 0 {
		t.Fatalf("a mention-only change is subjects, got %+v %+v", moved.Subjects, moved.Changes)
	}
}

func TestMoodsFollowTheTable(t *testing.T) {
	for action, want := range map[string]ledger.Mood{
		"archive.deleted":   ledger.MoodDanger,
		"roles.jail":        ledger.MoodWarn,
		"roles.release":     ledger.MoodOK,
		"rotation.dryrun":   ledger.MoodIdle,
		"never.heard_of_it": ledger.MoodInfo,
	} {
		if got := toEntry("100", "418", action, "", "", at).Mood; got != want {
			t.Errorf("%s: mood %q, want %q", action, got, want)
		}
	}
}

func TestBrandWearsMerlinsPalette(t *testing.T) {
	if brand.Color(ledger.MoodDanger) != core.ColorError || brand.Color(ledger.MoodOK) != core.ColorSuccess {
		t.Fatal("palette")
	}
}

// A message link is a subject named by what became of the message, never a
// masked link inside the grid's code block, where it shows as raw markdown.
func TestMessageLinksAreSubjects(t *testing.T) {
	e := toEntry("100", core.ActorSystem, "aimod.rewrite", "",
		"message=[Deleted](https://discord.com/channels/100/300/400) → [Reposted](https://discord.com/channels/100/300/401)", at)
	want := []ledger.Ref{ledger.Message("300", "400").As("deleted"), ledger.Message("300", "401").As("reposted")}
	if !slices.Equal(e.Subjects, want) || len(e.Facts) != 0 {
		t.Fatalf("subjects %+v facts %+v", e.Subjects, e.Facts)
	}
	jump := toEntry("100", core.ActorSystem, "aimod.flag", "", "message=[Jump to message](https://discord.com/channels/100/300/400)", at)
	if !slices.Equal(jump.Subjects, []ledger.Ref{ledger.Message("300", "400")}) {
		t.Fatalf("a plain jump link is unlabelled: %+v", jump.Subjects)
	}
}

func TestOldMentionsAloneAreTheSubject(t *testing.T) {
	e := toEntry("100", core.ActorSystem, "archive.deleted", "<#300>", "", at)
	if !slices.Equal(e.Subjects, []ledger.Ref{ledger.Channel("300")}) {
		t.Fatalf("subjects %+v", e.Subjects)
	}
	if d := toEntry("100", "418", "aimod.funding_cleared", "", "tip jar removed", at); len(d.Facts) != 0 {
		t.Fatalf("a detail repeating the summary: %+v", d.Facts)
	}
}
