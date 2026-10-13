package core

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// Every plugin has an icon: a pixel-art tile drawn by tools/icons into
// assets/emoji, uploaded as one of merlin's application emoji at boot. Once
// synced, an icon is an emoji on Discord's CDN, written inline in text and
// shown on a select option or a page thumbnail without uploading anything
// with the message. Until then (a fresh app, Discord unreachable at boot) a
// heading is text alone and a thumbnail goes as an attachment, so nothing
// ever points at an emoji that is not there.
//
//go:embed assets/emoji/*.png
var emojiArt embed.FS

// Emoji is one of merlin's application emoji. Key is what code asks for
// ("plugin_rotation"); Name is what Discord calls it: the key and the start
// of its PNG's hash, so a redrawn icon is a new emoji and an unchanged one is
// never uploaded twice.
type Emoji struct {
	Key, Name string
	PNG       []byte
}

// Emojis is every emoji merlin uploads, read from the embedded art.
func Emojis() []Emoji {
	entries, _ := emojiArt.ReadDir("assets/emoji") // embedded; the pattern matched at build
	out := make([]Emoji, 0, len(entries))
	for _, e := range entries {
		png, _ := emojiArt.ReadFile("assets/emoji/" + e.Name())
		sum := sha256.Sum256(png)
		key := strings.TrimSuffix(e.Name(), ".png")
		out = append(out, Emoji{Key: key, Name: key + "_" + hex.EncodeToString(sum[:3]), PNG: png})
	}
	return out
}

type emojiRef struct{ id, name string }

// emojiIDs is key -> emoji for everything the last sync found or made. Nil
// until a sync has run. Swapped in whole, so a reader never sees one half
// built and takes no lock.
var emojiIDs atomic.Pointer[map[string]emojiRef]

func emojiFor(key string) (emojiRef, bool) {
	ids := emojiIDs.Load()
	if ids == nil {
		return emojiRef{}, false
	}
	r, ok := (*ids)[key]
	return r, ok
}

// PluginEmoji is plugin's icon written inline, <:name:id>, or "" until the
// emoji are synced.
func PluginEmoji(plugin string) string {
	r, ok := emojiFor("plugin_" + plugin)
	if !ok {
		return ""
	}
	return "<:" + r.name + ":" + r.id + ">"
}

// PluginComponentEmoji is plugin's icon for a select option or a button, or
// nil until the emoji are synced.
func PluginComponentEmoji(plugin string) *discordgo.ComponentEmoji {
	r, ok := emojiFor("plugin_" + plugin)
	if !ok {
		return nil
	}
	return &discordgo.ComponentEmoji{ID: r.id, Name: r.name}
}

// PluginIcon is plugin's icon for a thumbnail: its CDN URL and no file once
// synced, otherwise the PNG and the attachment:// URL that names it. A
// plugin with no icon gets ("", nil).
func PluginIcon(plugin string) (string, *discordgo.File) {
	key := "plugin_" + plugin
	if r, ok := emojiFor(key); ok {
		// PNG rather than WebP: Discord does not take WebP in every image slot.
		return "https://cdn.discordapp.com/emojis/" + r.id + ".png", nil
	}
	png, err := emojiArt.ReadFile("assets/emoji/" + key + ".png")
	if err != nil {
		return "", nil
	}
	return "attachment://" + key + ".png", &discordgo.File{Name: key + ".png", ContentType: "image/png", Reader: bytes.NewReader(png)}
}

// EmojiREST is the REST SyncEmoji calls; disgo's client satisfies it.
type EmojiREST interface {
	GetApplicationEmojis(appID snowflake.ID, opts ...rest.RequestOpt) ([]discord.Emoji, error)
	CreateApplicationEmoji(appID snowflake.ID, create discord.EmojiCreate, opts ...rest.RequestOpt) (*discord.Emoji, error)
}

// SyncEmoji makes sure every icon is uploaded to appID, creating those
// missing by name and leaving every other emoji alone, then serves icons
// from them. Safe on every boot: with nothing changed it is one list call.
// An icon it could not create still goes as an attachment.
//
// ponytail: a redrawn icon leaves its old emoji behind; an application holds
// 2000, so pruning the stale ones waits until that is near.
func SyncEmoji(s EmojiREST, appID snowflake.ID) error {
	have, err := s.GetApplicationEmojis(appID)
	if err != nil {
		return fmt.Errorf("listing application emoji: %w", err)
	}
	byName := make(map[string]string, len(have))
	for _, e := range have {
		byName[e.Name] = e.ID.String()
	}
	ids := map[string]emojiRef{}
	var errs []error
	for _, e := range Emojis() {
		if id, ok := byName[e.Name]; ok {
			ids[e.Key] = emojiRef{id, e.Name}
			continue
		}
		made, err := s.CreateApplicationEmoji(appID, discord.EmojiCreate{
			Name:  e.Name,
			Image: *discord.NewIconRaw(discord.IconTypePNG, e.PNG),
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name, err))
			continue
		}
		ids[e.Key] = emojiRef{made.ID.String(), e.Name}
	}
	emojiIDs.Store(&ids)
	return errors.Join(errs...)
}
