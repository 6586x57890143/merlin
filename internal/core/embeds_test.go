package core

import (
	"testing"

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
