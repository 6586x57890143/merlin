package adminconfig

import (
	"context"
	"fmt"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/ui"
	"github.com/6586x57890143/merlin/internal/settings"
)

// Names /config setup uses for the things it can create, never on its own:
// every one of them sits behind an explicit button on its wizard step, and
// every step offers picking something the guild already has first. A server
// that already keeps a mod-log channel or a moderator role shouldn't end up
// with a bot-made duplicate it never asked for.
const (
	birdAuditLogChannelName = "bird-audit-log"
	birdStatusChannelName   = "bird-status"
	merlinModRoleName       = "merlin Mod"
)

// The wizard's steps, in order. setupStepCount is the count, not a step.
const (
	setupStepWelcome = iota
	setupStepAuditLog
	setupStepStatus
	setupStepModRole
	setupStepAdmins
	setupStepDone
	setupStepCount
)

// Component CustomIDs, all namespaced under config:setup: (spec.MD §4a).
// setupStepPrefix is a prefix (the target step number is appended, exactly
// like core's list pagination); the rest are whole IDs.
const (
	setupStepPrefix = "config:setup:step:"

	setupAuditLogSelectCustomID = "config:setup:auditlog:select"
	setupAuditLogCreateCustomID = "config:setup:auditlog:create"
	setupStatusSelectCustomID   = "config:setup:status:select"
	setupStatusCreateCustomID   = "config:setup:status:create"
	setupModRoleSelectCustomID  = "config:setup:modrole:select"
	setupModRoleCreateCustomID  = "config:setup:modrole:create"
	setupAdminsSelectCustomID   = "config:setup:admins:select"
)

// maxSetupAdminPicks is how many users one pass of the admins step can add.
// Discord caps a user select at 25; this is deliberately lower, since standing
// admin access is the most powerful thing this wizard hands out, so the
// picker shouldn't invite bulk-granting it in one careless click.
const maxSetupAdminPicks = 5

// registerSetupComponents wires every button and select menu the wizard
// renders. All of them mutate guild config (or navigate a view that does),
// so they carry the same TierAdmin/actionMutate spec as /config setup
// itself: a component interaction is its own fresh interaction and gets no
// exemption from authorization just because the message it's attached to
// was rendered for someone allowed to see it.
func (p *Plugin) registerSetupComponents() {
	spec := core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}
	p.commands.HandleComponentIx(p.Name(), setupStepPrefix, spec, p.handleSetupStep)
	p.commands.HandleComponentIx(p.Name(), setupAuditLogSelectCustomID, spec, p.handleSetupAuditLogSelect)
	p.commands.HandleComponentIx(p.Name(), setupAuditLogCreateCustomID, spec, p.handleSetupAuditLogCreate)
	p.commands.HandleComponentIx(p.Name(), setupStatusSelectCustomID, spec, p.handleSetupStatusSelect)
	p.commands.HandleComponentIx(p.Name(), setupStatusCreateCustomID, spec, p.handleSetupStatusCreate)
	p.commands.HandleComponentIx(p.Name(), setupModRoleSelectCustomID, spec, p.handleSetupModRoleSelect)
	p.commands.HandleComponentIx(p.Name(), setupModRoleCreateCustomID, spec, p.handleSetupModRoleCreate)
	p.commands.HandleComponentIx(p.Name(), setupAdminsSelectCustomID, spec, p.handleSetupAdminsSelect)
}

// handleSetup opens the setup wizard on its first page. The wizard creates
// nothing by itself: each step offers whatever the guild already has
// (channel/role/user pickers) alongside an optional button to have the
// default created, and skipping a step is always allowed. State lives
// entirely in guild settings and is re-read on every render, so there's no
// wizard session to expire, two admins can drive it at once without
// clobbering each other, and re-running it later is a perfectly good "how's
// my setup going" review rather than a first-run-only ritual.
func (p *Plugin) handleSetup(ctx context.Context, ix *core.Ix) {
	embed, components := renderSetupStep(p.settings.GuildSettings(ix.GuildID), setupStepWelcome, "")
	if err := ix.RespondEmbedWithComponents(embed, components); err != nil {
		p.log.Error("adminconfig: setup response failed", "err", err)
	}
}

