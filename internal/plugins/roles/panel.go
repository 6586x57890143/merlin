package roles

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
	"github.com/6586x57890143/merlin/internal/scripts"
)

// /roles settings is one panel (core.Panel) for everything that used to be
// /roles configure and /roles scripts: what a jailed or vacationing member
// can still see, the roles a new member is given, and the scripts. The
// commands that act on a person (jail, release, grant, ...) stay commands.
//
// Each control keeps its subcommand's action: the jail channel settings are
// roles.configure_jail_channels, turning a script on or off is
// roles.scripts, and defining an eternal role is roles.scripts_define with
// the owner-or-operator check on top, exactly as before.
const (
	panelView    = "roles:panel:view:"
	panelJail    = "roles:panel:jail:"
	panelScripts = "roles:panel:scripts:"
	panelDefine  = "roles:panel:define:"
)

var (
	viewSpec    = core.PermSpec{Tier: core.TierMod, Action: actionList}
	jailSpec    = core.PermSpec{Tier: core.TierAdmin, Action: actionConfigureJailCh}
	scriptsSpec = core.PermSpec{Tier: core.TierAdmin, Action: actionScripts}
	defineSpec  = core.PermSpec{Tier: core.TierAdmin, Action: actionScriptsDefine}
)

var rolesPages = []struct{ key, label, about string }{
	{"jail", "Jail", "What a jailed member can still see"},
	{"vacation", "Vacation", "The island's role and what it can see"},
	{"join", "New members", "The roles every new member is given"},
	{"scripts", "Scripts", "Server scripts that override admins"},
}

func (p *Plugin) registerPanel() {
	p.commands.HandleComponentIx(p.Name(), panelView, viewSpec, p.handlePanelView)
	p.commands.HandleComponentIx(p.Name(), panelJail, jailSpec, p.handlePanelSet)
	p.commands.HandleComponentIx(p.Name(), panelScripts, scriptsSpec, p.handlePanelSet)
	p.commands.HandleComponentIx(p.Name(), panelDefine, defineSpec, p.handlePanelSet)
}

func (p *Plugin) handleSettings(ctx context.Context, ix *core.Ix) {
	if err := ix.RespondPanel(p.panel(ctx, ix, "jail", "", "")); err != nil {
		p.log.Error("roles: panel response failed", "err", err)
	}
}

// handlePanelView turns pages (roles:panel:view:page) and picks whose
// eternal roles to show (roles:panel:view:eternal).
func (p *Plugin) handlePanelView(ctx context.Context, ix *core.Ix, customID string) {
	page, arg := ix.FirstValue(), ""
	if strings.TrimPrefix(customID, panelView) == "eternal" {
		page, arg = "scripts", ix.FirstValue()
	}
	if err := ix.UpdatePanel(p.panel(ctx, ix, page, arg, "")); err != nil {
		p.log.Error("roles: panel update failed", "err", err)
	}
}

