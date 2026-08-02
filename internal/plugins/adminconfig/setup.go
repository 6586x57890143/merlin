package adminconfig

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/settings"
)

// Names /config setup uses for the things it can create — never on its own:
// every one of them sits behind an explicit button on its wizard step, and
// every step offers picking something the guild already has first. A server
// that already keeps a mod-log channel or a moderator role shouldn't end up
// with a bot-made duplicate it never asked for.
const (
	birdAuditLogChannelName = "bird-audit-log"
	birdStatusChannelName   = "bird-status"
	merlinModRoleName       = "Merlin Mod"
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
// Discord caps a user select at 25; this is deliberately lower — standing
// admin access is the most powerful thing this wizard hands out, so the
// picker shouldn't invite bulk-granting it in one careless click.
const maxSetupAdminPicks = 5

// registerSetupComponents wires every button and select menu the wizard
// renders. All of them mutate guild config (or navigate a view that does),
// so they carry the same TierAdmin/actionMutate spec as /config setup
// itself — a component interaction is its own fresh interaction and gets no
// exemption from authorization just because the message it's attached to
// was rendered for someone allowed to see it.
func (p *Plugin) registerSetupComponents() {
	spec := core.PermSpec{Tier: core.TierAdmin, Action: actionMutate}
	p.commands.HandleComponent(p.Name(), setupStepPrefix, spec, p.handleSetupStep)
	p.commands.HandleComponent(p.Name(), setupAuditLogSelectCustomID, spec, p.handleSetupAuditLogSelect)
	p.commands.HandleComponent(p.Name(), setupAuditLogCreateCustomID, spec, p.handleSetupAuditLogCreate)
	p.commands.HandleComponent(p.Name(), setupStatusSelectCustomID, spec, p.handleSetupStatusSelect)
	p.commands.HandleComponent(p.Name(), setupStatusCreateCustomID, spec, p.handleSetupStatusCreate)
	p.commands.HandleComponent(p.Name(), setupModRoleSelectCustomID, spec, p.handleSetupModRoleSelect)
	p.commands.HandleComponent(p.Name(), setupModRoleCreateCustomID, spec, p.handleSetupModRoleCreate)
	p.commands.HandleComponent(p.Name(), setupAdminsSelectCustomID, spec, p.handleSetupAdminsSelect)
}

// handleSetup opens the setup wizard on its first page. The wizard creates
// nothing by itself: each step offers whatever the guild already has
// (channel/role/user pickers) alongside an optional button to have the
// default created, and skipping a step is always allowed. State lives
// entirely in guild settings and is re-read on every render, so there's no
// wizard session to expire, two admins can drive it at once without
// clobbering each other, and re-running it later is a perfectly good "how's
// my setup going" review rather than a first-run-only ritual.
func (p *Plugin) handleSetup(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	embed, components := renderSetupStep(p.settings.GuildSettings(i.GuildID), setupStepWelcome, "")
	if err := core.RespondLandmarkEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("adminconfig: setup response failed", "err", err)
	}
}

func (p *Plugin) handleSetupStep(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	step, err := core.ParsePaginationPage(customID, setupStepPrefix)
	if err != nil {
		p.log.Error("adminconfig: parse setup step", "custom_id", customID, "err", err)
		step = setupStepWelcome
	}
	p.updateSetupStep(s, i, step, "")
}

// updateSetupStep re-renders the wizard in place on the message the
// component interaction arrived on. notice is a one-off line about whatever
// just happened (or failed), shown above the step's own text and gone again
// on the next navigation.
func (p *Plugin) updateSetupStep(s *discordgo.Session, i *discordgo.InteractionCreate, step int, notice string) {
	embed, components := renderSetupStep(p.settings.GuildSettings(i.GuildID), step, notice)
	update := core.UpdateEmbedWithComponents
	if step == setupStepDone {
		update = core.UpdateLandmarkEmbedWithComponents
	}
	if err := update(s, i, embed, components); err != nil {
		p.log.Error("adminconfig: setup step update failed", "step", step, "err", err)
	}
}