func (p *Plugin) handleSetupStep(ctx context.Context, ix *core.Ix, customID string) {
	step, err := core.ParsePaginationPage(customID, setupStepPrefix)
	if err != nil {
		p.log.Error("adminconfig: parse setup step", "custom_id", customID, "err", err)
		step = setupStepWelcome
	}
	p.updateSetupStep(ix, step, "")
}

// updateSetupStep re-renders the wizard in place on the message the
// component interaction arrived on. notice is a one-off line about whatever
// just happened (or failed), shown above the step's own text and gone again
// on the next navigation.
func (p *Plugin) updateSetupStep(ix *core.Ix, step int, notice string) {
	embed, components := renderSetupStep(p.settings.GuildSettings(ix.GuildID), step, notice)
	if err := ix.UpdateEmbedWithComponents(embed, components); err != nil {
		p.log.Error("adminconfig: setup step update failed", "step", step, "err", err)
	}
}

// renderSetupStep builds one step's embed and controls purely from the
// guild's current settings (no Plugin state, no wizard session), which is
// what makes the wizard resumable, concurrently drivable, and testable
// without a Discord session.
func renderSetupStep(gs settings.GuildSettings, step int, notice string) (discord.Embed, []discord.LayoutComponent) {
	step = min(max(step, 0), setupStepCount-1)

	var title, desc string
	var prompts []discord.LayoutComponent
	color := core.ColorInfo

	switch step {
	case setupStepWelcome:
		title = "Server setup"
		desc = "Four things worth setting before I get to work. I'll walk you through them one at a time.\n\n" +
			"I won't create or change anything unless you ask for it on the step itself. Every step offers what this server already has first, " +
			"and skipping a step is fine; you can come back to it whenever.\n\nHit **Next** to start."

	case setupStepAuditLog:
		title = "Audit log channel"
		desc = fmt.Sprintf("Where I write down config changes, rotations, and moderation actions. Your mod roles get access automatically.\n\nCurrently: **%s**",
			channelStatusText(gs.AuditLogChannelID))
		prompts = []discord.LayoutComponent{
			setupChannelSelectRow(setupAuditLogSelectCustomID, "Pick an existing channel for the audit log..."),
			setupCreateRow("Create #"+birdAuditLogChannelName+" for me", setupAuditLogCreateCustomID),
		}

	case setupStepStatus:
		title = "Status channel"
		desc = fmt.Sprintf("Where I flag operational trouble: a scheduled job failing over and over, a rotation I couldn't finish. Quiet when nothing's wrong.\n\nCurrently: **%s**",
			channelStatusText(gs.StatusChannelID))
		prompts = []discord.LayoutComponent{
			setupChannelSelectRow(setupStatusSelectCustomID, "Pick an existing channel for status alerts..."),
			setupCreateRow("Create #"+birdStatusChannelName+" for me", setupStatusCreateCustomID),
		}

	case setupStepModRole:
		title = "Mod role"
		desc = fmt.Sprintf("Which role counts as a moderator for mod-tier commands (jailing, releasing, running a rotation early). "+
			"Most servers already have one, so pick it rather than adding another.\n\nCurrently: **%s**",
			roleListText(gs.ModRoleIDs))
		prompts = []discord.LayoutComponent{
			setupRoleSelectRow(setupModRoleSelectCustomID, "Pick an existing role to treat as a mod role..."),
			setupCreateRow("Create a "+merlinModRoleName+" role for me", setupModRoleCreateCustomID),
		}

	case setupStepAdmins:
		title = "Admins"
		desc = fmt.Sprintf("Who can change my own configuration: permissions, plugins, rotation settings. "+
			"Anyone holding Discord's **Administrator** permission already counts, so this list is just for people who should have admin access without it.\n\nCurrently: **%s**",
			userListText(gs.AdminUserIDs))
		prompts = []discord.LayoutComponent{setupUserSelectRow(setupAdminsSelectCustomID, "Pick who should have standing admin access...")}

	case setupStepDone:
		color = core.ColorSuccess
		title = "Setup complete"
		desc = "That's everything worth setting up front. Anything you skipped can be filled in later; `/config setup` picks up exactly where this left off."
	}

	fields := []discord.EmbedField{{Name: "Setup so far", Value: setupChecklist(gs)}}
	if step == setupStepDone {
		fields = append(fields, discord.EmbedField{Name: "Where to go next", Value: strings.Join([]string{
			"Assign your mod role to the people who should actually have it.",
			"`/rotation` to start rotating a channel.",
			"`/roles settings` to choose what jailed members can still see.",
			"`/config` to change any of this later, or loosen or tighten a single command under Permissions.",
		}, "\n")})
	}
	if notice != "" {
		desc = notice + "\n\n" + desc
	}

	newEmbed := core.Embed
	if step == setupStepDone {
		newEmbed = core.LandmarkEmbed
	}
	return newEmbed(color, title, desc, fields...), append(prompts, setupNavRow(step))
}

