package core_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A command tree using everything merlin's plugins use registers in one
// request carrying all of it: groups, subcommands, choices, channel types,
// bounds, autocomplete and the default permission.
func TestRegisterGuildSendsTheWholeTree(t *testing.T) {
	wire := &puts{}
	rt := core.NewCommandRouter(core.NewPermissions(nil, nil, ""), openGate{}, quiet()).
		WithRest(core.NewRest("test", &http.Client{Transport: wire}))
	admin := int64(discordgo.PermissionManageGuild)
	min, max := 1.0, 30.0
	rt.RegisterCommand("x", &discordgo.ApplicationCommand{Name: "x", Description: "d", DefaultMemberPermissions: &admin,
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "g", Description: "d", Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "s", Description: "d", Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionChannel, Name: "where", Description: "d", Required: true,
						ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText}},
					{Type: discordgo.ApplicationCommandOptionInteger, Name: "days", Description: "d", MinValue: &min, MaxValue: max},
					{Type: discordgo.ApplicationCommandOptionString, Name: "kind", Description: "d", Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "a", Value: "a"}, {Name: "b", Value: "b"}}},
					{Type: discordgo.ApplicationCommandOptionString, Name: "who", Description: "d", Autocomplete: true},
				}},
			}},
		}})
	rt.HandleIx("x", "g/s", core.PermSpec{Tier: core.TierPublic}, func(context.Context, *core.Ix) {})
	if err := rt.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := rt.RegisterGuild(2, 100); err != nil {
		t.Fatal(err)
	}
	body := wire.body
	for _, want := range []string{`"default_member_permissions":"32"`, `"channel_types":[0]`, `"min_value":1`, `"max_value":30`,
		`"choices":[{"name":"a","value":"a"}`, `"autocomplete":true`, `"name":"g"`, `"name":"s"`} {
		if !strings.Contains(body, want) {
			t.Errorf("registration is missing %s:\n%s", want, body)
		}
	}
}

// A handler that has not answered by the end of Discord's window has its
// context cancelled, so its calls fail while it can still say why.
func TestAnUnansweredHandlerIsCutAtTheWindow(t *testing.T) {
	rt, _ := router()
	done := make(chan time.Duration, 1)
	rt.RegisterCommand("slow", &discordgo.ApplicationCommand{Name: "slow", Description: "d"})
	rt.HandleIx("slow", "", core.PermSpec{Tier: core.TierPublic}, func(ctx context.Context, ix *core.Ix) {
		start := time.Now()
		<-ctx.Done()
		done <- time.Since(start)
	})
	if err := rt.Finalize(); err != nil {
		t.Fatal(err)
	}
	// Created 2.3s ago: 200ms of the window left.
	rt.HandleRaw(commandAt(t, "slow", time.Now().Add(-2300*time.Millisecond)))
	if d := <-done; d > time.Second {
		t.Fatalf("cancelled after %s, want at the window", d)
	}
}

// A handler that has deferred keeps working past the window.
func TestAnAnsweredHandlerOutlivesTheWindow(t *testing.T) {
	rt, _ := router()
	errs := make(chan error, 1)
	rt.RegisterCommand("work", &discordgo.ApplicationCommand{Name: "work", Description: "d"})
	rt.HandleIx("work", "", core.PermSpec{Tier: core.TierPublic}, func(ctx context.Context, ix *core.Ix) {
		_ = ix.Defer()
		time.Sleep(300 * time.Millisecond)
		errs <- ctx.Err()
	})
	if err := rt.Finalize(); err != nil {
		t.Fatal(err)
	}
	rt.HandleRaw(commandAt(t, "work", time.Now().Add(-2400*time.Millisecond)))
	if err := <-errs; err != nil {
		t.Fatalf("a deferred handler lost its context at the window: %v", err)
	}
}

// commandAt is a /name interaction Discord created at created.
func commandAt(t *testing.T, name string, created time.Time) []byte {
	t.Helper()
	id := snowflake.New(created)
	return raw(t, map[string]any{
		"id": strconv.FormatUint(uint64(id), 10), "application_id": "2", "type": 2, "token": "t", "version": 1,
		"guild_id": "100", "channel": map[string]any{"id": "101", "type": 0},
		"member": map[string]any{"user": map[string]any{"id": "11", "username": "m"}, "permissions": "0", "roles": []any{}, "joined_at": "2026-01-01T00:00:00Z"},
		"data":   map[string]any{"id": "6", "name": name, "type": 1},
	})
}

// puts records the last request body and answers a command list.
type puts struct{ body string }

func (p *puts) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	p.body = string(b)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]")),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}