func (p *Plugin) panel(ctx context.Context, ix *core.Ix, page, arg, note string) core.Panel {
	perms := p.commands.Perms()
	canJail, canScripts := perms.Allows(ix, jailSpec), perms.Allows(ix, scriptsSpec)
	pn := core.Panel{Color: core.ColorInfo, Title: "Roles", Plugin: p.Name(), Note: note,
		Line:   "Jail, vacation, new members and scripts",
		Footer: "Jail, release and grants are still commands: /roles jail, /roles release, /roles grant. Every change here is audited."}
	cfg := p.jailChannelConfig
	g := ix.GuildID

	switch page {
	case "vacation":
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text("**Island role**: what a member sent on `/roles vacation` holds. merlin never creates it.\n-# " + p.vacationLine(g)),
			ui.Row(ui.Clearable(ui.RoleSelect(panelJail+"vacrole", "The island's role", []string{cfg.VacationRoleID(g)}, 1, !canJail))),
			ui.Text("**Read-only**: view and message history, without posting or joining voice. Existing allowed channels start here.\n-# Now: " + channelList(p.vacationAllowlist(g))),
			ui.Row(accessChannelSelect(panelJail+"vacallow", "Read-only vacation channels", cfg.VacationAllowedChannelIDs(g), !canJail, false)),
			ui.Text("**Conversation**: view, history and sending; voice channels also allow connecting and speaking. Every other channel is hidden.\n-# Now: " + channelList(cfg.VacationConversationChannelIDs(g))),
			ui.Row(accessChannelSelect(panelJail+"vacconversation", "Vacation conversation channels", cfg.VacationConversationChannelIDs(g), !canJail, true)),
		}

	case "join":
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text("The **join-role** script gives a new member these roles 15 seconds after joining, besides the member role.\n-# " + p.joinRoleLine(g)),
			ui.Row(ui.RoleSelect(panelJail+"join", "Roles every new member gets", cfg.JoinRoleIDs(g), ui.MaxSelectValues, !canJail)),
		}

	case "scripts":
		page = "scripts"
		pn.Parts = []discord.ContainerSubComponent{ui.Text("-# " + scripts.Warning)}
		recs, _ := p.store.ListEternalRoles(ctx, g)
		for _, name := range pluginScripts {
			on, state := false, "off"
			if p.scripts != nil {
				var err error
				if on, err = p.scripts.Enabled(ctx, g, name); err != nil {
					on, state = false, "unreadable (treated as off)"
				}
			}
			b := ui.Button("Turn on", panelScripts+"on:"+name, discord.ButtonStyleSecondary, !canScripts)
			if on {
				b, state = ui.Button("Turn off", panelScripts+"off:"+name, discord.ButtonStyleDanger, !canScripts), "**on**"
			}
			pn.Parts = append(pn.Parts, ui.Switch(fmt.Sprintf("`%s` is %s\n-# %s", name, state, p.scriptLine(g, name, recs)), b))
		}
		canDefine, _ := p.canDefineEternal(g, actorID(ix))
		pn.Parts = append(pn.Parts, ui.Gap(),
			ui.Text("**Eternal roles**: pick a member to choose the roles they always keep. Server owner only."),
			ui.Row(ui.UserSelect(panelView+"eternal", "Pick a member", []string{arg}, 1, false)))
		if arg != "" {
			var kept []string
			for _, r := range recs {
				if r.UserID == arg {
					kept = append(kept, r.OriginRoleID)
				}
			}
			pn.Parts = append(pn.Parts, ui.Row(ui.RoleSelect(panelDefine+"eternal:"+arg, "Roles they always keep", kept, ui.MaxSelectValues, !canDefine)))
		}

	default:
		page = "jail"
		marker := "merlin's own role"
		if id := cfg.JailMarkerRoleID(g); id != "" {
			marker = core.MentionRole(id)
		}
		announce := "only where the command was run"
		if id := cfg.JailAnnounceChannelID(g); id != "" {
			announce = "also " + core.MentionChannel(id)
		}
		baseline := "@everyone"
		if id := cfg.MemberRoleID(g); id != "" {
			baseline = core.MentionRole(id)
		}
		pn.Parts = []discord.ContainerSubComponent{
			ui.Text("**Marker role**: what a jailed member holds instead of their roles. Empty uses merlin's own.\n-# Now: " + marker),
			ui.Row(ui.Clearable(ui.RoleSelect(panelJail+"marker", "The jail marker role", []string{cfg.JailMarkerRoleID(g)}, 1, !canJail))),
			ui.Text("**Read-only**: view and message history, without posting or joining voice. Existing allowed channels start here.\n-# Now: " + channelList(cfg.JailAllowedChannelIDs(g))),
			ui.Row(accessChannelSelect(panelJail+"allow", "Read-only jail channels", cfg.JailAllowedChannelIDs(g), !canJail, false)),
			ui.Text("**Conversation**: view, history and sending; voice channels also allow connecting and speaking. Every other channel is hidden.\n-# Now: " + channelList(cfg.JailConversationChannelIDs(g))),
			ui.Row(accessChannelSelect(panelJail+"conversation", "Jail conversation channels", cfg.JailConversationChannelIDs(g), !canJail, true)),
			ui.Text("**Announcements**: jail and release notices go " + announce + "."),
			ui.Row(ui.Clearable(ui.ChannelSelect(panelJail+"announce", "Also announce in", cfg.JailAnnounceChannelID(g), !canJail, discord.ChannelTypeGuildText, discord.ChannelTypeGuildNews))),
			ui.Text("**Member role**: the role given to ordinary members by the join-role script. Jail and vacation access follows the lists above.\n-# Now: " + baseline),
			ui.Row(ui.Clearable(ui.RoleSelect(panelJail+"member", "The plain member role", []string{cfg.MemberRoleID(g)}, 1, !canJail))),
			ui.Switch("New channels don't pick up the jail and vacation rules on their own.", ui.Button("Sync every channel", panelJail+"sync:", discord.ButtonStyleSecondary, !canJail)),
		}
	}

	opts := make([]discord.StringSelectMenuOption, len(rolesPages))
	for n, pg := range rolesPages {
		opts[n] = discord.StringSelectMenuOption{Label: pg.label, Value: pg.key, Description: pg.about, Default: pg.key == page}
	}
	pn.Parts = append(pn.Parts, ui.Gap(), ui.Row(ui.StringSelect(panelView+"page", "Go to...", opts, false, false)))
	return pn
}

