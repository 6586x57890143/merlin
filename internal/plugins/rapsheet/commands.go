package rapsheet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// Action names, split by blast radius the way roles splits its own: a guild
// may hand `rapsheet.warn` to a trusted helper role via /config permissions
// allow without also handing them `rapsheet.void`, and the ban leaf sits at
// TierAdmin by default with its own action so lowering it is a decision a
// guild makes on purpose.
const (
	actionView      = "rapsheet.view"
	actionMe        = "rapsheet.me"
	actionWarn      = "rapsheet.warn"
	actionNote      = "rapsheet.note"
	actionVoid      = "rapsheet.void"
	actionEdit      = "rapsheet.edit"
	actionConfigure = "rapsheet.configure"
)

const (
	cmdBan       = "ban"
	cmdKick      = "kick"
	cmdTimeout   = "timeout"
	maxReasonLen = 500
	// maxOptionChoices is Discord's ceiling on a Choices list.
	maxOptionChoices = 25
)

func (p *Plugin) registerCommands() {
	userOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionUser, Name: name, Description: desc, Required: true}
	}
	reasonOpt := func() *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type: discordgo.ApplicationCommandOptionString, Name: "reason", Required: true, MaxLength: maxReasonLen,
			Description: "Why. The member sees this, and so does the next moderator.",
		}
	}
	caseOpt := &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionInteger, Name: "case", Required: true, MinValue: ptr(1.0),
		Description: "The case number, as shown on the sheet (#12 is 12).",
	}
	pageOpt := &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionInteger, Name: "page", MinValue: ptr(1.0),
		Description: "Page of entries to open at (newest first).",
	}

	cmd := &discordgo.ApplicationCommand{
		Name:        "rapsheet",
		Description: "Per-member moderation history: warnings, jails, timeouts, bans and what they add up to.",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "view",
				Description: "Read a member's rapsheet.",
				Options: []*discordgo.ApplicationCommandOption{
					// Text, not a User option, because the user picker only
					// lists current members and a sheet outlives membership.
					// Typing @ still opens Discord's member picker here,
					// which inserts <@id>; a bare ID reaches anyone.
					{
						Type: discordgo.ApplicationCommandOptionString, Name: "user", Required: true, MaxLength: 32,
						Description: "Whose sheet to read: @name, or a Discord user ID for somebody who has left.",
					},
					pageOpt,
					{
						Type: discordgo.ApplicationCommandOptionBoolean, Name: "public",
						Description: "Post it in this channel so it can be forwarded. Leaves out linked accounts and alt hints.",
					},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "me",
				Description: "Read your own record in this server.",
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "warn",
				Description: "Warn a member. Goes on their sheet, DMs them, and counts toward the ladder.",
				Options: []*discordgo.ApplicationCommandOption{
					userOpt("user", "Who to warn."),
					categoryOption(),
					reasonOpt(),
					{
						Type: discordgo.ApplicationCommandOptionInteger, Name: "points",
						Description: fmt.Sprintf("Override the category's points for this one (1-%d).", maxPoints),
						MinValue:    ptr(1.0), MaxValue: maxPoints,
					},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "note",
				Description: "Add a note to a member's sheet. Mods only, no points, no DM.",
				Options:     []*discordgo.ApplicationCommandOption{userOpt("user", "Who the note is about."), reasonOpt()},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "unban",
				Description: "Lift a ban, whoever placed it.",
				Options:     []*discordgo.ApplicationCommandOption{userOpt("user", "Who to unban."), reasonOpt()},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "summary",
				Description: "A short written summary of a member's record, from the guild's AI moderation model.",
				Options:     []*discordgo.ApplicationCommandOption{userOpt("user", "Whose record to summarise.")},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "link",
				Description: "Say two accounts are the same person. They share one sheet and one score from then on.",
				Options: []*discordgo.ApplicationCommandOption{
					userOpt("user", "The account to link."),
					userOpt("other", "The account already on record."),
					reasonOpt(),
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "unlink",
				Description: "Take an account back out of a linked group.",
				Options:     []*discordgo.ApplicationCommandOption{userOpt("user", "The account to unlink.")},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "void",
				Description: "Strike an entry. It stays visible, struck through, and counts for nothing.",
				Options:     []*discordgo.ApplicationCommandOption{caseOpt, reasonOpt()},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "edit",
				Description: "Rewrite an entry's reason.",
				Options:     []*discordgo.ApplicationCommandOption{caseOpt, reasonOpt()},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "list",
				Description: "The categories and the ladder, as configured here.",
				Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "categories", Description: "Every category and what one offence in it is worth."},
					{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "bands", Description: "The ladder: which score owes which consequence."},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "status",
				Description: "How rapsheets are set up in this server.",
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "settings",
				Description: "The ladder, half-life, mod channel, case files, alt hints and points, on one panel.",
			},
		},
	}
	p.commands.RegisterCommand(p.Name(), cmd)

	// /ban, /kick and /timeout are top level rather than under /rapsheet because
	// they are the three a moderator reaches for under pressure and types by
	// name. They stay this plugin's (disabling rapsheet disables them) and
	// keep their rapsheet.* action names, so a guild's existing tier, allow
	// and deny settings still apply.
	p.commands.RegisterCommand(p.Name(), &discordgo.ApplicationCommand{
		Name:        cmdTimeout,
		Description: "Time a member out (Discord's own mute, up to 28 days). Goes on their rapsheet.",
		Options: []*discordgo.ApplicationCommandOption{
			userOpt("user", "Who to time out."),
			{Type: discordgo.ApplicationCommandOptionString, Name: "duration", Required: true, Description: "How long: 10m, 2h, 3d, up to 28d."},
			categoryOption(),
			reasonOpt(),
		},
	})
	p.commands.RegisterCommand(p.Name(), &discordgo.ApplicationCommand{
		Name:        cmdKick,
		Description: "Remove a member from the server. They can rejoin. Goes on their rapsheet.",
		Options:     []*discordgo.ApplicationCommandOption{userOpt("user", "Who to kick."), categoryOption(), reasonOpt()},
	})
	p.commands.RegisterCommand(p.Name(), &discordgo.ApplicationCommand{
		Name:        cmdBan,
		Description: "Ban a member, for a while or for good. merlin lifts a temporary ban itself.",
		Options: []*discordgo.ApplicationCommandOption{
			userOpt("user", "Who to ban."),
			categoryOption(),
			reasonOpt(),
			{Type: discordgo.ApplicationCommandOptionString, Name: "duration", Description: "How long: 7d, 30d, up to 365d. Leave out for a permanent ban, and say so."},
			{Type: discordgo.ApplicationCommandOptionBoolean, Name: "permanent", Description: "A ban with no end date. Required if no duration is given."},
			{Type: discordgo.ApplicationCommandOptionInteger, Name: "delete_message_days", Description: "Also delete their messages from the last N days (0-7).", MinValue: ptr(0.0), MaxValue: maxDeleteDays},
		},
	})

	view := core.PermSpec{Tier: core.TierMod, Action: actionView}
	p.commands.Handle("rapsheet", "view", view, p.handleView)
	p.commands.Handle("rapsheet", "me", core.PermSpec{Tier: core.TierPublic, Action: actionMe}, p.handleMe)
	p.commands.Handle("rapsheet", "warn", core.PermSpec{Tier: core.TierMod, Action: actionWarn}, p.handleWarn)
	p.commands.Handle("rapsheet", "note", core.PermSpec{Tier: core.TierMod, Action: actionNote}, p.handleNote)
	p.commands.Handle(cmdTimeout, "", core.PermSpec{Tier: core.TierMod, Action: actionTimeout}, p.handleTimeout)
	p.commands.Handle(cmdKick, "", core.PermSpec{Tier: core.TierMod, Action: actionKick}, p.handleKick)
	// Ban is TierAdmin by default: it is the one consequence here a member
	// cannot see the end of from inside the server, and a guild that wants
	// mods to hold it lowers the bar on purpose with set-tier.
	p.commands.Handle(cmdBan, "", core.PermSpec{Tier: core.TierAdmin, Action: actionBan}, p.handleBan)
	p.commands.Handle("rapsheet", "unban", core.PermSpec{Tier: core.TierAdmin, Action: actionUnban}, p.handleUnban)
	p.commands.Handle("rapsheet", "summary", core.PermSpec{Tier: core.TierMod, Action: actionSummary}, p.handleSummary)
	linkSpec := core.PermSpec{Tier: core.TierMod, Action: actionLink}
	p.commands.Handle("rapsheet", "link", linkSpec, p.handleLink)
	p.commands.Handle("rapsheet", "unlink", linkSpec, p.handleUnlink)
	p.commands.HandleComponent(p.Name(), altPrefix, linkSpec, p.handleAltButton)
	p.commands.Handle("rapsheet", "void", core.PermSpec{Tier: core.TierMod, Action: actionVoid}, p.handleVoid)
	p.commands.Handle("rapsheet", "edit", core.PermSpec{Tier: core.TierMod, Action: actionEdit}, p.handleEdit)
	p.commands.Handle("rapsheet", "list/categories", view, p.handleListCategories)
	p.commands.Handle("rapsheet", "list/bands", view, p.handleListBands)
	p.commands.Handle("rapsheet", "status", view, p.handleStatus)
	p.commands.Handle("rapsheet", "settings", view, p.handleSettings)
	p.registerPanel()

	p.commands.HandleComponent(p.Name(), suggestPrefix, core.PermSpec{Tier: core.TierMod, Action: actionApply}, p.handleSuggestion)
	p.commands.HandleComponent(p.Name(), viewPrefix, view, p.handleViewPage)
	// Paging a public post is mod-only like the post itself: the button id
	// names the member, and the sheet it renders is one a mod chose to show.
	p.commands.HandleComponent(p.Name(), pubPrefix, view, p.handlePubPage)
	p.commands.HandleComponent(p.Name(), mePrefix, core.PermSpec{Tier: core.TierPublic, Action: actionMe}, p.handleMePage)
}