// setupNavRow is the wizard's Back/Next control, mirroring core.PaginationRow's
// shape and CustomID encoding (there's no server-side wizard session to
// consult, so the target step rides along in the button's own ID) but
// labelled as steps rather than pages, and always rendered: a wizard whose
// controls vanished on a single-step view would be a dead end.
func setupNavRow(step int) discord.LayoutComponent {
	return ui.Row(
		ui.Button("◀ Back", core.PaginationCustomID(setupStepPrefix, step-1), discord.ButtonStyleSecondary, step <= 0),
		ui.Button(fmt.Sprintf("Step %d/%d", step+1, setupStepCount), setupStepPrefix+"noop", discord.ButtonStyleSecondary, true),
		ui.Button("Next ▶", core.PaginationCustomID(setupStepPrefix, step+1), discord.ButtonStylePrimary, step >= setupStepCount-1),
	)
}

// Each picker gets its own ActionRow because Discord won't let a select menu
// share a row with anything else.
func setupChannelSelectRow(customID, placeholder string) discord.LayoutComponent {
	return ui.Row(discord.ChannelSelectMenuComponent{
		CustomID: customID, Placeholder: placeholder, ChannelTypes: []discord.ChannelType{discord.ChannelTypeGuildText},
	})
}

func setupRoleSelectRow(customID, placeholder string) discord.LayoutComponent {
	return ui.Row(discord.RoleSelectMenuComponent{CustomID: customID, Placeholder: placeholder})
}

func setupUserSelectRow(customID, placeholder string) discord.LayoutComponent {
	minValues := 1
	return ui.Row(discord.UserSelectMenuComponent{CustomID: customID, Placeholder: placeholder, MinValues: &minValues, MaxValues: maxSetupAdminPicks})
}

func setupCreateRow(label, customID string) discord.LayoutComponent {
	return ui.Row(ui.Button(label, customID, discord.ButtonStyleSecondary, false))
}

func setupChecklist(gs settings.GuildSettings) string {
	return strings.Join([]string{
		setupChecklistLine(gs.AuditLogChannelID != "", "Audit log", channelStatusText(gs.AuditLogChannelID)),
		setupChecklistLine(gs.StatusChannelID != "", "Status channel", channelStatusText(gs.StatusChannelID)),
		setupChecklistLine(len(gs.ModRoleIDs) > 0, "Mod role", roleListText(gs.ModRoleIDs)),
		setupChecklistLine(len(gs.AdminUserIDs) > 0, "Admins", userListText(gs.AdminUserIDs)),
	}, "\n")
}

func setupChecklistLine(done bool, label, value string) string {
	mark := "◻️"
	if done {
		mark = "✅"
	}
	return fmt.Sprintf("%s **%s**: %s", mark, label, value)
}

// channelStatusText renders a possibly-unset channel ID: "not set" instead
// of a broken <#> mention when there's nothing to link to.
func channelStatusText(channelID string) string {
	if channelID == "" {
		return "not set"
	}
	return fmt.Sprintf("<#%s>", channelID)
}

func roleListText(roleIDs []string) string {
	return mentionListText(roleIDs, "<@&%s>", "none set")
}

func userListText(userIDs []string) string {
	return mentionListText(userIDs, "<@%s>", "none set, Discord Administrators still count")
}

