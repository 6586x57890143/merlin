package contest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/omit"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/secret"
)

// Action names namespace the per-guild allow/deny/tier-override machinery
// (/config permissions). Split the way rotation splits structural changes
// from adjustments: running a contest and reconfiguring where contests live
// are different blast radii and a guild may want to loosen one without the
// other.
const (
	actionManage    = "contest.manage"
	actionConfigure = "contest.configure"
	// Its own action rather than actionManage, so a guild can raise the bar
	// on it with /config permissions set-tier: rejecting a pledge destroys
	// the code that came with it, which is a different blast radius from
	// running a contest, the same reasoning that gave roles.jail_role its
	// own action.
	actionReview = "contest.review"
)

// Default phase lengths. Only the title is required on /contest new, because
// the common case is a mod deciding to run something and not wanting to
// argue with a form about it. Every one of these is overridable.
const (
	defaultAnnounceFor = time.Hour
	defaultSubmitFor   = 48 * time.Hour
	defaultVoteFor     = 24 * time.Hour
	maxPicks           = 10
)

// prizeModalPrefix carries the contest id, so a modal left open while a
// contest ends still lands on the contest it was opened for rather than on
// whatever is running by the time it is submitted.
const prizeModalPrefix = "contest:prize:"

// Modal field ids. Named rather than positional because ModalValues keys by
// CustomID, which is the only thing that survives Discord's round trip.
const (
	prizeFieldTitle   = "title"
	prizeFieldDetails = "details"
	prizeFieldCode    = "code"
)

func actorID(ix *core.Ix) string { return ix.UserID }

func actorName(ix *core.Ix) string {
	if m := ix.Member(); m != nil {
		if m.Nick != nil && *m.Nick != "" {
			return *m.Nick
		}
		if m.User.GlobalName != nil && *m.User.GlobalName != "" {
			return *m.User.GlobalName
		}
		return m.User.Username
	}
	return "someone"
}

func (p *Plugin) registerCommands() {
	durationOpt := func(name, desc string) discord.ApplicationCommandOption {
		return discord.ApplicationCommandOptionString{
			Name: name, Description: desc,
		}
	}

	cmd := discord.SlashCommandCreate{
		Name:        "contest",
		Description: "Run a server contest: submissions, voting, prizes.",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionSubCommand{
				Name:        "new",
				Description: "Start a contest. Only a title is required.",
				Options: []discord.ApplicationCommandOption{
					discord.ApplicationCommandOptionString{Name: "title", Description: "What the contest is called.", Required: true},
					discord.ApplicationCommandOptionString{Name: "theme", Description: "The brief, if there is one."},
					durationOpt("announce-for", "Hype window before submissions open. e.g. 1h, 30m, 0 for none."),
					durationOpt("submit-for", "How long submissions stay open. e.g. 48h, 3d."),
					durationOpt("vote-for", "How long voting stays open. e.g. 24h."),
					discord.ApplicationCommandOptionInt{Name: "picks", Description: "How many entries each member may vote for.", MinValue: omit.Ptr(1), MaxValue: omit.Ptr(maxPicks)},
					discord.ApplicationCommandOptionChannel{Name: "channel", Description: "Where announcements go. Defaults to here.",
						ChannelTypes: []discord.ChannelType{discord.ChannelTypeGuildText}},
				},
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "prize",
				Description: "Pledge a prize for the winner. Opens a private form.",
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "unpledge",
				Description: "Take back a prize you pledged.",
				Options: []discord.ApplicationCommandOption{
					discord.ApplicationCommandOptionString{Name: "which", Description: "Which pledge. Leave empty for your most recent.", Autocomplete: true},
				},
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "link",
				Description: "Get your link for whatever the contest is doing right now.",
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "claim",
				Description: "Pick up a prize you won, if the DM did not reach you.",
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "status",
				Description: "Where the contest is up to, and whether anything is broken.",
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "review",
				Description: "Approve or reject pledged prizes. You never see the code.",
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "advance",
				Description: "Move the contest to its next phase now, without waiting for the deadline.",
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "cancel",
				Description: "Call the contest off. Nothing is deleted.",
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "settings",
				Description: "Who contests are for, where they're announced and filed, and picks per member.",
			},
		},
	}

	p.commands.RegisterCommand(p.Name(), cmd)

	mod := core.PermSpec{Tier: core.TierMod, Action: actionManage}
	admin := core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}
	public := core.PermSpec{Tier: core.TierPublic}

	p.commands.HandleIx("contest", "new", mod, p.handleNew)
	p.commands.HandleIx("contest", "prize", public, p.handlePrize)
	p.commands.HandleIx("contest", "unpledge", public, p.handleUnpledge)
	p.commands.AutocompleteIx("contest", "unpledge", p.autocompletePledges)
	p.commands.HandleIx("contest", "link", public, p.handleLink)
	p.commands.HandleIx("contest", "claim", public, p.handleClaim)
	p.commands.HandleIx("contest", "status", mod, p.handleStatus)
	p.commands.HandleIx("contest", "review", core.PermSpec{Tier: core.TierMod, Action: actionReview}, p.handleReview)
	p.commands.HandleIx("contest", "advance", mod, p.handleAdvance)
	p.commands.HandleIx("contest", "cancel", mod, p.handleCancel)
	p.commands.HandleIx("contest", "settings", admin, p.handleSettings)
	p.registerPanel()

	p.commands.HandleComponentIx(p.Name(), linkButtonPrefix, public, p.handleLinkButton)
	p.commands.HandleComponentIx(p.Name(), reviewPrefix,
		core.PermSpec{Tier: core.TierMod, Action: actionReview}, p.handleReviewButton)
	p.commands.HandleModalIx(p.Name(), prizeModalPrefix, public, p.handlePrizeModal)
}