// renderSetupStep builds one step's embed and controls purely from the
// guild's current settings — no Plugin state, no wizard session — which is
// what makes the wizard resumable, concurrently drivable, and testable
// without a Discord session.
func renderSetupStep(gs settings.GuildSettings, step int, notice string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	step = min(max(step, 0), setupStepCount-1)

	var title, desc string
	var prompts []discordgo.MessageComponent
	color := core.ColorInfo

	switch step {
	case setupStepWelcome:
		title = "Server setup"
		desc = "Four things are worth setting before putting the bird to work. I'll walk you through them one at a time.\n\n" +
			"Nothing gets created or changed unless you ask for it on the step itself — every step offers what this server already has first, " +
			"and skipping a step is fine; you can come back to it whenever.\n\nHit **Next** to start."

	case setupStepAuditLog:
		title = "Audit log channel"
		desc = fmt.Sprintf("Where config changes, rotations, and moderation actions get recorded. Mod roles are given access to it automatically.\n\nCurrently: **%s**",
			channelStatusText(gs.AuditLogChannelID))
		prompts = []discordgo.MessageComponent{
			setupChannelSelectRow(setupAuditLogSelectCustomID, "Pick an existing channel for the audit log…"),
			setupCreateRow("Create #"+birdAuditLogChannelName+" for me", setupAuditLogCreateCustomID),
		}

	case setupStepStatus:
		title = "Status channel"
		desc = fmt.Sprintf("Where the bird posts operational alerts — a scheduled job failing repeatedly, a rotation it couldn't complete. Quiet when nothing's wrong.\n\nCurrently: **%s**",
			channelStatusText(gs.StatusChannelID))
		prompts = []discordgo.MessageComponent{
			setupChannelSelectRow(setupStatusSelectCustomID, "Pick an existing channel for status alerts…"),
			setupCreateRow("Create #"+birdStatusChannelName+" for me", setupStatusCreateCustomID),
		}

	case setupStepModRole:
		title = "Mod role"
		desc = fmt.Sprintf("Which role counts as a moderator for mod-tier commands (jailing, releasing, running a rotation early). "+
			"Most servers already have one — pick it rather than adding another.\n\nCurrently: **%s**",
			roleListText(gs.ModRoleIDs))
		prompts = []discordgo.MessageComponent{
			setupRoleSelectRow(setupModRoleSelectCustomID, "Pick an existing role to treat as a mod role…"),
			setupCreateRow("Create a “"+merlinModRoleName+"” role for me", setupModRoleCreateCustomID),
		}

	case setupStepAdmins:
		title = "Admins"
		desc = fmt.Sprintf("Who can change the bird's own configuration — permissions, plugins, rotation settings. "+
			"Anyone holding Discord's **Administrator** permission already counts, so this list is only for people who should have admin access without it.\n\nCurrently: **%s**",
			userListText(gs.AdminUserIDs))
		prompts = []discordgo.MessageComponent{setupUserSelectRow(setupAdminsSelectCustomID, "Pick who should have standing admin access…")}

	case setupStepDone:
		color = core.ColorSuccess
		title = "Setup complete"
		desc = "That's everything worth setting up front. Anything you skipped can be filled in later — `/config setup` picks up exactly where this left off."
	}

	fields := []*discordgo.MessageEmbedField{{Name: "Setup so far", Value: setupChecklist(gs)}}
	if step == setupStepDone {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Where to go next", Value: strings.Join([]string{
			"Assign your mod role to the people who should actually have it.",
			"`/rotation configure add` to start rotating a channel.",
			"`/roles configure allow-channel` to choose what jailed members can still see.",
			"`/config permissions` to loosen or tighten any single command.",
		}, "\n")})
	}
	if notice != "" {
		desc = notice + "\n\n" + desc
	}

	newEmbed := core.NewEmbed
	if step == setupStepDone {
		newEmbed = core.NewLandmarkEmbed
	}
	return newEmbed(color, title, desc, fields...), append(prompts, setupNavRow(step))
}

