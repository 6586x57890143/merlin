package rapsheet

import (
	"context"
	"errors"
	"fmt"
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
	maxReasonLen = 500
	// maxOptionChoices is Discord's ceiling on a Choices list.
	maxOptionChoices = 25
)

func (p *Plugin) registerCommands() {
	userOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionUser, Name: name, Description: desc, Required: true}
	}
	reasonOpt := func(required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type: discordgo.ApplicationCommandOptionString, Name: "reason", Required: required, MaxLength: maxReasonLen,
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
				Options:     []*discordgo.ApplicationCommandOption{userOpt("user", "Whose sheet to read."), pageOpt},
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
					reasonOpt(true),
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
				Options:     []*discordgo.ApplicationCommandOption{userOpt("user", "Who the note is about."), reasonOpt(true)},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "void",
				Description: "Strike an entry. It stays visible, struck through, and counts for nothing.",
				Options:     []*discordgo.ApplicationCommandOption{caseOpt, reasonOpt(true)},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "edit",
				Description: "Rewrite an entry's reason.",
				Options:     []*discordgo.ApplicationCommandOption{caseOpt, reasonOpt(true)},
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
		},
	}
	p.commands.RegisterCommand(p.Name(), cmd)

	view := core.PermSpec{Tier: core.TierMod, Action: actionView}
	p.commands.Handle("rapsheet", "view", view, p.handleView)
	p.commands.Handle("rapsheet", "me", core.PermSpec{Tier: core.TierPublic, Action: actionMe}, p.handleMe)
	p.commands.Handle("rapsheet", "warn", core.PermSpec{Tier: core.TierMod, Action: actionWarn}, p.handleWarn)
	p.commands.Handle("rapsheet", "note", core.PermSpec{Tier: core.TierMod, Action: actionNote}, p.handleNote)
	p.commands.Handle("rapsheet", "void", core.PermSpec{Tier: core.TierMod, Action: actionVoid}, p.handleVoid)
	p.commands.Handle("rapsheet", "edit", core.PermSpec{Tier: core.TierMod, Action: actionEdit}, p.handleEdit)
	p.commands.Handle("rapsheet", "list/categories", view, p.handleListCategories)
	p.commands.Handle("rapsheet", "list/bands", view, p.handleListBands)
	p.commands.Handle("rapsheet", "status", view, p.handleStatus)

	p.commands.HandleComponent(p.Name(), viewPrefix, view, p.handleViewPage)
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
	userID := args["user"].Value.(string)
	page := 0
	if a, ok := args["page"]; ok {
		page = int(a.IntValue()) - 1
	}
	embed, components, err := p.renderFor(ctx, i.GuildID, userID, resolvedUser(i, userID), page, false)
	if err != nil {
		core.RespondErr(s, i, "Rapsheet", err)
		return
	}
	if err := core.RespondEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rapsheet: view response", "err", err)
	}
}

func (p *Plugin) handleViewPage(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	userID, page, err := parseViewCustomID(customID)
	if err != nil {
		p.log.Error("rapsheet: parse view page", "custom_id", customID, "err", err)
		return
	}
	embed, components, err := p.renderFor(ctx, i.GuildID, userID, nil, page, false)
	if err != nil {
		p.log.Error("rapsheet: view page", "err", err)
		return
	}
	if err := core.UpdateEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rapsheet: view page update", "err", err)
	}
}

func (p *Plugin) handleMe(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := actorID(i)
	embed, components, err := p.renderFor(ctx, i.GuildID, userID, i.Member.User, 0, true)
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
	embed, components, err := p.renderFor(ctx, i.GuildID, actorID(i), i.Member.User, page, true)
	if err != nil {
		p.log.Error("rapsheet: me page", "err", err)
		return
	}
	if err := core.UpdateEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("rapsheet: me page update", "err", err)
	}
}

