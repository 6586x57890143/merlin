package core

import (
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
)

// A handler still working as the window closes is deferred for, and what it
// answers afterwards still reaches the member: the redraw edits the panel it
// came from, a new message from a click is a follow-up rather than an
// overwrite of that panel, and a command's answer replaces "thinking".
func TestAutoDeferDeliversTheLateAnswer(t *testing.T) {
	pn := Panel{Color: ColorInfo, Title: "t", Parts: []discord.ContainerSubComponent{discord.TextDisplayComponent{Content: "x"}}}
	click := map[string]any{"custom_id": "c", "component_type": 2}
	command := map[string]any{"id": "6", "name": "x", "type": 1}
	for _, c := range []struct {
		name   string
		typ    int
		data   map[string]any
		answer func(*Ix) error
		defer_ string
		then   string
	}{
		{"click redraw", 3, click, func(ix *Ix) error { return ix.UpdatePanel(pn) }, `"type":6`, "PATCH /api/v10/webhooks/2/t/messages/@original"},
		{"click new message", 3, click, func(ix *Ix) error { return ix.RespondEmbed(Embed(ColorInfo, "t", "d")) }, `"type":6`, "POST /api/v10/webhooks/2/t"},
		{"command", 2, command, func(ix *Ix) error { return ix.RespondPanel(pn) }, `"type":5`, "PATCH /api/v10/webhooks/2/t/messages/@original"},
		{"handler deferred too", 2, command, (*Ix).Defer, `"type":5`, ""},
	} {
		ix, cap := answering(t, c.typ, c.data)
		ix.autoDefer()
		if err := c.answer(ix); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.Contains(cap.bodies[0], c.defer_) {
			t.Errorf("%s: deferred with %s", c.name, cap.bodies[0])
		}
		want := 2
		if c.then == "" {
			want = 1
		}
		if len(cap.calls) != want || (c.then != "" && cap.calls[1] != c.then) {
			t.Errorf("%s: calls %v, want the answer as %q", c.name, cap.calls, c.then)
		}
	}
}

// A handler that answered in time is left alone, and a form cannot be
// opened once merlin has deferred, so that says so rather than vanishing.
func TestAutoDeferOnlyWhenNothingAnswered(t *testing.T) {
	ix, cap := answering(t, 3, map[string]any{"custom_id": "c", "component_type": 2})
	if err := ix.DeferUpdate(); err != nil {
		t.Fatal(err)
	}
	ix.autoDefer()
	if len(cap.calls) != 1 {
		t.Errorf("deferred over an answer: %v", cap.calls)
	}

	late, _ := answering(t, 3, map[string]any{"custom_id": "c", "component_type": 2})
	late.autoDefer()
	if err := late.OpenModal("f", "t"); err == nil {
		t.Error("a form opened after the defer reported success")
	}
}

func TestUntilAutoDefer(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		age  time.Duration
		want time.Duration
	}{
		{500 * time.Millisecond, 1500 * time.Millisecond},
		{2500 * time.Millisecond, -500 * time.Millisecond},
		{-time.Second, autoDeferAfter}, // this host's clock is behind
		{time.Hour, autoDeferAfter},    // or ahead
	} {
		if got := untilAutoDefer(now.Add(-c.age), now); got != c.want {
			t.Errorf("age %v: wait %v, want %v", c.age, got, c.want)
		}
	}
}
