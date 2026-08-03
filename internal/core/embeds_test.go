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

// The footer and timestamp are gone on purpose. Together they drew a second
// "Merlin, today at 14:32" line immediately under the one Discord already
// puts above every message the bot sends, saying nothing new in a smaller
// font. This asserts on their absence because the natural instinct when
// adding a field to NewEmbed is to put the brand mark back.
func TestNewEmbedHasNoRedundantFooterOrTimestamp(t *testing.T) {
	for name, e := range map[string]*discordgo.MessageEmbed{
		"ok":       NewEmbed(ColorSuccess, "t", "d"),
		"error":    NewEmbed(ColorError, "t", "d"),
		"landmark": NewLandmarkEmbed(ColorInfo, "t", "d"),
	} {
		if e.Footer != nil {
			t.Errorf("%s: footer is back: %+v", name, e.Footer)
		}
		if e.Timestamp != "" {
			t.Errorf("%s: timestamp is back: %q", name, e.Timestamp)
		}
	}
}

// Nothing points at the avatar any more, so nothing should upload it. This
// is bytes on every single response, and an attachment nobody references
// also shows up in Discord's own attachment list on the message.
func TestEmbedFilesSkipsTheUnreferencedAvatar(t *testing.T) {
	for _, f := range EmbedFiles(NewEmbed(ColorSuccess, "t", "d")) {
		if f.Name == avatarAttachmentName {
			t.Error("the avatar is uploaded despite nothing in the embed referencing it")
		}
	}
	// But an embed that does reference it still gets it, which is what keeps
	// this from being a rule that silently breaks a future footer.
	e := NewEmbed(ColorSuccess, "t", "d")
	e.Footer = &discordgo.MessageEmbedFooter{Text: "x", IconURL: avatarAttachmentURL}
	var found bool
	for _, f := range EmbedFiles(e) {
		if f.Name == avatarAttachmentName {
			found = true
		}
	}
	if !found {
		t.Error("an embed referencing the avatar did not get it attached, so it renders as a broken frame")
	}
}

func TestNewEmbedWithNoFields(t *testing.T) {
	e := NewEmbed(ColorError, "Oops", "failed")
	if len(e.Fields) != 0 {
		t.Fatalf("expected no fields, got %+v", e.Fields)
	}
}

// TestTruncateEmbedField guards a hard Discord limit: an over-long field
// value doesn't get trimmed server-side, it rejects the entire message, so
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

// Every mood must resolve to a real embedded file. A Mood with no asset
// silently produces an embed that references an image nobody uploaded,
// which Discord renders as a broken frame to the whole channel.
func TestEveryMoodHasAnAsset(t *testing.T) {
	for _, m := range []Mood{MoodOK, MoodError, MoodWarn, MoodInfo, MoodNotice, MoodIdle} {
		f := moodFile(m)
		if f == nil {
			t.Errorf("mood %d has no file", m)
			continue
		}
		if f.Name == "" || f.Reader == nil {
			t.Errorf("mood %d has an empty asset", m)
		}
		if url := moodAttachmentURL(m); url != "attachment://"+f.Name {
			t.Errorf("mood %d URL %q does not match its file %q", m, url, f.Name)
		}
	}
	if moodFile(MoodNone) != nil {
		t.Error("MoodNone should have no file")
	}
}

// The mapping from colour to mood is what lets every existing call site
// pick up an icon without being touched, so each palette entry a responder
// uses has to land somewhere sensible.
func TestColorsMapToTheRightMood(t *testing.T) {
	for color, want := range map[int]Mood{
		ColorSuccess: MoodOK,
		ColorError:   MoodError,
		ColorWarning: MoodWarn,
		ColorInfo:    MoodInfo,
		ColorPrimary: MoodNotice,
	} {
		if got := moodForColor(color); got != want {
			t.Errorf("moodForColor(%#x) = %d, want %d", color, got, want)
		}
	}
}

// The invariant that actually protects the channel: whatever an embed
// references by attachment:// must be in the files that go with it.
func TestEmbedFilesCoversEveryReference(t *testing.T) {
	for name, embed := range map[string]*discordgo.MessageEmbed{
		"ok":       NewEmbed(ColorSuccess, "t", "d"),
		"error":    NewEmbed(ColorError, "t", "d"),
		"warn":     NewEmbed(ColorWarning, "t", "d"),
		"info":     NewEmbed(ColorInfo, "t", "d"),
		"notice":   NewEmbed(ColorPrimary, "t", "d"),
		"idle":     WithMood(NewEmbed(ColorWarning, "t", "d"), MoodIdle),
		"landmark": NewLandmarkEmbed(ColorInfo, "t", "d"),
	} {
		t.Run(name, func(t *testing.T) {
			attached := map[string]bool{}
			for _, f := range EmbedFiles(embed) {
				attached[f.Name] = true
			}
			for _, url := range referencedAttachments(embed) {
				if !attached[strings.TrimPrefix(url, "attachment://")] {
					t.Errorf("embed references %q but EmbedFiles does not include it", url)
				}
			}
		})
	}
}

// A landmark embed carries the banner instead of a mood thumbnail: with
// both it reads as cluttered.
func TestLandmarkEmbedHasNoMoodThumbnail(t *testing.T) {
	e := NewLandmarkEmbed(ColorInfo, "t", "d")
	if e.Thumbnail != nil {
		t.Errorf("landmark embed carries a thumbnail as well as its banner: %+v", e.Thumbnail)
	}
	if e.Image == nil {
		t.Error("landmark embed lost its banner")
	}
}

func referencedAttachments(e *discordgo.MessageEmbed) []string {
	var out []string
	add := func(u string) {
		if strings.HasPrefix(u, "attachment://") {
			out = append(out, u)
		}
	}
	if e.Footer != nil {
		add(e.Footer.IconURL)
	}
	if e.Thumbnail != nil {
		add(e.Thumbnail.URL)
	}
	if e.Image != nil {
		add(e.Image.URL)
	}
	return out
}
