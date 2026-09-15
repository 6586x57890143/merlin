package roles

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// The eternal-role script: a member always holds a role, and the role
// always looks the way it did when it was made eternal.
//
// A script (see internal/scripts) is a hook into work the plugin already
// does, off until a guild turns it on. This one rides the per-guild sweep
// and the role/member gateway handlers; all of them call
// enforceEternalRoles, which re-derives everything from live Discord state,
// so firing it twice for one event is harmless. While the script is on it
// is stronger than the guild's admins on purpose, so nothing an admin does
// through Discord can change the stored copy: it is captured once, when
// the role is added, and only merlin's own recreate moves RoleID and
// IconHash. An admin who wants a different eternal version has one option,
// turning the script off.
//
// Definitions live in script_eternal_roles, one row per member+role, so any
// server can use the script and none is named in code. Adding or removing
// one is for the guild owner or the bootstrap operator only (canDefineEternal),
// never TierAdmin: an admin who could add themselves on a role carrying
// Administrator would have merlin entrench them faster than anyone could
// strip it, which is the one thing this script must not do for whoever asks.
//
// Three things can go wrong with the role and each has one answer:
//   - removed from the member: added back
//   - deleted: recreated from the copy, assigned, and the copy retargeted
//   - edited: left alone; a fresh role matching the copy is created,
//     assigned, and placed directly above the edited one so it is the
//     member's top role, and the copy retargets to the new one
//
// It yields to jail. A jail strips every role on purpose and restores them
// from its own snapshot on release, and a script that fought it would loop
// against HandleMemberUpdate's re-strip.

const scriptEternalRole = "eternal-role"

// errEternalExists reports an add for a member+role already on record. The
// copy is taken once, so a second add must not overwrite it.
var errEternalExists = errors.New("that member already has that role as an eternal role")

// iconFetchTimeout and maxIconBytes bound the one outbound HTTP call this
// script makes, to Discord's CDN for the role icon. Discord caps uploads at
// 256KB, so the reader limit is generous rather than tight.
const (
	iconFetchTimeout = 10 * time.Second
	maxIconBytes     = 1 << 20
)

// fetchURL is the default Plugin.fetch: a bounded GET.
func fetchURL(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, iconFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxIconBytes))
}

