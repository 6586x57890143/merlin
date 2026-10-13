package ping

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
)

func TestPingAnswersOnlyWhoAsked(t *testing.T) {
	ix, wire := paneltest.IxCommand(t, "11", "ping", nil)
	New(nil).handlePing(context.Background(), ix)
	body := wire.All()
	for _, want := range []string{"kik-ong!", `"flags":64`, `"parse":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("reply is missing %s: %s", want, body)
		}
	}
	if !wire.Answered() {
		t.Fatal("nothing answered the interaction")
	}
}

func TestPingIsWiredPublic(t *testing.T) {
	p := New(nil)
	r := core.NewCommandRouter(core.NewPermissions(nil, nil, ""), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := p.Init(core.Deps{Commands: r}); err != nil {
		t.Fatal(err)
	}
	if err := r.Finalize(); err != nil {
		t.Fatal(err)
	}
	if r.Tiers()["ping"] != core.TierPublic {
		t.Fatalf("tiers %v", r.Tiers())
	}
	if p.Name() != "ping" || p.Help().About == "" || p.Start(context.Background()) != nil || p.Shutdown(context.Background()) != nil {
		t.Fatal("lifecycle")
	}
}
