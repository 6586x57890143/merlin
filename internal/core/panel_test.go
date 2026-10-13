package core

import (
	"slices"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// panelURLs walks a rendered panel for every attachment:// URL it points at.
func panelURLs(cs []discordgo.MessageComponent) []string {
	var out []string
	for _, c := range cs {
		switch v := c.(type) {
		case discordgo.Container:
			out = append(out, panelURLs(v.Components)...)
		case discordgo.Section:
			if th, ok := v.Accessory.(discordgo.Thumbnail); ok {
				out = append(out, th.Media.URL)
			}
		case discordgo.MediaGallery:
			for _, it := range v.Items {
				out = append(out, it.Media.URL)
			}
		}
	}
	return out
}

// An attachment:// URL with no upload behind it renders as a broken frame,
// and nothing in the code that built the panel would look wrong, so the
// files are checked against the URLs rather than trusted to match.
func TestPanelUploadsExactlyWhatItPointsAt(t *testing.T) {
	for _, p := range []Panel{
		{Color: ColorSuccess, Title: "t"},
		{Color: ColorWarning, Title: "t", Mood: MoodIdle},
		{Color: 0x123456, Title: "t", Line: "l", Note: "✓ n", Footer: "f", Body: []discordgo.MessageComponent{Text("b")}},
	} {
		var names []string
		for _, f := range p.Files() {
			names = append(names, "attachment://"+f.Name)
		}
		urls := panelURLs(p.Components())
		slices.Sort(names)
		slices.Sort(urls)
		if !slices.Equal(names, urls) {
			t.Errorf("files %v, but the panel points at %v", names, urls)
		}
	}
}

// The rule is what pins every page of a panel to one width; a panel that
// lost it would change size on every click.
func TestPanelCarriesTheRule(t *testing.T) {
	urls := panelURLs(Panel{Color: ColorInfo, Title: "t"}.Components())
	if !slices.Contains(urls, ruleAttachmentURL) {
		t.Fatalf("panel has no rule: %v", urls)
	}
	if !slices.Contains(urls, moodAttachmentURL(MoodInfo)) {
		t.Fatalf("panel does not wear the mood its colour implies: %v", urls)
	}
}

func TestReadoutLinesUp(t *testing.T) {
	got := Readout([][2]string{{"db", "up"}, {"jobs", "3"}})
	want := "```\ndb    up\njobs  3\n```"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
