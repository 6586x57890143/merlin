package core

import (
	"bytes"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
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

type fakeEmojiSession struct {
	have    []*discordgo.Emoji
	created []string
	failOn  string
}

func (f *fakeEmojiSession) ApplicationEmojis(string, ...discordgo.RequestOption) ([]*discordgo.Emoji, error) {
	return f.have, nil
}

func (f *fakeEmojiSession) ApplicationEmojiCreate(_ string, p *discordgo.EmojiParams, _ ...discordgo.RequestOption) (*discordgo.Emoji, error) {
	if strings.HasPrefix(p.Name, f.failOn) && f.failOn != "" {
		return nil, errors.New("refused")
	}
	if !strings.HasPrefix(p.Image, "data:image/png;base64,") {
		return nil, errors.New("not a data URI")
	}
	f.created = append(f.created, p.Name)
	return &discordgo.Emoji{ID: "new-" + p.Name, Name: p.Name}, nil
}

// The sync uploads only what is missing, and an icon it could not upload
// still renders, as an attachment.
func TestSyncEmojiCreatesOnlyWhatIsMissing(t *testing.T) {
	t.Cleanup(func() { emojiIDs.Store(nil) })

	if url, file := PluginIcon("rotation"); file == nil || !strings.HasPrefix(url, "attachment://") {
		t.Fatalf("before a sync the icon must be attached, got %q", url)
	}
	if PluginEmoji("rotation") != "" {
		t.Fatal("an emoji was written before any sync")
	}

	all := Emojis()
	f := &fakeEmojiSession{have: []*discordgo.Emoji{{ID: "old", Name: all[0].Name}}, failOn: "plugin_rotation"}
	if err := SyncEmoji(f, "app"); err == nil {
		t.Error("a refused upload was not reported")
	}
	if len(f.created) != len(all)-2 {
		t.Errorf("created %d, want every icon but the one present and the one refused", len(f.created))
	}
	if got := PluginEmoji(strings.TrimPrefix(all[0].Key, "plugin_")); got != "<:"+all[0].Name+":old>" {
		t.Errorf("existing emoji renders as %q", got)
	}
	if url, file := PluginIcon("rotation"); file == nil || !strings.HasPrefix(url, "attachment://") {
		t.Error("a refused icon must still go as an attachment")
	}
	if url, file := PluginIcon("roles"); file != nil || !strings.HasPrefix(url, "https://cdn.discordapp.com/emojis/") {
		t.Errorf("a synced icon must come from the CDN, got %q", url)
	}
	if PluginComponentEmoji("roles") == nil || PluginComponentEmoji("nonesuch") != nil {
		t.Error("component emoji wrong")
	}
	pn := Panel{Title: "Roles", Plugin: "roles"}
	if !strings.Contains(pn.Components()[0].(discordgo.Container).Components[0].(discordgo.Section).Components[0].(discordgo.TextDisplay).Content, PluginEmoji("roles")) {
		t.Error("a panel does not lead with its plugin's icon")
	}
}
