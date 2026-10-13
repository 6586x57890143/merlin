package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"

	"github.com/6586x57890143/merlin/internal/core"
)

// portal answers GetCurrentApplication with whatever flags it holds, or an
// error when it has none.
type portal struct {
	rest.Rest
	mu    sync.Mutex
	flags []discord.ApplicationFlags
}

func (p *portal) GetCurrentApplication(...rest.RequestOpt) (*discord.Application, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.flags) == 0 {
		return nil, errors.New("api down")
	}
	f := p.flags[0]
	if len(p.flags) > 1 {
		p.flags = p.flags[1:]
	}
	return &discord.Application{Flags: f}, nil
}

// An unchanged portal and a failed probe keep merlin running; a toggle
// switched since boot stops her so she comes back identifying with it.
func TestWatchIntentsRestartsOnlyOnAChange(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	granted := core.Intents{Members: true}
	members := discord.ApplicationFlags(discord.ApplicationFlagGatewayGuildMemberLimited)

	p := &portal{}
	ctx, cancel := context.WithCancel(context.Background())
	changed := make(chan struct{})
	go watchIntents(ctx, p, granted, time.Millisecond, changed, log)
	time.Sleep(20 * time.Millisecond) // failing probes
	p.mu.Lock()
	p.flags = []discord.ApplicationFlags{members}
	p.mu.Unlock()
	select {
	case <-changed:
		t.Fatal("an unchanged portal, or a failed probe, restarted merlin")
	case <-time.After(30 * time.Millisecond):
	}
	cancel()

	p.mu.Lock()
	p.flags = []discord.ApplicationFlags{members, members | discord.ApplicationFlagGatewayMessageContentLimited}
	p.mu.Unlock()
	changed = make(chan struct{})
	go watchIntents(context.Background(), p, granted, time.Millisecond, changed, log)
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("a toggle switched on in the portal did not restart merlin")
	}
}
