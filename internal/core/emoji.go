package core

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	cards "github.com/6586x57890143/cygnet/support"
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
	art := map[string][]byte{}
	for _, e := range entries {
		png, _ := emojiArt.ReadFile("assets/emoji/" + e.Name())
		key := strings.TrimSuffix(e.Name(), ".png")
		art[key] = png
	}
	// The shared generator owns payment marks. Register each in merlin's
	// existing hashed application-emoji library, overriding old copies.
	for _, key := range cards.Icons {
		if png, ok := cards.IconPNG(key); ok {
			art["support_"+key] = png
		}
	}
	keys := make([]string, 0, len(art))
	for key := range art {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]Emoji, 0, len(keys))
	for _, key := range keys {
		png := art[key]
		sum := sha256.Sum256(png)
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
func PluginEmoji(plugin string) string { return inlineEmoji("plugin_" + plugin) }

func inlineEmoji(key string) string {
	r, ok := emojiFor(key)
	if !ok {
		return ""
	}
	return "<:" + r.name + ":" + r.id + ">"
}

// labelKeys is which bullet a field label wears, by what the label means.
// A label not listed wears the plain dot, so every key on a card is
// bulleted and a new one needs no entry to fit in.
var labelKeys = map[string]string{
	"actor": "actor", "by": "actor", "set by": "actor", "written by": "actor",
	"member": "member", "members": "member", "user": "member", "users": "member",
	"winners": "member", "donor": "member",
	"channel": "channel", "channels": "channel", "forum": "channel", "where": "channel",
	"category": "channel", "categories": "channel",
	"role": "role", "roles": "role", "kept": "role", "restored": "role", "withheld": "role", "marker": "role",
	"policy": "policy", "allow": "policy", "deny": "policy",
	"confidence": "gauge", "score": "gauge", "points": "gauge", "votes": "gauge", "standings": "gauge",
	"for": "time", "until": "time", "when": "time", "duration": "time", "interval": "time",
	"retention": "time", "closes": "time", "next": "time", "voting opens": "time", "submissions open": "time",
	"message": "message", "messages": "message", "media": "message", "entries": "message", "the entries": "message",
	"reason": "reason", "reason given": "reason", "why": "reason", "detail": "reason", "details": "reason",
	"value": "change", "from": "change", "to": "change", "removed": "change",
	"link": "link", "linked to": "link", "gallery": "link", "voting page": "link", "case": "link",
	"prize": "prize", "prizes": "prize", "pledged": "prize", "your code": "prize", "code": "prize",
	"status": "status", "phase": "status", "enabled": "status", "tier": "status", "notice": "status",
	"visibility": "status", "who can see it": "status", "access": "status",
}

// Bulleted is e with an icon before each field label, the way /help puts
// a plugin's icon before its name, so a card's keys read as a bulleted list
// in merlin's own art. A copy: the caller's fields are left alone. Until the
// emoji are synced it is e unchanged. A spacer, or a label already led by
// an emoji of its own, is left as it is, which also makes a second pass a
// no-op.
func Bulleted(e discord.Embed) discord.Embed {
	if len(e.Fields) == 0 || emojiIDs.Load() == nil {
		return e
	}
	fields := slices.Clone(e.Fields)
	for i, f := range fields {
		if r, _ := utf8.DecodeRuneInString(f.Name); !unicode.IsLetter(r) {
			continue
		}
		key, ok := labelKeys[strings.ToLower(f.Name)]
		if !ok {
			key = "dot"
		}
		if mark := inlineEmoji("key_" + key); mark != "" {
			fields[i].Name = mark + " " + f.Name
		}
	}
	e.Fields = fields
	return e
}

// AppEmoji is the app emoji drawn from assets/emoji/<key>.png written
// inline, or "" until the emoji are synced.
func AppEmoji(key string) string { return inlineEmoji(key) }

// PluginOptionEmoji is plugin's icon for a select option or a button, or
// nil until the emoji are synced.
func PluginOptionEmoji(plugin string) *discord.ComponentEmoji {
	return AppComponentEmoji("plugin_" + plugin)
}

// AppComponentEmoji is the app emoji key for a select option or a button,
// or nil until the emoji are synced.
func AppComponentEmoji(key string) *discord.ComponentEmoji {
	r, ok := emojiFor(key)
	if !ok {
		return nil
	}
	id, err := snowflake.Parse(r.id)
	if err != nil {
		return nil
	}
	return &discord.ComponentEmoji{ID: id, Name: r.name}
}

// PluginUpload is plugin's icon for a thumbnail: its CDN URL and no file
// once synced, otherwise the PNG and the attachment:// URL that names it. A
// plugin with no icon gets ("", nil).
func PluginUpload(plugin string) (string, *discord.File) {
	key := "plugin_" + plugin
	if r, ok := emojiFor(key); ok {
		// PNG rather than WebP: Discord does not take WebP in every image slot.
		return "https://cdn.discordapp.com/emojis/" + r.id + ".png", nil
	}
	png, err := emojiArt.ReadFile("assets/emoji/" + key + ".png")
	if err != nil {
		return "", nil
	}
	return "attachment://" + key + ".png", asset{key + ".png", png}.upload()
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
