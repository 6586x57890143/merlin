package aimod

import (
	"context"
	"fmt"
	"slices"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// The opt-out list: members who have asked not to be read by this plugin at
// all, on a server whose owner has decided that is a choice worth offering.
//
// It is the mirror image of optin.go next door and the two must not be
// confused. That list only ever makes somebody *more* moderatable and is
// therefore safe to expose to anyone. This one makes somebody *less*
// moderatable, which is the direction that can be abused, so every part of
// its design is about who is allowed to decide and how little the decision
// can be stretched.
//
// Three separate limits, none of which is redundant:
//
//  1. **The guild switch is off until a guild owner turns it on**
//     (Config.MemberOptOut, default false, migration 0031). While it is off
//     the list below is inert: still stored, still shown on /aimod status,
//     read by nothing on the message path. So this feature changes the
//     behaviour of exactly zero existing deployments until somebody asks for
//     it, which is the same shape as ModeOff being this plugin's default in
//     the first place.
//
//  2. **Only the guild owner or the bootstrap operator may flip that switch**
//     (ownerOrOperator, the same gate the tip jar's address uses, and for a
//     related reason). TierAdmin is the coarse floor on the command; it is
//     not the real check. A guild with five admins would otherwise have five
//     accounts that can quietly open a hole in the server's own moderation,
//     and the person who carries the consequences of that hole is the owner.
//
//  3. **Nobody opts anybody else out.** /aimod opt-out sets the caller and
//     only the caller, at every tier, with no operator override. This is the
//     exact opposite of optin.go's rule and follows from the same principle:
//     consent to be moderated needs no permission, and consent is not
//     something a third party can give on your behalf. An admin who could opt
//     a member out could also exempt an account they control.
//
// **The child-safety carve-out is not negotiable, and it is the reason this
// file does not simply skip at rung 0.** EffectiveAction refuses to let
// /aimod policy set turn child_safety off, and validateCalibration refuses to
// let the weekly review stand the bucket down, because there is no legitimate
// reason to disable it and so there is no way to. A per-member opt-out that
// suppressed it would be turning the bucket off by a third route neither of
// those guards covers, one command at a time, for whoever wanted it off most.
// So scanExempt yields to mustScan: a message carrying the child-safety
// vocabulary is scanned exactly as it would have been, and everything
// downstream (which enforces that bucket unconditionally) behaves normally.
//
// What the opt-out genuinely buys is not being sent to a model. It is checked
// *after* rung 1, so the free pattern table still runs: a leaked bot token, a
// phishing domain and an SSN cost nothing to catch, and removing them is
// damage control for everyone reading rather than a judgement of the member.
// Opting out of a judgement is a reasonable thing to want; opting out of a
// credential leak being deleted is not a thing anybody is asking for.

// scanExempt reports whether this author's own opt-out keeps this message
// away from the model rungs.
//
// A pure function of the config and the text on purpose: it is on the hot
// path for every message in every guild, it has to be readable in one sitting
// by whoever is deciding whether to trust it, and the mustScan clause is the
// half that most needs to be visible.
func scanExempt(cfg Config, authorID, content string) bool {
	if !cfg.MemberOptOut || !slices.Contains(cfg.OptOutUserIDs, authorID) {
		return false
	}
	// One-way, exactly as in triage: matching here only ever causes a message
	// to be scanned as it otherwise would have been.
	return !mustScan(content)
}

// handleOptOut sets the caller's own opt-out, and nobody else's.
//
// TierPublic, like /aimod moderate-me, because a switch every member is meant
// to be able to reach is not a privilege. The refusals below are about
// whether the guild offers the choice, never about the caller's rank.
func (p *Plugin) handleOptOut(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := ""
	if i.Member != nil && i.Member.User != nil {
		userID = i.Member.User.ID
	}
	if userID == "" {
		core.RespondErr(s, i, "Could not tell who you are", fmt.Errorf("run this in a server, not a DM"))
		return
	}

	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}

	on := core.LeafArgs(i)["enabled"].BoolValue()

	// Refused rather than stored-for-later when the guild has not enabled it.
	// A command that accepted the setting and did nothing would leave
	// somebody believing they had opted out of something they had not, which
	// is the one misunderstanding this feature cannot afford.
	//
	// Turning it *off* stays allowed even here: removing yourself from an
	// inert list can only ever move you toward the default.
	if on && !cfg.MemberOptOut {
		core.RespondWarn(s, i, "This server doesn't offer that",
			"AI moderation applies to everyone here. Only the server owner can turn the opt-out on, with "+
				"`/aimod configure member-opt-out enabled:true`.")
		return
	}

	next, changed := toggle(cfg.OptOutUserIDs, userID, on)
	if !changed {
		core.RespondInfo(s, i, "No change", fmt.Sprintf("You were already %s.", optOutWord(on)))
		return
	}
	if err := p.store.SetOptOut(ctx, i.GuildID, next); err != nil {
		core.RespondErr(s, i, "Failed to update the list", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.optout_changed", "", core.MentionUser(userID)+" "+optOutWord(on))

	if !on {
		core.RespondOK(s, i, "Back in", "Your messages are read by the filter again, like everyone else's.")
		return
	}
	core.RespondOK(s, i, "Opted out",
		"I'll stop sending your messages to a model. Two things that does not cover, so you know where you stand:\n\n"+
			"- The built-in pattern checks still run on everyone. They catch leaked credentials and phishing links, "+
			"and none of it is a judgement of what you said.\n"+
			"- Anything reading as child safety is still checked. That one has no opt-out on this bot, for anybody.\n\n"+
			"Moderators can see who has opted out, on `/aimod status`. Undo it any time with the same command set to false.")
}

// handleSetMemberOptOut flips the guild's switch. Owner or operator only; see
// the file comment for why that is narrower than the TierAdmin on the leaf.
func (p *Plugin) handleSetMemberOptOut(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	actor := ""
	if i.Member != nil && i.Member.User != nil {
		actor = i.Member.User.ID
	}
	allowed, err := p.ownerOrOperator(i.GuildID, actor)
	if err != nil {
		core.RespondErr(s, i, "Could not check who you are", err)
		return
	}
	if !allowed {
		core.RespondWarn(s, i, "Not yours to set",
			"Only the server owner can decide whether members may opt out of AI moderation. It is the one setting "+
				"here that makes the filter cover less of the server, so it does not travel with admin.")
		return
	}

	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Failed to read the configuration", err)
		return
	}
	on := core.LeafArgs(i)["enabled"].BoolValue()
	if on == cfg.MemberOptOut {
		core.RespondInfo(s, i, "No change", fmt.Sprintf("Member opt-out is already %s.", onOff(on)))
		return
	}
	if err := p.store.SetMemberOptOut(ctx, i.GuildID, on); err != nil {
		core.RespondErr(s, i, "Failed to update the setting", err)
		return
	}
	p.auditConfig(ctx, i, "aimod.member_opt_out_set", onOff(cfg.MemberOptOut), onOff(on))

	if !on {
		// The list survives, and saying so is the point: an owner who turns
		// this off and back on next month should not be surprised to find the
		// same people still opted out, and one who wants it genuinely empty
		// needs to know it is not.
		core.RespondOK(s, i, "Member opt-out off",
			fmt.Sprintf("Everyone is covered by the filter again, including the %d who had opted out. Their choices "+
				"are kept rather than discarded, so turning this back on restores them instead of re-enrolling everybody.",
				len(cfg.OptOutUserIDs)))
		return
	}
	core.RespondWarn(s, i, "Member opt-out on",
		"Members can now run `/aimod opt-out enabled:true` and their messages stop being sent to a model.\n\n"+
			"What that does not cover: the built-in pattern checks still run on everyone, and anything reading as "+
			"child safety is still scanned regardless. Nobody can opt anybody else out, and `/aimod status` lists who has.")
}

func optOutWord(on bool) string {
	if on {
		return "opted out of AI moderation"
	}
	return "covered by AI moderation"
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