// setupNavRow is the wizard's Back/Next control, mirroring core.PaginationRow's
// shape and CustomID encoding (there's no server-side wizard session to
// consult, so the target step rides along in the button's own ID) but
// labelled as steps rather than pages, and always rendered — a wizard whose
// controls vanished on a single-step view would be a dead end.
func setupNavRow(step int) discordgo.MessageComponent {
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{
			Label:    "◀ Back",
			Style:    discordgo.SecondaryButton,
			CustomID: core.PaginationCustomID(setupStepPrefix, step-1),
			Disabled: step <= 0,
		},
		discordgo.Button{
			Label:    fmt.Sprintf("Step %d/%d", step+1, setupStepCount),
			Style:    discordgo.SecondaryButton,
			CustomID: setupStepPrefix + "noop",
			Disabled: true,
		},
		discordgo.Button{
			Label:    "Next ▶",
			Style:    discordgo.PrimaryButton,
			CustomID: core.PaginationCustomID(setupStepPrefix, step+1),
			Disabled: step >= setupStepCount-1,
		},
	}}
}

// Each picker gets its own ActionRow because Discord won't let a select menu
// share a row with anything else.
func setupChannelSelectRow(customID, placeholder string) discordgo.MessageComponent {
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
		MenuType:     discordgo.ChannelSelectMenu,
		CustomID:     customID,
		Placeholder:  placeholder,
		ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText},
	}}}
}

func setupRoleSelectRow(customID, placeholder string) discordgo.MessageComponent {
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
		MenuType:    discordgo.RoleSelectMenu,
		CustomID:    customID,
		Placeholder: placeholder,
	}}}
}

func setupUserSelectRow(customID, placeholder string) discordgo.MessageComponent {
	minValues := 1
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
		MenuType:    discordgo.UserSelectMenu,
		CustomID:    customID,
		Placeholder: placeholder,
		MinValues:   &minValues,
		MaxValues:   maxSetupAdminPicks,
	}}}
}

func setupCreateRow(label, customID string) discordgo.MessageComponent {
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: label, Style: discordgo.SecondaryButton, CustomID: customID},
	}}
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
	return fmt.Sprintf("%s **%s** — %s", mark, label, value)
}

// channelStatusText renders a possibly-unset channel ID — "not set" instead
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
	return mentionListText(userIDs, "<@%s>", "none set — Discord Administrators still count")
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
// re-renders — advancing a step on success (the picked value is already
// visible in the next step's checklist, so there's nothing to confirm by
// staying put) and holding position with a notice on failure, so a mistyped
// permission or a Discord error doesn't drop the admin out of setup.

func (p *Plugin) handleSetupAuditLogSelect(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	p.setupChannelPicked(ctx, s, i, setupStepAuditLog, "Audit log", p.settings.SetAuditLogChannel)
}

func (p *Plugin) handleSetupStatusSelect(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	p.setupChannelPicked(ctx, s, i, setupStepStatus, "Status channel", p.settings.SetStatusChannel)
}

func (p *Plugin) setupChannelPicked(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, step int, label string, set func(ctx context.Context, guildID, channelID string) error) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return
	}
	channelID := values[0]
	if err := set(ctx, i.GuildID, channelID); err != nil {
		p.updateSetupStep(s, i, step, fmt.Sprintf("⚠️ Couldn't save that channel: %v", err))
		return
	}
	p.grantModRolesChannelAccess(s, i.GuildID)
	p.audit(ctx, i, "config.setup", "", fmt.Sprintf("%s=<#%s> (existing)", label, channelID))
	p.updateSetupStep(s, i, step+1, fmt.Sprintf("✅ %s is now <#%s>.", label, channelID))
}

func (p *Plugin) handleSetupAuditLogCreate(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	p.setupChannelCreate(ctx, s, i, setupStepAuditLog, "Audit log", birdAuditLogChannelName, p.settings.SetAuditLogChannel)
}

func (p *Plugin) handleSetupStatusCreate(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	p.setupChannelCreate(ctx, s, i, setupStepStatus, "Status channel", birdStatusChannelName, p.settings.SetStatusChannel)
}