// renderFor loads and renders one member's sheet for either audience.
func (p *Plugin) renderFor(ctx context.Context, guildID, userID string, u *discordgo.User, page int, forMember bool) (*discordgo.MessageEmbed, []discordgo.MessageComponent, error) {
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
	if !forMember {
		if hints, err = p.store.Hints(ctx, guildID, userID); err != nil {
			// Hints are decoration on the sheet, not the sheet.
			p.log.Error("rapsheet: read hints", "guild", guildID, "user", userID, "err", err)
		}
	}
	embed, components := renderSheet(sheetView{
		Sheet: sh, Config: cfg, UserID: userID, Name: displayName(u, cfPtr, userID),
		Now: p.now(), Page: page, ForMember: forMember, Hints: hints,
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
func (p *Plugin) checkTarget(ctx context.Context, i *discordgo.InteractionCreate, userID string) (*discordgo.Member, error) {
	if userID == actorID(i) {
		return nil, errors.New("you cannot put an entry on your own sheet")
	}
	if u := resolvedUser(i, userID); u != nil && u.Bot {
		return nil, errors.New("that is a bot")
	}
	member, err := p.ops(i.GuildID).GuildMember(i.GuildID, userID)
	if err != nil {
		if core.IsUnknownResource(err) {
			// Not in the server. Their sheet still exists and a note or a
			// warning about somebody who left is a legitimate thing to
			// record, so the rank check runs on an empty role set: it still
			// refuses the bootstrap operator and DB-listed admins.
			member = &discordgo.Member{User: &discordgo.User{ID: userID}}
		} else {
			return nil, fmt.Errorf("look up member: %w", err)
		}
	}
	_ = ctx
	if err := p.perms.CanModerate(i.GuildID, i.Member, userID, member.Roles); err != nil {
		return nil, err
	}
	return member, nil
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
	if _, err := p.checkTarget(ctx, i, userID); err != nil {
		_ = core.FollowUpErr(s, i, "Warn", err)
		return
	}

	cfg := p.config(ctx, i.GuildID)
	e, _, err := p.record(ctx, cfg, newEntry{
		GuildID: i.GuildID, UserID: userID, Kind: KindWarn, Category: category,
		ActorID: actorID(i), Reason: reason, Source: SourceCommand,
		PointsOverride: override, Identity: resolvedUser(i, userID),
	})
	if err != nil {
		_ = core.FollowUpErr(s, i, "Warn", err)
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

// caseSummary is the follow-up after a scored entry: the case number and
// where it leaves the score, so a mod sees the ladder move without opening
// the sheet.
func (p *Plugin) caseSummary(ctx context.Context, cfg Config, e Entry) string {
	sh, err := p.loadSheet(ctx, cfg, e.GuildID, e.UserID)
	if err != nil {
		return fmt.Sprintf("Case #%d recorded for %s.", e.ID, core.MentionUser(e.UserID))
	}
	return fmt.Sprintf("Case #%d recorded for %s (%d pts). Score is now %.0f; ladder: %s.",
		e.ID, core.MentionUser(e.UserID), e.Points, sh.Score, recWords(sh.Rec))
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
		_ = core.FollowUpErr(s, i, "Void", err)
		return
	}
	if e.Voided() {
		_ = core.FollowUpErr(s, i, "Void", fmt.Errorf("case #%d is already voided", e.ID))
		return
	}
	if err := p.canAmend(i.GuildID, i.Member, e); err != nil {
		_ = core.FollowUpErr(s, i, "Void", err)
		return
	}
	if e.Kind == KindBan && e.Standing(p.now()) {
		// Voiding the record of a ban that is still in force would leave a
		// banned member with nothing saying so and nothing scheduled to lift
		// it. The unban is the decision; the void follows from it.
		_ = core.FollowUpErr(s, i, "Void", fmt.Errorf("case #%d is a ban still in force; unban first", e.ID))
		return
	}
	if err := p.store.Void(ctx, i.GuildID, e.ID, actorID(i), reason, p.now()); err != nil {
		_ = core.FollowUpErr(s, i, "Void", err)
		return
	}
	e.VoidedBy, e.VoidReason = actorID(i), reason
	p.afterAmend(ctx, e)
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "rapsheet.void", "",
		fmt.Sprintf("case #%d user=%s was %s reason=%q", e.ID, core.MentionUser(e.UserID), kindWords(e), reason)); err != nil {
		p.log.Error("rapsheet: audit void", "guild", i.GuildID, "err", err)
	}
	msg := fmt.Sprintf("Case #%d voided. It stays on the sheet, struck through, and counts for nothing.", e.ID)
	if e.Kind == KindJail && e.Standing(p.now()) {
		msg += " The jail itself is still in force; `/roles release` ends it."
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
		_ = core.FollowUpErr(s, i, "Edit", err)
		return
	}
	if err := p.canAmend(i.GuildID, i.Member, e); err != nil {
		_ = core.FollowUpErr(s, i, "Edit", err)
		return
	}
	old := e.Reason
	if err := p.store.UpdateReason(ctx, i.GuildID, e.ID, reason); err != nil {
		_ = core.FollowUpErr(s, i, "Edit", err)
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
// it. Filled in by the case-file slice: the mirrored message is edited in
// place rather than posted again.
func (p *Plugin) afterAmend(ctx context.Context, e Entry) {
	_ = ctx
	_ = e
}

// --- list / status ----------------------------------------------------------

func (p *Plugin) handleListCategories(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg := p.config(ctx, i.GuildID)
	var b strings.Builder
	for _, c := range categories {
		pts := pointsFor(cfg, KindWarn, c, actorID(i), 0)
		fmt.Fprintf(&b, "`%s` · %d pts", c, pts)
		if _, tuned := cfg.CategoryPoints[c]; tuned {
			b.WriteString(" (tuned)")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nAn automatic consequence (a jail aimod or the ladder applied) carries no points of its own: the offence behind it already did.")
	core.RespondInfo(s, i, "Categories", b.String())
}

func (p *Plugin) handleListBands(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg := p.config(ctx, i.GuildID)
	bands := cfg.Bands
	note := ""
	if len(bands) == 0 {
		bands = defaultBands
		note = "\nThese are the defaults; `/rapsheet configure bands` changes them."
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
		core.RespondWarn(s, i, "Rapsheet status", b.String())
		return
	}
	core.RespondInfo(s, i, "Rapsheet status", b.String())
}