// categoryOption is the fixed category picker. Twelve values known at
// compile time, so a plain Choices list rather than autocomplete (spec.MD
// §4a's autocomplete rule is for values that come from bot state).
func categoryOption() *discordgo.ApplicationCommandOption {
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(categories))
	for _, c := range categories {
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: categoryLabel(c), Value: string(c)})
	}
	if len(choices) > maxOptionChoices {
		panic("rapsheet: more categories than Discord allows choices")
	}
	return &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: "category", Required: true,
		Description: "Which rule it broke. Decides the points.",
		Choices:     choices,
	}
}

func ptr[T any](v T) *T { return &v }

func actorID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	return ""
}

// resolvedUser is the user object Discord attached to a User option, free
// with the interaction, or nil for an id it did not resolve.
func resolvedUser(i *discordgo.InteractionCreate, userID string) *discordgo.User {
	data := i.ApplicationCommandData()
	if data.Resolved == nil {
		return nil
	}
	return data.Resolved.Users[userID]
}

// viewTarget reads /rapsheet view's user input: an @mention (<@id> or
// <@!id>) or a bare user ID. A User-typed option is taken as is, which is
// what a guild still holding the old registration sends until the next
// GuildCreate re-registers it.
//
// Text carries no resolved user, so a bare ID costs one lookup. It names
// the sheet, and it is the only thing that tells a typo from somebody
// with a clean record: Discord saying the account does not exist refuses;
// any other failure renders without a name rather than blocking a read.
func (p *Plugin) viewTarget(i *discordgo.InteractionCreate, o *discordgo.ApplicationCommandInteractionDataOption) (string, *discordgo.User, error) {
	if o.Type == discordgo.ApplicationCommandOptionUser {
		id := o.Value.(string)
		return id, resolvedUser(i, id), nil
	}
	raw := strings.TrimSpace(o.StringValue())
	id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(raw, "<@"), "!"), ">")
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", nil, fmt.Errorf("%q is not a member or a user ID; use @name, or paste the numeric ID", raw)
	}
	u, err := p.ops(i.GuildID).User(id)
	if err != nil {
		if core.IsUnknownResource(err) {
			return "", nil, fmt.Errorf("no Discord account has the ID %s", id)
		}
		p.log.Error("rapsheet: look up user", "user", id, "err", err)
		return id, nil, nil
	}
	return id, u, nil
}

