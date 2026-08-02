package core

import (
	"errors"
	"fmt"
	"slices"

	"github.com/bwmarrin/discordgo"
)

// PermTier is how restrictive a command/subcommand is. The zero value,
// tierUnset, is deliberately invalid. Every leaf command registered with
// CommandRouter must declare an explicit tier, so a plugin author forgetting
// to set one fails loudly at startup instead of silently defaulting to the
// most permissive tier (spec.MD Design Principle 2, "fail safe not fail
// silent").
type PermTier int

const (
	tierUnset PermTier = iota
	// TierPublic requires no authorization at all, e.g. /ping.
	TierPublic
	// TierMod requires the invoker to hold one of the guild's configured mod
	// roles, be a configured admin (admins satisfy every mod-tier check), or
	// be individually whitelisted for this command's Action.
	TierMod
	// TierAdmin requires the invoker to be a configured admin, the
	// bootstrap admin, or hold Discord's own Administrator permission in
	// the guild (see Authorize), or be individually whitelisted for this
	// command's Action. Mutating the admin list or granting a whitelist
	// entry must always be TierAdmin, never TierMod, or a mod could
	// grant themselves admin.
	TierAdmin
)

// IsSet reports whether t is an explicit tier, as opposed to the zero value
// (tierUnset) meaning "no per-guild override configured for this action",
// used by ActionPolicy.RequiredTier consumers instead of comparing against
// the unexported tierUnset directly.
func (t PermTier) IsSet() bool { return t != tierUnset }

// String renders t for user-facing text (e.g. /config permissions list,
// /config permissions set-tier's confirmation message).
func (t PermTier) String() string {
	switch t {
	case TierAdmin:
		return "Admins only"
	case TierMod:
		return "Admins + Mods"
	case TierPublic:
		return "Public"
	default:
		return "default"
	}
}

// PermSpec is what a registered command/subcommand requires. Action namespaces
// the per-command whitelist independently of Tier, e.g. "rotation.configure",
// "admin.run_now", so a specific role/user can be granted just that one
// action without becoming a full mod or admin. Action also doubles as the
// key a guild can use to override the effective tier and grant/deny specific
// people, via ActionPolicy below. A command with no Action can't be
// customized per guild at all, only ever governed by its compiled-in Tier.
type PermSpec struct {
	Tier   PermTier
	Action string
}

// ActionPolicy is a guild's customization of one Action, layered on top of
// the command's compiled-in PermSpec (see Authorize). RequiredTier ==
// tierUnset means "no override, use the command's own PermSpec.Tier". A
// guild only ever explicitly sets this to TierMod or TierAdmin, never back
// to tierUnset except via an explicit "clear" mutation. Deny always wins
// over Allow (and over tier/Administrator-bit) for the same person/role,
// except for the bootstrap admin, which nothing can deny (see Authorize).
type ActionPolicy struct {
	RequiredTier PermTier
	AllowRoleIDs []string
	AllowUserIDs []string
	DenyRoleIDs  []string
	DenyUserIDs  []string
}

// GuildAuthData is the narrow, in-memory view of a guild's authorization
// settings that Permissions needs. Implemented by internal/settings.Store,
// referenced here only as an interface so this package (which
// internal/settings must import for the EventBus/EventConfigChanged it
// publishes on writes) never imports internal/settings back.
type GuildAuthData interface {
	// ModRoleIDs returns the guild's configured mod role IDs. Empty (not an
	// error) for a guild with no settings configured yet.
	ModRoleIDs(guildID string) []string
	// AdminUserIDs returns the guild's configured admin user IDs, beyond the
	// bootstrap admin.
	AdminUserIDs(guildID string) []string
	// ActionPolicy returns guildID's customization of action (tier override,
	// allow-list, deny-list), or the zero value (tierUnset, no lists) if the
	// guild hasn't customized this action at all.
	ActionPolicy(guildID, action string) ActionPolicy
}

// PluginGate answers whether a whole plugin is enabled in a guild: a
// coarser, precondition check made once per dispatch by CommandRouter,
// before Authorize (or any per-action policy) ever runs. Deliberately its
// own interface, not folded into GuildAuthData/Permissions: "is this plugin
// even active here" isn't a per-user authorization question. Implemented by
// internal/settings.Store like everything else in this file.
type PluginGate interface {
	PluginEnabled(guildID, pluginName string) bool
}

// Permissions implements spec.MD §4a's tiered authorization: every
// privileged command/subcommand's registered PermSpec is checked centrally
// by CommandRouter before its handler ever runs, so there is no per-plugin
// choke point to forget.
type Permissions struct {
	session     *discordgo.Session
	settings    GuildAuthData
	bootstrapID string
}