// scriptLine is one script's state line on the scripts page.
func (p *Plugin) scriptLine(guildID, name string, recs []EternalRoleRecord) string {
	switch name {
	case scriptEternalRole:
		return eternalRolesLine(recs)
	case scriptVacation:
		return p.vacationLine(guildID)
	case scriptJoinRole:
		return p.joinRoleLine(guildID)
	}
	return ""
}

// pageOf is the page a control lives on, for the redraw after it.
var pageOf = map[string]string{
	"conversation": "jail", "vacconversation": "vacation",
	"marker": "jail", "allow": "jail", "announce": "jail", "member": "jail", "sync": "jail",
	"vacrole": "vacation", "vacallow": "vacation", "join": "join",
	"on": "scripts", "off": "scripts", "eternal": "scripts",
}

// handlePanelSet applies one control. Every one is deferred: several walk
// every channel in the guild (the marker, member and island roles, sync,
// turning a script on), and answering them all the same way means none can
// be the one that misses Discord's 3 seconds.
func (p *Plugin) handlePanelSet(ctx context.Context, ix *core.Ix, customID string) {
	rest := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(customID, panelJail), panelScripts), panelDefine)
	verb, arg, _ := strings.Cut(rest, ":")
	if err := ix.DeferPanelUpdate(); err != nil {
		p.log.Error("roles: defer failed", "err", err)
		return
	}
	var note string
	switch verb {
	case "marker":
		note = p.setMarkerRole(ctx, ix, ix.FirstValue())
	case "allow":
		note = p.setAllowlist(ctx, ix, panelJail+"allow", "jailed members", p.jailChannelConfig.AddJailAllowedChannel, p.jailChannelConfig.RemoveJailAllowedChannel, "allow", p.syncJailOne)
	case "vacallow":
		note = p.setAllowlist(ctx, ix, panelJail+"vacallow", "members on vacation", p.jailChannelConfig.AddVacationAllowedChannel, p.jailChannelConfig.RemoveVacationAllowedChannel, "vacation_allow", p.syncVacationChannel)
	case "conversation":
		note = p.setAllowlist(ctx, ix, panelJail+"conversation", "jailed members", p.jailChannelConfig.AddJailConversationChannel, p.jailChannelConfig.RemoveJailConversationChannel, "conversation", p.syncJailOne)
	case "vacconversation":
		note = p.setAllowlist(ctx, ix, panelJail+"vacconversation", "members on vacation", p.jailChannelConfig.AddVacationConversationChannel, p.jailChannelConfig.RemoveVacationConversationChannel, "vacation_conversation", p.syncVacationChannel)
	case "announce":
		note = p.setAnnounceChannel(ctx, ix, ix.FirstValue())
	case "member":
		note = p.setMemberRole(ctx, ix, ix.FirstValue())
	case "vacrole":
		note = p.setVacationRole(ctx, ix, ix.FirstValue())
	case "join":
		note = p.setJoinRoles(ctx, ix)
	case "sync":
		note = p.syncChannels(ctx, ix.GuildID)
	case "on", "off":
		note = p.setScript(ctx, ix, arg, verb == "on")
	case "eternal":
		note = p.setEternalRoles(ctx, ix, arg)
	default:
		note = "✗ That control is out of date. Run `/roles settings` again."
	}
	if err := ix.FollowUpPanel(p.panel(ctx, ix, pageOf[verb], arg, note)); err != nil {
		p.log.Error("roles: panel follow-up failed", "err", err)
	}
}

func (p *Plugin) auditConfig(ctx context.Context, ix *core.Ix, oldValue, newValue string) {
	if err := p.audit.Record(ctx, ix.GuildID, actorID(ix), "roles.configure_jail_channels", oldValue, newValue); err != nil {
		p.log.Error("roles: audit configure failed", "guild", ix.GuildID, "err", err)
	}
}