// displayName is how the sheet titles its subject: the case file's snapshot
// when there is one, the resolved user otherwise, the bare id as a last
// resort. Never a REST call for a title.
func displayName(u *discordgo.User, cf *CaseFile, userID string) string {
	if u != nil {
		if u.GlobalName != "" {
			return u.GlobalName + " (" + u.Username + ")"
		}
		return u.Username
	}
	if cf != nil && cf.Username != "" {
		if cf.GlobalName != "" {
			return cf.GlobalName + " (" + cf.Username + ")"
		}
		return cf.Username
	}
	return userID
}

// --- view -----------------------------------------------------------------

func (p *Plugin) handleView(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	args := core.LeafArgs(i)
	userID, u, err := p.viewTarget(i, args["user"])
	if err != nil {
		core.RespondErr(s, i, "Rapsheet", err)
		return
	}
	page := 0
	if a, ok := args["page"]; ok {
		page = int(a.IntValue()) - 1
	}
	if a, ok := args["public"]; ok && a.BoolValue() {
		p.postView(ctx, s, i, userID, u, page)
		return
	}
	embed, components, err := p.renderFor(ctx, i.GuildID, userID, u, page, false, false)
	if err != nil {
		core.RespondErr(s, i, "Rapsheet", err)
		return
	}
	if err := core.RespondEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rapsheet: view response", "err", err)
	}
}