// enforceEternalRoles runs every eternal role defined in guildID, if the
// script is on there. An unreadable switch reads as off.
func (p *Plugin) enforceEternalRoles(ctx context.Context, guildID string) error {
	if p.scripts == nil || p.dryRun(guildID) {
		return nil
	}
	on, err := p.scripts.Enabled(ctx, guildID, scriptEternalRole)
	if err != nil {
		p.log.Error("roles: eternal-role: read script switch, treating as off", "guild", guildID, "err", err)
		return nil
	}
	if !on {
		return nil
	}
	recs, err := p.store.ListEternalRoles(ctx, guildID)
	if err != nil {
		return fmt.Errorf("list eternal roles: %w", err)
	}
	var firstErr error
	for _, rec := range recs {
		if err := p.enforceEternalRole(ctx, rec); err != nil {
			p.log.Error("roles: eternal-role: enforce failed", "guild", guildID, "user", rec.UserID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (p *Plugin) enforceEternalRole(ctx context.Context, rec EternalRoleRecord) error {
	if _, jailed, err := p.store.GetJail(ctx, rec.GuildID, rec.UserID); err != nil {
		return fmt.Errorf("look up jail: %w", err)
	} else if jailed {
		return nil
	}

	ops := p.ops(rec.GuildID)
	rolesList, err := ops.GuildRoles(rec.GuildID)
	if err != nil {
		return fmt.Errorf("list roles: %w", err)
	}
	role := findRole(rolesList, rec.RoleID)
	switch {
	case role == nil:
		if role, err = p.recreateEternalRole(ctx, &rec, nil); err != nil {
			return err
		}
	case eternalRoleDiverged(role, rec):
		if role, err = p.recreateEternalRole(ctx, &rec, role); err != nil {
			return err
		}
	}

	member, err := ops.GuildMember(rec.GuildID, rec.UserID)
	if err != nil {
		if core.HasDiscordErrorCode(err, discordgo.ErrCodeUnknownMember) {
			return nil // not in the guild; HandleMemberJoin or the next sweep
		}
		return fmt.Errorf("fetch member: %w", err)
	}
	if slices.Contains(member.Roles, role.ID) {
		return nil
	}
	if err := ops.GuildMemberRoleAdd(rec.GuildID, rec.UserID, role.ID); err != nil {
		return fmt.Errorf("re-add role: %w", err)
	}
	p.log.Warn("roles: eternal-role: role re-added", "guild", rec.GuildID, "user", rec.UserID, "role", role.ID)
	p.auditEternal(ctx, rec.GuildID, "roles.eternal_reassigned", core.MentionRole(role.ID),
		fmt.Sprintf("%s had lost it; given back", core.MentionUser(rec.UserID)))
	return nil
}

func findRole(list []*discordgo.Role, id string) *discordgo.Role {
	i := slices.IndexFunc(list, func(r *discordgo.Role) bool { return r.ID == id })
	if i < 0 {
		return nil
	}
	return list[i]
}

// addEternalRole defines a new eternal role by capturing roleID as it is
// right now, icon bytes included. An unreachable icon is captured as its
// hash alone rather than failing: drift on the standing role is still
// detected, and a recreate simply comes back without the picture. The copy
// is taken once, so nothing retries this, and a second add for the same
// pair is refused rather than overwriting it.
func (p *Plugin) addEternalRole(ctx context.Context, guildID, userID, roleID, actor string) (EternalRoleRecord, error) {
	existing, err := p.store.ListEternalRoles(ctx, guildID)
	if err != nil {
		return EternalRoleRecord{}, fmt.Errorf("list eternal roles: %w", err)
	}
	for _, e := range existing {
		if e.UserID == userID && (e.OriginRoleID == roleID || e.RoleID == roleID) {
			return EternalRoleRecord{}, errEternalExists
		}
	}
	rolesList, err := p.ops(guildID).GuildRoles(guildID)
	if err != nil {
		return EternalRoleRecord{}, fmt.Errorf("list roles: %w", err)
	}
	live := findRole(rolesList, roleID)
	if live == nil {
		return EternalRoleRecord{}, fmt.Errorf("role %s does not exist in this server", core.MentionRole(roleID))
	}
	if live.Managed {
		return EternalRoleRecord{}, fmt.Errorf("%s is managed by an integration and cannot be copied or assigned", core.MentionRole(roleID))
	}

	rec := EternalRoleRecord{
		GuildID: guildID, UserID: userID, OriginRoleID: roleID, RoleID: live.ID,
		Name: live.Name, Color: live.Color, Hoist: live.Hoist, Mentionable: live.Mentionable,
		Permissions: live.Permissions, UnicodeEmoji: live.UnicodeEmoji, IconHash: live.Icon,
		CapturedAt: p.now(),
	}
	if live.Icon != "" {
		icon, err := p.fetch(ctx, live.IconURL("1024"))
		if err != nil {
			p.log.Error("roles: eternal-role: icon not captured", "guild", guildID, "role", live.ID, "err", err)
		} else {
			rec.Icon = icon
		}
	}
	if err := p.store.PutEternalRole(ctx, rec); err != nil {
		return EternalRoleRecord{}, fmt.Errorf("store copy: %w", err)
	}
	if err := p.audit.Record(ctx, guildID, actor, "roles.eternal_added", core.MentionRole(live.ID),
		fmt.Sprintf("now an eternal role of %s", core.MentionUser(userID))); err != nil {
		p.log.Error("roles: eternal-role: audit failed", "guild", guildID, "err", err)
	}
	return rec, nil
}

// eternalRoleDiverged reports whether live no longer matches the copy.
// Position is deliberately not compared: it shifts whenever any other role
// is reordered, and a role that has merely moved is still the same role.
func eternalRoleDiverged(live *discordgo.Role, rec EternalRoleRecord) bool {
	return live.Name != rec.Name || live.Color != rec.Color || live.Hoist != rec.Hoist ||
		live.Mentionable != rec.Mentionable || live.Permissions != rec.Permissions ||
		live.UnicodeEmoji != rec.UnicodeEmoji || live.Icon != rec.IconHash
}

// recreateEternalRole creates a fresh role from the copy and retargets the
// copy to it. above is the edited role to sit directly over, or nil when the
// old one is gone. The icon hash is re-read from what Discord created, since
// Discord's hash of the re-upload is the comparison key from now on.
func (p *Plugin) recreateEternalRole(ctx context.Context, rec *EternalRoleRecord, above *discordgo.Role) (*discordgo.Role, error) {
	ops := p.ops(rec.GuildID)
	params := &discordgo.RoleParams{
		Name: rec.Name, Color: &rec.Color, Hoist: &rec.Hoist, Mentionable: &rec.Mentionable,
		Permissions: &rec.Permissions,
	}
	if rec.UnicodeEmoji != "" {
		params.UnicodeEmoji = &rec.UnicodeEmoji
	}
	if len(rec.Icon) > 0 {
		icon := "data:image/png;base64," + base64.StdEncoding.EncodeToString(rec.Icon)
		params.Icon = &icon
	}
	role, err := ops.GuildRoleCreate(rec.GuildID, params)
	if err != nil && (params.Icon != nil || params.UnicodeEmoji != nil) {
		// Icons and emoji need the guild's ROLE_ICONS feature, which comes
		// and goes with boost level. A role without its picture is still the
		// role; no role at all is the failure this script exists to prevent.
		p.log.Warn("roles: eternal-role: create with icon failed, retrying without", "guild", rec.GuildID, "err", err)
		params.Icon, params.UnicodeEmoji = nil, nil
		role, err = ops.GuildRoleCreate(rec.GuildID, params)
	}
	if err != nil {
		return nil, fmt.Errorf("recreate role: %w", err)
	}

	reason := "it had been deleted"
	if above != nil {
		reason = fmt.Sprintf("%s had been changed", core.MentionRole(above.ID))
		// Non-fatal, like rotation.restorePosition: the role exists and is
		// about to be assigned, and an error here would have the sweep
		// create a second one.
		if _, err := ops.GuildRoleReorder(rec.GuildID, []*discordgo.Role{{ID: role.ID, Position: above.Position + 1}}); err != nil {
			p.log.Warn("roles: eternal-role: could not place recreated role above the changed one", "guild", rec.GuildID, "role", role.ID, "err", err)
		}
	}

	old := rec.RoleID
	rec.RoleID, rec.IconHash = role.ID, role.Icon
	if err := p.store.PutEternalRole(ctx, *rec); err != nil {
		return nil, fmt.Errorf("retarget copy: %w", err)
	}
	p.log.Warn("roles: eternal-role: role recreated", "guild", rec.GuildID, "user", rec.UserID, "old", old, "new", role.ID)
	p.auditEternal(ctx, rec.GuildID, "roles.eternal_recreated", core.MentionRole(old),
		fmt.Sprintf("%s recreated for %s: %s", core.MentionRole(role.ID), core.MentionUser(rec.UserID), reason))
	return role, nil
}

func (p *Plugin) auditEternal(ctx context.Context, guildID, action, oldValue, newValue string) {
	if err := p.audit.Record(ctx, guildID, core.ActorSystem, action, oldValue, newValue); err != nil {
		p.log.Error("roles: eternal-role: audit failed", "guild", guildID, "action", action, "err", err)
	}
}

// HandleRoleUpdated reacts to any role edit in guildID by re-checking the
// eternal roles there. Latency only; the sweep is the mechanism.
func (p *Plugin) HandleRoleUpdated(ctx context.Context, guildID string) {
	if err := p.enforceEternalRoles(ctx, guildID); err != nil {
		p.log.Error("roles: eternal-role: enforce on role update", "guild", guildID, "err", err)
	}
}

// memberChanged is the script's hook into HandleMemberJoin and
// HandleMemberUpdate: a member who is eternal somewhere gets checked the
// moment their roles move. Everyone else costs one indexed read.
func (p *Plugin) memberChanged(ctx context.Context, guildID, userID string) {
	recs, err := p.store.ListEternalRoles(ctx, guildID)
	if err != nil || !slices.ContainsFunc(recs, func(r EternalRoleRecord) bool { return r.UserID == userID }) {
		return
	}
	if err := p.enforceEternalRoles(ctx, guildID); err != nil {
		p.log.Error("roles: eternal-role: enforce on member change", "guild", guildID, "user", userID, "err", err)
	}
}

// canDefineEternal is the gate on adding and removing eternal roles: the
// guild owner or the bootstrap operator, nobody else, for the entrenchment
// reason in the file comment. Every branch either fails hard or compares
// against a single identity; nothing widens on a missing dependency. Same
// shape as aimod's ownerOrOperator.
func (p *Plugin) canDefineEternal(guildID, userID string) (bool, error) {
	if guildID == "" || userID == "" {
		return false, fmt.Errorf("could not tell who you are, or which server this is")
	}
	if p.perms != nil && p.perms.IsBootstrapAdmin(userID) {
		return true, nil
	}
	guild, err := p.ops(guildID).Guild(guildID)
	if err != nil {
		return false, fmt.Errorf("could not read this server to check who owns it: %w", err)
	}
	if guild == nil {
		return false, fmt.Errorf("this server could not be read, so ownership cannot be confirmed")
	}
	return guild.OwnerID == userID, nil
}

// eternalRolesLine renders a guild's definitions for /roles scripts list.
func eternalRolesLine(recs []EternalRoleRecord) string {
	if len(recs) == 0 {
		return "none defined"
	}
	parts := make([]string, 0, len(recs))
	for _, r := range recs {
		parts = append(parts, core.MentionUser(r.UserID)+" keeps "+core.MentionRole(r.RoleID))
	}
	return strings.Join(parts, ", ")
}