// NewPermissions constructs Permissions. bootstrapAdminUserID is a bootstrap
// identity (env-sourced, never DB-backed) that always satisfies TierAdmin
// regardless of settings state, so a wiped or not-yet-configured guild's
// settings can never permanently lock the operator out.
func NewPermissions(s *discordgo.Session, settings GuildAuthData, bootstrapAdminUserID string) *Permissions {
	return &Permissions{session: s, settings: settings, bootstrapID: bootstrapAdminUserID}
}

// Authorize checks spec against the invoking member. Discord's own
// default_member_permissions is intentionally NOT re-checked here. For
// Mod/Admin-tier commands it's registered as unset (visible to @everyone at
// the Discord layer) precisely so a whitelisted non-mod user, or a mod role
// that happens to carry no matching Discord permission bit, is never blocked
// before this check runs. This is the sole real gate; it can't be forgotten
// because CommandRouter calls it for every dispatched command (after the
// plugin-enabled gate, see PluginGate/CommandRouter).
//
// Checks run in three layers, coarsest first:
//
//  1. Deny: if spec.Action is set and the guild has denied this action to
//     the invoker's user ID or any of their roles, they're rejected
//     immediately: deny wins over everything below, including
//     Administrator and an explicit allow-grant. The one exception is the
//     bootstrap admin: nothing can deny it, preserving Milestone 4's
//     "never permanently lock the operator out" guarantee.
//  2. Tier: TierPublic always passes. Otherwise the effective tier is the
//     guild's ActionPolicy.RequiredTier override if one is set for
//     spec.Action, else spec.Tier itself. TierAdmin has three independent
//     paths, any one sufficient: the bootstrap admin, a user
//     explicitly added via /config admins, or Discord's own Administrator
//     permission bit (member.Permissions, already computed and attached to
//     every interaction by Discord, no extra API call). The Administrator
//     path exists so a guild's own trusted admins can self-serve
//     /config setup and everything else without first needing an existing
//     bot-admin to add them. It's deliberately NOT extended to TierMod:
//     mods stay purely /config mod-roles-driven, with no permission-bit
//     shortcut, so holding an elevated Discord permission never grants
//     bot-reconfiguration rights on its own. Note the resulting asymmetry:
//     /config admins remove only ever revokes the DB-listed path. A
//     Discord Administrator keeps access via that path regardless, until a
//     fellow Administrator changes their Discord role; that's intentional,
//     since Discord's own role system is authoritative for that path, not
//     ours.
//  3. Allow: independent of tier, so a specific role/user can be granted
//     exactly one action without becoming a full mod or admin.
func (p *Permissions) Authorize(i *discordgo.InteractionCreate, spec PermSpec) error {
	member := i.Member
	if member != nil && member.User == nil {
		// Discord always populates Member.User on a real interaction. If it
		// somehow isn't there we have no identity to authorize, so refuse
		// rather than nil-deref our way through a security check.
		return ErrForbidden{Reason: "member has no user identity"}
	}

	var policy ActionPolicy
	if spec.Action != "" {
		policy = p.settings.ActionPolicy(i.GuildID, spec.Action)
		if member != nil && member.User.ID != p.bootstrapID {
			userID := member.User.ID
			if slices.Contains(policy.DenyUserIDs, userID) || hasAnyRole(member.Roles, policy.DenyRoleIDs) {
				return ErrForbidden{Reason: "denied for " + spec.Action}
			}
		}
	}

	// The effective tier is resolved *before* the TierPublic shortcut, not
	// after. Short-circuiting on spec.Tier instead would silently ignore a
	// guild's set-tier override on any action whose compiled-in tier is
	// TierPublic, a fail-open in the one function that must fail closed.
	// The reverse can't happen: /config permissions set-tier only ever offers
	// Mod/Admin, so an override can raise a public command's bar but can
	// never lower a privileged one to public.
	// Only Mod and Admin overrides are honored, never a stored TierPublic.
	// /config permissions set-tier offers nothing else, so a public override
	// could only arrive via a corrupt row, a hand-edited database, or some
	// future import path, and honoring one would silently strip every check
	// off a privileged action. Loosening Admin to Mod is a deliberate feature;
	// loosening anything to "no check at all" is the one direction a stored
	// value must never be able to move a command in.
	effectiveTier := spec.Tier
	if policy.RequiredTier == TierMod || policy.RequiredTier == TierAdmin {
		effectiveTier = policy.RequiredTier
	}

	if effectiveTier == TierPublic {
		return nil
	}
	if member == nil {
		return ErrForbidden{Reason: "no guild member context"}
	}
	userID := member.User.ID

	isAdmin := userID == p.bootstrapID ||
		slices.Contains(p.settings.AdminUserIDs(i.GuildID), userID) ||
		member.Permissions&discordgo.PermissionAdministrator != 0

	switch effectiveTier {
	case TierAdmin:
		if isAdmin {
			return nil
		}
	case TierMod:
		if isAdmin || hasAnyRole(member.Roles, p.settings.ModRoleIDs(i.GuildID)) {
			return nil
		}
	}

	// Additive whitelist path: independent of (effective) tier, so a
	// specific role/user can be granted exactly this one action without
	// becoming a full mod or admin.
	if hasAnyRole(member.Roles, policy.AllowRoleIDs) || slices.Contains(policy.AllowUserIDs, userID) {
		return nil
	}
	return ErrForbidden{Reason: "not authorized for " + spec.Action}
}