// handleNew creates a contest, its forum, and the opening announcement.
// Defers first: this makes a channel, writes an overwrite and posts a
// message, none of which fits in Discord's three second window.
func (p *Plugin) handleNew(ctx context.Context, ix *core.Ix) {
	if err := ix.Defer(); err != nil {
		p.log.Error("contest: defer new", "err", err)
		return
	}
	args := ix.Args()

	if _, err := p.store.LiveContest(ctx, ix.GuildID); err == nil {
		followErr(ix, p, "There's already a contest running", ErrAlreadyLive)
		return
	} else if !errors.Is(err, ErrNoLiveContest) {
		followErr(ix, p, "Couldn't check for a running contest", err)
		return
	}

	cfg, err := p.store.GetConfig(ctx, ix.GuildID)
	if err != nil {
		followErr(ix, p, "Couldn't read the contest setup", err)
		return
	}

	// Refuse rather than guess. merlin does not know this server's layout,
	// and the guess that is wrong publishes members' work to accounts the
	// server has deliberately not let in -- which is not something an
	// admin finds out about until it has already happened. A genuinely open
	// server answers this in one command too, by pointing gate-like at a
	// channel @everyone can see, and the config then records a decision
	// somebody made rather than a default nobody chose.
	if !cfg.Gated() {
		followErr(ix, p, "Nothing is gating the contest forum", ErrUngated)
		return
	}

	announceFor, err := optDuration(args, "announce-for", defaultAnnounceFor, true)
	if err != nil {
		followErr(ix, p, "That announce window doesn't parse", err)
		return
	}
	submitFor, err := optDuration(args, "submit-for", defaultSubmitFor, false)
	if err != nil {
		followErr(ix, p, "That submission window doesn't parse", err)
		return
	}
	voteFor, err := optDuration(args, "vote-for", defaultVoteFor, false)
	if err != nil {
		followErr(ix, p, "That voting window doesn't parse", err)
		return
	}

	slug, err := newSlug()
	if err != nil {
		followErr(ix, p, "Couldn't start the contest", err)
		return
	}

	now := p.now()
	c := Contest{
		ID:                newID(),
		GuildID:           ix.GuildID,
		Slug:              slug,
		Title:             strings.TrimSpace(args["title"].String()),
		MaxVotes:          cfg.DefaultMaxVotes,
		SubmitAt:          now.Add(announceFor),
		AnnounceChannelID: cfg.AnnounceChannelID,
		CreatedBy:         actorID(ix),
		Phase:             PhaseAnnounce,
	}
	c.VoteAt = c.SubmitAt.Add(submitFor)
	c.ResultsAt = c.VoteAt.Add(voteFor)

	if opt, ok := args["theme"]; ok {
		c.Theme = strings.TrimSpace(opt.String())
	}
	if opt, ok := args["picks"]; ok {
		c.MaxVotes = int(int64(opt.Int()))
	}
	if opt, ok := args["channel"]; ok {
		c.AnnounceChannelID = opt.Snowflake().String()
	}
	if c.AnnounceChannelID == "" {
		c.AnnounceChannelID = ix.ChannelID
	}
	if c.Title == "" {
		followErr(ix, p, "A contest needs a title", errors.New("contest: empty title"))
		return
	}

	// An announce window of zero is legal and means "start now", so the
	// contest goes straight to submit. Doing that here rather than letting
	// the first tick catch it means the forum is open before the
	// announcement telling people to post in it.
	if announceFor == 0 {
		c.Phase = PhaseSubmit
	}

	if err := p.store.CreateContest(ctx, c); err != nil {
		followErr(ix, p, "Couldn't save the contest", err)
		return
	}

	forumID, err := p.createForum(cfg, c, c.Phase == PhaseSubmit)
	if err != nil {
		// The contest row is already committed, so leaving it would hand the
		// guild a live contest with no forum: /contest new refuses it as
		// ErrAlreadyLive, syncSubmissions no-ops on the empty channel ID, and
		// it ticks through every phase collecting nothing. Retiring it here
		// costs nothing (cancelling a contest deletes nothing, and there is
		// nothing to delete yet) and leaves the admin able to just try again.
		if _, cerr := p.store.AdvancePhase(ctx, c.ID, c.Phase, PhaseCancelled); cerr != nil {
			p.log.Error("contest: cancel after failed forum create", "contest", c.ID, "err", cerr)
		}
		followErr(ix, p, "Couldn't create the contest forum", err)
		return
	}
	c.ForumChannelID = forumID
	if err := p.store.SetForumChannel(ctx, c.ID, forumID); err != nil {
		// Same retirement the failed-create path above does, and for a worse
		// version of the same reason: the forum exists but nothing records
		// which one it is, so syncSubmissions no-ops on the empty channel ID
		// and the contest ticks through every phase collecting nothing while
		// /contest new refuses to start another as ErrAlreadyLive. Retiring it
		// costs nothing and leaves the admin able to try again. The channel is
		// left where it is: deleting one has no undo, and cancelling a contest
		// deliberately deletes nothing.
		if _, cerr := p.store.AdvancePhase(ctx, c.ID, c.Phase, PhaseCancelled); cerr != nil {
			p.log.Error("contest: cancel after unrecorded forum", "contest", c.ID, "err", cerr)
		}
		followErr(ix, p, "Couldn't record the contest forum", err)
		return
	}
	p.announceCreated(ctx, c)
	if c.Phase == PhaseSubmit {
		p.announceSubmissionsOpen(ctx, c)
	}
	p.pushBestEffort(ctx, c)

	p.mu.Lock()
	p.reconcileTickJob(ctx, ix.GuildID)
	p.mu.Unlock()

	if err := p.audit.Record(ctx, ix.GuildID, actorID(ix), "contest.created", "",
		c.Title+" in "+core.MentionChannel(forumID)); err != nil {
		p.log.Error("contest: audit create", "contest", c.ID, "err", err)
	}

	if err := ix.FollowUpOK("Contest started",
		c.Title+" is live in "+core.MentionChannel(forumID)+". Announcements go to "+
			core.MentionChannel(c.AnnounceChannelID)+"."); err != nil {
		p.log.Error("contest: follow up new", "err", err)
	}
}

