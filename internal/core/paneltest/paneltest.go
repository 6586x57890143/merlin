// Package paneltest drives a core.Panel from a test the way Discord would:
// a click or a form submission from a member, against a panel as Discord
// hands it back, through a session whose requests are recorded rather than
// sent.
package paneltest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// Wire records every request a session makes.
type Wire struct {
	mu     sync.Mutex
	Bodies []string
	Paths  []string
}

func (w *Wire) RoundTrip(r *http.Request) (*http.Response, error) {
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	w.mu.Lock()
	w.Bodies, w.Paths = append(w.Bodies, body), append(w.Paths, r.Method+" "+r.URL.Path)
	w.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

// All is every body sent, joined.
func (w *Wire) All() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.Bodies, "\n")
}

// Answered is whether the interaction was responded to at all.
func (w *Wire) Answered() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.Paths {
		if strings.HasSuffix(p, "/callback") {
			return true
		}
	}
	return false
}

// Session is a discordgo session that sends nothing anywhere.
func Session() (*discordgo.Session, *Wire) {
	w := &Wire{}
	s, _ := discordgo.New("Bot test")
	s.Client = &http.Client{Transport: w}
	s.State.User = &discordgo.User{ID: "bot"}
	return s, w
}

// Received is pn as Discord hands it back on a click: through JSON, so
// select defaults come back in the types discordgo really decodes.
func Received(pn core.Panel) *discordgo.Message {
	raw, err := json.Marshal(pn.Components())
	if err != nil {
		panic(err)
	}
	var m discordgo.Message
	if err := json.Unmarshal([]byte(`{"components":`+string(raw)+`}`), &m); err != nil {
		panic(err)
	}
	return &m
}

// Click is a member's click on customID in guild g, picking values, on the
// panel msg (nil for a slash command).
func Click(user, customID string, msg *discordgo.Message, values ...string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "i", Token: "t", Type: discordgo.InteractionMessageComponent, GuildID: "g1",
		Member:  &discordgo.Member{User: &discordgo.User{ID: user}},
		Message: msg,
		Data:    discordgo.MessageComponentInteractionData{CustomID: customID, Values: values},
	}}
}

// Submit is a member's submission of the form customID with fields.
func Submit(user, customID string, fields map[string]string) *discordgo.InteractionCreate {
	var rows []discordgo.MessageComponent
	for k, v := range fields {
		rows = append(rows, &discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: k, Value: v}}})
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "i", Token: "t", Type: discordgo.InteractionModalSubmit, GuildID: "g1",
		Member: &discordgo.Member{User: &discordgo.User{ID: user}},
		Data:   discordgo.ModalSubmitInteractionData{CustomID: customID, Components: rows},
	}}
}

// Selects is every select on a panel, by CustomID.
func Selects(pn core.Panel) map[string]discordgo.SelectMenu {
	out := map[string]discordgo.SelectMenu{}
	walk(pn.Components(), func(c discordgo.MessageComponent) {
		if m, ok := c.(discordgo.SelectMenu); ok {
			out[m.CustomID] = m
		}
	})
	return out
}

// Buttons is every button on a panel, by CustomID.
func Buttons(pn core.Panel) map[string]discordgo.Button {
	out := map[string]discordgo.Button{}
	walk(pn.Components(), func(c discordgo.MessageComponent) {
		if b, ok := c.(discordgo.Button); ok {
			out[b.CustomID] = b
		}
	})
	return out
}

func walk(cs []discordgo.MessageComponent, fn func(discordgo.MessageComponent)) {
	for _, c := range cs {
		fn(c)
		switch v := c.(type) {
		case discordgo.Container:
			walk(v.Components, fn)
		case discordgo.ActionsRow:
			walk(v.Components, fn)
		case discordgo.Section:
			walk(v.Components, fn)
			if v.Accessory != nil {
				fn(v.Accessory)
			}
		}
	}
}
