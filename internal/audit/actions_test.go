package audit

import (
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/core"
)

// The degradation ladder is the whole point of pairing a table with a
// mechanical fallback: an action nobody added a row for still has to produce
// something a moderator can read. Blank would be strictly worse than the raw
// dotted key this replaced, so that is the property asserted.
func TestUnlistedActionsStillGetAReadableTitle(t *testing.T) {
	for _, action := range []string{
		"factions.member_joined",
		"reporting.report_routed",
		"something.with.several.dots",
		"nodots",
	} {
		got := metaFor(action).title
		if got == "" {
			t.Errorf("action %q rendered a blank title", action)
		}
		if got == action {
			t.Errorf("action %q was passed through raw rather than humanized", action)
		}
		if strings.Contains(got, "_") {
			t.Errorf("action %q kept its underscores: %q", action, got)
		}
	}
}

// An empty action is not expected, but a blank embed title is a broken
// message rather than a cosmetic problem, so it has a floor too.
func TestEmptyActionStillGetsATitle(t *testing.T) {
	if got := metaFor("").title; got == "" {
		t.Error("an empty action produced a blank title")
	}
}

func TestHumanizeKeepsTheNamespace(t *testing.T) {
	cases := map[string]string{
		"config.mod_role_added":    "Config: mod role added",
		"rotation.channel_deleted": "Rotation: channel deleted",
		"nodots":                   "Nodots",
	}
	for in, want := range cases {
		if got := humanize(in); got != want {
			t.Errorf("humanize(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every entry was ColorSuccess before, including permanent channel deletion,
// so the single visual cue this format has was telling a moderator that an
// irreversible delete had gone well. Severity now has to actually vary, and
// specifically must not call destruction a success.
func TestDestructiveActionsAreNotReportedAsSuccess(t *testing.T) {
	for _, action := range []string{
		"archive.deleted", "rotation.channel_deleted", "roles.jail", "config.admin_removed",
	} {
		if got := metaFor(action).color; got == core.ColorSuccess {
			t.Errorf("action %q is presented as a success", action)
		}
	}
	for _, action := range []string{"config.admin_added", "roles.release"} {
		if got := metaFor(action).color; got != core.ColorSuccess {
			t.Errorf("action %q should read as a success, got colour %#x", action, got)
		}
	}
}

// Dry-run entries wear the idle face rather than the alarmed one: the bot is
// doing exactly what it was told. Same precedent as /config pause.
func TestDryRunActionsUseTheIdleFace(t *testing.T) {
	for _, action := range []string{"rotation.dryrun", "archive.dryrun"} {
		embed := buildEmbed(core.ActorSystem, action, "", "would have happened")
		if embed.Thumbnail == nil {
			t.Fatalf("action %q has no mood thumbnail", action)
		}
		if !strings.Contains(embed.Thumbnail.URL, "idle") {
			t.Errorf("action %q does not wear the idle face: %q", action, embed.Thumbnail.URL)
		}
	}
}

// "system" told a moderator nothing about who or what acted. Roughly half of
// all audit entries are automated, so this is the field most in need of
// saying something.
func TestAutomatedEntriesNameMerlinRatherThanSystem(t *testing.T) {
	embed := buildEmbed(core.ActorSystem, "channel.rotated", "", "")
	actor := embed.Fields[0].Value
	if actor == core.ActorSystem {
		t.Fatal("the automated actor is still rendered as the bare word system")
	}
	if !strings.Contains(strings.ToLower(actor), "merlin") {
		t.Errorf("the automated actor does not name Merlin: %q", actor)
	}

	human := buildEmbed("418", "config.admin_added", "", "<@99>")
	if got := human.Fields[0].Value; got != "<@418>" {
		t.Errorf("a human actor should render as a mention, got %q", got)
	}
}

// An embed that references an attachment nobody uploaded renders as a broken
// frame, and nothing about the code that built it would look wrong.
func TestAuditEmbedCarriesTheFileItReferences(t *testing.T) {
	embed := buildEmbed("418", "config.admin_added", "", "<@99>")
	if embed.Thumbnail == nil {
		t.Fatal("audit embed has no mood thumbnail")
	}
	name, ok := strings.CutPrefix(embed.Thumbnail.URL, "attachment://")
	if !ok {
		t.Fatalf("thumbnail is not an attachment reference: %q", embed.Thumbnail.URL)
	}
	var found bool
	for _, f := range core.EmbedFiles(embed) {
		if f.Name == name {
			found = true
		}
	}
	if !found {
		t.Errorf("embed references %q but EmbedFiles does not supply it", name)
	}
}

// A value over 1024 bytes fails the whole message rather than being trimmed
// by Discord, so one long sticky or a sweep listing many channels would cost
// the guild its live notification for that action entirely.
func TestLongValuesAreTruncatedRatherThanFailingTheMessage(t *testing.T) {
	long := strings.Repeat("x", 4000)
	embed := buildEmbed("418", "rotation.edit", long, long)
	if len(embed.Fields) != 3 {
		t.Fatalf("fields = %d, want actor plus before and after", len(embed.Fields))
	}
	for _, f := range embed.Fields {
		if len(f.Value) > 1024 {
			t.Errorf("field %q is %d bytes, over Discord's 1024 limit", f.Name, len(f.Value))
		}
	}
}

// With neither value set the embed used to carry a lone Actor line; with only
// one set it used to be a half-width field with a gap beside it. Neither is
// wrong exactly, but both read as a message that lost something.
func TestValueFieldsMatchWhatWasActuallySupplied(t *testing.T) {
	if got := len(buildEmbed("418", "config.setup", "", "").Fields); got != 1 {
		t.Errorf("with no values, fields = %d, want just the actor", got)
	}
	only := buildEmbed("418", "config.admin_added", "", "<@99>")
	if len(only.Fields) != 2 || only.Fields[1].Name != "Details" {
		t.Errorf("a single value should be one full-width Details field, got %+v", only.Fields)
	}
	if only.Fields[1].Inline {
		t.Error("the single-value field should not be inline; there is nothing to sit beside it")
	}
	removed := buildEmbed("418", "config.admin_removed", "<@99>", "")
	if len(removed.Fields) != 2 || removed.Fields[1].Name != "Removed" {
		t.Errorf("an old-only value should render as Removed, got %+v", removed.Fields)
	}
}