// optDuration reads one of the phase-length options. Reuses
// core.ParseFlexibleDuration so "90m", "3d" and "2h" all work the same way
// they do everywhere else in this bot.
func optDuration(args map[string]discord.SlashCommandOption, name string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	opt, ok := args[name]
	if !ok {
		return fallback, nil
	}
	raw := strings.TrimSpace(opt.String())
	if raw == "" {
		return fallback, nil
	}
	if allowZero && (raw == "0" || raw == "none" || raw == "off") {
		return 0, nil
	}
	d, err := core.ParseFlexibleDuration(raw)
	if err != nil {
		return 0, err
	}
	if d < minPhase {
		return 0, fmt.Errorf("%s has to be at least %s", name, core.FormatDuration(minPhase))
	}
	if d > maxPhase {
		return 0, fmt.Errorf("%s has to be under %s", name, core.FormatDuration(maxPhase))
	}
	return d, nil
}

// handlePrize opens the modal. A modal rather than command options because
// a prize code typed as a slash-command argument is visible to anybody
// looking over the shoulder of whoever typed it, appears in Discord's own
// client-side command history, and is exactly the thing that must not be
// seen once.
func (p *Plugin) handlePrize(ctx context.Context, ix *core.Ix) {
	c, err := p.store.LiveContest(ctx, ix.GuildID)
	if err != nil {
		ix.RespondWarn("Nothing to pledge to", "There's no contest running right now.")
		return
	}

	code := discord.TextInputComponent{
		CustomID:    prizeFieldCode,
		Style:       discord.TextInputStyleParagraph,
		Placeholder: "Only merlin sees this, and only the winner gets it.",
		MaxLength:   1000,
	}
	codeLabel := "Code, key or claim link (optional)"
	if p.sealer == nil {
		// No MERLIN_SECRET_KEY means there is nowhere safe to put a code, so
		// the field is not offered at all. Refusing to store it is right;
		// offering a box and then dropping what is typed into it is not.
		codeLabel = "Codes are off on this bot"
		code.Placeholder = "MERLIN_SECRET_KEY is not set, so leave this empty."
		code.MaxLength = 1
	}

	err = ix.OpenModal(prizeModalPrefix+c.ID, truncate("Pledge a prize: "+c.Title, 45),
		discord.LabelComponent{Label: "What is it", Component: discord.TextInputComponent{
			CustomID: prizeFieldTitle, Style: discord.TextInputStyleShort,
			Placeholder: "a steam key for Hollow Knight", Required: true, MaxLength: 100,
		}},
		discord.LabelComponent{Label: "Anything the winner should know", Component: discord.TextInputComponent{
			CustomID: prizeFieldDetails, Style: discord.TextInputStyleParagraph, MaxLength: 500,
		}},
		discord.LabelComponent{Label: codeLabel, Component: code},
	)
	if err != nil {
		p.log.Error("contest: open prize modal", "err", err)
	}
}

