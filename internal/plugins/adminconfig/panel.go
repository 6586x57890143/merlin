package adminconfig

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
)

// /config is one panel. It used to be eighteen subcommands in four groups,
// each answering with its own embed, so changing two things meant two
// commands and reading what was set meant a third. Every control on it maps
// onto exactly what one of those subcommands did, with the same settings
// call, the same audit action and the same guard, so nothing about what may
// change or who is told moved; only where it is done from.
//
// Two prefixes, two specs. Moving around the panel reads (config.read,
// TierMod, as /config status and the *.list commands were); every control
// that changes something mutates (config.mutate, TierAdmin, as every add,
// remove and set was). The router checks those on every click, and the
// panel also draws the controls disabled for someone who would be refused,
// so a mod sees the whole configuration without being offered anything
// that would bounce.
const (
	panelView = "config:panel:view:"
	panelSet  = "config:panel:set:"
)

// The panel's pages, in the order its page select lists them.
var panelPages = []struct{ key, label, about string }{
	{"home", "Overview", "Is merlin okay here, at a glance"},
	{"access", "Admins and mod roles", "Who can change merlin, and who counts as a mod"},
	{"channels", "Channels", "Where the audit log and status alerts go"},
	{"plugins", "Plugins", "Switch a whole plugin on or off here"},
	{"permissions", "Permissions", "Retier one action, or allow or block someone"},
	{"safety", "Pause and dry-run", "The emergency stop and rehearsal mode"},
}

var (
	readSpec   = core.PermSpec{Tier: core.TierMod, Action: actionRead}
	mutateSpec = core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}
)

func (p *Plugin) registerPanel() {
	p.commands.HandleComponentIx(p.Name(), panelView, readSpec, p.handlePanelView)
	p.commands.HandleComponentIx(p.Name(), panelSet, mutateSpec, p.handlePanelSet)
}

func (p *Plugin) handleConfig(ctx context.Context, ix *core.Ix) {
	if err := ix.RespondPanel(p.panel(ctx, ix, "home", "", "")); err != nil {
		p.log.Error("adminconfig: panel response failed", "err", err)
	}
}

// handlePanelView turns pages. Its CustomIDs are config:panel:view:page
// (the page select), config:panel:view:area (permissions' plugin select) and
// config:panel:view:action (permissions' action select).
func (p *Plugin) handlePanelView(ctx context.Context, ix *core.Ix, customID string) {
	page, arg := "home", ""
	v := ix.FirstValue()
	switch strings.TrimPrefix(customID, panelView) {
	case "page":
		page = v
	case "area":
		page, arg = "permissions", v
	case "action":
		page, arg = "permissions", v
	}
	p.redraw(ctx, ix, page, arg, "")
}

func (p *Plugin) redraw(ctx context.Context, ix *core.Ix, page, arg, note string) {
	if err := ix.UpdatePanel(p.panel(ctx, ix, page, arg, note)); err != nil {
		p.log.Error("adminconfig: panel update failed", "page", page, "err", err)
	}
}

