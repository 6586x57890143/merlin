package statistics

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
)

// /statistics status is a panel (core.Panel): what is being counted, since
// when, and for how long, with the one setting there is beside it. It
// replaced /statistics configure retention, a whole subcommand group for a
// single number.
const (
	panelSet = "statistics:panel:set:"
	formSet  = "statistics:form:set:"
)

var configureSpec = core.PermSpec{Tier: core.TierAdmin, Action: "statistics.configure"}

func (p *Plugin) registerPanel(commands *core.CommandRouter) {
	p.commands = commands
	commands.HandleComponentIx(p.Name(), panelSet, configureSpec, p.handlePanelSet)
	commands.HandleModalIx(p.Name(), formSet, configureSpec, p.handleForm)
}

func (p *Plugin) handleStatus(ctx context.Context, ix *core.Ix) {
	if err := ix.RespondPanel(p.panel(ctx, ix, "")); err != nil {
		p.log.Error("statistics: panel response failed", "err", err)
	}
}

func (p *Plugin) panel(ctx context.Context, ix *core.Ix, note string) core.Panel {
	pn := core.Panel{Color: core.ColorInfo, Title: "Statistics", Plugin: p.Name(), Note: note,
		Line:   "who was talking where, counted by the hour",
		Footer: "/statistics channels and members read it; only merlin's operator can name people."}
	cfg, err := p.store.Config(ctx, ix.GuildID)
	if err != nil {
		pn.Color = core.ColorError
		pn.Parts = []discord.ContainerSubComponent{ui.Text("✗ could not read the statistics: " + err.Error())}
		return pn
	}
	oldest, _ := p.store.OldestHour(ctx, ix.GuildID)
	back, _ := p.store.BackfillStatus(ctx, ix.GuildID)

	rows := [][2]string{{"counting", "not started yet"}}
	if !cfg.LiveSince.IsZero() {
		rows[0][1] = "since " + cfg.LiveSince.Format("2006-01-02 15:04") + " utc"
	}
	if !oldest.IsZero() {
		rows = append(rows, [2]string{"oldest hour", oldest.Format("2006-01-02 15:04") + " utc"})
	}
	rows = append(rows, [2]string{"kept", fmt.Sprintf("%d days", cfg.RetentionDays)})
	switch {
	case back.Pending > 0:
		rows = append(rows, [2]string{"backfill", fmt.Sprintf("%d to read, %d done", back.Pending, back.Done)})
	case back.Done > 0 || back.Failed > 0:
		done := fmt.Sprintf("done, %d channels", back.Done)
		if back.Failed > 0 {
			done += fmt.Sprintf(", %d unreadable", back.Failed)
		}
		rows = append(rows, [2]string{"backfill", done})
	}
	pn.Parts = []discord.ContainerSubComponent{
		ui.Text(core.Readout(rows)),
		ui.Switch(fmt.Sprintf("hourly counts are kept for **%d** days, then pruned within the hour.", cfg.RetentionDays),
			ui.Button("Change", panelSet+"retention", discord.ButtonStyleSecondary, !p.commands.Perms().AllowsIx(ix, configureSpec))),
	}
	return pn
}

func (p *Plugin) handlePanelSet(ctx context.Context, ix *core.Ix, customID string) {
	if strings.TrimPrefix(customID, panelSet) != "retention" {
		if err := ix.UpdatePanel(p.panel(ctx, ix, "✗ that control is out of date; run /statistics status again")); err != nil {
			p.log.Error("statistics: panel update failed", "err", err)
		}
		return
	}
	cfg, _ := p.store.Config(ctx, ix.GuildID)
	if err := ix.OpenModal(formSet+"retention", "Retention",
		ui.Field("days", fmt.Sprintf("days of hourly counts to keep, 1 to %d", maxRetentionDays), strconv.Itoa(cfg.RetentionDays), strconv.Itoa(defaultRetentionDays), false, true)); err != nil {
		p.log.Error("statistics: open form failed", "err", err)
	}
}

func (p *Plugin) handleForm(ctx context.Context, ix *core.Ix, _ string) {
	days, err := strconv.Atoi(strings.TrimSpace(ix.ModalValues()["days"]))
	note := p.setRetention(ctx, ix.GuildID, days)
	if err != nil {
		note = fmt.Sprintf("✗ retention is a whole number of days, 1 to %d", maxRetentionDays)
	}
	if err := ix.UpdatePanel(p.panel(ctx, ix, note)); err != nil {
		p.log.Error("statistics: panel update failed", "err", err)
	}
}

func (p *Plugin) setRetention(ctx context.Context, guildID string, days int) string {
	if days < 1 || days > maxRetentionDays {
		return fmt.Sprintf("✗ retention must be between 1 and %d days", maxRetentionDays)
	}
	if err := p.store.SetRetention(ctx, guildID, days); err != nil {
		return "✗ could not save: " + err.Error()
	}
	return fmt.Sprintf("✓ hourly counts are kept for %d days. anything older goes on the next prune, within the hour.", days)
}

// Help is this plugin's page in /help.
func (p *Plugin) Help() core.Help {
	return core.Help{
		Title: "Statistics",
		Line:  "who was talking where, counted by the hour",
		About: "i count messages per channel and per member by the hour, nothing more, and keep the counts as long as this server says. `/statistics channels` and `/statistics members` read them back. admins only, and naming people needs my operator.",
	}
}
