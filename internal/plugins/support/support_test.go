package support

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/voice"
)

const owner = "175928847299117063"

type sent struct {
	channel string
	data    discord.MessageCreate
}

type fakeSender struct {
	mu   sync.Mutex
	sent []sent
}

func (f *fakeSender) CreateMessage(channelID snowflake.ID, data discord.MessageCreate) (*discord.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sent{channelID.String(), data})
	return &discord.Message{}, nil
}

type fakeSettings struct {
	off     bool
	channel string
}

func (f fakeSettings) PluginEnabled(string, string) bool { return !f.off }
func (f fakeSettings) SupporterChannelID(string) string   { return f.channel }

func testPlugin(t *testing.T, settings Settings) (*Plugin, *fakeSender) {
	t.Helper()
	sp, err := voice.New(slog.New(slog.DiscardHandler), voice.WithRand(func(int) int { return 0 }))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSender{}
	p := New(func(string) Sender { return f }, settings, sp, owner)
	p.log = slog.New(slog.DiscardHandler)
	return p, f
}

// /support posts in the channel for everyone, then for the cooldown answers
// only whoever asked, then posts again.
func TestSupportPostsPubliclyThenOnlyToTheAsker(t *testing.T) {
	p, _ := testPlugin(t, fakeSettings{})
	now := time.Date(2026, 10, 25, 21, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	ask := func() string {
		ix, wire := paneltest.IxCommand(t, "5", "support", nil)
		p.handleSupport(context.Background(), ix)
		if len(wire.Bodies) == 0 {
			t.Fatal("nothing sent")
		}
		return wire.Bodies[0]
	}
	first := ask()
	for _, want := range []string{"back the birds", "merlin_supporter_hero.png", "@" + owner, "https://cygnet.lol/support"} {
		if !strings.Contains(first, want) {
			t.Errorf("the advert lacks %q", want)
		}
	}
	if !strings.Contains(first, `"flags":32768`) {
		t.Fatalf("the first /support wasn't posted for everyone: %s", first)
	}
	if second := ask(); !strings.Contains(second, `"flags":32832`) {
		t.Fatalf("a repeat inside the cooldown was posted for everyone: %s", second)
	}
	now = now.Add(cooldown + time.Minute)
	if third := ask(); !strings.Contains(third, `"flags":32768`) {
		t.Fatalf("after the cooldown it stayed private: %s", third)
	}
}

func layoutText(m discord.MessageCreate) string {
	var b strings.Builder
	for _, c := range m.Components {
		fmt.Fprintf(&b, "%+v", c)
	}
	return b.String()
}

func TestNewSupportersAreThankedInTheirChannel(t *testing.T) {
	p, f := testPlugin(t, fakeSettings{channel: "300"})
	p.thank(context.Background(), "100", []string{"1", "2"})
	if len(f.sent) != 1 {
		t.Fatalf("%d cards, want one", len(f.sent))
	}
	m := f.sent[0]
	text := layoutText(m.data)
	if m.channel != "300" || m.data.Flags != discord.MessageFlagIsComponentsV2 || !strings.Contains(text, "<@1> and <@2>") || !strings.Contains(text, "thank you") {
		t.Fatalf("card in %s: %s", m.channel, text)
	}
	if !strings.Contains(text, "merlin_supporter.png") || strings.Contains(text, "merlin_supporter_hero.png") {
		t.Fatal("the welcome should wear her heart-in-beak face, not the banner")
	}
	var names []string
	for _, file := range m.data.Files {
		names = append(names, file.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "merlin_supporter.png") {
		t.Fatalf("the face isn't uploaded: %v", names)
	}
}

func TestABacklogIsStillOneCard(t *testing.T) {
	p, f := testPlugin(t, fakeSettings{channel: "300"})
	ids := make([]string, maxThanked+2)
	for i := range ids {
		ids[i] = fmt.Sprint(i + 1)
	}
	p.thank(context.Background(), "100", ids)
	if len(f.sent) != 1 || !strings.Contains(layoutText(f.sent[0].data), "and 2 more") {
		t.Fatalf("a backlog: %d cards", len(f.sent))
	}
}

func TestNoChannelOrSwitchedOffMeansNoCard(t *testing.T) {
	p, f := testPlugin(t, fakeSettings{})
	p.thank(context.Background(), "100", []string{"1"})
	off, g := testPlugin(t, fakeSettings{off: true, channel: "300"})
	off.handleJoined(context.Background(), core.Event{Type: core.EventSupportersJoined, GuildID: "100", Payload: core.SupportersJoinedPayload{UserIDs: []string{"1"}}})
	if len(f.sent) != 0 || len(g.sent) != 0 {
		t.Fatalf("cards with no channel (%d) or the plugin off (%d)", len(f.sent), len(g.sent))
	}
}

// The bus hands the card to a goroutine; it still lands.
func TestTheBusEventPostsTheCard(t *testing.T) {
	p, f := testPlugin(t, fakeSettings{channel: "300"})
	p.handleJoined(context.Background(), core.Event{Type: core.EventSupportersJoined, GuildID: "100", Payload: core.SupportersJoinedPayload{UserIDs: []string{"1"}}})
	for range 100 {
		f.mu.Lock()
		n := len(f.sent)
		f.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the bus event posted nothing")
}

func TestHelpSaysWhatItDoes(t *testing.T) {
	p, _ := testPlugin(t, fakeSettings{})
	if h := p.Help(); h.Title == "" || !strings.Contains(h.About, "/support") {
		t.Fatalf("help %+v", h)
	}
}