// setMarkerRole switches the jail marker, or clears it to merlin's own
// role, and resyncs every channel against the new one at the moment it
// changes rather than as a side effect of some later cold resolve.
func (p *Plugin) setMarkerRole(ctx context.Context, ix *core.Ix, roleID string) string {
	if roleID == "" {
		if err := p.jailChannelConfig.ClearJailMarkerRole(ctx, ix.GuildID); err != nil {
			return fmt.Sprintf("✗ Failed to clear marker role: %v", err)
		}
		p.forgetJailRole(ix.GuildID)
		p.markVisibilityDirty(ix.GuildID)
		p.auditConfig(ctx, ix, "", "marker_role=none")
		return "✓ Cleared jail role: merlin will use its own birdjailed role again."
	}
	if err := p.jailChannelConfig.SetJailMarkerRole(ctx, ix.GuildID, roleID); err != nil {
		return fmt.Sprintf("✗ Failed to save marker role: %v", err)
	}
	p.forgetJailRole(ix.GuildID)
	p.markVisibilityDirty(ix.GuildID)
	p.auditConfig(ctx, ix, "", "marker_role="+core.MentionRole(roleID))
	if err := p.syncAllJailChannelOverwrites(ix.GuildID, roleID); err != nil {
		return fmt.Sprintf("✗ Marker saved, but channel restrictions were not fully applied: %v. The sweep will retry.", err)
	}
	return fmt.Sprintf("✓ Configured jail role: jailed members will be assigned %s.", core.MentionRole(roleID))
}

// syncJailOne applies the allowlist to one channel's overwrite for the jail
// role, if the guild has one yet. Without one nothing is synced: the list is
// saved and applies when the role is first resolved and does its full sync.
func (p *Plugin) syncJailOne(guildID, channelID string) (discord.Permissions, error) {
	roleID, err := p.existingJailRole(guildID)
	if err != nil || roleID == "" {
		return 0, err
	}
	return p.syncJailChannelOverwrite(guildID, roleID, channelID)
}

func accessChannelSelect(id, label string, ids []string, disabled, conversation bool) discord.ChannelSelectMenuComponent {
	menu := ui.ChannelSelectMany(id, label, ids, ui.MaxSelectValues, disabled)
	menu.ChannelTypes = []discord.ChannelType{discord.ChannelTypeGuildText, discord.ChannelTypeGuildNews, discord.ChannelTypeGuildVoice}
	if !conversation {
		menu.ChannelTypes = append(menu.ChannelTypes, discord.ChannelTypeGuildStageVoice, discord.ChannelTypeGuildForum, discord.ChannelTypeGuildMedia)
	}
	return menu
}

func (p *Plugin) setAllowlist(ctx context.Context, ix *core.Ix, customID, who string,
	add, remove func(ctx context.Context, guildID, channelID string) error, auditKey string, sync func(guildID, channelID string) (discord.Permissions, error)) string {
	added, removed := ix.SelectDiff(customID)
	mode := "read-only"
	conversation := strings.HasSuffix(customID, "conversation")
	if conversation {
		mode = "conversation"
	}
	// Validate before saving any changes, including forged/stale selectors.
	for _, id := range added {
		ch, err := guildChannel(p.ops(ix.GuildID), core.ID(id))
		if err != nil {
			return fmt.Sprintf("✗ Cannot read channel: %v", err)
		}
		if ch.GuildID().String() != ix.GuildID || !jailManagedChannelType(ch.Type()) || (conversation && !conversationChannel(ch.Type())) {
			return "✗ That channel does not support this access mode. Forums, media and stage channels support read-only access."
		}
	}
	var notes []string
	change := func(id string, adding bool) bool {
		// Even a reported save failure can follow a committed write whose
		// cache refresh failed. Keep reconciliation pending in that case too.
		p.markVisibilityDirty(ix.GuildID)
		var err error
		if adding {
			err = add(ctx, ix.GuildID, id)
		} else {
			err = remove(ctx, ix.GuildID, id)
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("✗ Failed to save: %v", err))
			return false
		}
		p.auditConfig(ctx, ix, "", fmt.Sprintf("%s=%s selected=%t", auditKey, core.MentionChannel(id), adding))
		if p.dryRun(ix.GuildID) {
			notes = append(notes, "Settings saved; channel sync is pending while dry-run is enabled.")
		} else if _, err := sync(ix.GuildID, id); err != nil {
			notes = append(notes, fmt.Sprintf("✗ Settings saved, but access for <#%s> was not synced: %v. The sweep will retry.", id, err))
		} else if adding {
			notes = append(notes, fmt.Sprintf("✓ Channel allowed: <#%s> is configured for %s access for %s. Member-specific restrictions still apply.", id, mode, who))
		} else {
			notes = append(notes, fmt.Sprintf("✓ Channel access updated: <#%s> was removed from the %s list.", id, mode))
		}
		return true
	}
	for _, id := range added {
		if !change(id, true) {
			return strings.Join(notes, "\n")
		}
	}
	for _, id := range removed {
		if !change(id, false) {
			break
		}
	}
	return strings.Join(notes, "\n")
}

