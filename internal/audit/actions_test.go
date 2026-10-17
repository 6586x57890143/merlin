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