// postView answers /rapsheet view public:true in the channel rather than
// privately, because Discord will not forward an ephemeral message. It is
// the mod view with the linked-accounts and alt-hint lines left off (see
// view.go), since a channel post can be forwarded anywhere.
func (p *Plugin) postView(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, userID string, u *discordgo.User, page int) {
	embed, components, err := p.renderFor(ctx, i.GuildID, userID, u, page, false, true)
	if err != nil {
		core.RespondErr(s, i, "Rapsheet", err)
		return
	}
	err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds:          []*discordgo.MessageEmbed{embed},
			Components:      components,
			Files:           core.EmbedFiles(embed),
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		},
	})
	if err != nil {
		p.log.Error("rapsheet: public view response", "err", err)
	}
}

func (p *Plugin) handleViewPage(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	p.turnPage(ctx, s, i, customID, viewPrefix, false)
}

// handlePubPage pages a public post. Its buttons carry their own prefix so
// a click keeps the alts lines off: sharing the mod view's buttons would put
// linked accounts and hints into the channel on the first click.
func (p *Plugin) handlePubPage(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	p.turnPage(ctx, s, i, customID, pubPrefix, true)
}

func (p *Plugin) turnPage(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID, prefix string, hideAlts bool) {
	userID, page, err := parseSheetCustomID(prefix, customID)
	if err != nil {
		p.log.Error("rapsheet: parse sheet page", "custom_id", customID, "err", err)
		return
	}
	embed, components, err := p.renderFor(ctx, i.GuildID, userID, nil, page, false, hideAlts)
	if err != nil {
		p.log.Error("rapsheet: sheet page", "err", err)
		return
	}
	if err := core.UpdateEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rapsheet: sheet page update", "err", err)
	}
}

func (p *Plugin) handleMe(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := actorID(i)
	embed, components, err := p.renderFor(ctx, i.GuildID, userID, i.Member.User, 0, true, false)
	if err != nil {
		core.RespondErr(s, i, "Your record", err)
		return
	}
	if err := core.RespondEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rapsheet: me response", "err", err)
	}
}

func (p *Plugin) handleMePage(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	page, err := core.ParsePaginationPage(customID, mePrefix)
	if err != nil {
		p.log.Error("rapsheet: parse me page", "custom_id", customID, "err", err)
		return
	}
	// The subject is whoever clicked, never anything in the CustomID: a
	// member paging their own sheet must not be able to page somebody
	// else's by editing a button id.
	embed, components, err := p.renderFor(ctx, i.GuildID, actorID(i), i.Member.User, page, true, false)
	if err != nil {
		p.log.Error("rapsheet: me page", "err", err)
		return
	}
	if err := core.UpdateEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rapsheet: me page update", "err", err)
	}
}

// renderFor loads and renders one member's sheet for either audience;
// hideAlts drops the linked-accounts and alt-hint lines from the mod view.
func (p *Plugin) renderFor(ctx context.Context, guildID, userID string, u *discordgo.User, page int, forMember, hideAlts bool) (*discordgo.MessageEmbed, []discordgo.MessageComponent, error) {
	cfg := p.config(ctx, guildID)
	sh, err := p.loadSheet(ctx, cfg, guildID, userID)
	if err != nil {
		return nil, nil, err
	}
	var cfPtr *CaseFile
	if cf, ok, err := p.store.CaseFile(ctx, guildID, userID); err == nil && ok {
		cfPtr = &cf
	}
	var hints []AltHint
	if !forMember && !hideAlts {
		if hints, err = p.store.Hints(ctx, guildID, userID); err != nil {
			// Hints are decoration on the sheet, not the sheet.
			p.log.Error("rapsheet: read hints", "guild", guildID, "user", userID, "err", err)
		}
	}
	embed, components := renderSheet(sheetView{
		Sheet: sh, Config: cfg, UserID: userID, Name: displayName(u, cfPtr, userID),
		Now: p.now(), Page: page, ForMember: forMember, HideAlts: hideAlts, Hints: hints,
	})
	return embed, components, nil
}