// CanModerate reports whether actor may take a *restrictive* action (today:
// /roles jail and /roles release) against the member identified by
// targetUserID/targetRoleIDs.
//
// This closes the one privilege inversion the tier model alone doesn't:
// jail is TierMod, and jailing strips every role the bot can manage, which
// for an admin whose authority comes from Discord's own Administrator bit
// also strips their TierAdmin path here (see Authorize). Without this check
// a single rogue or compromised mod could neutralize a guild's entire admin
// team one /roles jail at a time, and nothing in the tier layer would notice,
// because at no point did anyone run a command above their own tier.
//
// The rule is deliberately narrow: only admin-equivalent *targets* are
// protected, and only from non-admin actors. Mod-on-mod moderation stays
// allowed: a mod acting against a peer is ordinary, sometimes necessary
// moderation, and admins can always intervene. The bootstrap identity is
// protected from everyone, matching the deny-list's own carve-out: it is the
// operator's guaranteed way back in, so no in-guild action may disable it.
//
// Fails closed: if the guild's state can't be resolved (so we can't tell
// whether the target is an admin), the action is refused rather than allowed.
func (p *Permissions) CanModerate(guildID string, actor *discordgo.Member, targetUserID string, targetRoleIDs []string) error {
	if targetUserID == p.bootstrapID {
		return ErrForbidden{Reason: "the bootstrap admin can't be targeted"}
	}

	targetIsAdmin, err := p.isAdminMember(guildID, targetUserID, targetRoleIDs)
	if err != nil {
		return fmt.Errorf("core: resolve target's privilege in guild %s: %w", guildID, err)
	}
	if !targetIsAdmin {
		return nil
	}

	if actor == nil || actor.User == nil {
		return ErrForbidden{Reason: "target is an admin"}
	}
	actorIsAdmin, err := p.isAdminMember(guildID, actor.User.ID, actor.Roles)
	if err != nil {
		return fmt.Errorf("core: resolve actor's privilege in guild %s: %w", guildID, err)
	}
	if !actorIsAdmin {
		return ErrForbidden{Reason: "target is an admin, only another admin can do that"}
	}
	return nil
}

// isAdminMember answers "would this member satisfy TierAdmin here?" for an
// *arbitrary* member, as opposed to Authorize's invoker. It can't reuse
// Authorize's cheap member.Permissions check: that bitmask is computed by
// Discord and attached only to the interaction's own member, so for anyone
// else the Administrator bit has to be recomputed from the guild's roles.
// The guild owner is included because Discord grants them every permission
// implicitly, regardless of which roles they hold.
func (p *Permissions) isAdminMember(guildID, userID string, roleIDs []string) (bool, error) {
	if userID == p.bootstrapID || slices.Contains(p.settings.AdminUserIDs(guildID), userID) {
		return true, nil
	}
	guild, err := p.guild(guildID)
	if err != nil {
		return false, err
	}
	if guild.OwnerID == userID {
		return true, nil
	}
	for _, r := range guild.Roles {
		if r.Permissions&discordgo.PermissionAdministrator != 0 && slices.Contains(roleIDs, r.ID) {
			return true, nil
		}
	}
	return false, nil
}

// guild resolves guildID from the session's state cache, reporting a clear
// error instead of panicking when there's no session at all (which is how
// Permissions is constructed in unit tests, and would otherwise turn every
// hierarchy check into a nil dereference on a security path).
func (p *Permissions) guild(guildID string) (*discordgo.Guild, error) {
	if p.session == nil || p.session.State == nil {
		return nil, errors.New("core: no session state available to resolve guild")
	}
	return p.session.State.Guild(guildID)
}

// CanManageRole enforces layer 3: the bot's own top role must sit strictly
// above targetRoleID in the guild's role hierarchy. Discord enforces this
// API-side too. This check exists so we fail cleanly with a clear message
// instead of surfacing a raw 403 from Discord.
func (p *Permissions) CanManageRole(guildID, targetRoleID string) error {
	guild, err := p.guild(guildID)
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