// mentionListText joins IDs as mentions, truncating past a handful so a
// server with a long list can't blow past the embed field's length limit.
func mentionListText(ids []string, format, empty string) string {
	if len(ids) == 0 {
		return empty
	}
	const maxShown = 5
	shown := ids
	suffix := ""
	if len(shown) > maxShown {
		shown, suffix = shown[:maxShown], fmt.Sprintf(" +%d more", len(ids)-maxShown)
	}
	mentions := make([]string, len(shown))
	for idx, id := range shown {
		mentions[idx] = fmt.Sprintf(format, id)
	}
	return strings.Join(mentions, ", ") + suffix
}

// The wizard's action handlers. Each one applies its change and then
// re-renders, advancing a step on success (the picked value is already
// visible in the next step's checklist, so there's nothing to confirm by
// staying put) and holding position with a notice on failure, so a mistyped
// permission or a Discord error doesn't drop the admin out of setup.

func (p *Plugin) handleSetupAuditLogSelect(ctx context.Context, ix *core.Ix, customID string) {
	p.setupChannelPicked(ctx, ix, setupStepAuditLog, "Audit log", p.settings.SetAuditLogChannel)
}

func (p *Plugin) handleSetupStatusSelect(ctx context.Context, ix *core.Ix, customID string) {
	p.setupChannelPicked(ctx, ix, setupStepStatus, "Status channel", p.settings.SetStatusChannel)
}

func (p *Plugin) setupChannelPicked(ctx context.Context, ix *core.Ix, step int, label string, set func(ctx context.Context, guildID, channelID string) error) {
	values := ix.Values()
	if len(values) == 0 {
		return
	}
	channelID := values[0]
	if err := set(ctx, ix.GuildID, channelID); err != nil {
		p.updateSetupStep(ix, step, fmt.Sprintf("⚠️ Couldn't save that channel: %v", err))
		return
	}
	p.grantModRolesChannelAccess(ix.GuildID)
	p.audit(ctx, ix, "config.setup", "", fmt.Sprintf("%s=<#%s> (existing)", label, channelID))

	// Picking an existing channel deliberately changes no permissions on it,
	// unlike the "create it for me" path, which denies @everyone up front.
	// That is the right default (this is a channel the guild already uses
	// for something, and silently locking the server out of it would be a
	// surprise), but it does mean an admin can point the audit log at a
	// public channel and publish every jail and config change to the whole
	// server. Say so at the moment the choice is made, and accept it if they
	// meant it. A failed lookup says nothing either way, so it stays quiet
	// rather than crying wolf; /config status re-checks this on every run.
	notice := fmt.Sprintf("✅ %s is now <#%s>.", label, channelID)
	if ch, err := p.guildChannel(channelID); err == nil && everyoneCanRead(ch) {
		notice += " ⚠️ Everyone can read that channel, so anything recorded there is public."
	}
	p.updateSetupStep(ix, step+1, notice)
}

func (p *Plugin) handleSetupAuditLogCreate(ctx context.Context, ix *core.Ix, customID string) {
	p.setupChannelCreate(ctx, ix, setupStepAuditLog, "Audit log", birdAuditLogChannelName, p.settings.SetAuditLogChannel)
}

func (p *Plugin) handleSetupStatusCreate(ctx context.Context, ix *core.Ix, customID string) {
	p.setupChannelCreate(ctx, ix, setupStepStatus, "Status channel", birdStatusChannelName, p.settings.SetStatusChannel)
}

func (p *Plugin) setupChannelCreate(ctx context.Context, ix *core.Ix, step int, label, channelName string, set func(ctx context.Context, guildID, channelID string) error) {
	gid, err := snowflake.Parse(ix.GuildID)
	if err != nil {
		p.updateSetupStep(ix, step, fmt.Sprintf("⚠️ Couldn't create #%s: %v", channelName, err))
		return
	}
	created, err := p.rest.CreateGuildChannel(gid, discord.GuildTextChannelCreate{
		Name:                 channelName,
		PermissionOverwrites: core.DenyEveryone(nil, gid, p.botID, botChannelAllow),
	})
	if err != nil {
		p.updateSetupStep(ix, step, fmt.Sprintf("⚠️ Couldn't create #%s: %v", channelName, err))
		return
	}
	ch := struct{ ID string }{created.ID().String()}
	if err := set(ctx, ix.GuildID, ch.ID); err != nil {
		p.updateSetupStep(ix, step, fmt.Sprintf("⚠️ #%s was created but I couldn't save it: %v", channelName, err))
		return
	}
	p.grantModRolesChannelAccess(ix.GuildID)
	p.audit(ctx, ix, "config.setup", "", fmt.Sprintf("%s=<#%s> (created)", label, ch.ID))
	p.updateSetupStep(ix, step+1, fmt.Sprintf("✅ Created <#%s> and set it as the %s.", ch.ID, strings.ToLower(label)))
}