// --- warn / note ------------------------------------------------------------

// checkTarget is the rank check every leaf that puts points on a sheet or
// restricts a member runs first. Same rule as /roles jail: a mod may act on
// anyone who is not admin-equivalent, an admin on anyone but the bootstrap
// operator, and an unresolvable target is refused rather than assumed
// ordinary. Bots and the actor themselves are refused outright, since
// neither is a moderation decision.
//
// present reports whether the target is in the server. Somebody who left
// still has a sheet, and a warning or a ban about them is a legitimate
// thing to record, so the rank check runs on an empty role set (which still
// refuses the bootstrap operator and DB-listed admins) and the caller
// decides whether its action makes sense for an absent member.
func (p *Plugin) checkTarget(_ context.Context, i *discordgo.InteractionCreate, userID string) (member *discordgo.Member, present bool, err error) {
	if userID == actorID(i) {
		return nil, false, errors.New("you cannot put an entry on your own sheet")
	}
	if u := resolvedUser(i, userID); u != nil && u.Bot {
		return nil, false, errors.New("that is a bot")
	}
	member, err = p.ops(i.GuildID).GuildMember(i.GuildID, userID)
	present = err == nil
	if err != nil {
		if !core.IsUnknownResource(err) {
			return nil, false, fmt.Errorf("look up member: %w", err)
		}
		member = &discordgo.Member{User: &discordgo.User{ID: userID}}
	}
	if err := p.perms.CanModerate(i.GuildID, i.Member, userID, member.Roles); err != nil {
		var forbidden core.ErrForbidden
		if errors.As(err, &forbidden) {
			if strings.Contains(forbidden.Reason, "bootstrap") {
				return nil, false, fmt.Errorf("%s is the bootstrap operator and cannot be actioned; nothing was recorded", core.MentionUser(userID))
			}
			return nil, false, fmt.Errorf("%s outranks you; nothing was recorded", core.MentionUser(userID))
		}
		return nil, false, err
	}
	return member, present, nil
}

func (p *Plugin) handleWarn(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	args := core.LeafArgs(i)
	userID := args["user"].Value.(string)
	category := Category(args["category"].StringValue())
	reason := strings.TrimSpace(args["reason"].StringValue())
	override := 0
	if a, ok := args["points"]; ok {
		override = int(a.IntValue())
	}

	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("rapsheet: defer warn", "err", err)
		return
	}
	if _, _, err := p.checkTarget(ctx, i, userID); err != nil {
		_ = core.FollowUpErr(s, i, "Not warned", err)
		return
	}

	cfg := p.config(ctx, i.GuildID)
	e, _, err := p.record(ctx, cfg, newEntry{
		GuildID: i.GuildID, UserID: userID, Kind: KindWarn, Category: category,
		ActorID: actorID(i), Reason: reason, Source: SourceCommand,
		PointsOverride: override, Identity: resolvedUser(i, userID),
	})
	if err != nil {
		_ = core.FollowUpErr(s, i, "Not warned", err)
		return
	}

	p.notifyWarned(ctx, i.GuildID, userID, category, reason)
	p.auditEntry(ctx, "rapsheet.warn", e)
	_ = core.FollowUpOK(s, i, "Warned", p.caseSummary(ctx, cfg, e))
}

func (p *Plugin) handleNote(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	args := core.LeafArgs(i)
	userID := args["user"].Value.(string)
	reason := strings.TrimSpace(args["reason"].StringValue())

	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("rapsheet: defer note", "err", err)
		return
	}
	// A note is not a moderation action, so no rank check: a mod may note
	// that an admin said something worth remembering. It carries no points
	// and is never shown to its subject, so there is nothing to escalate
	// and nothing to retaliate against.
	cfg := p.config(ctx, i.GuildID)
	e, _, err := p.record(ctx, cfg, newEntry{
		GuildID: i.GuildID, UserID: userID, Kind: KindNote,
		ActorID: actorID(i), Reason: reason, Source: SourceCommand, Identity: resolvedUser(i, userID),
	})
	if err != nil {
		_ = core.FollowUpErr(s, i, "Note", err)
		return
	}
	p.auditEntry(ctx, "rapsheet.note", e)
	_ = core.FollowUpOK(s, i, "Noted", fmt.Sprintf("Case #%d added to %s's sheet.", e.ID, core.MentionUser(userID)))
}

