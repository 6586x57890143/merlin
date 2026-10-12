package core

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
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

func TestPanelGoesOutAsComponentsV2AndPingsNobody(t *testing.T) {
	cap := &captureTransport{}
	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &http.Client{Transport: cap}
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "i1", Token: "tok", Type: discordgo.InteractionMessageComponent}}

	for _, send := range []func(*discordgo.Session, *discordgo.InteractionCreate, Panel) error{RespondPanel, UpdatePanel, FollowUpPanel} {
		cap.bodies = nil
		if err := send(s, i, Panel{Color: ColorInfo, Title: "t", Body: []discordgo.MessageComponent{Text("<@1>")}}); err != nil {
			t.Fatal(err)
		}
		body := strings.Join(cap.bodies, "")
		for _, want := range []string{`"allowed_mentions":{"parse":null`, `"accent_color"`, `"attachments":[]`} {
			if want == `"attachments":[]` && strings.Contains(body, `"type":4`) {
				continue // a fresh message has nothing to replace
			}
			if !strings.Contains(body, want) {
				t.Errorf("request missing %s:\n%s", want, body)
			}
		}
	}
}

// drawn round-trips a select through JSON, so SelectDiff is tested against
// the types discordgo really decodes off an incoming interaction, not the
// value types a panel is built from.
func drawn(t *testing.T, menu discordgo.SelectMenu, picked ...string) *discordgo.InteractionCreate {
	t.Helper()
	color := 1
	// Message marshals without its components, so the array is wrapped by
	// hand the way Discord sends it.
	raw, err := json.Marshal([]discordgo.MessageComponent{
		discordgo.Container{AccentColor: &color, Components: []discordgo.MessageComponent{Row(menu)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var msg discordgo.Message
	if err := json.Unmarshal([]byte(`{"components":`+string(raw)+`}`), &msg); err != nil {
		t.Fatal(err)
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:    discordgo.InteractionMessageComponent,
		Message: &msg,
		Data:    discordgo.MessageComponentInteractionData{CustomID: menu.CustomID, Values: picked},
	}}
}

// The diff is against what this panel drew, so a stale panel can only undo
// what its own member saw and unpicked, never what somebody added since.
func TestSelectDiffIsAgainstWhatWasDrawn(t *testing.T) {
	menu := discordgo.SelectMenu{MenuType: discordgo.UserSelectMenu, CustomID: "x:admins", DefaultValues: []discordgo.SelectMenuDefaultValue{
		{ID: "a", Type: discordgo.SelectMenuDefaultValueUser}, {ID: "b", Type: discordgo.SelectMenuDefaultValueUser},
	}}
	added, removed := SelectDiff(drawn(t, menu, "b", "c"), "x:admins")
	if !slices.Equal(added, []string{"c"}) || !slices.Equal(removed, []string{"a"}) {
		t.Fatalf("added %v removed %v, want [c] [a]", added, removed)
	}

	str := discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: "x:plugins", Options: []discordgo.SelectMenuOption{
		{Label: "p", Value: "p", Default: true}, {Label: "q", Value: "q"},
	}}
	added, removed = SelectDiff(drawn(t, str, "q"), "x:plugins")
	if !slices.Equal(added, []string{"q"}) || !slices.Equal(removed, []string{"p"}) {
		t.Fatalf("string select: added %v removed %v, want [q] [p]", added, removed)
	}
}

func TestReadoutLinesUp(t *testing.T) {
	got := Readout([][2]string{{"db", "up"}, {"jobs", "3"}})
	want := "```\ndb    up\njobs  3\n```"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