func (p *Plugin) handleSetupModRoleSelect(ctx context.Context, ix *core.Ix, customID string) {
	values := ix.Values()
	if len(values) == 0 {
		return
	}
	p.setupAddModRole(ctx, ix, values[0], fmt.Sprintf("✅ <@&%s> now counts as a mod role.", values[0]))
}

func (p *Plugin) handleSetupModRoleCreate(ctx context.Context, ix *core.Ix, customID string) {
	gid, err := snowflake.Parse(ix.GuildID)
	if err != nil {
		p.updateSetupStep(ix, setupStepModRole, fmt.Sprintf("⚠️ Couldn't create the %s role: %v", merlinModRoleName, err))
		return
	}
	// Not mentionable, which is Discord's default when unsaid.
	created, err := p.rest.CreateRole(gid, discord.RoleCreate{Name: merlinModRoleName})
	if err != nil {
		p.updateSetupStep(ix, setupStepModRole, fmt.Sprintf("⚠️ Couldn't create the %s role: %v", merlinModRoleName, err))
		return
	}
	roleID := created.ID.String()
	p.setupAddModRole(ctx, ix, roleID, fmt.Sprintf("✅ Created <@&%s>. Assign it to your moderators.", roleID))
}

func (p *Plugin) setupAddModRole(ctx context.Context, ix *core.Ix, roleID, notice string) {
	if err := p.settings.AddModRole(ctx, ix.GuildID, roleID); err != nil {
		p.updateSetupStep(ix, setupStepModRole, fmt.Sprintf("⚠️ Couldn't save that mod role: %v", err))
		return
	}
	p.grantModRoleChannelAccess(ix.GuildID, roleID)
	p.audit(ctx, ix, "config.setup", "", "mod_role="+core.MentionRole(roleID))
	p.updateSetupStep(ix, setupStepModRole+1, notice)
}

func (p *Plugin) handleSetupAdminsSelect(ctx context.Context, ix *core.Ix, customID string) {
	values := ix.Values()
	if len(values) == 0 {
		return
	}
	for _, userID := range values {
		if err := p.settings.AddAdmin(ctx, ix.GuildID, userID); err != nil {
			p.updateSetupStep(ix, setupStepAdmins, fmt.Sprintf("⚠️ Couldn't save <@%s> as an admin: %v", userID, err))
			return
		}
		p.audit(ctx, ix, "config.setup", "", "admin="+core.MentionUser(userID))
	}
	p.updateSetupStep(ix, setupStepAdmins+1, fmt.Sprintf("✅ Added %s as admin.", mentionListText(values, "<@%s>", "")))
}

// grantModRolesChannelAccess re-grants every mod role access to whichever of
// the audit/status channels are configured. Run after either channel
// changes, not just when a mod role is added: a mod role configured before
// the channels existed would otherwise never get access once they show up.
func (p *Plugin) grantModRolesChannelAccess(guildID string) {
	// One call, not one per role: grantRolesChannelAccess reads each channel
	// once and writes only the roles actually missing the grant, so re-running
	// this on an already-correct guild costs no permission writes and no
	// Discord audit-log entries.
	p.grantRolesChannelAccess(guildID, p.settings.GuildSettings(guildID).ModRoleIDs)
}

// botChannelAllow is what the bot needs on the audit-log/status channels:
// view/post/embed, all bits it already holds via @everyone's own guild
// defaults in a typical guild. Unlike rotation's staging channel, this
// never needs Manage Messages, so there's no risk of Discord rejecting the
// overwrite for granting a bit the bot doesn't actually hold (see
// core.DenyEveryone's doc comment for why that matters).
const botChannelAllow = discord.PermissionViewChannel | discord.PermissionSendMessages | discord.PermissionEmbedLinks