// caseSummary is the follow-up after a scored entry: the case number, what
// was recorded, where it leaves the score, and what the ladder did about
// it, so a mod sees the whole effect without opening the sheet. The ladder
// clause is the ladder's own account (recorded on the entry by escalate),
// never a bare band name, which read as a consequence being owed.
func (p *Plugin) caseSummary(ctx context.Context, cfg Config, e Entry) string {
	line := fmt.Sprintf("Case #%d: %s %s", e.ID, core.MentionUser(e.UserID), kindWords(e))
	if e.Points > 0 {
		line += fmt.Sprintf(" (%d pts)", e.Points)
	}
	line += "."
	sh, err := p.loadSheet(ctx, cfg, e.GuildID, e.UserID)
	if err != nil {
		return line
	}
	line += fmt.Sprintf(" Score is now %.0f.", sh.Score)
	if e.ladderNote != "" {
		line += "\n" + e.ladderNote
	}
	return line
}

// auditEntry writes the audit line for a command-made entry. Log-and-
// continue, per the audit-failure policy: the entry is already recorded.
func (p *Plugin) auditEntry(ctx context.Context, action string, e Entry) {
	detail := fmt.Sprintf("case #%d user=%s category=%s points=%d reason=%q",
		e.ID, core.MentionUser(e.UserID), e.Category, e.Points, e.Reason)
	if e.Duration > 0 {
		detail += " duration=" + core.FormatDuration(e.Duration)
	}
	if err := p.audit.Record(ctx, e.GuildID, e.ActorID, action, "", detail); err != nil {
		p.log.Error("rapsheet: audit", "action", action, "guild", e.GuildID, "err", err)
	}
}

// --- void / edit ------------------------------------------------------------

// canAmend decides whether actor may void or edit e.
//
// Bernard gates this on role hierarchy and so does this: a mod may not
// strike an admin's entry. Concretely, anyone may amend an automatic entry
// (correcting the machine is what humans are for) or their own; anything
// else runs CanModerate against the entry's recorder, which refuses when
// they are admin-equivalent and the amender is not. A recorder who has left
// the server outranks nobody. An unresolvable recorder fails closed.
func (p *Plugin) canAmend(guildID string, actor *discordgo.Member, e Entry) error {
	if e.ActorID == core.ActorSystem || e.ActorID == "" || (actor != nil && actor.User != nil && e.ActorID == actor.User.ID) {
		return nil
	}
	recorder, err := p.ops(guildID).GuildMember(guildID, e.ActorID)
	if err != nil {
		if core.IsUnknownResource(err) {
			return nil
		}
		return fmt.Errorf("look up who recorded #%d: %w", e.ID, err)
	}
	if err := p.perms.CanModerate(guildID, actor, e.ActorID, recorder.Roles); err != nil {
		return fmt.Errorf("case #%d was recorded by %s, who outranks you", e.ID, core.MentionUser(e.ActorID))
	}
	return nil
}

func (p *Plugin) handleVoid(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	args := core.LeafArgs(i)
	id := args["case"].IntValue()
	reason := strings.TrimSpace(args["reason"].StringValue())

	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("rapsheet: defer void", "err", err)
		return
	}
	e, err := p.store.Entry(ctx, i.GuildID, id)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Not voided", err)
		return
	}
	if e.Voided() {
		_ = core.FollowUpErr(s, i, "Not voided", fmt.Errorf("case #%d is already voided", e.ID))
		return
	}
	if err := p.canAmend(i.GuildID, i.Member, e); err != nil {
		_ = core.FollowUpErr(s, i, "Not voided", err)
		return
	}
	if e.Kind == KindBan && e.Standing(p.now()) {
		// Voiding the record of a ban that is still in force would leave a
		// banned member with nothing saying so and nothing scheduled to lift
		// it. The unban is the decision; the void follows from it.
		_ = core.FollowUpErr(s, i, "Not voided", fmt.Errorf("case #%d is a ban still in force; unban first", e.ID))
		return
	}
	now := p.now()
	// Read before the entry is marked voided below: a voided entry is by
	// definition not standing, and the consequence still is.
	wasStanding := e.Standing(now)
	if err := p.store.Void(ctx, i.GuildID, e.ID, actorID(i), reason, now); err != nil {
		_ = core.FollowUpErr(s, i, "Not voided", err)
		return
	}
	e.VoidedAt, e.VoidedBy, e.VoidReason = &now, actorID(i), reason
	p.afterAmend(ctx, e)
	if e.Points > 0 {
		p.withdrawStaleSuggestions(ctx, p.config(ctx, i.GuildID), i.GuildID, e.UserID)
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "rapsheet.void", "",
		fmt.Sprintf("case #%d user=%s was %s reason=%q", e.ID, core.MentionUser(e.UserID), kindWords(e), reason)); err != nil {
		p.log.Error("rapsheet: audit void", "guild", i.GuildID, "err", err)
	}
	msg := fmt.Sprintf("Case #%d voided. It stays on the sheet, struck through, and counts for nothing.", e.ID)
	switch {
	case e.Kind == KindJail && wasStanding:
		msg += " The jail itself is still in force; `/roles release` ends it."
	case e.Kind == KindTimeout && wasStanding:
		// A timeout is this plugin's own to lift, so voiding its record
		// lifts it: a struck-through timeout that stays in force would be
		// the sheet saying one thing and Discord doing another.
		if err := p.ops(i.GuildID).GuildMemberTimeout(i.GuildID, e.UserID, nil); err != nil {
			msg += fmt.Sprintf(" The timeout itself could not be lifted (%v); clear it by hand.", err)
		} else {
			msg += " The timeout was lifted."
		}
	}
	_ = core.FollowUpOK(s, i, "Voided", msg)
}