func (p *Plugin) setAnnounceChannel(ctx context.Context, ix *core.Ix, channelID string) string {
	if err := p.jailChannelConfig.SetJailAnnounceChannel(ctx, ix.GuildID, channelID); err != nil {
		return fmt.Sprintf("✗ Failed to save: %v", err)
	}
	detail := "none"
	if channelID != "" {
		detail = core.MentionChannel(channelID)
	}
	p.auditConfig(ctx, ix, "", "announce_channel="+detail)
	if channelID == "" {
		return "✓ Announcement channel cleared: notices only go where the command was run."
	}
	return fmt.Sprintf("✓ Announcement channel set: notices also go to <#%s>.", channelID)
}

// The member role drives the join script; marker access uses explicit modes.
func (p *Plugin) setMemberRole(ctx context.Context, ix *core.Ix, roleID string) string {
	if err := p.jailChannelConfig.SetMemberRole(ctx, ix.GuildID, roleID); err != nil {
		return fmt.Sprintf("✗ Failed to save: %v", err)
	}
	detail := "none"
	if roleID != "" {
		detail = core.MentionRole(roleID)
	}
	p.auditConfig(ctx, ix, "", "member_role="+detail)
	p.watchJoinRoles(ix.GuildID)
	if roleID == "" {
		return "✓ Member role cleared. Channel access follows the read-only and conversation lists."
	}
	return fmt.Sprintf("✓ Member role set: %s. Channel access follows the read-only and conversation lists.", core.MentionRole(roleID))
}

// setVacationRole sets or clears the island's marker, syncs every channel
// for the new one, and reads back what it can now see.
func (p *Plugin) setVacationRole(ctx context.Context, ix *core.Ix, roleID string) string {
	if err := p.jailChannelConfig.SetVacationRole(ctx, ix.GuildID, roleID); err != nil {
		return fmt.Sprintf("✗ Failed to save: %v", err)
	}
	p.forgetVacationRole(ix.GuildID)
	p.markVisibilityDirty(ix.GuildID)
	detail := "none"
	if roleID != "" {
		detail = core.MentionRole(roleID)
	}
	p.auditConfig(ctx, ix, "", "vacation_role="+detail)
	if roleID == "" {
		if ix.GuildID == meltingPotGuildID {
			return fmt.Sprintf("✓ Vacation role cleared: merlin will use %s, the Melting Pot's own island role.", core.MentionRole(meltingPotDefaultVacationRoleID))
		}
		return "✓ Vacation role cleared."
	}
	if err := p.syncAllVacationOverwrites(ix.GuildID); err != nil {
		return fmt.Sprintf("✗ Vacation role saved, but channel restrictions were not fully applied: %v. The sweep will retry.", err)
	}
	return fmt.Sprintf("✓ Vacation role set: members on vacation will hold %s.\n-# %s", core.MentionRole(roleID),
		strings.ReplaceAll(p.describeRoleAccess(ix.GuildID, roleID), "\n", " "))
}

