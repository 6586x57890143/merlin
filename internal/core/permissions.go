package core

import (
	"slices"

	"github.com/bwmarrin/discordgo"
)

// PermTier is how restrictive a command/subcommand is. The zero value,
// tierUnset, is deliberately invalid — every leaf command registered with
// CommandRouter must declare an explicit tier, so a plugin author forgetting
// to set one fails loudly at startup instead of silently defaulting to the
// most permissive tier (spec.MD Design Principle 2, "fail safe not fail
// silent").
type PermTier int

const (
	tierUnset PermTier = iota
	// TierPublic requires no authorization at all — e.g. /ping.
	TierPublic
	// TierMod requires the invoker to hold one of the guild's configured mod
	// roles, be a configured admin (admins satisfy every mod-tier check), or
	// be individually whitelisted for this command's Action.
	TierMod
	// TierAdmin requires the invoker to be a configured admin (or the
	// break-glass bootstrap admin), or be individually whitelisted for this
	// command's Action. Mutating the admin list or granting a whitelist
	// entry must always be TierAdmin, never TierMod — otherwise a mod could
	// grant themselves admin.
	TierAdmin
)

// PermSpec is what a registered command/subcommand requires. Action namespaces
// the per-command whitelist independently of Tier — e.g. "rotation.configure",
// "admin.run_now" — so a specific role/user can be granted just that one
// action without becoming a full mod or admin.
type PermSpec struct {
	Tier   PermTier
	Action string
}

// GuildAuthData is the narrow, in-memory view of a guild's authorization
// settings that Permissions needs. Implemented by internal/settings.Store —
// referenced here only as an interface so this package (which
// internal/settings must import for the EventBus/EventConfigChanged it
// publishes on writes) never imports internal/settings back.
type GuildAuthData interface {
	// ModRoleIDs returns the guild's configured mod role IDs. Empty (not an
	// error) for a guild with no settings configured yet.
	ModRoleIDs(guildID string) []string
	// AdminUserIDs returns the guild's configured admin user IDs, beyond the
	// break-glass bootstrap admin.
	AdminUserIDs(guildID string) []string
	// ActionOverride returns the role/user IDs individually whitelisted for
	// action in guildID, independent of tier.
	ActionOverride(guildID, action string) (roleIDs, userIDs []string)
}

// Permissions implements spec.MD §4a's tiered authorization: every
// privileged command/subcommand's registered PermSpec is checked centrally
// by CommandRouter before its handler ever runs — there is no per-plugin
// choke point to forget.
type Permissions struct {
	session      *discordgo.Session
	settings     GuildAuthData
	breakGlassID string
}

// NewPermissions constructs Permissions. breakGlassAdminUserID is a
// bootstrap identity (env-sourced, never DB-backed) that always satisfies
// TierAdmin regardless of settings state, so a wiped or not-yet-configured
// guild's settings can never permanently lock the operator out.
func NewPermissions(s *discordgo.Session, settings GuildAuthData, breakGlassAdminUserID string) *Permissions {
	return &Permissions{session: s, settings: settings, breakGlassID: breakGlassAdminUserID}
}

// Authorize checks spec against the invoking member. Discord's own
// default_member_permissions is intentionally NOT re-checked here — for
// Mod/Admin-tier commands it's registered as unset (visible to @everyone at
// the Discord layer) precisely so a whitelisted non-mod user, or a mod role
// that happens to carry no matching Discord permission bit, is never blocked
// before this check runs. This is the sole real gate; it can't be forgotten
// because CommandRouter calls it for every dispatched command.
func (p *Permissions) Authorize(i *discordgo.InteractionCreate, spec PermSpec) error {
	if spec.Tier == TierPublic {
		return nil
	}
	member := i.Member
	if member == nil {
		return ErrForbidden{Reason: "no guild member context"}
	}
	userID := member.User.ID

	isAdmin := userID == p.breakGlassID || slices.Contains(p.settings.AdminUserIDs(i.GuildID), userID)
	switch spec.Tier {
	case TierAdmin:
		if isAdmin {
			return nil
		}
	case TierMod:
		if isAdmin || hasAnyRole(member.Roles, p.settings.ModRoleIDs(i.GuildID)) {
			return nil
		}
	}

	// Additive whitelist path: independent of tier, so a specific role/user
	// can be granted exactly this one action without becoming a full mod or
	// admin.
	if spec.Action != "" {
		roleIDs, userIDs := p.settings.ActionOverride(i.GuildID, spec.Action)
		if hasAnyRole(member.Roles, roleIDs) || slices.Contains(userIDs, userID) {
			return nil
		}
	}
	return ErrForbidden{Reason: "not authorized for " + spec.Action}
}

// CanManageRole enforces layer 3: the bot's own top role must sit strictly
// above targetRoleID in the guild's role hierarchy. Discord enforces this
// API-side too — this check exists so we fail cleanly with a clear message
// instead of surfacing a raw 403 from Discord.
func (p *Permissions) CanManageRole(guildID, targetRoleID string) error {
	guild, err := p.session.State.Guild(guildID)
	if err != nil {
		return err
	}
	botTop, err := p.botTopRolePosition(guild)
	if err != nil {
		return err
	}
	for _, r := range guild.Roles {
		if r.ID == targetRoleID {
			if r.Position >= botTop {
				return ErrForbidden{Reason: "target role at/above bot's top role"}
			}
			return nil
		}
	}
	return ErrForbidden{Reason: "target role not found in guild"}
}

func (p *Permissions) botTopRolePosition(guild *discordgo.Guild) (int, error) {
	self, err := p.session.State.Member(guild.ID, p.session.State.User.ID)
	if err != nil {
		return 0, err
	}
	top := -1
	for _, roleID := range self.Roles {
		for _, r := range guild.Roles {
			if r.ID == roleID && r.Position > top {
				top = r.Position
			}
		}
	}
	return top, nil
}

func hasAnyRole(memberRoles, allowed []string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, id := range allowed {
		set[id] = struct{}{}
	}
	for _, r := range memberRoles {
		if _, ok := set[r]; ok {
			return true
		}
	}
	return false
}