func (p *Plugin) handlePrizeModal(ctx context.Context, ix *core.Ix, customID string) {
	contestID := strings.TrimPrefix(customID, prizeModalPrefix)
	vals := ix.ModalValues()
	title := strings.TrimSpace(vals[prizeFieldTitle])
	if title == "" {
		ix.RespondWarn("Nothing pledged", "A prize needs at least a name.")
		return
	}

	c, err := p.store.LiveContest(ctx, ix.GuildID)
	if err != nil || c.ID != contestID {
		ix.RespondWarn("That contest has moved on",
			"The contest this form was opened for isn't taking pledges any more.")
		return
	}

	prize := Prize{
		ID:        newID(),
		ContestID: c.ID,
		DonorID:   actorID(ix),
		DonorName: actorName(ix),
		Title:     title,
		Details:   strings.TrimSpace(vals[prizeFieldDetails]),
	}

	if code := strings.TrimSpace(vals[prizeFieldCode]); code != "" {
		// Derived here and nowhere else, because here is the only place the
		// plaintext exists. Everything downstream reviews the shape.
		prize.CodeShape = codeShape(code)
		sealed, err := p.sealer.Seal(code)
		if err != nil {
			if errors.Is(err, secret.ErrNoKey) {
				ix.RespondWarn("Codes are off on this bot",
					"MERLIN_SECRET_KEY isn't set, so there's nowhere safe to keep a code. Pledge it without one and hand it over yourself.")
				return
			}
			ix.RespondErr("Couldn't store that code", err)
			return
		}
		prize.SecretSealed = sealed
	}

	if err := p.store.AddPrize(ctx, prize); err != nil {
		ix.RespondErr("Couldn't record the pledge", err)
		return
	}

	// The audit row names the prize and never the code, and there is no
	// branch here that could change that.
	if err := p.audit.Record(ctx, ix.GuildID, actorID(ix), "contest.prize_pledged", "", prize.Title); err != nil {
		p.log.Error("contest: audit prize", "contest", c.ID, "err", err)
	}

	// Nothing public happens here any more. The snapshot push and the
	// announce-channel line both moved to the approval, because this command
	// is TierPublic and everything it used to do put a member's own words,
	// under their own name, straight onto the gallery and into a channel.
	note := "A mod looks at it before it shows up. You keep hold of it until the winner is announced."
	if prize.HasSecret() {
		note = "Code stored encrypted, and nobody reviewing it can read it. merlin sends it straight to the winner and wipes it after."
	}
	ix.RespondOK("Pledged, waiting on a mod", note)
}