// setJoinRoles edits the join-role script's extra roles. Adding one checks
// merlin can give it and starts watching it, so an edit to it in Discord is
// reported from then on.
func (p *Plugin) setJoinRoles(ctx context.Context, ix *core.Ix) string {
	added, removed := ix.SelectDiff(panelJail + "join")
	for _, roleID := range added {
		if roleID == ix.GuildID {
			return "✗ @everyone is not a role merlin can give."
		}
		if err := p.perms.CanManageRole(ix.GuildID, roleID); err != nil {
			return fmt.Sprintf("✗ merlin cannot give %s: %v", core.MentionRole(roleID), err)
		}
		if err := p.jailChannelConfig.AddJoinRole(ctx, ix.GuildID, roleID); err != nil {
			return fmt.Sprintf("✗ Failed to save: %v", err)
		}
		p.auditConfig(ctx, ix, "", "join_role_added="+core.MentionRole(roleID))
	}
	for _, roleID := range removed {
		if err := p.jailChannelConfig.RemoveJoinRole(ctx, ix.GuildID, roleID); err != nil {
			return fmt.Sprintf("✗ Failed to save: %v", err)
		}
		p.auditConfig(ctx, ix, "", "join_role_removed="+core.MentionRole(roleID))
	}
	if len(added) > 0 {
		p.watchJoinRoles(ix.GuildID)
	}
	if len(added)+len(removed) == 0 {
		return ""
	}
	return "✓ Join roles updated: " + p.joinRoleLine(ix.GuildID) + "."
}

// syncChannels recomputes every channel for the jail role (creating it if
// the guild has none) and the island's, and clears member-level denies an
// older release left behind (clearLeftoverMemberOverwrites).
func (p *Plugin) syncChannels(ctx context.Context, guildID string) string {
	p.markVisibilityDirty(guildID)
	_, err := p.resolveJailRole(guildID)
	if err != nil {
		return fmt.Sprintf("✗ Sync completed with errors resolving the jail role: %v. The sweep will retry.", err)
	}
	syncErr := p.reconcileVisibility(guildID, true)
	if syncErr != nil {
		return fmt.Sprintf("✗ Sync completed with errors: %v", syncErr)
	}
	note := "✓ Channels synced: every channel's jail and vacation visibility matches the lists."
	switch n, err := p.clearLeftoverMemberOverwrites(ctx, guildID); {
	case err != nil:
		note += fmt.Sprintf("\n! Could not check for leftover member overwrites from old jails (merlin needs View Audit Log): %v", err)
	case n > 0:
		note += fmt.Sprintf("\nAlso removed %d member overwrite(s) old jails had left on members since released.", n)
	}
	return note
}

// setScript turns a script on or off. Turning one on runs its first
// enforcement now, so anything already defined is put right at once rather
// than up to a sweep later, and the note reads as the warning it is: the
// admin has just handed merlin authority over their own future actions.
func (p *Plugin) setScript(ctx context.Context, ix *core.Ix, name string, enabled bool) string {
	if !slices.Contains(pluginScripts, name) {
		return fmt.Sprintf("✗ Unknown script: %q is not a script of this plugin.", name)
	}
	if p.scripts == nil {
		return "✗ This deployment has no script store."
	}
	if err := p.scripts.SetEnabled(ctx, ix.GuildID, name, enabled); err != nil {
		return fmt.Sprintf("✗ Failed to change script: %v", err)
	}
	if !enabled {
		p.auditScript(ctx, ix, "roles.script_disabled", name)
		return fmt.Sprintf("✓ Script off: `%s`. Whatever it protected is ordinary again; its definitions are kept.", name)
	}
	p.auditScript(ctx, ix, "roles.script_enabled", name)
	switch name {
	case scriptEternalRole:
		if err := p.enforceEternalRoles(ctx, ix.GuildID); err != nil {
			p.log.Error("roles: first enforcement after enabling script", "guild", ix.GuildID, "script", name, "err", err)
		}
	case scriptVacation:
		// Turning the script
		// on is the moment the admin opts into deny-by-default there.
		p.markVisibilityDirty(ix.GuildID)
		if err := p.syncAllVacationOverwrites(ix.GuildID); err != nil {
			return fmt.Sprintf("✗ Vacation enabled, but channel restrictions were not fully applied: %v. The sweep will retry.", err)
		}
	}
	return fmt.Sprintf("! Script on: %s. merlin now overrides admins on what it protects.\n-# %s", name, scriptAbout[name])
}

// scriptAbout is what an admin is told they just turned on.
var scriptAbout = map[string]string{
	scriptJoinRole: "New members get the member role and the join roles 15 seconds after joining, except roles they already hold and nothing for anyone jailed or on vacation. Joins from now on only.",
	scriptVacation: "/roles vacation does what /roles jail does with the island's role as the marker. merlin hides every channel from that role except the vacation list, applying read-only or conversation access to listed channels.",
	scriptEternalRole: "merlin keeps a copy of each eternal role as it was when added: given back if removed, recreated if deleted, " +
		"a fresh copy placed above it if edited. Only the server owner can choose them.",
}

