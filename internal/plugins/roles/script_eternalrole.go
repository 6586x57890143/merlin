package roles

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// The eternal-role script: one member always holds one role, and the role
// always looks the way it did when merlin first saw it.
//
// A script (see internal/scripts) is a table of data plus a hook into work
// the plugin already does. This one rides the per-guild sweep and the
// role/member gateway handlers; all of them call enforceEternalRoles, which
// re-derives everything from live Discord state, so firing it twice for
// one event is harmless. While the script is on it is stronger than the
// guild's admins on purpose, so nothing an admin does through Discord can
// change the stored copy: it is captured once and only merlin's own
// recreate moves RoleID and IconHash. An admin who wants a different
// eternal version has one option, turning the script off.
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

type eternalRole struct{ guildID, userID, roleID string }

// eternalRoles is the whole configuration. Adding a person is a one-line
// change here, reviewed like any other; there is deliberately no command.
var eternalRoles = []eternalRole{
	{guildID: meltingPotGuildID, userID: "1539052473690366022", roleID: "1547265306408517662"},
}

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

// enforceEternalRoles runs every eternal role defined for guildID, if the
// script is on there. An unreadable switch reads as off.
func (p *Plugin) enforceEternalRoles(ctx context.Context, guildID string) error {
	if p.scripts == nil || p.dryRun(guildID) {
		return nil
	}
	var mine []eternalRole
	for _, e := range eternalRoles {
		if e.guildID == guildID {
			mine = append(mine, e)
		}
	}
	if len(mine) == 0 {
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
	var firstErr error
	for _, e := range mine {
		if err := p.enforceEternalRole(ctx, e); err != nil {
			p.log.Error("roles: eternal-role: enforce failed", "guild", guildID, "user", e.userID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (p *Plugin) enforceEternalRole(ctx context.Context, e eternalRole) error {
	if _, jailed, err := p.store.GetJail(ctx, e.guildID, e.userID); err != nil {
		return fmt.Errorf("look up jail: %w", err)
	} else if jailed {
		return nil
	}

	ops := p.ops(e.guildID)
	rolesList, err := ops.GuildRoles(e.guildID)
	if err != nil {
		return fmt.Errorf("list roles: %w", err)
	}
	byID := func(id string) *discordgo.Role {
		i := slices.IndexFunc(rolesList, func(r *discordgo.Role) bool { return r.ID == id })
		if i < 0 {
			return nil
		}
		return rolesList[i]
	}

	rec, ok, err := p.store.GetEternalRole(ctx, e.guildID, e.userID, e.roleID)
	if err != nil {
		return fmt.Errorf("read copy: %w", err)
	}
	if !ok {
		// First run: the live role is the eternal version from here on.
		// Nothing to copy from means nothing to enforce; merlin never
		// invents a role.
		live := byID(e.roleID)
		if live == nil {
			p.log.Warn("roles: eternal-role: origin role not found, nothing captured", "guild", e.guildID, "role", e.roleID)
			return nil
		}
		rec, err = p.captureEternalRole(ctx, e, live)
		if err != nil {
			return err
		}
	}

	role := byID(rec.RoleID)
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

	member, err := ops.GuildMember(e.guildID, e.userID)
	if err != nil {
		if core.HasDiscordErrorCode(err, discordgo.ErrCodeUnknownMember) {
			return nil // not in the guild; HandleMemberJoin or the next sweep
		}
		return fmt.Errorf("fetch member: %w", err)
	}
	if slices.Contains(member.Roles, role.ID) {
		return nil
	}
	if err := ops.GuildMemberRoleAdd(e.guildID, e.userID, role.ID); err != nil {
		return fmt.Errorf("re-add role: %w", err)
	}
	p.log.Warn("roles: eternal-role: role re-added", "guild", e.guildID, "user", e.userID, "role", role.ID)
	p.auditEternal(ctx, e.guildID, "roles.eternal_reassigned", core.MentionRole(role.ID),
		fmt.Sprintf("%s had lost it; given back", core.MentionUser(e.userID)))
	return nil
}

// captureEternalRole records live as the eternal version, icon bytes
// included. An unreachable icon is captured as its hash alone rather than
// failing: drift on the standing role is still detected, and a recreate
// simply comes back without the picture. The role's name and colour are
// the part people see, and the copy is taken once, so nothing retries this.
func (p *Plugin) captureEternalRole(ctx context.Context, e eternalRole, live *discordgo.Role) (EternalRoleRecord, error) {
	rec := EternalRoleRecord{
		GuildID: e.guildID, UserID: e.userID, OriginRoleID: e.roleID, RoleID: live.ID,
		Name: live.Name, Color: live.Color, Hoist: live.Hoist, Mentionable: live.Mentionable,
		Permissions: live.Permissions, UnicodeEmoji: live.UnicodeEmoji, IconHash: live.Icon,
		CapturedAt: p.now(),
	}
	if live.Icon != "" {
		icon, err := p.fetch(ctx, live.IconURL("1024"))
		if err != nil {
			p.log.Error("roles: eternal-role: icon not captured", "guild", e.guildID, "role", live.ID, "err", err)
		} else {
			rec.Icon = icon
		}
	}
	if err := p.store.PutEternalRole(ctx, rec); err != nil {
		return EternalRoleRecord{}, fmt.Errorf("store copy: %w", err)
	}
	p.auditEternal(ctx, e.guildID, "roles.eternal_captured", core.MentionRole(live.ID),
		fmt.Sprintf("now the eternal role of %s", core.MentionUser(e.userID)))
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