// panel draws one page. arg is the permissions page's area or action.
func (p *Plugin) panel(ctx context.Context, ix *core.Ix, page, arg, note string) core.Panel {
	can := p.perms.AllowsIx(ix, mutateSpec)
	gs := p.settings.GuildSettings(ix.GuildID)
	pn := core.Panel{Color: core.ColorInfo, Title: "Settings", Plugin: p.Name(), Note: note,
		Footer: "Mods can read this; only admins can change it. Every change is written to the audit log."}
	if !can {
		pn.Footer = "You can read this, but only admins can change it."
	}

	switch page {
	case "access":
		pn.Line = "Who can change merlin, and who counts as a mod"
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text("**Admins** can change merlin's own configuration. Anyone holding Discord's Administrator permission already counts, so this is for people who should have it without that.\n" +
				"-# Now: " + userListText(gs.AdminUserIDs)),
			ui.Row(ui.UserSelect(panelSet+"admins", "Pick who has standing admin access", gs.AdminUserIDs, core.MaxSelectValues, !can)),
			ui.Text("**Mod roles** can use mod-tier commands: jailing, releasing, running a rotation early.\n" +
				"-# Now: " + roleListText(gs.ModRoleIDs)),
			ui.Row(ui.RoleSelect(panelSet+"modroles", "Pick which roles count as mod", gs.ModRoleIDs, core.MaxSelectValues, !can)),
		}

	case "channels":
		audit, _ := p.channelState(gs.AuditLogChannelID)
		status, _ := p.channelState(gs.StatusChannelID)
		pn.Line = "Where the audit log and status alerts go"
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text("**Audit log**: config changes, rotations and moderation actions. Mod roles are given access.\n-# Now: " + audit),
			ui.Row(ui.ChannelSelect(panelSet+"audit", "Pick the audit log channel", gs.AuditLogChannelID, !can, discord.ChannelTypeGuildText)),
			ui.Text("**Status channel**: operational trouble, like a job failing over and over. Quiet when nothing is wrong.\n-# Now: " + status),
			ui.Row(ui.ChannelSelect(panelSet+"status", "Pick the status channel", gs.StatusChannelID, !can, discord.ChannelTypeGuildText)),
			ui.Row(discord.ButtonComponent{Label: "Guided setup", Style: discord.ButtonStyleSecondary, CustomID: panelSet + "setup", Disabled: !can}),
		}
		pn.Footer = "Guided setup can create either channel for you, locked to mods."

	case "plugins":
		names := p.commands.Plugins()
		sort.Strings(names)
		var rows [][2]string
		var opts []discord.StringSelectMenuOption
		for _, name := range names {
			on := p.settings.PluginEnabled(ix.GuildID, name)
			state := "on"
			if !on {
				state = "off"
			}
			rows = append(rows, [2]string{name, state})
			if name != p.Name() {
				opts = append(opts, discord.StringSelectMenuOption{Label: name, Value: name, Default: on})
			}
		}
		pn.Line = "Switch a whole plugin on or off here"
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text("A plugin that is off answers nobody here, admins included, and its scheduled work stops.\n" + core.Readout(rows)),
		}
		if len(opts) > 0 {
			pn.Parts = append(pn.Parts, ui.Row(ui.StringSelect(panelSet+"plugins", "Pick which plugins are on", opts, true, !can)))
		}
		pn.Footer = p.Name() + " runs this panel, so it stays on."

	case "permissions":
		pn.Line = "Retier one action, or allow or block someone"
		pn.Parts = p.permissionsBody(ix.GuildID, arg, can)

	case "safety":
		pn.Line = "The emergency stop and rehearsal mode"
		pauseState, pauseBtn := "Acting normally.", discord.ButtonComponent{Label: "Pause", Style: discord.ButtonStyleDanger, CustomID: panelSet + "pause:on", Disabled: !can}
		if gs.WritesPaused {
			pauseState, pauseBtn = "⛔ **Paused.** Scheduled jobs stay due and resume where they left off.", discord.ButtonComponent{Label: "Resume", Style: discord.ButtonStyleSuccess, CustomID: panelSet + "pause:off", Disabled: !can}
		}
		dryState, dryBtn := "Acting for real.", discord.ButtonComponent{Label: "Rehearse", Style: discord.ButtonStyleSecondary, CustomID: panelSet + "dryrun:on", Disabled: !can}
		if gs.WritesDryRun {
			dryState, dryBtn = "🧪 **Rehearsing.** Nothing is being changed.", discord.ButtonComponent{Label: "Act for real", Style: discord.ButtonStyleSuccess, CustomID: panelSet + "dryrun:off", Disabled: !can}
		}
		pn.Parts = []discord.ContainerSubComponent{
			ui.Switch("**Pause** refuses every rotation, archive deletion, jail and role change in this server. Reading keeps working.\n-# " + pauseState, pauseBtn),
			ui.Switch("**Dry-run** writes to the audit log what rotation, sweeps and jails *would* do, and does none of it.\n-# " + dryState, dryBtn),
		}
		if _, err := os.Stat(p.legacyPath); err == nil {
			pn.Parts = append(pn.Parts, ui.Switch("**Import** seeds this server's settings, once, from the legacy config file on disk.", discord.ButtonComponent{Label: "Import", Style: discord.ButtonStyleSecondary, CustomID: panelSet + "import", Disabled: !can}))
		}
		if gs.WritesPaused || gs.WritesDryRun {
			pn.Color, pn.Mood = core.ColorWarning, core.MoodIdle
		}

	default:
		page = "home"
		body, sev := p.statusReport(ctx, ix.GuildID)
		pn.Line = "This server's settings, and whether merlin is okay"
		pn.Parts = []discord.ContainerSubComponent{ui.Text(body)}
		switch sev {
		case sevError, sevWarn:
			pn.Color = core.ColorWarning
		case sevStopped:
			pn.Color, pn.Mood = core.ColorWarning, core.MoodIdle
		}
	}

	pn.Parts = append(pn.Parts, ui.Gap(), ui.Row(pageSelect(page)))
	return pn
}

