// Package help is /help, merlin's field guide: an index of every plugin that
// has a page, and a page per plugin, in one panel turned with a select.
//
// The guide holds no state. The custom ID and the value picked are the whole
// of it, so any guide merlin ever sent keeps working across restarts. Picking
// a page on a private guide turns it in place; a guide an admin posted to
// the channel stays as it is, and whoever clicked gets their own copy of the
// page, so one member's click doesn't change it for everyone else.
package help

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
	"github.com/6586x57890143/merlin/internal/fleet"
)

const (
	viewPrefix   = "help:view:"
	switchPrefix = "help:switch:"
	source       = "https://github.com/6586x57890143/merlin"
)

// switchSpec is the plugin toggle's own spec, config.mutate at TierAdmin,
// so the switch here and the plugins page on /config are the same lever
// behind the same lock.
var switchSpec = core.PermSpec{Tier: core.TierAdmin, Action: "config.mutate"}

// fixed can't be switched off: without adminconfig nobody could switch
// anything back on, and without help nobody could find the switch.
var fixed = map[string]bool{"adminconfig": true, "help": true}

// Toggles is the slice of settings the switch needs.
type Toggles interface {
	PluginEnabled(guildID, pluginName string) bool
	EnablePlugin(ctx context.Context, guildID, pluginName string) error
	DisablePlugin(ctx context.Context, guildID, pluginName string) error
}

type Plugin struct {
	plugins  func() []core.Plugin
	toggles  Toggles
	commands *core.CommandRouter
	audit    core.AuditWriter
	log      *slog.Logger
}

// New takes the registered plugins, read when the guide is drawn, and the
// per-guild switches.
func New(plugins func() []core.Plugin, toggles Toggles) *Plugin {
	return &Plugin{plugins: plugins, toggles: toggles}
}

func (p *Plugin) Name() string { return "help" }

func (p *Plugin) Help() core.Help {
	return core.Help{
		Title: "Help",
		Line:  "this guide, one page per thing i do",
		About: "pick a page from the menu to see what each part of me does and the commands it answers to. " +
			"`mod` and `admin` beside a command say who may run it here; anything unmarked is for everyone. " +
			"admins can post the guide in a channel with `/help post:true`, and switch a whole part off for this server from its page.",
	}
}

func (p *Plugin) Init(deps core.Deps) error {
	p.commands, p.audit, p.log = deps.Commands, deps.Audit, deps.Logger
	p.commands.RegisterCommand(p.Name(), &discordgo.ApplicationCommand{
		Name:        "help",
		Description: "What merlin does here, and every command she answers to",
		Options: []*discordgo.ApplicationCommandOption{{
			Type: discordgo.ApplicationCommandOptionBoolean, Name: "post",
			Description: "Admins: post it in the channel for everyone",
		}},
	})
	p.commands.HandleIx("help", "", core.PermSpec{Tier: core.TierPublic}, p.handleHelp)
	p.commands.HandleComponentIx(p.Name(), viewPrefix, core.PermSpec{Tier: core.TierPublic}, p.handleView)
	p.commands.HandleComponentIx(p.Name(), switchPrefix, switchSpec, p.handleSwitch)
	return nil
}

func (p *Plugin) Start(context.Context) error    { return nil }
func (p *Plugin) Shutdown(context.Context) error { return nil }

func (p *Plugin) handleHelp(ctx context.Context, ix *core.Ix) {
	post := false
	if o, ok := ix.Args()["post"]; ok {
		post = o.Bool()
	}
	admin := p.commands.Perms().AllowsIx(ix, switchSpec)
	if post && !admin {
		ix.RespondErr("Not yours to post", fmt.Errorf("only this server's admins can post the guide; `/help` on its own shows it to you"))
		return
	}
	// A posted guide is read by everyone, so it lists only what's on.
	pn := p.index(ix.GuildID, admin && !post)
	var err error
	if post {
		err = ix.RespondPanelPublic(pn)
	} else {
		err = ix.RespondPanel(pn)
	}
	if err != nil {
		p.log.Error("help: respond", "err", err)
	}
}

// handleView is the page select (help:view:page) and the index button
// (help:view:index).
func (p *Plugin) handleView(ctx context.Context, ix *core.Ix, customID string) {
	name := ""
	if strings.TrimPrefix(customID, viewPrefix) == "page" {
		name = ix.FirstValue()
	}
	p.show(ix, name, "")
}

