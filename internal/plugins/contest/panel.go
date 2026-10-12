package contest

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// /contest settings is one panel (core.Panel) for what used to be the four
// /contest configure subcommands: who contests are for, where they are
// announced and filed, how many picks each member gets, and the resync of a
// running contest's forum. Running a contest stays commands. Every control
// is contest.configure, as every configure leaf was.
const panelSet = "contest:panel:set:"

var configureSpec = core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}

func (p *Plugin) registerPanel() {
	p.commands.HandleComponent(p.Name(), panelSet, configureSpec, p.handlePanelSet)
}

func (p *Plugin) handleSettings(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.RespondPanel(s, i, p.panel(ctx, i, "")); err != nil {
		p.log.Error("contest: panel response failed", "err", err)
	}
}

func (p *Plugin) panel(ctx context.Context, i *discordgo.InteractionCreate, note string) core.Panel {
	can := p.commands.Perms().Allows(i, configureSpec)
	pn := core.Panel{Color: core.ColorInfo, Title: "Contests", Note: note,
		Line:   "Art contests: entries in a forum, a gallery, a vote",
		Footer: "Running one stays commands: /contest new, advance, cancel, status. Every change here is audited."}
	cfg, err := p.store.GetConfig(ctx, i.GuildID)
	if err != nil {
		pn.Color = core.ColorError
		pn.Body = []discordgo.MessageComponent{core.Text("✗ Couldn't read the setup: " + err.Error())}
		return pn
	}
	// The gate leads, and an ungated guild reads as a warning rather than a
	// blank, because it is the one setting here that stops /contest new.
	gate := "! **Nobody**, so `/contest new` will refuse. Pick a channel to copy, or roles."
	switch {
	case len(cfg.AccessRoleIDs) > 0:
		gate = "These roles: " + roleList(cfg.AccessRoleIDs)
	case cfg.GateChannelID != "":
		gate = "Whoever can see " + core.MentionChannel(cfg.GateChannelID)
	default:
		pn.Color = core.ColorWarning
	}
	picks := make([]discordgo.SelectMenuOption, 0, maxPicks)
	for n := 1; n <= maxPicks; n++ {
		picks = append(picks, discordgo.SelectMenuOption{Label: fmt.Sprintf("%d pick(s) per member", n), Value: strconv.Itoa(n), Default: n == cfg.DefaultMaxVotes})
	}
	anyChannel := []discordgo.ChannelType{discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildForum, discordgo.ChannelTypeGuildNews, discordgo.ChannelTypeGuildCategory}
	pn.Body = []discordgo.MessageComponent{
		core.Text("**Who contests are for**\n" + gate),
		// A channel rather than a role list, because a role list is a second
		// copy of a fact the server already states, and a second copy goes
		// stale. Access roles are the escape hatch, and win over it.
		core.Row(core.Clearable(core.ChannelSelect(panelSet+"gate", "Copy who can see this channel", cfg.GateChannelID, !can, anyChannel...))),
		core.Row(core.EntitySelect(discordgo.RoleSelectMenu, panelSet+"access", "Or name the roles outright", cfg.AccessRoleIDs, core.MaxSelectValues, !can)),
		core.Row(core.EntitySelect(discordgo.RoleSelectMenu, panelSet+"media", "Roles that may attach files, if kept apart", cfg.MediaRoleIDs, core.MaxSelectValues, !can)),
		core.Text("**Announcements** go to " + orNone(core.MentionChannel(cfg.AnnounceChannelID), "wherever /contest new is run") +
			"; **forums** are made in " + orNone(core.MentionChannel(cfg.ForumCategoryID), "no category") + "."),
		core.Row(core.Clearable(core.ChannelSelect(panelSet+"announce", "Announcement channel", cfg.AnnounceChannelID, !can, discordgo.ChannelTypeGuildText))),
		core.Row(core.Clearable(core.ChannelSelect(panelSet+"category", "Category for contest forums", cfg.ForumCategoryID, !can, discordgo.ChannelTypeGuildCategory))),
		core.Row(core.StringSelect(panelSet+"picks", "Picks per member", picks, false, !can)),
		core.Switch("A running contest keeps the permissions it was made with until its forum is resynced.",
			core.Button("Resync forum", panelSet+"sync", discordgo.SecondaryButton, !can)),
	}
	return pn
}

