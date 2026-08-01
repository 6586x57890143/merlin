package core

import (
	"fmt"
	"slices"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/config"
)

// PermCheck describes what a privileged command/action requires at layers 1
// and 2. Action namespaces the internal allow-list check independently of
// the Discord bit — e.g. "admin.run_now", "rotation.configure".
type PermCheck struct {
	Required int64
	Action   string
}

// Permissions implements spec.MD §4's layered authorization: every
// privileged action checks Discord permission, an internal allow-list, and
// role hierarchy — Discord perms alone are necessary but never sufficient.
type Permissions struct {
	session *discordgo.Session
	cfg     *config.Loader
}

func NewPermissions(s *discordgo.Session, cfg *config.Loader) *Permissions {
	return &Permissions{session: s, cfg: cfg}
}

// Authorize runs layers 1-2 and must be called by every privileged command
// handler before acting — there is no single automatic choke point, because
// different commands need different Required/Action values.
func (p *Permissions) Authorize(i *discordgo.InteractionCreate, check PermCheck) error {
	gc, err := p.cfg.Guild(i.GuildID)
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}

	member := i.Member
	if member == nil {
		return ErrForbidden{Reason: "no guild member context"}
	}

	// Layer 1: native Discord permission of the invoking user.
	if member.Permissions&check.Required != check.Required {
		return ErrForbidden{Reason: "missing discord permission"}
	}

	// Layer 2: internal, config-driven allow-list — independent of Discord
	// perms, so a compromised or misconfigured Discord role doesn't alone
	// grant bot-admin.
	if !hasAnyRole(member.Roles, gc.ModRoleIDs) && !slices.Contains(gc.AdminUserIDs, member.User.ID) {
		return ErrForbidden{Reason: "not on internal allow-list for " + check.Action}
	}
	return nil
}

// CanManageRole enforces layer 3: the bot's own top role must sit strictly
// above targetRoleID in the guild's role hierarchy. Discord enforces this
// API-side too — this check exists so we fail cleanly with a clear message
// instead of surfacing a raw 403 from Discord.
func (p *Permissions) CanManageRole(guildID, targetRoleID string) error {
	guild, err := p.session.State.Guild(guildID)
	if err != nil {
		return fmt.Errorf("can manage role: %w", err)
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

// RegisterCommands fails closed: any command in cmds without an explicit
// DefaultMemberPermissions is rejected rather than silently left open to
// @everyone (spec.MD §4 layer 4). Pass an explicit zero-value permission
// pointer for the rare, intentionally-public command so the omission is
// visibly deliberate, not an oversight.
func RegisterCommands(s *discordgo.Session, appID, guildID string, cmds []*discordgo.ApplicationCommand) error {
	for _, c := range cmds {
		if c.DefaultMemberPermissions == nil {
			return fmt.Errorf("command %q missing explicit DefaultMemberPermissions", c.Name)
		}
	}
	_, err := s.ApplicationCommandBulkOverwrite(appID, guildID, cmds)
	return err
}