// show draws the index (name "") or one plugin's page on the guide that was
// clicked: in place on a private one, as a private copy of a posted one.
func (p *Plugin) show(ix *core.Ix, name, note string) {
	admin := p.commands.Perms().AllowsIx(ix, switchSpec)
	pn, ok := p.page(ix.GuildID, name, admin)
	if name == "" || !ok {
		pn = p.index(ix.GuildID, admin)
		if name != "" {
			pn.Note = "✗ That page isn't here anymore."
		}
	}
	if note != "" {
		pn.Note = note
	}
	var err error
	if !ix.OnPrivateMessage() {
		err = ix.RespondPanel(pn)
	} else {
		err = ix.UpdatePanel(pn)
	}
	if err != nil {
		p.log.Error("help: show", "page", name, "err", err)
	}
}

type entry struct {
	name string
	help core.Help
}

// entries is every plugin with a page that the viewer may see: one switched
// off in this guild shows only to its admins, who can switch it back on.
func (p *Plugin) entries(guildID string, admin bool) []entry {
	var out []entry
	for _, pl := range p.plugins() {
		h, ok := pl.(core.Helper)
		if !ok || (!admin && !p.toggles.PluginEnabled(guildID, pl.Name())) {
			continue
		}
		out = append(out, entry{pl.Name(), h.Help()})
	}
	return out
}

// index is compact: the avatar beside the title, then every plugin as its
// icon and name on a short line that never wraps, with what it does as
// subtext below, which can wrap on a phone without dragging anything out of
// line.
func (p *Plugin) index(guildID string, admin bool) core.Panel {
	es := p.entries(guildID, admin)
	var list strings.Builder
	commands := 0
	for _, e := range es {
		commands += len(p.commands.CommandsOf(e.name))
		mark := core.PluginEmoji(e.name)
		if mark != "" {
			mark += " "
		}
		fmt.Fprintf(&list, "%s**%s**", mark, e.help.Title)
		if !p.toggles.PluginEnabled(guildID, e.name) {
			list.WriteString(" · `off here`")
		}
		fmt.Fprintf(&list, "\n-# %s\n", e.help.Line)
	}
	url, file := core.AvatarUpload()
	return core.Panel{
		Color: core.ColorPrimary, Title: "merlin", Line: "a merlin falcon, perched in this server",
		Icon: url, IconUpload: file,
		Parts: []discord.ContainerSubComponent{
			ui.Text(strings.TrimSuffix(list.String(), "\n")),
			ui.Text(fmt.Sprintf("-# %d parts · %d commands · build `%s` · nothing i post pings anyone", len(es), commands, fleet.Build())),
			ui.Row(menu(es, "")),
			ui.Row(discord.ButtonComponent{Label: "Source", Style: discord.ButtonStyleLink, URL: source}),
		},
	}
}

// page is one plugin's page, or false when it has none or is hidden from
// the viewer. The about goes below the icon at full width, because beside a
// thumbnail a phone gives text a narrow column; an admin also gets the
// plugin's switch.
func (p *Plugin) page(guildID, name string, admin bool) (core.Panel, bool) {
	es := p.entries(guildID, admin)
	for _, e := range es {
		if e.name != name {
			continue
		}
		url, file := core.PluginUpload(name)
		pn := core.Panel{Color: core.ColorInfo, Title: e.help.Title, Line: e.help.Line, Icon: url, IconUpload: file,
			Parts: []discord.ContainerSubComponent{ui.Text(e.help.About)}}
		if g := p.grid(name); g != "" {
			pn.Parts = append(pn.Parts, ui.Text(g))
		} else {
			pn.Parts = append(pn.Parts, ui.Text("-# no commands: it works on its own"))
		}
		if admin && !fixed[name] {
			state, b := "**on** in this server", ui.Button("Switch off", switchPrefix+"off:"+name, discord.ButtonStyleDanger, false)
			if !p.toggles.PluginEnabled(guildID, name) {
				state, b = "**off** in this server", ui.Button("Switch on", switchPrefix+"on:"+name, discord.ButtonStyleSuccess, false)
			}
			pn.Parts = append(pn.Parts, ui.Gap(), ui.Switch(state+"\n-# off answers nobody here and stops its scheduled work; other servers keep it", b))
		}
		pn.Parts = append(pn.Parts, ui.Gap(), ui.Row(menu(es, name)),
			ui.Row(ui.Button("◂ Index", viewPrefix+"index", discord.ButtonStyleSecondary, false)))
		return pn, true
	}
	return core.Panel{}, false
}