func (p *Plugin) handleUnpledge(ctx context.Context, ix *core.Ix) {
	c, err := p.store.LiveContest(ctx, ix.GuildID)
	if err != nil {
		ix.RespondWarn("Nothing running", "There's no contest to pull a pledge from.")
		return
	}
	mine, err := p.myPledges(ctx, c, actorID(ix))
	if err != nil {
		ix.RespondErr("Couldn't read the prize pool", err)
		return
	}
	if len(mine) == 0 {
		ix.RespondWarn("Nothing to take back", "You haven't pledged anything to this one.")
		return
	}

	target := mine[len(mine)-1]
	if opt, ok := ix.Args()["which"]; ok && opt.String() != "" {
		wanted := opt.String()
		found := false
		for _, pr := range mine {
			if pr.ID == wanted {
				target, found = pr, true
				break
			}
		}
		if !found {
			ix.RespondWarn("Can't find that one", "That pledge isn't yours, or it's already been handed out.")
			return
		}
	}

	ok, err := p.store.RemovePrize(ctx, c.ID, target.ID, actorID(ix))
	if err != nil {
		ix.RespondErr("Couldn't take that back", err)
		return
	}
	if !ok {
		ix.RespondWarn("Too late", "That one has already been handed out.")
		return
	}
	p.pushBestEffort(ctx, c)
	ix.RespondOK("Taken back", target.Title+" is out of the pool.")
}

