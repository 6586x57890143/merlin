package roles

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/disgoorg/disgo/rest"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
	"github.com/6586x57890143/merlin/internal/scheduler"
)

// Supporters is who backs the birds now, as cygnet keeps it
// (cygnet.supporters). main.go wires the fleet link in, so this package
// never imports it.
type Supporters interface {
	Supporters(ctx context.Context) ([]Backer, error)
}

// SupportersFunc is a function as Supporters.
type SupportersFunc func(ctx context.Context) ([]Backer, error)

// Supporters calls f.
func (f SupportersFunc) Supporters(ctx context.Context) ([]Backer, error) { return f(ctx) }

// Backer is one supporter: a Discord user and when their paid time ends.
type Backer struct {
	UserID string
	Until  time.Time
}

// The Melting Pot's existing supporter role, and how often the list is read.
const (
	meltingPotSupporterRoleID = "1562888379799183421"
	supporterSyncInterval     = 10 * time.Minute
	supporterReason           = "supporter, via cygnet"
)

// WithSupporters turns on the supporter role sync where a guild has a
// supporter role (the Melting Pot's, today).
func (p *Plugin) WithSupporters(s Supporters) { p.supporters = s }

// supporterRole is guildID's supporter role, or "" where there is none.
func supporterRole(guildID string) string {
	if guildID == meltingPotGuildID {
		return meltingPotSupporterRoleID
	}
	return ""
}

// registerSupporterSync arms the sync job for guildID if it has a supporter
// role and the fleet link is wired. Called from SyncGuild with p.mu held.
func (p *Plugin) registerSupporterSync(guildID string) {
	role := supporterRole(guildID)
	if p.supporters == nil || role == "" {
		return
	}
	key := scheduler.JobKey(guildID, "roles-supporters")
	err := p.sched.Register(key, core.CronSpec{Schedule: core.IntervalSchedule{Interval: supporterSyncInterval}}, func(ctx context.Context) error {
		if err := p.syncSupporters(ctx, guildID, role); err != nil && !discordguard.Skipped(err) {
			return err
		}
		return nil
	})
	if err != nil {
		p.log.Error("roles: register supporter sync", "guild", guildID, "err", err)
	}
}

// syncSupporters gives every current supporter the role as a timed grant
// ending when their paid time does, so the grant timer and the sweep take it
// away again with nothing new to keep. It only ever adds and re-dates: a
// member who holds the role without a grant from merlin (given by hand) and
// a permanent grant are left alone, and a jailed member is skipped until the
// jail ends. A renewal moves the grant's end later, never earlier.
func (p *Plugin) syncSupporters(ctx context.Context, guildID, roleID string) error {
	if p.dryRun(guildID) {
		return nil
	}
	if err := p.perms.CanManageRole(guildID, roleID); err != nil {
		return fmt.Errorf("supporter role: %w", err)
	}
	backers, err := p.supporters.Supporters(ctx)
	if err != nil {
		return fmt.Errorf("supporters: %w", err)
	}
	var errs []error
	for _, b := range backers {
		if !b.Until.After(p.now()) {
			continue
		}
		if err := p.syncSupporter(ctx, guildID, roleID, b); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Plugin) syncSupporter(ctx context.Context, guildID, roleID string, b Backer) error {
	rec, granted, err := p.store.GetGrant(ctx, guildID, b.UserID, roleID)
	if err != nil {
		return err
	}
	if granted && (rec.ExpiresAt == nil || !rec.ExpiresAt.Before(b.Until)) {
		return nil
	}
	if _, jailed, err := p.store.GetJail(ctx, guildID, b.UserID); err != nil || jailed {
		return err
	}
	member, err := p.ops(guildID).GetMember(core.ID(guildID), core.ID(b.UserID))
	if core.HasDiscordErrorCode(err, int(rest.JSONErrorCodeUnknownMember)) {
		return nil // not in this server; the next read gives it them if they join
	}
	if err != nil {
		return err
	}
	holds := slices.Contains(core.IDStrings(member.RoleIDs), roleID)
	if !granted && holds {
		return nil // given by hand: not merlin's to time out
	}
	if !holds {
		if err := p.ops(guildID).AddMemberRole(core.ID(guildID), core.ID(b.UserID), core.ID(roleID)); err != nil {
			return err
		}
	}
	until := b.Until
	if err := p.store.InsertGrant(ctx, GrantRecord{
		GuildID: guildID, UserID: b.UserID, RoleID: roleID,
		GrantedAt: p.now(), ExpiresAt: &until, GrantedBy: core.ActorSystem, Reason: supporterReason,
	}); err != nil {
		return err
	}
	p.armGrantRevoke(guildID, b.UserID, roleID, until)
	action := "roles.supporter_grant"
	if granted {
		action = "roles.supporter_renew"
	}
	if err := p.audit.Record(ctx, guildID, core.ActorSystem, action, "", fmt.Sprintf("user=%s role=%s until=%s", core.MentionUser(b.UserID), core.MentionRole(roleID), until.UTC().Format(time.RFC3339))); err != nil {
		p.log.Error("roles: audit supporter grant failed", "guild", guildID, "user", b.UserID, "err", err)
	}
	return nil
}
