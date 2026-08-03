package core

import (
	"strings"
	"testing"
)

// Empty in, empty out is load bearing rather than tidiness. Several audit
// call sites build "role=%s user=%s" summaries where exactly one of the two
// is ever set, and "<@&>" renders as literal garbage in the channel.
func TestMentionsOfNothingRenderNothing(t *testing.T) {
	for name, got := range map[string]string{
		"user":    MentionUser(""),
		"channel": MentionChannel(""),
		"role":    MentionRole(""),
	} {
		if got != "" {
			t.Errorf("empty %s id rendered %q, want empty", name, got)
		}
	}
}

func TestMentionsKeepTheRawSnowflake(t *testing.T) {
	// A later audit search matches on the ID, so the mention has to still
	// contain it verbatim.
	for _, got := range []string{MentionUser("418"), MentionChannel("418"), MentionRole("418")} {
		if !strings.Contains(got, "418") {
			t.Errorf("%q lost the snowflake", got)
		}
	}
	if MentionUser("418") == MentionRole("418") {
		t.Error("user and role mentions are indistinguishable")
	}
}

// Half the audit trail is automated and recorded the bare word "system",
// which tells a moderator reading the channel nothing about who acted.
func TestFormatActorNamesMerlinForAutomatedEntries(t *testing.T) {
	got := FormatActor(ActorSystem)
	if got == ActorSystem {
		t.Fatal("the automated actor is still the bare sentinel")
	}
	if !strings.Contains(strings.ToLower(got), "merlin") {
		t.Errorf("FormatActor(%q) = %q, does not name Merlin", ActorSystem, got)
	}
	if FormatActor("") == "" {
		t.Error("an unknown actor rendered as an empty field")
	}
	if got := FormatActor("418"); got != "<@418>" {
		t.Errorf("FormatActor(\"418\") = %q, want a user mention", got)
	}
}

// Over 4096 bytes Discord rejects the whole message rather than trimming, so
// /config status would fail exactly when it had a long error to report.
func TestTruncateEmbedDescriptionBoundsTheBody(t *testing.T) {
	short := "all healthy"
	if got := TruncateEmbedDescription(short); got != short {
		t.Errorf("a short description was altered: %q", got)
	}
	long := strings.Repeat("x", maxEmbedDescriptionLen+500)
	got := TruncateEmbedDescription(long)
	if len(got) > maxEmbedDescriptionLen {
		t.Errorf("truncated description is %d bytes, over the %d limit", len(got), maxEmbedDescriptionLen)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncation was silent; the tail vanished with nothing saying so")
	}
}

// Slicing mid-rune yields invalid UTF-8, which Discord rejects outright, so
// the cut has to land on a boundary.
func TestTruncateEmbedDescriptionCutsOnARuneBoundary(t *testing.T) {
	got := TruncateEmbedDescription(strings.Repeat("é", maxEmbedDescriptionLen))
	if !utf8ValidString(got) {
		t.Error("truncation produced invalid UTF-8")
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