func (p *Plugin) myPledges(ctx context.Context, c Contest, userID string) ([]Prize, error) {
	all, err := p.store.Prizes(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	var mine []Prize
	for _, pr := range all {
		// A rejected pledge is already out of the pool, so offering to take
		// it back is offering to undo something that has happened.
		if pr.DonorID == userID && pr.AwardedAt == nil && !pr.Rejected() {
			mine = append(mine, pr)
		}
	}
	return mine, nil
}

func (p *Plugin) autocompletePledges(ctx context.Context, ix *core.Ix, _, focused string) []discord.AutocompleteChoice {
	c, err := p.store.LiveContest(ctx, ix.GuildID)
	if err != nil {
		return nil
	}
	mine, err := p.myPledges(ctx, c, actorID(ix))
	if err != nil {
		return nil
	}
	var out []discord.AutocompleteChoice
	for _, pr := range mine {
		if focused != "" && !strings.Contains(strings.ToLower(pr.Title), strings.ToLower(focused)) {
			continue
		}
		out = append(out, discord.AutocompleteChoiceString{
			Name: truncate(pr.Title, 100), Value: pr.ID,
		})
		// Discord rejects a response over 25 choices outright, leaving the
		// member with no suggestions at all rather than a trimmed list.
		if len(out) == 25 {
			break
		}
	}
	return out
}

func (p *Plugin) handleLink(ctx context.Context, ix *core.Ix) {
	p.sendLink(ctx, ix)
}

func (p *Plugin) handleLinkButton(ctx context.Context, ix *core.Ix, _ string) {
	p.sendLink(ctx, ix)
}

// sendLink is the one contextual surface: it answers with whatever the
// current phase actually calls for, so nobody has to know which of three
// commands applies right now.
func (p *Plugin) sendLink(ctx context.Context, ix *core.Ix) {
	c, err := p.store.LatestContest(ctx, ix.GuildID)
	if err != nil {
		ix.RespondWarn("Nothing running", "No contest right now. Ask a mod to start one.")
		return
	}

	switch c.Phase {
	case PhaseAnnounce:
		ix.RespondInfo(c.Title,
			"Submissions open "+discordTS(c.SubmitAt.Unix())+" in "+core.MentionChannel(c.ForumChannelID)+
				". You can pledge a prize now with `/contest prize`.")
	case PhaseSubmit:
		ix.RespondInfo(c.Title,
			"Post your entry in "+core.MentionChannel(c.ForumChannelID)+". One each, closing "+
				discordTS(c.VoteAt.Unix())+".")
	case PhaseVote:
		if !p.worker.Configured() {
			ix.RespondWarn(c.Title, "There's no voting page set up on this bot.")
			return
		}
		ix.RespondOK(c.Title,
			p.worker.PageURL(c.Slug)+"\n\nSign in with Discord on the page and pick up to "+
				strconv.Itoa(c.MaxVotes)+". Voting closes "+discordTS(c.ResultsAt.Unix())+".")
	default:
		if !p.worker.Configured() {
			ix.RespondInfo(c.Title, "This one is finished.")
			return
		}
		ix.RespondInfo(c.Title, "Finished. The gallery is still up:\n"+p.worker.PageURL(c.Slug))
	}
}

// handleClaim is the fallback for a prize DM that never landed, which is
// the common case for anybody with DMs closed to non-friends. An ephemeral
// interaction response reaches them where a DM cannot.
func (p *Plugin) handleClaim(ctx context.Context, ix *core.Ix) {
	// Every contest this guild has run, not just the newest one. Reading the
	// latest contest meant a winner whose DM bounced had until the next
	// /contest new to collect, after which their prize was unreachable and
	// its ciphertext sat in contest_prizes forever.
	prizes, err := p.store.PrizesAwardedTo(ctx, ix.GuildID, actorID(ix))
	if err != nil {
		ix.RespondErr("Couldn't read the prize pool", err)
		return
	}

	var fields []discord.EmbedField
	var toWipe []string
	for _, pr := range prizes {
		fields = append(fields, discord.EmbedField{
			Name: core.TruncateEmbedField(pr.Title), Value: prizeClaimBody(p, pr),
		})
		if pr.HasSecret() {
			toWipe = append(toWipe, pr.ID)
		}
	}
	if len(fields) == 0 {
		ix.RespondWarn("Nothing to claim", "Nothing here is waiting for you.")
		return
	}

	if err := ix.RespondEmbed(core.Embed(core.ColorSuccess, "your prizes",
		"here you go. this message is only visible to you.", fields...)); err != nil {
		// Wiping after a failed send would destroy the only copy, so the
		// wipe below is deliberately downstream of a successful response.
		ix.RespondErr("Couldn't show that", err)
		return
	}
	for _, id := range toWipe {
		if err := p.store.ClearPrizeSecret(ctx, id); err != nil {
			p.log.Error("contest: clear claimed secret", "prize", id, "err", err)
		}
	}
}

func prizeClaimBody(p *Plugin, pr Prize) string {
	var b strings.Builder
	if pr.Details != "" {
		b.WriteString(pr.Details + "\n")
	}
	if pr.HasSecret() {
		code, err := p.sealer.Open(pr.SecretSealed)
		if err != nil {
			b.WriteString("the code is stored but merlin can't open it, tell an admin MERLIN_SECRET_KEY changed.")
		} else {
			b.WriteString("```" + code + "```")
		}
	} else {
		b.WriteString("talk to " + core.MentionUser(pr.DonorID) + ", merlin holds nothing here.")
	}
	return core.TruncateEmbedField(b.String())
}

func (p *Plugin) handleStatus(ctx context.Context, ix *core.Ix) {
	if err := ix.Defer(); err != nil {
		p.log.Error("contest: defer status", "err", err)
		return
	}
	c, err := p.store.LatestContest(ctx, ix.GuildID)
	if err != nil {
		if err := ix.FollowUpOK("Contests", "Nothing has been run in this server yet."); err != nil {
			p.log.Error("contest: follow up status", "err", err)
		}
		return
	}

	subs, subErr := p.store.Submissions(ctx, c.ID)
	prizes, prizeErr := p.store.Prizes(ctx, c.ID)
	cfg, cfgErr := p.store.GetConfig(ctx, ix.GuildID)

	fields := []discord.EmbedField{
		{Name: "phase", Value: string(c.Phase), Inline: core.Inline()},
		{Name: "entries", Value: countOrErr(len(subs), subErr), Inline: core.Inline()},
		{Name: "prizes", Value: prizeCount(prizes, prizeErr), Inline: core.Inline()},
		{Name: "forum", Value: core.MentionChannel(c.ForumChannelID), Inline: core.Inline()},
	}
	if d, has := c.Deadline(); has {
		fields = append(fields, discord.EmbedField{
			Name: "next", Value: discordTS(d.Unix()), Inline: core.Inline(),
		})
	}

	// Severity is tracked as the body is built, never recovered by scanning
	// the finished text for warning glyphs: that reads a control signal out
	// of prose merlin does not author, which is the bug /config status had.
	colour := core.ColorInfo

	// Who can see the contest is reported next to what phase it is in,
	// because it is the question a mod is asked after the fact and the one
	// nothing else on this screen answers. A forum made before the gate
	// existed, or before the guild configured one, keeps the permissions it
	// was created with until sync-forum, so saying "gated" here without
	// saying "and this contest predates it" would be the wrong kind of
	// reassuring.
	switch {
	case cfgErr != nil:
		colour = core.ColorWarning
		fields = append(fields, discord.EmbedField{
			Name: "who can see it", Value: core.TruncateEmbedField("couldn't read the setup: " + cfgErr.Error()),
		})
	case len(cfg.AccessRoleIDs) > 0:
		fields = append(fields, discord.EmbedField{
			Name: "who can see it", Value: core.TruncateEmbedField(roleList(cfg.AccessRoleIDs)),
		})
	case cfg.GateChannelID != "":
		fields = append(fields, discord.EmbedField{
			Name: "who can see it", Value: "whoever can see " + core.MentionChannel(cfg.GateChannelID),
		})
	default:
		colour = core.ColorWarning
		fields = append(fields, discord.EmbedField{
			Name:  "who can see it",
			Value: "nobody has said, so this forum inherited its category. `/contest settings`: copy a channel, then Resync forum.",
		})
	}

	if p.worker.Configured() {
		voters, votes, err := p.worker.Stats(ctx, c.Slug)
		if err != nil {
			colour = core.ColorWarning
			fields = append(fields, discord.EmbedField{
				Name: "voting page", Value: core.TruncateEmbedField("unreachable: " + err.Error()),
			})
		} else {
			fields = append(fields, discord.EmbedField{
				Name: "votes", Value: strconv.Itoa(votes) + " from " + strconv.Itoa(voters) + " people", Inline: core.Inline(),
			})
		}
		fields = append(fields, discord.EmbedField{
			Name: "gallery", Value: p.worker.PageURL(c.Slug),
		})
	} else {
		colour = core.ColorWarning
		fields = append(fields, discord.EmbedField{
			Name:  "voting page",
			Value: "not set up. MERLIN_CONTEST_WORKER_URL is empty, so nothing can be browsed or voted on.",
		})
	}

	if c.TallyError != "" {
		colour = core.ColorError
		fields = append(fields, discord.EmbedField{
			Name:  "last close attempt failed",
			Value: core.TruncateEmbedField(c.TallyError + "\nThe scheduler keeps retrying."),
		})
	}
	if p.sealer == nil {
		fields = append(fields, discord.EmbedField{
			Name:  "prize codes",
			Value: "off. MERLIN_SECRET_KEY is not set, so pledges cannot carry a code.",
		})
	}

	if err := ix.FollowUpEmbed(core.Embed(colour, c.Title, "where this one is up to.", fields...)); err != nil {
		p.log.Error("contest: follow up status", "err", err)
	}
}

// prizeCount is the prize line on /contest status: how many are actually in
// the pool, and how many are still waiting on a mod.
//
// The pending half is what stops an unattended queue from being invisible.
// Approval is always required and a pledge nobody rules on appears nowhere
// else at all, so without a count here a server could quietly stop taking
// prizes and the only symptom would be donors wondering where theirs went.
func prizeCount(prizes []Prize, err error) string {
	if err != nil {
		return "unreadable"
	}
	var pool, pending int
	for _, pr := range prizes {
		switch {
		case pr.Approved:
			pool++
		case pr.Pending():
			pending++
		}
	}
	out := strconv.Itoa(pool)
	if pending > 0 {
		out += " (" + strconv.Itoa(pending) + " waiting on `/contest review`)"
	}
	return out
}

func countOrErr(n int, err error) string {
	if err != nil {
		return "unreadable"
	}
	return strconv.Itoa(n)
}

func (p *Plugin) handleAdvance(ctx context.Context, ix *core.Ix) {
	if err := ix.Defer(); err != nil {
		p.log.Error("contest: defer advance", "err", err)
		return
	}
	c, err := p.store.LiveContest(ctx, ix.GuildID)
	if err != nil {
		followErr(ix, p, "Nothing to advance", err)
		return
	}
	from := c.Phase
	if err := p.advance(ctx, c); err != nil {
		followErr(ix, p, "Couldn't advance", err)
		return
	}
	if err := p.audit.Record(ctx, ix.GuildID, actorID(ix), "contest.phase_advanced",
		string(from), "forced early"); err != nil {
		p.log.Error("contest: audit advance", "contest", c.ID, "err", err)
	}
	if err := ix.FollowUpOK("Moved on", c.Title+" is past "+string(from)+"."); err != nil {
		p.log.Error("contest: follow up advance", "err", err)
	}
}

// handleCancel stops a contest without destroying anything. The forum, the
// posts and the pledges all stay: cancelling is a decision about the
// contest, not about anybody's work, and channel deletion has no undo.
func (p *Plugin) handleCancel(ctx context.Context, ix *core.Ix) {
	c, err := p.store.LiveContest(ctx, ix.GuildID)
	if err != nil {
		ix.RespondWarn("Nothing to cancel", "No contest is running.")
		return
	}
	won, err := p.store.AdvancePhase(ctx, c.ID, c.Phase, PhaseCancelled)
	if err != nil {
		ix.RespondErr("Couldn't cancel", err)
		return
	}
	if !won {
		ix.RespondWarn("Already moved", "The contest changed phase while you were typing. Try again.")
		return
	}

	c.Phase = PhaseCancelled
	if err := p.setForumOpen(c, false); err != nil {
		p.log.Error("contest: lock forum on cancel", "contest", c.ID, "err", err)
	}
	p.post(ctx, c, core.Embed(core.ColorWarning, c.Title+" is off",
		"this one is called off. the posts stay where they are."), nil)
	p.pushBestEffort(ctx, c)
	p.afterFinish(ctx, c)

	if err := p.audit.Record(ctx, ix.GuildID, actorID(ix), "contest.cancelled", c.Title, ""); err != nil {
		p.log.Error("contest: audit cancel", "contest", c.ID, "err", err)
	}
	ix.RespondOK("Called off",
		"Nothing was deleted. "+core.MentionChannel(c.ForumChannelID)+" is still there.")
}

// roleList names roles rather than counting them, matching the channel lines
// beside it and the rule /config status already follows.
func roleList(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, core.MentionRole(id))
	}
	return strings.Join(out, " ")
}

func orNone(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// followErr is the deferred-response half of core.RespondErr, with the
// logging every call site would otherwise repeat.
func followErr(ix *core.Ix, p *Plugin, title string, err error) {
	if ferr := ix.FollowUpErr(title, err); ferr != nil {
		p.log.Error("contest: follow up error", "title", title, "err", ferr)
	}
}
