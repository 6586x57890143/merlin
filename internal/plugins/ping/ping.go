// Package ping is the reference plugin for Milestone 0: a trivial command
// wired through the real Plugin lifecycle, so it's the concrete shape a
// future plugin (scheduler, rotation, ...) drops into.
package ping

import (
	"context"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

type Plugin struct {
	session *discordgo.Session
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "ping" }

func (p *Plugin) Init(deps core.Deps) error {
	p.session = deps.Session
	deps.Session.AddHandler(p.handleInteraction)
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	// /ping is intentionally public — no DefaultMemberPermissions is set,
	// unlike every other command which must go through core.RegisterCommands
	// and fail closed (spec.MD §4 layer 4).
	cmd := &discordgo.ApplicationCommand{
		Name:        "ping",
		Description: "Health check - replies pong",
	}
	_, err := p.session.ApplicationCommandCreate(p.session.State.User.ID, "", cmd)
	return err
}

func (p *Plugin) Shutdown(ctx context.Context) error { return nil }

func (p *Plugin) handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand || i.ApplicationCommandData().Name != "ping" {
		return
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: "pong"},
	})
}
