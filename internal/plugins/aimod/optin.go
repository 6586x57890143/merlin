package aimod

import (
	"context"
	"fmt"
	"slices"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// The opt-in list: members who have asked to be sanctioned by this bot even
// though their rank would otherwise protect them.
//
// The problem it solves is narrow and real. Message removal already applies
// to everybody, admins included, because a message that gets a server
// terminated does not care who posted it. Sanctions do not: jail and timeout
// both refuse an admin-equivalent target, deliberately, because an automated
// action that can jail an admin is an automated action somebody will
// eventually learn to aim, and a bot that can be induced into jailing the
// admin team is a bot that can be used to take a server over.
//
// That protection is correct as a default and wrong as an absolute. An admin
// who wants to be held to the same standard as everyone else currently
// cannot be, and on a server whose whole culture is "the rules apply to the
// people who wrote them" that is a genuine gap.
//
// So the list is **purely additive**, and that invariant is what makes it
// safe to expose at all:
//
//   - Being on it can only ever make somebody *more* moderatable, never less.
//     Nothing consults it to decide whether to protect anyone.
//   - Adding somebody who was never protected changes nothing, so the only
//     meaningful use is adding an admin-equivalent, which is the case it
//     exists for.
//   - Anyone may add themselves, at any tier. That is consent, and consent
//     to be moderated needs no permission.
//   - Adding somebody *else* is not an admin power. It belongs to the
//     bootstrap operator alone (MERLIN_BOOTSTRAP_ADMIN_USER_ID), which is
//     deliberately narrower than TierAdmin: opting somebody in strips the
//     protection that stops this bot being aimed at a guild's staff, so
//     "any admin may do it to any other admin" would hand every admin a
//     one-command route to arranging a peer's automatic jail.
//
//     Reusing the bootstrap identity rather than adding a second configured
//     one is not laziness about the config: it is already defined as the
//     single account that outranks every guild's own settings and that no
//     in-guild state can deny, so a separate override would be a second
//     answer to a question this deployment has already answered once.
//   - The bootstrap identity is never sanctionable, listed or not. It is the
//     operator's guaranteed way back into every guild, and no in-guild
//     action, including its own holder's, may disable it. Same absolute
//     carve-out core.Permissions.CanModerate already makes.
//
// Removing yourself is always allowed and costs nothing, because opting out
// only returns somebody to the default, and for an ordinary member the
// default is already "sanctionable". There is nothing to game.

// PrivilegeChecker is the narrow slice of core.Permissions this plugin
// needs: whether somebody is the bootstrap operator.
type PrivilegeChecker interface {
	IsBootstrapAdmin(userID string) bool
}

// sanctionable reports whether an automated sanction may target userID even
// if their rank would normally protect them.
func sanctionable(cfg Config, userID string) bool {
	return slices.Contains(cfg.SanctionOptInUserIDs, userID)
}

// canManageOptIns reports whether userID may add or remove somebody other
// than themselves. Everyone else, at every tier, is limited to
// /aimod moderate-me.
func (p *Plugin) canManageOptIns(userID string) bool {
	return userID != "" && p.privilege != nil && p.privilege.IsBootstrapAdmin(userID)
}

func (p *Plugin) handleModerateMe(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	on := core.LeafArgs(i)["enabled"].BoolValue()
	userID := ""
	if i.Member != nil && i.Member.User != nil {
		userID = i.Member.User.ID
	}
	if userID == "" {
		core.RespondErr(s, i, "Could not tell who you are", fmt.Errorf("run this in a server, not a DM"))
		return
	}
	p.setOptIn(ctx, s, i, userID, on, true)
}

func (p *Plugin) handleModerateUser(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	actor := ""
	if i.Member != nil && i.Member.User != nil {
		actor = i.Member.User.ID
	}
	opts := core.LeafArgs(i)
	targetID := opts["user"].Value.(string)

	// The guard is here rather than in the PermSpec because there is no tier
	// that means "one specific person". Same shape as
	// adminconfig.validateTierChange: a pure refusal in the handler, for the
	// one setting whose misuse is the whole risk.
	//
	// Redirecting rather than only refusing: somebody reaching for this
	// command usually wants to opt themselves in, and telling them the
	// command that does work is more useful than telling them this one does
	// not.
	if !p.canManageOptIns(actor) {
		if targetID == actor {
			p.setOptIn(ctx, s, i, actor, opts["enabled"].BoolValue(), true)
			return
		}
		core.RespondWarn(s, i, "Not yours to set",
			"Only the operator can put somebody else on the automatic-sanction list. "+
				"Use `/aimod moderate-me` to put yourself on it.")
		return
	}
	p.setOptIn(ctx, s, i, targetID, opts["enabled"].BoolValue(), targetID == actor)
}

func (p *Plugin) setOptIn(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, userID string, on, self bool) {
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	next, changed := toggle(cfg.SanctionOptInUserIDs, userID, on)
	if !changed {
		core.RespondInfo(s, i, "No change", fmt.Sprintf("%s was already %s.", core.MentionUser(userID), optInWord(on)))
		return
	}
	if err := p.store.SetSanctionOptIn(ctx, i.GuildID, next); err != nil {
		core.RespondErr(s, i, "Failed to update the list", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.optin_changed", "", core.MentionUser(userID)+" "+optInWord(on))

	if !on {
		core.RespondOK(s, i, "Removed from the list",
			fmt.Sprintf("%s is back to the default. For an ordinary member that still means fully moderated: this "+
				"list only ever adds coverage, it never removes it.", core.MentionUser(userID)))
		return
	}

	who := core.MentionUser(userID) + " will"
	if self {
		who = "You will"
	}
	core.RespondOK(s, i, "Added to the list",
		fmt.Sprintf("%s now be jailed by the automatic sanctions like anyone else, even holding a moderator or admin "+
			"role.\n\nMessage removal already applied regardless of rank; this is only about the sanction that follows "+
			"one. Undo it any time with the same command set to false.", who))
}

func optInWord(on bool) string {
	if on {
		return "opted in to automatic sanctions"
	}
	return "on the default (rank-protected) footing"
}