func (p *Plugin) handlePanelSet(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	verb := strings.TrimPrefix(customID, panelSet)
	if verb == "sync" {
		// Reads a channel, resolves every role, and may write a whole
		// overwrite list. None of that fits in Discord's three seconds.
		if err := core.DeferPanelUpdate(s, i); err != nil {
			p.log.Error("contest: defer sync-forum", "err", err)
			return
		}
		note := p.syncRunningForum(ctx, i)
		if err := core.FollowUpPanel(s, i, p.panel(ctx, i, note)); err != nil {
			p.log.Error("contest: panel follow-up failed", "err", err)
		}
		return
	}
	note := p.applySetting(ctx, i, verb)
	if err := core.UpdatePanel(s, i, p.panel(ctx, i, note)); err != nil {
		p.log.Error("contest: panel update failed", "err", err)
	}
}

// applySetting is every picker. One read-modify-write of the config and one
// audit line with the before and after, as configure set and access-role
// wrote.
func (p *Plugin) applySetting(ctx context.Context, i *discordgo.InteractionCreate, verb string) string {
	cfg, err := p.store.GetConfig(ctx, i.GuildID)
	if err != nil {
		return "✗ Couldn't read the setup: " + err.Error()
	}
	old := describeConfig(cfg)
	switch verb {
	case "gate":
		cfg.GateChannelID = core.FirstValue(i)
	case "announce":
		cfg.AnnounceChannelID = core.FirstValue(i)
	case "category":
		cfg.ForumCategoryID = core.FirstValue(i)
	case "picks":
		n, err := strconv.Atoi(core.FirstValue(i))
		if err != nil || n < 1 || n > maxPicks {
			return fmt.Sprintf("✗ Picks are 1 to %d.", maxPicks)
		}
		cfg.DefaultMaxVotes = n
	case "access", "media":
		list := &cfg.AccessRoleIDs
		if verb == "media" {
			list = &cfg.MediaRoleIDs
		}
		added, removed := core.SelectDiff(i, panelSet+verb)
		// @everyone is not an access role. Whether everybody sees contests
		// is what the copied channel answers, and taking it here would be a
		// second way to say it that the mirror would then disagree with.
		if slices.Contains(added, i.GuildID) {
			return "✗ @everyone isn't an access role: copy a channel everybody can see instead."
		}
		for _, id := range added {
			if !slices.Contains(*list, id) {
				*list = append(*list, id)
			}
		}
		*list = slices.DeleteFunc(*list, func(id string) bool { return slices.Contains(removed, id) })
	default:
		return "✗ That control is out of date. Run `/contest settings` again."
	}
	if err := p.store.SetConfig(ctx, cfg); err != nil {
		return "✗ Couldn't save the setup: " + err.Error()
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "contest.configured", old, describeConfig(cfg)); err != nil {
		p.log.Error("contest: audit configure", "guild", i.GuildID, "err", err)
	}
	return "✓ Saved. New contests use this; a running one keeps its permissions until its forum is resynced."
}

func describeConfig(cfg Config) string {
	return fmt.Sprintf("announce=%s category=%s picks=%d gate=%s access=%s media=%s",
		core.MentionChannel(cfg.AnnounceChannelID), core.MentionChannel(cfg.ForumCategoryID), cfg.DefaultMaxVotes,
		core.MentionChannel(cfg.GateChannelID), roleList(cfg.AccessRoleIDs), roleList(cfg.MediaRoleIDs))
}

// syncRunningForum reapplies the gate to a running contest's forum. Without
// it, configuring the gate while a contest is live changes nothing, since
// overwrites are written when the forum is created.
func (p *Plugin) syncRunningForum(ctx context.Context, i *discordgo.InteractionCreate) string {
	cfg, err := p.store.GetConfig(ctx, i.GuildID)
	if err != nil {
		return "✗ Couldn't read the setup: " + err.Error()
	}
	if !cfg.Gated() {
		return "✗ Nothing is gating the contest forum: " + ErrUngated.Error()
	}
	c, err := p.store.LiveContest(ctx, i.GuildID)
	if err != nil {
		return "✗ Nothing to sync: " + err.Error()
	}
	changed, err := p.syncForum(cfg, c)
	if err != nil {
		return "✗ Couldn't apply the permissions: " + err.Error()
	}
	if !changed {
		return "✓ " + core.MentionChannel(c.ForumChannelID) + " already matches the gate, so nothing was written."
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "contest.forum_synced", c.Title, ""); err != nil {
		p.log.Error("contest: audit sync-forum", "guild", i.GuildID, "err", err)
	}
	return "✓ " + core.MentionChannel(c.ForumChannelID) + " now matches the gate."
}