func (p *Plugin) handleEdit(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	args := core.LeafArgs(i)
	id := args["case"].IntValue()
	reason := strings.TrimSpace(args["reason"].StringValue())

	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("rapsheet: defer edit", "err", err)
		return
	}
	e, err := p.store.Entry(ctx, i.GuildID, id)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Not edited", err)
		return
	}
	if err := p.canAmend(i.GuildID, i.Member, e); err != nil {
		_ = core.FollowUpErr(s, i, "Not edited", err)
		return
	}
	old := e.Reason
	if err := p.store.UpdateReason(ctx, i.GuildID, e.ID, reason); err != nil {
		_ = core.FollowUpErr(s, i, "Not edited", err)
		return
	}
	e.Reason = reason
	p.afterAmend(ctx, e)
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "rapsheet.edit", old, fmt.Sprintf("case #%d: %s", e.ID, reason)); err != nil {
		p.log.Error("rapsheet: audit edit", "guild", i.GuildID, "err", err)
	}
	_ = core.FollowUpOK(s, i, "Edited", fmt.Sprintf("Case #%d's reason updated.", e.ID))
}

// afterAmend is everything that follows a void or edit and must not fail
// it: the mirrored message is edited in place rather than posted again.
func (p *Plugin) afterAmend(ctx context.Context, e Entry) {
	p.remirror(p.config(ctx, e.GuildID), e)
}

// --- list / status ----------------------------------------------------------

