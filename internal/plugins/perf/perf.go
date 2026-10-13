// Package perf is /perf: where merlin spends her time, plugin by plugin and
// stage by stage, from the Go runtime up to Discord's API, read off what
// internal/obs records and laid out as a code-block grid, slowest first,
// each row with a bar scaled to the slowest so the outlier shows at a
// glance. A panel with a refresh button, the same layout as everything else.
//
// The numbers are the whole process's, every server merlin is in, so they
// say how busy other servers keep her. That makes it the operator's alone:
// TierAdmin is the floor and the bootstrap identity is the gate, exactly as
// /statistics report does, and the command is hidden from members' pickers.
package perf

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
	"github.com/6586x57890143/merlin/internal/fleet"
	"github.com/6586x57890143/merlin/internal/obs"
)

const refreshID = "perf:panel:refresh"

var spec = core.PermSpec{Tier: core.TierAdmin, Action: "perf.read"}

// top is how many stages the readout shows.
const top = 12

type Plugin struct {
	rec      *obs.Recorder
	runtime  func() (sched, gc time.Duration)
	commands *core.CommandRouter
	perms    *core.Permissions
}

func New(rec *obs.Recorder) *Plugin { return &Plugin{rec: rec, runtime: obs.Runtime} }

func (p *Plugin) Name() string { return "perf" }

func (p *Plugin) Help() core.Help {
	return core.Help{
		Title: "Perf",
		Line:  "where my time goes, slowest first",
		About: "`/perf` lists where my time goes in each part of me, slowest first. every call is split up: in is the trip " +
			"from discord's gateway to me, run is my own work (a command, a button or a scheduled job), http is a request " +
			"to discord and wait is how long discord told me to hold off. the go runtime's own overhead is underneath. " +
			"it covers every server i'm in, so only my operator can run it.",
	}
}

func (p *Plugin) Init(deps core.Deps) error {
	p.commands, p.perms = deps.Commands, deps.Perms
	adminOnly := int64(0)
	p.commands.RegisterCommand(p.Name(), &discordgo.ApplicationCommand{
		Name:        "perf",
		Description: "Where merlin spends her time, slowest first (operator only)",
		// Hidden from members in the picker; the operator check is the gate.
		DefaultMemberPermissions: &adminOnly,
	})
	p.commands.HandleIx("perf", "", spec, p.handlePerf)
	p.commands.HandleComponentIx(p.Name(), refreshID, spec, p.handleRefresh)
	return nil
}

func (p *Plugin) Start(context.Context) error    { return nil }
func (p *Plugin) Shutdown(context.Context) error { return nil }

func (p *Plugin) operator(ix *core.Ix) bool {
	return p.perms != nil && ix.Member() != nil && p.perms.IsBootstrapAdmin(ix.UserID)
}

func (p *Plugin) handlePerf(ctx context.Context, ix *core.Ix) {
	if !p.operator(ix) {
		ix.RespondErr("Operator only", fmt.Errorf("these numbers cover every server merlin is in, so only her operator can read them"))
		return
	}
	_ = ix.RespondPanel(p.panel())
}

func (p *Plugin) handleRefresh(ctx context.Context, ix *core.Ix, _ string) {
	if !p.operator(ix) {
		ix.RespondErr("Operator only", fmt.Errorf("these numbers cover every server merlin is in"))
		return
	}
	_ = ix.UpdatePanel(p.panel())
}

func (p *Plugin) panel() core.Panel {
	return core.Panel{
		Color: core.ColorInfo, Title: "Perf", Plugin: p.Name(), Line: "since boot, every server",
		Parts: []discord.ContainerSubComponent{
			ui.Text(p.readout()),
			ui.Row(ui.Button("Refresh", refreshID, discord.ButtonStyleSecondary, false)),
		},
		Footer: "in: gateway to dispatch · run: handler or job · http: round trip · wait: a 429's hold · build " + fleet.Build(),
	}
}

// The grid's columns: label, bar, p99, p50, count. 19+2+7+7+5 is 40, the
// widest a code block line should be. A value is at most 6 wide, so its
// column keeps a gap, and the label gets the room a route's name needs.
const labelW, barW = 19, 2

func (p *Plugin) readout() string {
	rows := p.rec.Rows()
	var b strings.Builder
	b.WriteString("```\n")
	fmt.Fprintf(&b, "%-*s%7s%7s%5s\n", labelW+barW, "", "p99", "p50", "n")
	if len(rows) == 0 {
		b.WriteString("nothing measured yet\n")
	}
	var slowest time.Duration
	if len(rows) > 0 {
		slowest = rows[0].P99
	}
	for _, r := range rows[:min(top, len(rows))] {
		label := clip(r.Who, labelW-2-len(r.Stage.String())) + " " + r.Stage.String()
		fmt.Fprintf(&b, "%-*s%-*s%7s%7s%5s\n", labelW, label, barW, bar(r.P99, slowest, barW-1), dur(r.P99), dur(r.P50), count(r.N))
	}
	sched, gc := p.runtime()
	fmt.Fprintf(&b, "%s\n%-*s%7s\n%-*s%7s\n```", strings.Repeat("·", 40), labelW+barW, "go sched", dur(sched), labelW+barW, "go gc pause", dur(gc))
	return b.String()
}

// bar is d against max as up to width cells of eighths, so two stages a
// few percent apart still look apart.
func bar(d, max time.Duration, width int) string {
	if max <= 0 {
		return ""
	}
	eighths := int(int64(d) * int64(width*8) / int64(max))
	if eighths == 0 && d > 0 {
		eighths = 1
	}
	parts := []rune(" ▏▎▍▌▋▊▉")
	return strings.Repeat("█", eighths/8) + strings.TrimSpace(string(parts[eighths%8]))
}

// dur is a duration as a short number with its unit, down to microseconds.
// A value not measured yet says so rather than reading as zero.
func dur(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Millisecond:
		return fmt.Sprintf("%d µs", d.Microseconds())
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func count(n uint64) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%dk", n/1000)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
