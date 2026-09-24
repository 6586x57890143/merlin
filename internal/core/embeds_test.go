package core

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// captureTransport records the body of every Discord request, so a test can
// assert what was actually put on the wire rather than what a helper meant.
type captureTransport struct{ bodies []string }

func (c *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		c.bodies = append(c.bodies, string(raw))
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    r,
	}, nil
}

// Discord fixes ephemerality when an interaction is acknowledged, so the
// difference between these two helpers is decided at defer time and cannot be
// corrected by the follow-up that lands afterwards. A tip jar deferred
// privately stays private no matter what is edited into it, which is why the
// public variant exists as a separate call rather than a flag further down.
func TestDeferResponsePublicIsNotEphemeral(t *testing.T) {
	const ephemeralFlag = 64 // 1 << 6, Discord's MessageFlagsEphemeral

	for _, tc := range []struct {
		name         string
		defer_       func(*discordgo.Session, *discordgo.InteractionCreate) error
		wantFlagSeen bool
	}{
		{"private", DeferResponse, true},
		{"public", DeferResponsePublic, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &captureTransport{}
			s, err := discordgo.New("Bot test-token")
			if err != nil {
				t.Fatalf("discordgo.New: %v", err)
			}
			s.Client = &http.Client{Transport: cap}

			i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
				ID: "i1", Token: "tok", Type: discordgo.InteractionApplicationCommand,
			}}
			if err := tc.defer_(s, i); err != nil {
				t.Fatalf("defer: %v", err)
			}
			if len(cap.bodies) != 1 {
				t.Fatalf("sent %d requests, want 1", len(cap.bodies))
			}

			var sent struct {
				Data *struct {
					Flags int `json:"flags"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(cap.bodies[0]), &sent); err != nil {
				t.Fatalf("body was not JSON: %v (%s)", err, cap.bodies[0])
			}
			gotFlag := sent.Data != nil && sent.Data.Flags&ephemeralFlag != 0
			if gotFlag != tc.wantFlagSeen {
				t.Fatalf("ephemeral flag set = %v, want %v (body %s)", gotFlag, tc.wantFlagSeen, cap.bodies[0])
			}
		})
	}
}

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
// "merlin, today at 14:32" line immediately under the one Discord already
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

// TestFollowUpReplacesAttachmentsOnEveryEdit: an omitted attachments field
// keeps what is already on the message and appends the new upload, and a
// placeholder edited every twenty seconds hit Discord's cap of ten on the
// eleventh edit, three minutes in, after which every edit was refused. The
// wire body must say "attachments: []" so each edit replaces the last.
func TestFollowUpReplacesAttachmentsOnEveryEdit(t *testing.T) {
	cap := &captureTransport{}
	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	s.Client = &http.Client{Transport: cap}
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "i1", Token: "tok", Type: discordgo.InteractionApplicationCommand,
	}}

	if err := FollowUpEmbed(s, i, NewEmbed(ColorInfo, "Still counting", "9369 messages so far")); err != nil {
		t.Fatalf("FollowUpEmbed: %v", err)
	}
	if len(cap.bodies) != 1 {
		t.Fatalf("expected one request, got %d", len(cap.bodies))
	}
	body := cap.bodies[0]
	if !strings.Contains(body, `"attachments":[]`) {
		t.Fatalf("edit must clear the previous upload; body was:\n%s", body)
	}
	if !strings.Contains(body, `filename="merlin_info.png"`) {
		t.Fatalf("edit must still carry this response's own thumbnail; body was:\n%s", body)
	}
}

// The separator is what pins every embed to one width, and an image URL with
// no matching upload renders as a broken frame, so both halves are asserted.
func TestEveryEmbedCarriesTheRuleItReferences(t *testing.T) {
	e := NewEmbed(ColorInfo, "t", "d")
	if e.Image == nil || e.Image.URL != ruleAttachmentURL {
		t.Fatalf("NewEmbed image = %+v, want the separator", e.Image)
	}
	var found bool
	for _, f := range EmbedFiles(e) {
		if f.Name == ruleAttachmentName {
			found = true
		}
	}
	if !found {
		t.Error("the separator is referenced but not uploaded")
	}
	if l := NewLandmarkEmbed(ColorInfo, "t", "d"); l.Image.URL != bannerAttachmentURL {
		t.Errorf("a landmark embed should show the banner, got %q", l.Image.URL)
	}
}