func (p *Plugin) handleListCategories(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg := p.config(ctx, i.GuildID)
	// Highest first, then by name, so the list reads as the ladder does.
	sorted := append([]Category(nil), categories...)
	pts := func(c Category) int { return pointsFor(cfg, KindWarn, c, actorID(i), 0) }
	sort.SliceStable(sorted, func(a, b int) bool {
		if pts(sorted[a]) != pts(sorted[b]) {
			return pts(sorted[a]) > pts(sorted[b])
		}
		return sorted[a] < sorted[b]
	})
	var b strings.Builder
	for _, c := range sorted {
		fmt.Fprintf(&b, "**%s** · %d pts", categoryLabel(c), pts(c))
		if _, tuned := cfg.CategoryPoints[c]; tuned {
			b.WriteString(" (tuned)")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nThese are what one offence costs when a moderator records it, or when aimod removes a message. A jail, timeout or ban that follows from an offence carries no points of its own: the offence already did.")
	core.RespondInfo(s, i, "Categories", b.String())
}

func (p *Plugin) handleListBands(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg := p.config(ctx, i.GuildID)
	bands := cfg.Bands
	note := ""
	if len(bands) == 0 {
		bands = defaultBands
		note = "\nThese are the defaults; Ladder bands on `/rapsheet settings` changes them."
	}
	var b strings.Builder
	for _, band := range bands {
		b.WriteString(band.String() + "\n")
	}
	fmt.Fprintf(&b, "\nMode: **%s**. Half-life: **%s**.%s", cfg.EscalationMode, core.FormatDuration(cfg.HalfLife), note)
	core.RespondInfo(s, i, "Ladder", b.String())
}

func (p *Plugin) handleStatus(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg := p.config(ctx, i.GuildID)
	var b strings.Builder
	warn := false
	fmt.Fprintf(&b, "**Escalation:** %s\n", cfg.EscalationMode)
	fmt.Fprintf(&b, "**Half-life:** %s\n", core.FormatDuration(cfg.HalfLife))
	if cfg.ModChannelID != "" {
		fmt.Fprintf(&b, "**Mod channel:** %s\n", core.MentionChannel(cfg.ModChannelID))
	} else {
		b.WriteString("**Mod channel:** not set. Ladder suggestions and alt hints have nowhere to go.\n")
		if cfg.EscalationMode != ModeOff {
			warn = true
		}
	}
	switch ok, err := p.auditLogPermission(i.GuildID); {
	case err != nil:
		// Not interpolated: a Discord error is a paragraph, and the one
		// fact that matters here is that the check did not happen.
		p.log.Warn("rapsheet: check View Audit Log", "guild", i.GuildID, "err", err)
		b.WriteString("**Discord's own bans and kicks:** could not check whether merlin holds View Audit Log; try again in a moment.\n")
	case ok:
		b.WriteString("**Discord's own bans and kicks:** recorded (View Audit Log held)\n")
	default:
		b.WriteString("⚠️ merlin does not hold View Audit Log, so bans, kicks and timeouts done through Discord itself are not recorded. Re-invite with the rapsheet link in the README.\n")
		warn = true
	}
	if n, err := p.store.CountPendingBans(ctx, i.GuildID); err == nil && n > 0 {
		fmt.Fprintf(&b, "**Temporary bans pending:** %d\n", n)
	}
	switch {
	case p.reviewer == nil:
		b.WriteString("**Model:** none in this build; summaries and the weekly review are off.\n")
	case p.reviewRegistered[i.GuildID]:
		b.WriteString("**Weekly review:** Mondays 09:00 UTC, in the mod channel, on the AI moderation model and budget.\n")
	default:
		b.WriteString("**Weekly review:** not running (needs a mode other than off and a mod channel). `/rapsheet summary` still works if AI moderation has a key.\n")
	}
	if cfg.ForumChannelID != "" {
		fmt.Fprintf(&b, "**Case files:** %s\n", core.MentionChannel(cfg.ForumChannelID))
		if n, err := p.store.CountUnmirrored(ctx, i.GuildID); err == nil && n > 0 {
			fmt.Fprintf(&b, "⚠️ %d entries are not mirrored into a case file yet.\n", n)
			warn = true
		}
	} else {
		b.WriteString("**Case files:** no forum set; entries live in the database only.\n")
	}
	if warn {
		core.RespondWarn(s, i, "Rapsheet status", strings.TrimRight(b.String(), "\n"))
		return
	}
	core.RespondInfo(s, i, "Rapsheet status", strings.TrimRight(b.String(), "\n"))
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func orUnset(s string) string {
	if s == "" {
		return "not set"
	}
	return s
}

func effectiveBands(bands []Band) []Band {
	if len(bands) == 0 {
		return defaultBands
	}
	return bands
}

func bandsWords(bands []Band) string {
	if len(bands) == 0 {
		return "default"
	}
	parts := make([]string, 0, len(bands))
	for _, b := range bands[1:] {
		parts = append(parts, b.String())
	}
	return strings.Join(parts, ", ")
}

// parseBands reads "25 notice, 50 jail 2h, 100 jail 1d, 200 ban 7d". The
// zero band is implicit. Every rule ValidateBands enforces is reported in
// the same words it uses, since that is the contract.
func parseBands(spec string) ([]Band, error) {
	bands := []Band{{Min: 0, Action: ActionNone}}
	for _, part := range strings.Split(spec, ",") {
		fields := strings.Fields(part)
		if len(fields) < 2 || len(fields) > 3 {
			return nil, fmt.Errorf("%w: %q should read like \"50 jail 2h\" or \"25 notice\"", ErrBadBands, strings.TrimSpace(part))
		}
		minPts, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || minPts <= 0 {
			return nil, fmt.Errorf("%w: %q is not a points threshold", ErrBadBands, fields[0])
		}
		b := Band{Min: minPts, Action: Action(strings.ToLower(fields[1]))}
		if len(fields) == 3 {
			d, err := core.ParseFlexibleDuration(fields[2])
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrBadBands, err)
			}
			b.Duration = d
		}
		bands = append(bands, b)
	}
	if err := ValidateBands(bands); err != nil {
		return nil, err
	}
	return bands, nil
}