func pageSelect(current string) discord.StringSelectMenuComponent {
	opts := make([]discord.StringSelectMenuOption, len(panelPages))
	for n, pg := range panelPages {
		opts[n] = discord.StringSelectMenuOption{Label: pg.label, Value: pg.key, Description: pg.about, Default: pg.key == current}
	}
	return ui.StringSelect(panelView+"page", "Go to...", opts, false, false)
}

// permissionsBody is the permissions page: pick a plugin, then an action,
// then that action's tier, extra allows and blocks. An area is the part of
// an action before its dot. Two selects rather than one because there are
// more actions than a select can hold.
func (p *Plugin) permissionsBody(guildID, arg string, can bool) []discord.ContainerSubComponent {
	actions := p.commands.Actions()
	sort.Strings(actions)
	area, action := arg, ""
	if a, _, ok := strings.Cut(arg, "."); ok {
		area, action = a, arg
	}

	var areas []string
	byArea := map[string][]string{}
	for _, a := range actions {
		ar, _, _ := strings.Cut(a, ".")
		if byArea[ar] == nil {
			areas = append(areas, ar)
		}
		byArea[ar] = append(byArea[ar], a)
	}
	var areaOpts []discord.StringSelectMenuOption
	for _, ar := range areas {
		areaOpts = append(areaOpts, discord.StringSelectMenuOption{Label: ar, Value: ar, Default: ar == area})
	}

	body := []discord.ContainerSubComponent{}
	if area == "" {
		lines := permissionOverrideLines(p.settings.Overrides(guildID))
		summary := "No action has been changed from its default here."
		if len(lines) > 0 {
			summary = core.TruncateEmbedField(strings.Join(lines, "\n"))
		}
		body = append(body, ui.Text("Every command runs as an **action**. Pick one to change who may run it.\n"+summary))
	}
	if len(areaOpts) > 0 {
		body = append(body, ui.Row(ui.StringSelect(panelView+"area", "Pick a plugin", areaOpts, false, false)))
	}
	if area == "" {
		return body
	}

	var actOpts []discord.StringSelectMenuOption
	for _, a := range byArea[area] {
		actOpts = append(actOpts, discord.StringSelectMenuOption{Label: a, Value: a, Default: a == action})
	}
	if len(actOpts) > 0 {
		body = append(body, ui.Row(ui.StringSelect(panelView+"action", "Pick an action", actOpts, false, false)))
	}
	if action == "" {
		return body
	}

	var tier core.PermTier
	var allowR, allowU, denyR, denyU []string
	for _, o := range p.settings.Overrides(guildID) {
		if o.Action == action {
			tier = o.RequiredTier
			allowR, allowU, denyR, denyU = o.RoleIDs, o.UserIDs, o.DenyRoleIDs, o.DenyUserIDs
		}
	}
	requires := "its default"
	if tier.IsSet() {
		requires = "**" + tier.String() + "**"
	}
	tierBtn := func(label, key string, on bool) discord.ButtonComponent {
		style := discord.ButtonStyleSecondary
		if on {
			style = discord.ButtonStylePrimary
		}
		return discord.ButtonComponent{Label: label, Style: style, CustomID: panelSet + "tier:" + key + ":" + action, Disabled: !can || on}
	}
	return append(body,
		ui.Text(fmt.Sprintf("`%s` requires %s here.", action, requires)),
		ui.Row(
			tierBtn("Default", "default", !tier.IsSet()),
			tierBtn("Admins and mods", "mod", tier == core.TierMod),
			tierBtn("Admins only", "admin", tier == core.TierAdmin),
		),
		ui.Text("**Also allowed**, whatever their tier:"),
		ui.Row(ui.RoleSelect(panelSet+"allow-roles:"+action, "Roles also allowed", allowR, core.MaxSelectValues, !can)),
		ui.Row(ui.UserSelect(panelSet+"allow-users:"+action, "Members also allowed", allowU, core.MaxSelectValues, !can)),
		ui.Text("**Blocked**, whatever their tier. A block wins over tier, Administrator and any allow."),
		ui.Row(ui.RoleSelect(panelSet+"deny-roles:"+action, "Roles blocked", denyR, core.MaxSelectValues, !can)),
		ui.Row(ui.UserSelect(panelSet+"deny-users:"+action, "Members blocked", denyU, core.MaxSelectValues, !can)),
	)
}