func menu(es []entry, current string) discord.StringSelectMenuComponent {
	opts := make([]discord.StringSelectMenuOption, 0, len(es))
	for _, e := range es {
		opts = append(opts, discord.StringSelectMenuOption{
			Label: e.help.Title, Value: e.name, Description: clip(e.help.Line, 100),
			Emoji: core.PluginOptionEmoji(e.name), Default: e.name == current,
		})
	}
	return ui.StringSelect(viewPrefix+"page", "Pick a page", opts, false, false)
}

// handleSwitch turns a plugin on or off for this guild, behind the same lock
// as /config's plugins page and audited the same way. The name came from the
// member's client, so it is checked against what is really registered.
func (p *Plugin) handleSwitch(ctx context.Context, ix *core.Ix, customID string) {
	verb, name, _ := strings.Cut(strings.TrimPrefix(customID, switchPrefix), ":")
	known := false
	for _, pl := range p.plugins() {
		known = known || pl.Name() == name
	}
	note := ""
	switch {
	case !known || fixed[name]:
		note = "✗ That can't be switched here."
	case verb == "on":
		if err := p.toggles.EnablePlugin(ctx, ix.GuildID, name); err != nil {
			note = "✗ Couldn't switch it on: " + err.Error()
			break
		}
		p.record(ctx, ix, "config.plugin_enabled", name)
		note = "✓ On here again."
	default:
		if err := p.toggles.DisablePlugin(ctx, ix.GuildID, name); err != nil {
			note = "✗ Couldn't switch it off: " + err.Error()
			break
		}
		p.record(ctx, ix, "config.plugin_disabled", name)
		note = "✓ Off here. Nobody, admins included, can use it until it's switched back on."
	}
	p.show(ix, name, note)
}

func (p *Plugin) record(ctx context.Context, ix *core.Ix, action, name string) {
	actor := ""
	if ix.Member() != nil {
		actor = ix.UserID
	}
	if err := p.audit.Record(ctx, ix.GuildID, actor, action, "", name); err != nil {
		p.log.Error("help: audit switch", "err", err)
	}
}

// grid is a plugin's commands as a code block: each command, its
// subcommands branching below it, and who may run each when that isn't
// everyone. Kept narrow, since a phone fits about 32 columns of code block.
func (p *Plugin) grid(plugin string) string {
	var rows [][2]string
	for _, cmd := range p.commands.CommandsOf(plugin) {
		var leaves []string
		for _, o := range cmd.Options {
			switch o.Type {
			case discordgo.ApplicationCommandOptionSubCommand:
				leaves = append(leaves, o.Name)
			case discordgo.ApplicationCommandOptionSubCommandGroup:
				for _, sub := range o.Options {
					leaves = append(leaves, o.Name+" "+sub.Name)
				}
			}
		}
		if len(leaves) == 0 {
			rows = append(rows, [2]string{"/" + cmd.Name, tierWord(p.commands.LeafTier(cmd.Name, ""))})
			continue
		}
		rows = append(rows, [2]string{"/" + cmd.Name, ""})
		for n, leaf := range leaves {
			branch := "├ "
			if n == len(leaves)-1 {
				branch = "└ "
			}
			rows = append(rows, [2]string{branch + leaf, tierWord(p.commands.LeafTier(cmd.Name, strings.ReplaceAll(leaf, " ", "/")))})
		}
	}
	if len(rows) == 0 {
		return ""
	}
	width := 0
	for _, r := range rows {
		width = max(width, utf8.RuneCountInString(r[0]))
	}
	var b strings.Builder
	b.WriteString("```\n")
	for _, r := range rows {
		if r[1] == "" {
			b.WriteString(r[0] + "\n")
			continue
		}
		b.WriteString(r[0] + strings.Repeat(" ", width+2-utf8.RuneCountInString(r[0])) + r[1] + "\n")
	}
	b.WriteString("```")
	return b.String()
}

func tierWord(t core.PermTier) string {
	switch t {
	case core.TierMod:
		return "mod"
	case core.TierAdmin:
		return "admin"
	}
	return ""
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