func (p *Plugin) auditScript(ctx context.Context, ix *core.Ix, action, name string) {
	if err := p.audit.Record(ctx, ix.GuildID, actorID(ix), action, "", name); err != nil {
		p.log.Error("roles: audit script toggle failed", "guild", ix.GuildID, "err", err)
	}
}

// setEternalRoles is the eternal roles picker for one member. The
// owner-or-operator check is the real gate (canDefineEternal), on top of the
// action's tier, exactly as eternal-add and eternal-remove had it.
func (p *Plugin) setEternalRoles(ctx context.Context, ix *core.Ix, userID string) string {
	ok, err := p.canDefineEternal(ix.GuildID, actorID(ix))
	if err != nil {
		return "✗ " + err.Error()
	}
	if !ok {
		return "✗ Not allowed: only the server owner (or merlin's operator) can choose eternal roles; admins can only turn the script off."
	}
	added, removed := ix.SelectDiff(panelDefine + "eternal:" + userID)
	var notes []string
	for _, roleID := range added {
		if _, err := p.addEternalRole(ctx, ix.GuildID, userID, roleID, actorID(ix)); err != nil {
			return fmt.Sprintf("✗ Not added: %v", err)
		}
		notes = append(notes, fmt.Sprintf("✓ Eternal role added: %s keeps %s, copied as it is right now.", core.MentionUser(userID), core.MentionRole(roleID)))
	}
	if len(removed) > 0 {
		recs, err := p.store.ListEternalRoles(ctx, ix.GuildID)
		if err != nil {
			return fmt.Sprintf("✗ Not removed: %v", err)
		}
		for _, roleID := range removed {
			idx := slices.IndexFunc(recs, func(r EternalRoleRecord) bool {
				return r.UserID == userID && (r.OriginRoleID == roleID || r.RoleID == roleID)
			})
			if idx < 0 {
				return fmt.Sprintf("✗ Not removed: %s does not keep %s.", core.MentionUser(userID), core.MentionRole(roleID))
			}
			rec := recs[idx]
			if err := p.store.DeleteEternalRole(ctx, ix.GuildID, rec.UserID, rec.OriginRoleID); err != nil {
				return fmt.Sprintf("✗ Not removed: %v", err)
			}
			if err := p.audit.Record(ctx, ix.GuildID, actorID(ix), "roles.eternal_removed", core.MentionRole(rec.RoleID),
				fmt.Sprintf("no longer an eternal role of %s", core.MentionUser(userID))); err != nil {
				p.log.Error("roles: audit eternal-remove failed", "guild", ix.GuildID, "err", err)
			}
			notes = append(notes, fmt.Sprintf("✓ Eternal role removed: %s no longer keeps %s. The role itself is untouched.", core.MentionUser(userID), core.MentionRole(rec.RoleID)))
		}
	}
	if len(added) > 0 {
		if err := p.enforceEternalRoles(ctx, ix.GuildID); err != nil {
			p.log.Error("roles: enforce after eternal-add", "guild", ix.GuildID, "err", err)
		}
	}
	if len(added) > 0 && !p.scriptOn(ctx, ix.GuildID, scriptEternalRole) {
		notes = append(notes, "! The script is **off** here, so nothing is enforced yet.")
	}
	return strings.Join(notes, "\n")
}

// channelList is ids as mentions, or "nothing".
func channelList(ids []string) string {
	if len(ids) == 0 {
		return "nothing"
	}
	out := make([]string, len(ids))
	for n, id := range ids {
		out[n] = core.MentionChannel(id)
	}
	return strings.Join(out, " ")
}

// Help is this plugin's page in /help.
func (p *Plugin) Help() core.Help {
	return core.Help{
		Title: "Roles",
		Line:  "jail, vacation and temporary roles",
		About: "`/roles jail` takes away someone's roles for a while and gives them back on their own when the time is up; leaving and rejoining doesn't shake it. `/roles vacation` is the same thing with a nicer room. `/roles grant` hands out one role, optionally for a while. what a jailed member can still see, the roles new members get, and the scripts all live on `/roles settings`.",
	}
}
