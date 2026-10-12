package adminconfig

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
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
	p.commands.HandleComponent(p.Name(), panelView, readSpec, p.handlePanelView)
	p.commands.HandleComponent(p.Name(), panelSet, mutateSpec, p.handlePanelSet)
}

func (p *Plugin) handleConfig(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.RespondPanel(s, i, p.panel(ctx, i, "home", "", "")); err != nil {
		p.log.Error("adminconfig: panel response failed", "err", err)
	}
}

// handlePanelView turns pages. Its CustomIDs are config:panel:view:page
// (the page select), config:panel:view:area (permissions' plugin select) and
// config:panel:view:action (permissions' action select).
func (p *Plugin) handlePanelView(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	page, arg := "home", ""
	v := core.FirstValue(i)
	switch strings.TrimPrefix(customID, panelView) {
	case "page":
		page = v
	case "area":
		page, arg = "permissions", v
	case "action":
		page, arg = "permissions", v
	}
	p.redraw(ctx, s, i, page, arg, "")
}

func (p *Plugin) redraw(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, page, arg, note string) {
	if err := core.UpdatePanel(s, i, p.panel(ctx, i, page, arg, note)); err != nil {
		p.log.Error("adminconfig: panel update failed", "page", page, "err", err)
	}
}

// panel draws one page. arg is the permissions page's area or action.
func (p *Plugin) panel(ctx context.Context, i *discordgo.InteractionCreate, page, arg, note string) core.Panel {
	can := p.perms.Allows(i, mutateSpec)
	gs := p.settings.GuildSettings(i.GuildID)
	pn := core.Panel{Color: core.ColorInfo, Title: "Settings", Plugin: p.Name(), Note: note,
		Footer: "Mods can read this; only admins can change it. Every change is written to the audit log."}
	if !can {
		pn.Footer = "You can read this, but only admins can change it."
	}

	switch page {
	case "access":
		pn.Line = "Who can change merlin, and who counts as a mod"
		pn.Body = []discordgo.MessageComponent{
			core.Text("**Admins** can change merlin's own configuration. Anyone holding Discord's Administrator permission already counts, so this is for people who should have it without that.\n" +
				"-# Now: " + userListText(gs.AdminUserIDs)),
			core.Row(core.EntitySelect(discordgo.UserSelectMenu, panelSet+"admins", "Pick who has standing admin access", gs.AdminUserIDs, core.MaxSelectValues, !can)),
			core.Text("**Mod roles** can use mod-tier commands: jailing, releasing, running a rotation early.\n" +
				"-# Now: " + roleListText(gs.ModRoleIDs)),
			core.Row(core.EntitySelect(discordgo.RoleSelectMenu, panelSet+"modroles", "Pick which roles count as mod", gs.ModRoleIDs, core.MaxSelectValues, !can)),
		}

	case "channels":
		audit, _ := p.channelState(gs.AuditLogChannelID)
		status, _ := p.channelState(gs.StatusChannelID)
		pn.Line = "Where the audit log and status alerts go"
		pn.Body = []discordgo.MessageComponent{
			core.Text("**Audit log**: config changes, rotations and moderation actions. Mod roles are given access.\n-# Now: " + audit),
			core.Row(core.ChannelSelect(panelSet+"audit", "Pick the audit log channel", gs.AuditLogChannelID, !can, discordgo.ChannelTypeGuildText)),
			core.Text("**Status channel**: operational trouble, like a job failing over and over. Quiet when nothing is wrong.\n-# Now: " + status),
			core.Row(core.ChannelSelect(panelSet+"status", "Pick the status channel", gs.StatusChannelID, !can, discordgo.ChannelTypeGuildText)),
			core.Row(discordgo.Button{Label: "Guided setup", Style: discordgo.SecondaryButton, CustomID: panelSet + "setup", Disabled: !can}),
		}
		pn.Footer = "Guided setup can create either channel for you, locked to mods."

	case "plugins":
		names := p.commands.Plugins()
		sort.Strings(names)
		var rows [][2]string
		var opts []discordgo.SelectMenuOption
		for _, name := range names {
			on := p.settings.PluginEnabled(i.GuildID, name)
			state := "on"
			if !on {
				state = "off"
			}
			rows = append(rows, [2]string{name, state})
			if name != p.Name() {
				opts = append(opts, discordgo.SelectMenuOption{Label: name, Value: name, Default: on})
			}
		}
		pn.Line = "Switch a whole plugin on or off here"
		pn.Body = []discordgo.MessageComponent{
			core.Text("A plugin that is off answers nobody here, admins included, and its scheduled work stops.\n" + core.Readout(rows)),
		}
		if len(opts) > 0 {
			pn.Body = append(pn.Body, core.Row(core.StringSelect(panelSet+"plugins", "Pick which plugins are on", opts, true, !can)))
		}
		pn.Footer = p.Name() + " runs this panel, so it stays on."

	case "permissions":
		pn.Line = "Retier one action, or allow or block someone"
		pn.Body = p.permissionsBody(i.GuildID, arg, can)

	case "safety":
		pn.Line = "The emergency stop and rehearsal mode"
		pauseState, pauseBtn := "Acting normally.", discordgo.Button{Label: "Pause", Style: discordgo.DangerButton, CustomID: panelSet + "pause:on", Disabled: !can}
		if gs.WritesPaused {
			pauseState, pauseBtn = "⛔ **Paused.** Scheduled jobs stay due and resume where they left off.", discordgo.Button{Label: "Resume", Style: discordgo.SuccessButton, CustomID: panelSet + "pause:off", Disabled: !can}
		}
		dryState, dryBtn := "Acting for real.", discordgo.Button{Label: "Rehearse", Style: discordgo.SecondaryButton, CustomID: panelSet + "dryrun:on", Disabled: !can}
		if gs.WritesDryRun {
			dryState, dryBtn = "🧪 **Rehearsing.** Nothing is being changed.", discordgo.Button{Label: "Act for real", Style: discordgo.SuccessButton, CustomID: panelSet + "dryrun:off", Disabled: !can}
		}
		pn.Body = []discordgo.MessageComponent{
			discordgo.Section{
				Components: []discordgo.MessageComponent{core.Text("**Pause** refuses every rotation, archive deletion, jail and role change in this server. Reading keeps working.\n-# " + pauseState)},
				Accessory:  pauseBtn,
			},
			discordgo.Section{
				Components: []discordgo.MessageComponent{core.Text("**Dry-run** writes to the audit log what rotation, sweeps and jails *would* do, and does none of it.\n-# " + dryState)},
				Accessory:  dryBtn,
			},
		}
		if _, err := os.Stat(p.legacyPath); err == nil {
			pn.Body = append(pn.Body, discordgo.Section{
				Components: []discordgo.MessageComponent{core.Text("**Import** seeds this server's settings, once, from the legacy config file on disk.")},
				Accessory:  discordgo.Button{Label: "Import", Style: discordgo.SecondaryButton, CustomID: panelSet + "import", Disabled: !can},
			})
		}
		if gs.WritesPaused || gs.WritesDryRun {
			pn.Color, pn.Mood = core.ColorWarning, core.MoodIdle
		}

	default:
		page = "home"
		body, sev := p.statusReport(ctx, i.GuildID)
		pn.Line = "This server's settings, and whether merlin is okay"
		pn.Body = []discordgo.MessageComponent{core.Text(body)}
		switch sev {
		case sevError, sevWarn:
			pn.Color = core.ColorWarning
		case sevStopped:
			pn.Color, pn.Mood = core.ColorWarning, core.MoodIdle
		}
	}

	pn.Body = append(pn.Body, core.Gap(), core.Row(pageSelect(page)))
	return pn
}

