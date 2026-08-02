package core

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

func TestNewEmbedSetsFields(t *testing.T) {
	f := &discordgo.MessageEmbedField{Name: "k", Value: "v"}
	e := NewEmbed(ColorSuccess, "Title", "Description", f)

	if e.Title != "Title" || e.Description != "Description" || e.Color != ColorSuccess {
		t.Fatalf("unexpected embed: %+v", e)
	}
	if len(e.Fields) != 1 || e.Fields[0] != f {
		t.Fatalf("expected the given field to be preserved, got %+v", e.Fields)
	}
}

func TestNewEmbedWithNoFields(t *testing.T) {
	e := NewEmbed(ColorError, "Oops", "failed")
	if len(e.Fields) != 0 {
		t.Fatalf("expected no fields, got %+v", e.Fields)
	}
}

// TestTruncateEmbedField guards a hard Discord limit: an over-long field
// value doesn't get trimmed server-side, it rejects the entire message — so
// one guild with a long sticky-message set would lose the whole response.
func TestTruncateEmbedField(t *testing.T) {
	short := strings.Repeat("a", maxEmbedFieldValue)
	if got := TruncateEmbedField(short); got != short {
		t.Error("a value exactly at the limit should pass through untouched")
	}

	long := strings.Repeat("a", maxEmbedFieldValue*2)
	got := TruncateEmbedField(long)
	if len(got) > maxEmbedFieldValue {
		t.Errorf("truncated value is %d bytes, still over the %d limit", len(got), maxEmbedFieldValue)
	}
	if !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("truncation should be marked, got tail %q", got[len(got)-20:])
	}
}

// TestTruncateEmbedFieldCutsOnRuneBoundary keeps the truncation from slicing
// through a multi-byte character: invalid UTF-8 is rejected by the API just
// as hard as an over-long value, so a guild whose sticky messages contain
// emoji would hit exactly the failure this function exists to prevent.
func TestTruncateEmbedFieldCutsOnRuneBoundary(t *testing.T) {
	// Every rune here is 4 bytes, so an unaligned cut is near-certain if the
	// boundary isn't respected.
	got := TruncateEmbedField(strings.Repeat("🦅", maxEmbedFieldValue))
	if !utf8.ValidString(got) {
		t.Fatal("truncation produced invalid UTF-8")
	}
	if len(got) > maxEmbedFieldValue {
		t.Errorf("truncated value is %d bytes, over the %d limit", len(got), maxEmbedFieldValue)
	}
}
