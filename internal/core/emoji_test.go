package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// nameMethod is a plugin's Name, which is what its icon is looked up by.
var nameMethod = regexp.MustCompile(`func \(\w+ \*\w+\) Name\(\) string\s*\{\s*return "([a-z]+)"\s*\}`)

// Every plugin has an icon, the one /help and its panel wear. The names
// checked are what each Name method returns, read from source, so a new
// plugin cannot ship without its glyph in tools/icons.
func TestEveryPluginHasAnIcon(t *testing.T) {
	var files []string
	for _, pattern := range []string{"../plugins/*/*.go", "../scheduler/*.go"} {
		m, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range nameMethod.FindAllSubmatch(src, -1) {
			found++
			if _, err := emojiArt.Open("assets/emoji/plugin_" + string(m[1]) + ".png"); err != nil {
				t.Errorf("plugin %s has no icon: add its glyph to tools/icons and run go run ./tools/icons", m[1])
			}
		}
	}
	if found < 8 {
		t.Fatalf("found only %d plugins; the Name pattern no longer matches", found)
	}
}

// Every plugin has a /help page, so the guide lists all of merlin and a new
// plugin cannot ship without saying what it does.
func TestEveryPluginHasAHelpPage(t *testing.T) {
	dirs, err := filepath.Glob("../plugins/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range append(dirs, "../scheduler") {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		var named, helped bool
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, _ := os.ReadFile(f)
			named = named || nameMethod.Match(src)
			helped = helped || strings.Contains(string(src), ") Help() core.Help {") || strings.Contains(string(src), ") Help() Help {")
		}
		if named && !helped {
			t.Errorf("%s has no Help(): give it a /help page", dir)
		}
	}
}

// An icon is pixel art on an exact grid: 128 px, every 4 px cell one flat
// colour, small enough that uploading it costs nothing.
func TestIconsAreOnTheirGrid(t *testing.T) {
	for _, e := range Emojis() {
		if len(e.PNG) > 4<<10 {
			t.Errorf("%s is %d bytes; it was written true colour or the art changed", e.Key, len(e.PNG))
		}
		img, err := png.Decode(bytes.NewReader(e.PNG))
		if err != nil {
			t.Fatalf("%s: %v", e.Key, err)
		}
		if b := img.Bounds(); b.Dx() != 128 || b.Dy() != 128 {
			t.Errorf("%s is %dx%d, want 128x128", e.Key, b.Dx(), b.Dy())
		}
		for y := 0; y < 128; y++ {
			for x := 0; x < 128; x++ {
				if img.At(x, y) != img.At(x-x%4, y-y%4) {
					t.Fatalf("%s: cell at %d,%d is not flat", e.Key, x-x%4, y-y%4)
				}
			}
		}
		if len(e.Name) > 32 {
			t.Errorf("%s: emoji name %q is over Discord's 32", e.Key, e.Name)
		}
	}
}

type fakeEmojiREST struct {
	have    []discord.Emoji
	created []string
	failOn  string
}

func (f *fakeEmojiREST) GetApplicationEmojis(snowflake.ID, ...rest.RequestOpt) ([]discord.Emoji, error) {
	return f.have, nil
}

func (f *fakeEmojiREST) CreateApplicationEmoji(_ snowflake.ID, c discord.EmojiCreate, _ ...rest.RequestOpt) (*discord.Emoji, error) {
	if strings.HasPrefix(c.Name, f.failOn) && f.failOn != "" {
		return nil, errors.New("refused")
	}
	if b, _ := json.Marshal(c.Image); !strings.HasPrefix(string(b), `"data:image/png;base64,`) {
		return nil, errors.New("not a data URI: " + string(b))
	}
	f.created = append(f.created, c.Name)
	return &discord.Emoji{ID: snowflake.ID(1000 + len(f.created)), Name: c.Name}, nil
}