// handlePanelSet applies one control. Every value it reads came from the
// member's client, so an action or plugin name is checked against what is
// really registered before it reaches the store, exactly as the
// autocompleted options it replaces should have been.
func (p *Plugin) handlePanelSet(ctx context.Context, ix *core.Ix, customID string) {
	what, arg, _ := strings.Cut(strings.TrimPrefix(customID, panelSet), ":")
	switch what {
	case "admins":
		p.redraw(ctx, ix, "access", "", p.applyAdmins(ctx, ix))
	case "modroles":
		p.redraw(ctx, ix, "access", "", p.applyModRoles(ctx, ix))
	case "audit":
		p.redraw(ctx, ix, "channels", "", p.applyChannel(ctx, ix, "Audit log", p.settings.SetAuditLogChannel))
	case "status":
		p.redraw(ctx, ix, "channels", "", p.applyChannel(ctx, ix, "Status channel", p.settings.SetStatusChannel))
	case "setup":
		p.handleSetup(ctx, ix)
	case "plugins":
		p.redraw(ctx, ix, "plugins", "", p.applyPlugins(ctx, ix))
	case "pause":
		p.redraw(ctx, ix, "safety", "", p.applyPause(ctx, ix, arg == "on"))
	case "dryrun":
		p.redraw(ctx, ix, "safety", "", p.applyDryRun(ctx, ix, arg == "on"))
	case "import":
		p.redraw(ctx, ix, "safety", "", p.applyImport(ctx, ix))
	case "tier":
		tier, action, _ := strings.Cut(arg, ":")
		p.redraw(ctx, ix, "permissions", action, p.applyTier(ctx, ix, action, tier))
	case "allow-roles", "allow-users", "deny-roles", "deny-users":
		p.redraw(ctx, ix, "permissions", arg, p.applyOverride(ctx, ix, what, arg))
	default:
		p.redraw(ctx, ix, "home", "", "✗ That control is out of date. Run `/config` again.")
	}
}

func (p *Plugin) applyAdmins(ctx context.Context, ix *core.Ix) string {
	added, removed := ix.SelectDiff(panelSet+"admins")
	for _, id := range added {
		if err := p.settings.AddAdmin(ctx, ix.GuildID, id); err != nil {
			return fmt.Sprintf("✗ Couldn't add <@%s> as an admin: %v", id, err)
		}
		p.audit(ctx, ix, "config.admin_added", "", core.MentionUser(id))
	}
	for _, id := range removed {
		if err := p.settings.RemoveAdmin(ctx, ix.GuildID, id); err != nil {
			return fmt.Sprintf("✗ Couldn't remove <@%s> as an admin: %v", id, err)
		}
		p.audit(ctx, ix, "config.admin_removed", core.MentionUser(id), "")
	}
	return changeNote(added, removed, core.MentionUser, "admin")
}

func (p *Plugin) applyModRoles(ctx context.Context, ix *core.Ix) string {
	added, removed := ix.SelectDiff(panelSet+"modroles")
	for _, id := range added {
		if err := p.settings.AddModRole(ctx, ix.GuildID, id); err != nil {
			return fmt.Sprintf("✗ Couldn't add <@&%s> as a mod role: %v", id, err)
		}
		p.audit(ctx, ix, "config.mod_role_added", "", core.MentionRole(id))
	}
	if len(added) > 0 {
		p.grantRolesChannelAccess(p.session, ix.GuildID, added)
	}
	for _, id := range removed {
		if err := p.settings.RemoveModRole(ctx, ix.GuildID, id); err != nil {
			return fmt.Sprintf("✗ Couldn't remove <@&%s> as a mod role: %v", id, err)
		}
		p.audit(ctx, ix, "config.mod_role_removed", core.MentionRole(id), "")
	}
	return changeNote(added, removed, core.MentionRole, "mod role")
}

