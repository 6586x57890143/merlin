// Package ping is the reference plugin: a trivial command wired through the
// real Plugin lifecycle and the shared CommandRouter (spec.MD §4a), so it's
// the concrete shape a future plugin drops into.
package ping

import (
	"context"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

type Plugin struct{}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "ping" }

func (p *Plugin) Init(deps core.Deps) error {
	deps.Commands.RegisterCommand(p.Name(), &discordgo.ApplicationCommand{
		Name:        "ping",
		Description: "Health check - replies pong",
	})
	// /ping is intentionally public — TierPublic, the only tier that never
	// requires an Action, unlike every other command (spec.MD §4a).
	deps.Commands.Handle("ping", "", core.PermSpec{Tier: core.TierPublic}, handlePing)
	return nil
}

func (p *Plugin) Start(ctx context.Context) error { return nil }

func (p *Plugin) Shutdown(ctx context.Context) error { return nil }

func handlePing(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: "kik-ong!"},
	})
}
