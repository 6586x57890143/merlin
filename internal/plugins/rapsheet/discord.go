package rapsheet

import (
	"context"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// What moderators do through the Discord client rather than through merlin.
//
// A ban placed from a member's context menu is as much a part of their
// record as one placed with /rapsheet ban, and a sheet that only knew about
// merlin's own actions would be a sheet that lied by omission. Discord tells
// the bot about these through GUILD_AUDIT_LOG_ENTRY_CREATE, one event per
// audit-log entry, carrying the target, the actor and the reason the mod
// typed. That is strictly better than watching GUILD_BAN_ADD and then asking
// the audit log who did it: the same fact, already attributed, with no race
// against the log being written.
//
// It arrives under IntentsGuildBans (GUILD_MODERATION), which is
// unprivileged, but only if the bot holds View Audit Log in the guild; with
// that permission missing Discord sends nothing and says nothing, which is
// why /rapsheet status checks for it.
//
// merlin's own bans, kicks and timeouts arrive here too, and are told apart
// by the actor: an entry whose user is the bot was already recorded by the
// command or the sweep that did it.

// HandleAuditLogEntry is called from cmd/bot/main.go for every audit-log
// entry Discord delivers. botUserID is merlin's own id.
func (p *Plugin) HandleAuditLogEntry(ctx context.Context, botUserID string, e *discordgo.GuildAuditLogEntryCreate) {
	if e == nil || e.AuditLogEntry == nil || e.ActionType == nil || e.GuildID == "" || e.TargetID == "" || !p.enabled(e.GuildID) {
		return
	}
	if e.UserID == botUserID {
		return
	}
	in, ok := auditEntryToEntry(e, p.now())
	if !ok {
		return
	}
	in.GuildID = e.GuildID
	p.detached(func(ctx context.Context) {
		cfg := p.config(ctx, e.GuildID)
		if in.Kind == KindUnban {
			// Whoever lifted it, the ledger's ban is over. Marked before the
			// unban entry is written so a sweep landing between the two does
			// not try to lift a ban that is already gone.
			if _, _, err := p.liftBan(ctx, e.GuildID, e.TargetID); err != nil {
				p.log.Error("rapsheet: lift ban on audit-log unban", "guild", e.GuildID, "user", e.TargetID, "err", err)
			}
		}
		rec, written, err := p.record(ctx, cfg, in)
		if err != nil {
			p.log.Error("rapsheet: ingest audit-log entry", "guild", e.GuildID, "action", *e.ActionType, "err", err)
			return
		}
		if written && rec.Kind == KindBan {
			p.mu.Lock()
			p.reconcileSweepJob(ctx, e.GuildID)
			p.mu.Unlock()
		}
	})
	_ = ctx
}

// auditEntryToEntry translates the audit-log actions this plugin cares
// about. Anything else (a channel edit, a role change) is not a moderation
// action against a member and is ignored.
func auditEntryToEntry(e *discordgo.GuildAuditLogEntryCreate, now time.Time) (newEntry, bool) {
	in := newEntry{
		UserID:   e.TargetID,
		Category: CategoryServerRule,
		ActorID:  e.UserID,
		Reason:   e.Reason,
		Source:   SourceDiscord,
		Ref:      e.ID,
	}
	if in.ActorID == "" {
		in.ActorID = core.ActorSystem
	}
	switch *e.ActionType {
	case discordgo.AuditLogActionMemberBanAdd:
		// A ban from the client has no end date. If the moderator lifts it
		// later, that arrives as its own entry.
		in.Kind = KindBan
		if in.Reason == "" {
			in.Reason = "banned through Discord"
		}
	case discordgo.AuditLogActionMemberBanRemove:
		in.Kind = KindUnban
		if in.Reason == "" {
			in.Reason = "unbanned through Discord"
		}
	case discordgo.AuditLogActionMemberKick:
		in.Kind = KindKick
		if in.Reason == "" {
			in.Reason = "kicked through Discord"
		}
	case discordgo.AuditLogActionMemberUpdate:
		until, ok := timeoutChange(e, now)
		if !ok {
			return newEntry{}, false
		}
		if until == nil {
			in.Kind = KindRelease
			in.Reason = "timeout cleared through Discord"
			return in, true
		}
		in.Kind = KindTimeout
		in.EndsAt = until
		in.Duration = until.Sub(now).Round(time.Minute)
		if in.Reason == "" {
			in.Reason = "timed out through Discord"
		}
	default:
		return newEntry{}, false
	}
	return in, true
}

// timeoutChange finds the communication_disabled_until change on a member
// update, if it has one. ok is false when the update was about something
// else (a nickname, a role); a nil time with ok true means the timeout was
// cleared.
func timeoutChange(e *discordgo.GuildAuditLogEntryCreate, now time.Time) (*time.Time, bool) {
	for _, ch := range e.Changes {
		if ch == nil || ch.Key == nil || *ch.Key != discordgo.AuditLogChangeKeyCommunicationDisabledUntil {
			continue
		}
		s, _ := ch.NewValue.(string)
		if s == "" {
			return nil, true
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			// A timestamp this build cannot read is still a timeout; record
			// it without an end rather than not at all.
			return nil, true
		}
		if !t.After(now) {
			// Discord clears a timeout by writing a past instant as well as
			// by writing null.
			return nil, true
		}
		return &t, true
	}
	return nil, false
}

// auditLogPermission is what /rapsheet status checks: without it Discord
// delivers no audit-log events and the sheet silently misses every ban a
// moderator places by hand.
func (p *Plugin) auditLogPermission(guildID string) (bool, error) {
	botID, err := p.botUserID(guildID)
	if err != nil {
		return false, err
	}
	member, err := p.ops(guildID).GuildMember(guildID, botID)
	if err != nil {
		return false, fmt.Errorf("look up merlin's own member: %w", err)
	}
	roles, err := p.ops(guildID).GuildRoles(guildID)
	if err != nil {
		return false, fmt.Errorf("list roles: %w", err)
	}
	var perms int64
	for _, r := range roles {
		if r.ID == guildID {
			perms |= r.Permissions
		}
		for _, id := range member.Roles {
			if r.ID == id {
				perms |= r.Permissions
			}
		}
	}
	return perms&(discordgo.PermissionViewAuditLogs|discordgo.PermissionAdministrator) != 0, nil
}