// changeNote says what a set select changed, or nothing when it changed
// nothing (a member closing the picker without touching it).
func changeNote(added, removed []string, render func(string) string, noun string) string {
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+mentionList(added, render))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+mentionList(removed, render))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("✓ %s: %s.", strings.ToUpper(noun[:1])+noun[1:]+"s", strings.Join(parts, "; "))
}

// applyChannel is the wizard's pick step (setupChannelPicked) without the
// wizard: same setter, same re-grant of mod access, same warning when the
// channel is one everybody can read.
func (p *Plugin) applyChannel(ctx context.Context, ix *core.Ix, label string, set func(ctx context.Context, guildID, channelID string) error) string {
	channelID := ix.FirstValue()
	if channelID == "" {
		return ""
	}
	if err := set(ctx, ix.GuildID, channelID); err != nil {
		return fmt.Sprintf("✗ Couldn't save that channel: %v", err)
	}
	p.grantModRolesChannelAccess(p.session, ix.GuildID)
	p.audit(ctx, ix, "config.setup", "", fmt.Sprintf("%s=<#%s> (existing)", label, channelID))
	note := fmt.Sprintf("✓ %s is now <#%s>.", label, channelID)
	if ch, err := p.session.Channel(channelID); err == nil && everyoneCanRead(ch) {
		note += " ⚠️ Everyone can read that channel, so anything recorded there is public."
	}
	return note
}

func (p *Plugin) applyPlugins(ctx context.Context, ix *core.Ix) string {
	added, removed := ix.SelectDiff(panelSet+"plugins")
	known := map[string]bool{}
	for _, n := range p.commands.Plugins() {
		known[n] = true
	}
	for _, name := range added {
		if !known[name] {
			return "✗ That plugin isn't running anymore. Run `/config` again."
		}
		if err := p.settings.EnablePlugin(ctx, ix.GuildID, name); err != nil {
			return fmt.Sprintf("✗ Couldn't switch `%s` on: %v", name, err)
		}
		p.audit(ctx, ix, "config.plugin_enabled", "", name)
	}
	for _, name := range removed {
		// Never offered, and refused anyway: switching off the plugin that
		// owns this panel would lock the server out of switching anything
		// back on, itself included.
		if name == p.Name() || !known[name] {
			continue
		}
		if err := p.settings.DisablePlugin(ctx, ix.GuildID, name); err != nil {
			return fmt.Sprintf("✗ Couldn't switch `%s` off: %v", name, err)
		}
		p.audit(ctx, ix, "config.plugin_disabled", "", name)
	}
	code := func(s string) string { return "`" + s + "`" }
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "on: "+mentionList(added, code))
	}
	if len(removed) > 0 {
		parts = append(parts, "off: "+mentionList(removed, code))
	}
	if len(parts) == 0 {
		return ""
	}
	return "✓ Switched " + strings.Join(parts, "; ") + "."
}

// applyPause is the in-Discord emergency stop. It deliberately does not
// touch command dispatch: reading keeps working while paused, so whoever
// pulled the lever can still see what the bot believes is going on. The
// process-wide equivalent is MERLIN_PAUSE_ALL_WRITES, for when the database
// itself is the problem.
func (p *Plugin) applyPause(ctx context.Context, ix *core.Ix, paused bool) string {
	if err := p.settings.SetWritesPaused(ctx, ix.GuildID, paused); err != nil {
		return fmt.Sprintf("✗ Couldn't change the pause: %v", err)
	}
	p.audit(ctx, ix, "config.writes_paused", "", fmt.Sprint(paused))
	if paused {
		return "✓ Paused. merlin refuses every destructive action here until you resume."
	}
	return "✓ Resumed. Anything that came due while paused runs on its next tick."
}

// applyDryRun is rehearsal mode: rotation, sweep and jail make their full
// decision and write their audit trail but touch nothing in Discord. It
// exists because what it covers has no undo.
func (p *Plugin) applyDryRun(ctx context.Context, ix *core.Ix, on bool) string {
	if err := p.settings.SetWritesDryRun(ctx, ix.GuildID, on); err != nil {
		return fmt.Sprintf("✗ Couldn't change dry-run: %v", err)
	}
	p.audit(ctx, ix, "config.writes_dry_run", "", fmt.Sprint(on))
	if on {
		return "✓ Rehearsing. The audit log shows what *would* have happened."
	}
	return "✓ Acting for real again."
}

func (p *Plugin) applyImport(ctx context.Context, ix *core.Ix) string {
	guilds, err := p.settings.ImportFromLegacyYAML(ctx, p.legacyPath)
	if err != nil {
		return fmt.Sprintf("✗ Import failed: %v", err)
	}
	p.audit(ctx, ix, "config.imported", "", strings.Join(guilds, ", "))
	return fmt.Sprintf("✓ Imported settings for %d server(s).", len(guilds))
}

func (p *Plugin) knownAction(action string) bool {
	for _, a := range p.commands.Actions() {
		if a == action {
			return true
		}
	}
	return false
}

func (p *Plugin) applyTier(ctx context.Context, ix *core.Ix, action, key string) string {
	if !p.knownAction(action) {
		return "✗ That action isn't registered anymore."
	}
	if key == "default" {
		if err := p.settings.ClearActionTier(ctx, ix.GuildID, action); err != nil {
			return fmt.Sprintf("✗ Couldn't reset the tier: %v", err)
		}
		p.audit(ctx, ix, "config.permission_tier_cleared", action, "")
		return fmt.Sprintf("✓ `%s` uses its default tier again.", action)
	}
	var tier core.PermTier
	switch key {
	case "admin":
		tier = core.TierAdmin
	case "mod":
		tier = core.TierMod
	default:
		return "✗ That isn't a tier."
	}
	if err := validateTierChange(action, tier); err != nil {
		return "✗ " + err.Error()
	}
	if err := p.settings.SetActionTier(ctx, ix.GuildID, action, tier); err != nil {
		return fmt.Sprintf("✗ Couldn't set the tier: %v", err)
	}
	p.audit(ctx, ix, "config.permission_tier_set", "", fmt.Sprintf("action=%s tier=%s", action, tier))
	return fmt.Sprintf("✓ `%s` now requires **%s**.", action, tier)
}

// applyOverride is the four allow/block pickers. Each added or removed role
// or member is one settings call and one audit line, the same calls and the
// same lines /config permissions allow and deny wrote one at a time.
func (p *Plugin) applyOverride(ctx context.Context, ix *core.Ix, what, action string) string {
	if !p.knownAction(action) {
		return "✗ That action isn't registered anymore."
	}
	added, removed := ix.SelectDiff(panelSet+what+":"+action)
	roles := strings.HasSuffix(what, "-roles")
	target := func(id string) (string, string) {
		if roles {
			return id, ""
		}
		return "", id
	}
	set, unset := p.settings.GrantOverride, p.settings.RevokeOverride
	setAudit, unsetAudit := "config.permission_granted", "config.permission_revoked"
	if strings.HasPrefix(what, "deny") {
		set, unset = p.settings.DenyOverride, p.settings.UndenyOverride
		setAudit, unsetAudit = "config.permission_blocked", "config.permission_unblocked"
	}
	for _, id := range added {
		r, u := target(id)
		if err := set(ctx, ix.GuildID, action, r, u); err != nil {
			return fmt.Sprintf("✗ Couldn't save that: %v", err)
		}
		p.audit(ctx, ix, setAudit, "", permissionTarget(action, r, u))
	}
	for _, id := range removed {
		r, u := target(id)
		if err := unset(ctx, ix.GuildID, action, r, u); err != nil {
			return fmt.Sprintf("✗ Couldn't save that: %v", err)
		}
		p.audit(ctx, ix, unsetAudit, permissionTarget(action, r, u), "")
	}
	if len(added)+len(removed) == 0 {
		return ""
	}
	return fmt.Sprintf("✓ Updated who may run `%s`.", action)
}

// Help is this plugin's page in /help.
func (p *Plugin) Help() core.Help {
	return core.Help{
		Title: "Settings",
		Line:  "who runs me here, where i write things down, and what is switched on",
		About: "`/config` opens my settings for this server: who counts as an admin or a mod, where the audit log and status alerts go, which parts of me are on, and who may run any single command. mods can read it; only admins can change it. pause stops everything destructive at once, and dry-run makes me say what i would have done without doing it.",
	}
}