func pageSelect(current string) discordgo.SelectMenu {
	opts := make([]discordgo.SelectMenuOption, len(panelPages))
	for n, pg := range panelPages {
		opts[n] = discordgo.SelectMenuOption{Label: pg.label, Value: pg.key, Description: pg.about, Default: pg.key == current}
	}
	return core.StringSelect(panelView+"page", "Go to...", opts, false, false)
}

// permissionsBody is the permissions page: pick a plugin, then an action,
// then that action's tier, extra allows and blocks. An area is the part of
// an action before its dot. Two selects rather than one because there are
// more actions than a select can hold.
func (p *Plugin) permissionsBody(guildID, arg string, can bool) []discordgo.MessageComponent {
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
	var areaOpts []discordgo.SelectMenuOption
	for _, ar := range areas {
		areaOpts = append(areaOpts, discordgo.SelectMenuOption{Label: ar, Value: ar, Default: ar == area})
	}

	body := []discordgo.MessageComponent{}
	if area == "" {
		lines := permissionOverrideLines(p.settings.Overrides(guildID))
		summary := "No action has been changed from its default here."
		if len(lines) > 0 {
			summary = core.TruncateEmbedField(strings.Join(lines, "\n"))
		}
		body = append(body, core.Text("Every command runs as an **action**. Pick one to change who may run it.\n"+summary))
	}
	if len(areaOpts) > 0 {
		body = append(body, core.Row(core.StringSelect(panelView+"area", "Pick a plugin", areaOpts, false, false)))
	}
	if area == "" {
		return body
	}

	var actOpts []discordgo.SelectMenuOption
	for _, a := range byArea[area] {
		actOpts = append(actOpts, discordgo.SelectMenuOption{Label: a, Value: a, Default: a == action})
	}
	if len(actOpts) > 0 {
		body = append(body, core.Row(core.StringSelect(panelView+"action", "Pick an action", actOpts, false, false)))
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
	tierBtn := func(label, key string, on bool) discordgo.Button {
		style := discordgo.SecondaryButton
		if on {
			style = discordgo.PrimaryButton
		}
		return discordgo.Button{Label: label, Style: style, CustomID: panelSet + "tier:" + key + ":" + action, Disabled: !can || on}
	}
	return append(body,
		core.Text(fmt.Sprintf("`%s` requires %s here.", action, requires)),
		core.Row(
			tierBtn("Default", "default", !tier.IsSet()),
			tierBtn("Admins and mods", "mod", tier == core.TierMod),
			tierBtn("Admins only", "admin", tier == core.TierAdmin),
		),
		core.Text("**Also allowed**, whatever their tier:"),
		core.Row(core.EntitySelect(discordgo.RoleSelectMenu, panelSet+"allow-roles:"+action, "Roles also allowed", allowR, core.MaxSelectValues, !can)),
		core.Row(core.EntitySelect(discordgo.UserSelectMenu, panelSet+"allow-users:"+action, "Members also allowed", allowU, core.MaxSelectValues, !can)),
		core.Text("**Blocked**, whatever their tier. A block wins over tier, Administrator and any allow."),
		core.Row(core.EntitySelect(discordgo.RoleSelectMenu, panelSet+"deny-roles:"+action, "Roles blocked", denyR, core.MaxSelectValues, !can)),
		core.Row(core.EntitySelect(discordgo.UserSelectMenu, panelSet+"deny-users:"+action, "Members blocked", denyU, core.MaxSelectValues, !can)),
	)
}

// handlePanelSet applies one control. Every value it reads came from the
// member's client, so an action or plugin name is checked against what is
// really registered before it reaches the store, exactly as the
// autocompleted options it replaces should have been.
func (p *Plugin) handlePanelSet(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	what, arg, _ := strings.Cut(strings.TrimPrefix(customID, panelSet), ":")
	switch what {
	case "admins":
		p.redraw(ctx, s, i, "access", "", p.applyAdmins(ctx, i))
	case "modroles":
		p.redraw(ctx, s, i, "access", "", p.applyModRoles(ctx, s, i))
	case "audit":
		p.redraw(ctx, s, i, "channels", "", p.applyChannel(ctx, s, i, "Audit log", p.settings.SetAuditLogChannel))
	case "status":
		p.redraw(ctx, s, i, "channels", "", p.applyChannel(ctx, s, i, "Status channel", p.settings.SetStatusChannel))
	case "setup":
		p.handleSetup(ctx, s, i)
	case "plugins":
		p.redraw(ctx, s, i, "plugins", "", p.applyPlugins(ctx, i))
	case "pause":
		p.redraw(ctx, s, i, "safety", "", p.applyPause(ctx, i, arg == "on"))
	case "dryrun":
		p.redraw(ctx, s, i, "safety", "", p.applyDryRun(ctx, i, arg == "on"))
	case "import":
		p.redraw(ctx, s, i, "safety", "", p.applyImport(ctx, i))
	case "tier":
		tier, action, _ := strings.Cut(arg, ":")
		p.redraw(ctx, s, i, "permissions", action, p.applyTier(ctx, i, action, tier))
	case "allow-roles", "allow-users", "deny-roles", "deny-users":
		p.redraw(ctx, s, i, "permissions", arg, p.applyOverride(ctx, i, what, arg))
	default:
		p.redraw(ctx, s, i, "home", "", "✗ That control is out of date. Run `/config` again.")
	}
}

func (p *Plugin) applyAdmins(ctx context.Context, i *discordgo.InteractionCreate) string {
	added, removed := core.SelectDiff(i, panelSet+"admins")
	for _, id := range added {
		if err := p.settings.AddAdmin(ctx, i.GuildID, id); err != nil {
			return fmt.Sprintf("✗ Couldn't add <@%s> as an admin: %v", id, err)
		}
		p.audit(ctx, i, "config.admin_added", "", core.MentionUser(id))
	}
	for _, id := range removed {
		if err := p.settings.RemoveAdmin(ctx, i.GuildID, id); err != nil {
			return fmt.Sprintf("✗ Couldn't remove <@%s> as an admin: %v", id, err)
		}
		p.audit(ctx, i, "config.admin_removed", core.MentionUser(id), "")
	}
	return changeNote(added, removed, core.MentionUser, "admin")
}

func (p *Plugin) applyModRoles(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) string {
	added, removed := core.SelectDiff(i, panelSet+"modroles")
	for _, id := range added {
		if err := p.settings.AddModRole(ctx, i.GuildID, id); err != nil {
			return fmt.Sprintf("✗ Couldn't add <@&%s> as a mod role: %v", id, err)
		}
		p.audit(ctx, i, "config.mod_role_added", "", core.MentionRole(id))
	}
	if len(added) > 0 {
		p.grantRolesChannelAccess(s, i.GuildID, added)
	}
	for _, id := range removed {
		if err := p.settings.RemoveModRole(ctx, i.GuildID, id); err != nil {
			return fmt.Sprintf("✗ Couldn't remove <@&%s> as a mod role: %v", id, err)
		}
		p.audit(ctx, i, "config.mod_role_removed", core.MentionRole(id), "")
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
func (p *Plugin) applyChannel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, label string, set func(ctx context.Context, guildID, channelID string) error) string {
	channelID := core.FirstValue(i)
	if channelID == "" {
		return ""
	}
	if err := set(ctx, i.GuildID, channelID); err != nil {
		return fmt.Sprintf("✗ Couldn't save that channel: %v", err)
	}
	p.grantModRolesChannelAccess(s, i.GuildID)
	p.audit(ctx, i, "config.setup", "", fmt.Sprintf("%s=<#%s> (existing)", label, channelID))
	note := fmt.Sprintf("✓ %s is now <#%s>.", label, channelID)
	if ch, err := s.Channel(channelID); err == nil && everyoneCanRead(ch) {
		note += " ⚠️ Everyone can read that channel, so anything recorded there is public."
	}
	return note
}

func (p *Plugin) applyPlugins(ctx context.Context, i *discordgo.InteractionCreate) string {
	added, removed := core.SelectDiff(i, panelSet+"plugins")
	known := map[string]bool{}
	for _, n := range p.commands.Plugins() {
		known[n] = true
	}
	for _, name := range added {
		if !known[name] {
			return "✗ That plugin isn't running anymore. Run `/config` again."
		}
		if err := p.settings.EnablePlugin(ctx, i.GuildID, name); err != nil {
			return fmt.Sprintf("✗ Couldn't switch `%s` on: %v", name, err)
		}
		p.audit(ctx, i, "config.plugin_enabled", "", name)
	}
	for _, name := range removed {
		// Never offered, and refused anyway: switching off the plugin that
		// owns this panel would lock the server out of switching anything
		// back on, itself included.
		if name == p.Name() || !known[name] {
			continue
		}
		if err := p.settings.DisablePlugin(ctx, i.GuildID, name); err != nil {
			return fmt.Sprintf("✗ Couldn't switch `%s` off: %v", name, err)
		}
		p.audit(ctx, i, "config.plugin_disabled", "", name)
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
func (p *Plugin) applyPause(ctx context.Context, i *discordgo.InteractionCreate, paused bool) string {
	if err := p.settings.SetWritesPaused(ctx, i.GuildID, paused); err != nil {
		return fmt.Sprintf("✗ Couldn't change the pause: %v", err)
	}
	p.audit(ctx, i, "config.writes_paused", "", fmt.Sprint(paused))
	if paused {
		return "✓ Paused. merlin refuses every destructive action here until you resume."
	}
	return "✓ Resumed. Anything that came due while paused runs on its next tick."
}

// applyDryRun is rehearsal mode: rotation, sweep and jail make their full
// decision and write their audit trail but touch nothing in Discord. It
// exists because what it covers has no undo.
func (p *Plugin) applyDryRun(ctx context.Context, i *discordgo.InteractionCreate, on bool) string {
	if err := p.settings.SetWritesDryRun(ctx, i.GuildID, on); err != nil {
		return fmt.Sprintf("✗ Couldn't change dry-run: %v", err)
	}
	p.audit(ctx, i, "config.writes_dry_run", "", fmt.Sprint(on))
	if on {
		return "✓ Rehearsing. The audit log shows what *would* have happened."
	}
	return "✓ Acting for real again."
}

func (p *Plugin) applyImport(ctx context.Context, i *discordgo.InteractionCreate) string {
	guilds, err := p.settings.ImportFromLegacyYAML(ctx, p.legacyPath)
	if err != nil {
		return fmt.Sprintf("✗ Import failed: %v", err)
	}
	p.audit(ctx, i, "config.imported", "", strings.Join(guilds, ", "))
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

func (p *Plugin) applyTier(ctx context.Context, i *discordgo.InteractionCreate, action, key string) string {
	if !p.knownAction(action) {
		return "✗ That action isn't registered anymore."
	}
	if key == "default" {
		if err := p.settings.ClearActionTier(ctx, i.GuildID, action); err != nil {
			return fmt.Sprintf("✗ Couldn't reset the tier: %v", err)
		}
		p.audit(ctx, i, "config.permission_tier_cleared", action, "")
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
	if err := p.settings.SetActionTier(ctx, i.GuildID, action, tier); err != nil {
		return fmt.Sprintf("✗ Couldn't set the tier: %v", err)
	}
	p.audit(ctx, i, "config.permission_tier_set", "", fmt.Sprintf("action=%s tier=%s", action, tier))
	return fmt.Sprintf("✓ `%s` now requires **%s**.", action, tier)
}

// applyOverride is the four allow/block pickers. Each added or removed role
// or member is one settings call and one audit line, the same calls and the
// same lines /config permissions allow and deny wrote one at a time.
func (p *Plugin) applyOverride(ctx context.Context, i *discordgo.InteractionCreate, what, action string) string {
	if !p.knownAction(action) {
		return "✗ That action isn't registered anymore."
	}
	added, removed := core.SelectDiff(i, panelSet+what+":"+action)
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
		if err := set(ctx, i.GuildID, action, r, u); err != nil {
			return fmt.Sprintf("✗ Couldn't save that: %v", err)
		}
		p.audit(ctx, i, setAudit, "", permissionTarget(action, r, u))
	}
	for _, id := range removed {
		r, u := target(id)
		if err := unset(ctx, i.GuildID, action, r, u); err != nil {
			return fmt.Sprintf("✗ Couldn't save that: %v", err)
		}
		p.audit(ctx, i, unsetAudit, permissionTarget(action, r, u), "")
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