// The sync uploads only what is missing, and an icon it could not upload
// still renders, as an attachment.
func TestSyncEmojiCreatesOnlyWhatIsMissing(t *testing.T) {
	t.Cleanup(func() { emojiIDs.Store(nil) })

	if url, file := PluginUpload("rotation"); file == nil || !strings.HasPrefix(url, "attachment://") {
		t.Fatalf("before a sync the icon must be attached, got %q", url)
	}
	if PluginEmoji("rotation") != "" {
		t.Fatal("an emoji was written before any sync")
	}

	all := Emojis()
	present := all[slices.IndexFunc(all, func(e Emoji) bool { return strings.HasPrefix(e.Key, "plugin_") })]
	f := &fakeEmojiREST{have: []discord.Emoji{{ID: 77, Name: present.Name}}, failOn: "plugin_rotation"}
	if err := SyncEmoji(f, 2); err == nil {
		t.Error("a refused upload was not reported")
	}
	if len(f.created) != len(all)-2 {
		t.Errorf("created %d, want every icon but the one present and the one refused", len(f.created))
	}
	if got := PluginEmoji(strings.TrimPrefix(present.Key, "plugin_")); got != "<:"+present.Name+":77>" {
		t.Errorf("existing emoji renders as %q", got)
	}
	if url, file := PluginUpload("rotation"); file == nil || !strings.HasPrefix(url, "attachment://") {
		t.Error("a refused icon must still go as an attachment")
	}
	if url, file := PluginUpload("roles"); file != nil || !strings.HasPrefix(url, "https://cdn.discordapp.com/emojis/") {
		t.Errorf("a synced icon must come from the CDN, got %q", url)
	}
	if PluginOptionEmoji("roles") == nil || PluginOptionEmoji("nonesuch") != nil {
		t.Error("component emoji wrong")
	}
	pn := Panel{Title: "Roles", Plugin: "roles"}
	if !strings.Contains(pn.Layout()[0].(discord.ContainerComponent).Components[0].(discord.SectionComponent).Components[0].(discord.TextDisplayComponent).Content, PluginEmoji("roles")) {
		t.Error("a panel does not lead with its plugin's icon")
	}
}

// Every field label wears a bullet once the emoji are synced: its own by
// meaning, the dot otherwise. Spacers and labels already led by an emoji
// are left alone, the caller's fields are not touched, and before a sync
// the embed goes out as built.
func TestBulletedLabels(t *testing.T) {
	zwsp := string(rune(0x200b)) // a grid spacer, as cygnet/ledger pads with
	t.Cleanup(func() { emojiIDs.Store(nil) })
	e := discord.Embed{Fields: []discord.EmbedField{
		{Name: "Member"}, {Name: "something new"}, {Name: zwsp}, {Name: "⛽ Scanning credit"},
	}}
	if got := Bulleted(e); got.Fields[0].Name != "Member" {
		t.Fatalf("before a sync: %q", got.Fields[0].Name)
	}
	emojiIDs.Store(&map[string]emojiRef{"key_member": {"1", "key_member_a"}, "key_dot": {"2", "key_dot_b"}})
	got := Bulleted(e)
	want := []string{"<:key_member_a:1> Member", "<:key_dot_b:2> something new", zwsp, "⛽ Scanning credit"}
	for i, w := range want {
		if got.Fields[i].Name != w {
			t.Errorf("field %d: %q, want %q", i, got.Fields[i].Name, w)
		}
	}
	if e.Fields[0].Name != "Member" {
		t.Error("the caller's fields were changed")
	}
	if again := Bulleted(got); again.Fields[0].Name != want[0] {
		t.Errorf("a second pass changed it: %q", again.Fields[0].Name)
	}
}

// Every bullet labelKeys names has its art.
func TestEveryLabelKeyHasArt(t *testing.T) {
	for label, key := range labelKeys {
		if _, err := emojiArt.Open("assets/emoji/key_" + key + ".png"); err != nil {
			t.Errorf("%q wears key_%s, which tools/icons does not draw", label, key)
		}
	}
}