func (p *Plugin) setupChannelCreate(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, step int, label, channelName string, set func(ctx context.Context, guildID, channelID string) error) {
	ch, err := s.GuildChannelCreateComplex(i.GuildID, discordgo.GuildChannelCreateData{
		Name:                 channelName,
		PermissionOverwrites: core.DenyEveryoneExceptBot(nil, i.GuildID, s.State.User.ID, botChannelAllow),
	})
	if err != nil {
		p.updateSetupStep(s, i, step, fmt.Sprintf("⚠️ Couldn't create #%s: %v", channelName, err))
		return
	}
	if err := set(ctx, i.GuildID, ch.ID); err != nil {
		p.updateSetupStep(s, i, step, fmt.Sprintf("⚠️ #%s was created but I couldn't save it: %v", channelName, err))
		return
	}
	p.grantModRolesChannelAccess(s, i.GuildID)
	p.audit(ctx, i, "config.setup", "", fmt.Sprintf("%s=<#%s> (created)", label, ch.ID))
	p.updateSetupStep(s, i, step+1, fmt.Sprintf("✅ Created <#%s> and set it as the %s.", ch.ID, strings.ToLower(label)))
}

func (p *Plugin) handleSetupModRoleSelect(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return
	}
	p.setupAddModRole(ctx, s, i, values[0], fmt.Sprintf("✅ <@&%s> now counts as a mod role.", values[0]))
}

func (p *Plugin) handleSetupModRoleCreate(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	mentionable := false
	role, err := s.GuildRoleCreate(i.GuildID, &discordgo.RoleParams{Name: merlinModRoleName, Mentionable: &mentionable})
	if err != nil {
		p.updateSetupStep(s, i, setupStepModRole, fmt.Sprintf("⚠️ Couldn't create the %s role: %v", merlinModRoleName, err))
		return
	}
	p.setupAddModRole(ctx, s, i, role.ID, fmt.Sprintf("✅ Created <@&%s> — assign it to your moderators.", role.ID))
}

func (p *Plugin) setupAddModRole(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, roleID, notice string) {
	if err := p.settings.AddModRole(ctx, i.GuildID, roleID); err != nil {
		p.updateSetupStep(s, i, setupStepModRole, fmt.Sprintf("⚠️ Couldn't save that mod role: %v", err))
		return
	}
	p.grantModRoleChannelAccess(s, i.GuildID, roleID)
	p.audit(ctx, i, "config.setup", "", "mod_role="+roleID)
	p.updateSetupStep(s, i, setupStepModRole+1, notice)
}

func (p *Plugin) handleSetupAdminsSelect(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return
	}
	for _, userID := range values {
		if err := p.settings.AddAdmin(ctx, i.GuildID, userID); err != nil {
			p.updateSetupStep(s, i, setupStepAdmins, fmt.Sprintf("⚠️ Couldn't save <@%s> as an admin: %v", userID, err))
			return
		}
		p.audit(ctx, i, "config.setup", "", "admin="+userID)
	}
	p.updateSetupStep(s, i, setupStepAdmins+1, fmt.Sprintf("✅ Added %s as admin.", mentionListText(values, "<@%s>", "")))
}

// grantModRolesChannelAccess re-grants every mod role access to whichever of
// the audit/status channels are configured. Run after either channel
// changes, not just when a mod role is added: a mod role configured before
// the channels existed would otherwise never get access once they show up.
func (p *Plugin) grantModRolesChannelAccess(s *discordgo.Session, guildID string) {
	for _, roleID := range p.settings.GuildSettings(guildID).ModRoleIDs {
		p.grantModRoleChannelAccess(s, guildID, roleID)
	}
}

// botChannelAllow is what the bot needs on the audit-log/status channels:
// view/post/embed, all bits it already holds via @everyone's own guild
// defaults in a typical guild — unlike rotation's staging channel, this
// never needs Manage Messages, so there's no risk of Discord rejecting the
// overwrite for granting a bit the bot doesn't actually hold (see
// core.DenyEveryoneExceptBot's doc comment for why that matters).
const botChannelAllow = discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionEmbedLinks
