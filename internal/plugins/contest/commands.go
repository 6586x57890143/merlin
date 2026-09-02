package contest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/secret"
	"github.com/6586x57890143/merlin/internal/voice"
)

// Action names namespace the per-guild allow/deny/tier-override machinery
// (/config permissions). Split the way rotation splits structural changes
// from adjustments: running a contest and reconfiguring where contests live
// are different blast radii and a guild may want to loosen one without the
// other.
const (
	actionManage    = "contest.manage"
	actionConfigure = "contest.configure"
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

func actorID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

func actorName(i *discordgo.InteractionCreate) string {
	if i.Member != nil {
		if i.Member.Nick != "" {
			return i.Member.Nick
		}
		if i.Member.User != nil {
			if i.Member.User.GlobalName != "" {
				return i.Member.User.GlobalName
			}
			return i.Member.User.Username
		}
	}
	return "someone"
}

func (p *Plugin) registerCommands() {
	durationOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type: discordgo.ApplicationCommandOptionString, Name: name, Description: desc,
		}
	}

	cmd := &discordgo.ApplicationCommand{
		Name:        "contest",
		Description: "Run a server contest: submissions, voting, prizes.",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "new",
				Description: "Start a contest. Only a title is required.",
				Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionString, Name: "title", Description: "What the contest is called.", Required: true},
					{Type: discordgo.ApplicationCommandOptionString, Name: "theme", Description: "The brief, if there is one."},
					durationOpt("announce-for", "Hype window before submissions open. e.g. 1h, 30m, 0 for none."),
					durationOpt("submit-for", "How long submissions stay open. e.g. 48h, 3d."),
					durationOpt("vote-for", "How long voting stays open. e.g. 24h."),
					{Type: discordgo.ApplicationCommandOptionInteger, Name: "picks", Description: "How many entries each member may vote for.", MinValue: ptr(1.0), MaxValue: maxPicks},
					{Type: discordgo.ApplicationCommandOptionChannel, Name: "channel", Description: "Where announcements go. Defaults to here.",
						ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText}},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "prize",
				Description: "Pledge a prize for the winner. Opens a private form.",
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "unpledge",
				Description: "Take back a prize you pledged.",
				Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionString, Name: "which", Description: "Which pledge. Leave empty for your most recent.", Autocomplete: true},
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "link",
				Description: "Get your link for whatever the contest is doing right now.",
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "claim",
				Description: "Pick up a prize you won, if the DM did not reach you.",
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "status",
				Description: "Where the contest is up to, and whether anything is broken.",
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "advance",
				Description: "Move the contest to its next phase now, without waiting for the deadline.",
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "cancel",
				Description: "Call the contest off. Nothing is deleted.",
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "configure",
				Description: "Where contests live in this server.",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "show",
						Description: "Show the current contest setup.",
					},
					{
						Type: discordgo.ApplicationCommandOptionSubCommand, Name: "set",
						Description: "Set where announcements go and where contest forums are created.",
						Options: []*discordgo.ApplicationCommandOption{
							{Type: discordgo.ApplicationCommandOptionChannel, Name: "announce-channel", Description: "Default channel for contest announcements.",
								ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText}},
							{Type: discordgo.ApplicationCommandOptionChannel, Name: "forum-category", Description: "Category new contest forums are created in.",
								ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildCategory}},
							{Type: discordgo.ApplicationCommandOptionInteger, Name: "picks", Description: "Default number of votes each member gets.", MinValue: ptr(1.0), MaxValue: maxPicks},
						},
					},
				},
			},
		},
	}

	p.commands.RegisterCommand(p.Name(), cmd)

	mod := core.PermSpec{Tier: core.TierMod, Action: actionManage}
	admin := core.PermSpec{Tier: core.TierAdmin, Action: actionConfigure}
	public := core.PermSpec{Tier: core.TierPublic}

	p.commands.Handle("contest", "new", mod, p.handleNew)
	p.commands.Handle("contest", "prize", public, p.handlePrize)
	p.commands.Handle("contest", "unpledge", public, p.handleUnpledge)
	p.commands.Autocomplete("contest", "unpledge", p.autocompletePledges)
	p.commands.Handle("contest", "link", public, p.handleLink)
	p.commands.Handle("contest", "claim", public, p.handleClaim)
	p.commands.Handle("contest", "status", mod, p.handleStatus)
	p.commands.Handle("contest", "advance", mod, p.handleAdvance)
	p.commands.Handle("contest", "cancel", mod, p.handleCancel)
	p.commands.Handle("contest", "configure/show", admin, p.handleConfigureShow)
	p.commands.Handle("contest", "configure/set", admin, p.handleConfigureSet)

	p.commands.HandleComponent(p.Name(), linkButtonPrefix, public, p.handleLinkButton)
	p.commands.HandleModal(p.Name(), prizeModalPrefix, public, p.handlePrizeModal)
}

func ptr(f float64) *float64 { return &f }

// handleNew creates a contest, its forum, and the opening announcement.
// Defers first: this makes a channel, writes an overwrite and posts a
// message, none of which fits in Discord's three second window.
func (p *Plugin) handleNew(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("contest: defer new", "err", err)
		return
	}
	args := core.LeafArgs(i)

	if _, err := p.store.LiveContest(ctx, i.GuildID); err == nil {
		followErr(s, i, p, "There's already a contest running", ErrAlreadyLive)
		return
	} else if !errors.Is(err, ErrNoLiveContest) {
		followErr(s, i, p, "Couldn't check for a running contest", err)
		return
	}

	cfg, err := p.store.GetConfig(ctx, i.GuildID)
	if err != nil {
		followErr(s, i, p, "Couldn't read the contest setup", err)
		return
	}

	announceFor, err := optDuration(args, "announce-for", defaultAnnounceFor, true)
	if err != nil {
		followErr(s, i, p, "That announce window doesn't parse", err)
		return
	}
	submitFor, err := optDuration(args, "submit-for", defaultSubmitFor, false)
	if err != nil {
		followErr(s, i, p, "That submission window doesn't parse", err)
		return
	}
	voteFor, err := optDuration(args, "vote-for", defaultVoteFor, false)
	if err != nil {
		followErr(s, i, p, "That voting window doesn't parse", err)
		return
	}

	slug, err := newSlug()
	if err != nil {
		followErr(s, i, p, "Couldn't start the contest", err)
		return
	}

	now := p.now()
	c := Contest{
		ID:                newID(),
		GuildID:           i.GuildID,
		Slug:              slug,
		Title:             strings.TrimSpace(args["title"].StringValue()),
		MaxVotes:          cfg.DefaultMaxVotes,
		SubmitAt:          now.Add(announceFor),
		AnnounceChannelID: cfg.AnnounceChannelID,
		CreatedBy:         actorID(i),
		Phase:             PhaseAnnounce,
	}
	c.VoteAt = c.SubmitAt.Add(submitFor)
	c.ResultsAt = c.VoteAt.Add(voteFor)

	if opt, ok := args["theme"]; ok {
		c.Theme = strings.TrimSpace(opt.StringValue())
	}
	if opt, ok := args["picks"]; ok {
		c.MaxVotes = int(opt.IntValue())
	}
	if opt, ok := args["channel"]; ok {
		c.AnnounceChannelID = opt.ChannelValue(nil).ID
	}
	if c.AnnounceChannelID == "" {
		c.AnnounceChannelID = i.ChannelID
	}
	if c.Title == "" {
		followErr(s, i, p, "A contest needs a title", errors.New("contest: empty title"))
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
		followErr(s, i, p, "Couldn't save the contest", err)
		return
	}

	forumID, err := p.createForum(ctx, c, cfg.ForumCategoryID)
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
		followErr(s, i, p, "Couldn't create the contest forum", err)
		return
	}
	c.ForumChannelID = forumID
	if err := p.store.SetForumChannel(ctx, c.ID, forumID); err != nil {
		followErr(s, i, p, "Couldn't record the contest forum", err)
		return
	}
	if c.Phase == PhaseSubmit {
		if err := p.setForumOpen(ctx, c, true); err != nil {
			p.log.Error("contest: open forum at create", "contest", c.ID, "err", err)
		}
	}

	p.announceCreated(ctx, c)
	if c.Phase == PhaseSubmit {
		p.announceSubmissionsOpen(ctx, c)
	}
	p.pushBestEffort(ctx, c)

	p.mu.Lock()
	p.reconcileTickJob(ctx, i.GuildID)
	p.mu.Unlock()

	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "contest.created", "",
		c.Title+" in "+core.MentionChannel(forumID)); err != nil {
		p.log.Error("contest: audit create", "contest", c.ID, "err", err)
	}

	if err := core.FollowUpOK(s, i, "Contest started",
		c.Title+" is live in "+core.MentionChannel(forumID)+". Announcements go to "+
			core.MentionChannel(c.AnnounceChannelID)+"."); err != nil {
		p.log.Error("contest: follow up new", "err", err)
	}
}

// optDuration reads one of the phase-length options. Reuses
// core.ParseFlexibleDuration so "90m", "3d" and "2h" all work the same way
// they do everywhere else in this bot.
func optDuration(args map[string]*discordgo.ApplicationCommandInteractionDataOption, name string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	opt, ok := args[name]
	if !ok {
		return fallback, nil
	}
	raw := strings.TrimSpace(opt.StringValue())
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
func (p *Plugin) handlePrize(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	c, err := p.store.LiveContest(ctx, i.GuildID)
	if err != nil {
		core.RespondWarn(s, i, "Nothing to pledge to", "There's no contest running right now.")
		return
	}

	code := &discordgo.TextInput{
		CustomID:    prizeFieldCode,
		Label:       "Code, key or claim link (optional)",
		Style:       discordgo.TextInputParagraph,
		Placeholder: "Only merlin sees this, and only the winner gets it.",
		Required:    false,
		MaxLength:   1000,
	}
	if p.sealer == nil {
		// No MERLIN_SECRET_KEY means there is nowhere safe to put a code, so
		// the field is not offered at all. Refusing to store it is right;
		// offering a box and then dropping what is typed into it is not.
		code.Label = "Codes are off on this bot"
		code.Placeholder = "MERLIN_SECRET_KEY is not set, so leave this empty."
		code.MaxLength = 1
	}

	err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: prizeModalPrefix + c.ID,
			Title:    truncate("Pledge a prize: "+c.Title, 45),
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{
					CustomID: prizeFieldTitle, Label: "What is it", Style: discordgo.TextInputShort,
					Placeholder: "a steam key for Hollow Knight", Required: true, MaxLength: 100,
				}}},
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{
					CustomID: prizeFieldDetails, Label: "Anything the winner should know",
					Style: discordgo.TextInputParagraph, Required: false, MaxLength: 500,
				}}},
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{code}},
			},
		},
	})
	if err != nil {
		p.log.Error("contest: open prize modal", "err", err)
	}
}

func (p *Plugin) handlePrizeModal(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	contestID := strings.TrimPrefix(customID, prizeModalPrefix)
	vals := core.ModalValues(i)
	title := strings.TrimSpace(vals[prizeFieldTitle])
	if title == "" {
		core.RespondWarn(s, i, "Nothing pledged", "A prize needs at least a name.")
		return
	}

	c, err := p.store.LiveContest(ctx, i.GuildID)
	if err != nil || c.ID != contestID {
		core.RespondWarn(s, i, "That contest has moved on",
			"The contest this form was opened for isn't taking pledges any more.")
		return
	}

	prize := Prize{
		ID:        newID(),
		ContestID: c.ID,
		DonorID:   actorID(i),
		DonorName: actorName(i),
		Title:     title,
		Details:   strings.TrimSpace(vals[prizeFieldDetails]),
	}

	if code := strings.TrimSpace(vals[prizeFieldCode]); code != "" {
		sealed, err := p.sealer.Seal(code)
		if err != nil {
			if errors.Is(err, secret.ErrNoKey) {
				core.RespondWarn(s, i, "Codes are off on this bot",
					"MERLIN_SECRET_KEY isn't set, so there's nowhere safe to keep a code. Pledge it without one and hand it over yourself.")
				return
			}
			core.RespondErr(s, i, "Couldn't store that code", err)
			return
		}
		prize.SecretSealed = sealed
	}

	if err := p.store.AddPrize(ctx, prize); err != nil {
		core.RespondErr(s, i, "Couldn't record the pledge", err)
		return
	}

	// The audit row names the prize and never the code, and there is no
	// branch here that could change that.
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "contest.prize_pledged", "", prize.Title); err != nil {
		p.log.Error("contest: audit prize", "contest", c.ID, "err", err)
	}
	p.pushBestEffort(ctx, c)

	line := p.speak(ctx, i.GuildID, voice.KeyContestPrizePledged, map[string]string{
		"donor": prize.DonorName,
		"prize": prize.Title,
	}, prize.DonorName+" put up "+prize.Title+".")
	p.post(ctx, c, core.NewEmbed(core.ColorSuccess, "prize pledged", line), nil)

	note := "It's in the pool. You keep hold of it until the winner is announced."
	if prize.HasSecret() {
		note = "Code stored encrypted. merlin sends it straight to the winner and wipes it after."
	}
	core.RespondOK(s, i, "Pledged", note)
}

func (p *Plugin) handleUnpledge(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	c, err := p.store.LiveContest(ctx, i.GuildID)
	if err != nil {
		core.RespondWarn(s, i, "Nothing running", "There's no contest to pull a pledge from.")
		return
	}
	mine, err := p.myPledges(ctx, c, actorID(i))
	if err != nil {
		core.RespondErr(s, i, "Couldn't read the prize pool", err)
		return
	}
	if len(mine) == 0 {
		core.RespondWarn(s, i, "Nothing to take back", "You haven't pledged anything to this one.")
		return
	}

	target := mine[len(mine)-1]
	if opt, ok := core.LeafArgs(i)["which"]; ok && opt.StringValue() != "" {
		wanted := opt.StringValue()
		found := false
		for _, pr := range mine {
			if pr.ID == wanted {
				target, found = pr, true
				break
			}
		}
		if !found {
			core.RespondWarn(s, i, "Can't find that one", "That pledge isn't yours, or it's already been handed out.")
			return
		}
	}

	ok, err := p.store.RemovePrize(ctx, c.ID, target.ID, actorID(i))
	if err != nil {
		core.RespondErr(s, i, "Couldn't take that back", err)
		return
	}
	if !ok {
		core.RespondWarn(s, i, "Too late", "That one has already been handed out.")
		return
	}
	p.pushBestEffort(ctx, c)
	core.RespondOK(s, i, "Taken back", target.Title+" is out of the pool.")
}

func (p *Plugin) myPledges(ctx context.Context, c Contest, userID string) ([]Prize, error) {
	all, err := p.store.Prizes(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	var mine []Prize
	for _, pr := range all {
		if pr.DonorID == userID && pr.AwardedAt == nil {
			mine = append(mine, pr)
		}
	}
	return mine, nil
}

func (p *Plugin) autocompletePledges(ctx context.Context, i *discordgo.InteractionCreate, _, focused string) []*discordgo.ApplicationCommandOptionChoice {
	c, err := p.store.LiveContest(ctx, i.GuildID)
	if err != nil {
		return nil
	}
	mine, err := p.myPledges(ctx, c, actorID(i))
	if err != nil {
		return nil
	}
	var out []*discordgo.ApplicationCommandOptionChoice
	for _, pr := range mine {
		if focused != "" && !strings.Contains(strings.ToLower(pr.Title), strings.ToLower(focused)) {
			continue
		}
		out = append(out, &discordgo.ApplicationCommandOptionChoice{
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

func (p *Plugin) handleLink(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	p.sendLink(ctx, s, i)
}

func (p *Plugin) handleLinkButton(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, _ string) {
	p.sendLink(ctx, s, i)
}

// sendLink is the one contextual surface: it answers with whatever the
// current phase actually calls for, so nobody has to know which of three
// commands applies right now.
func (p *Plugin) sendLink(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	c, err := p.store.LatestContest(ctx, i.GuildID)
	if err != nil {
		core.RespondWarn(s, i, "Nothing running", "No contest right now. Ask a mod to start one.")
		return
	}

	switch c.Phase {
	case PhaseAnnounce:
		core.RespondInfo(s, i, c.Title,
			"Submissions open "+discordTS(c.SubmitAt.Unix())+" in "+core.MentionChannel(c.ForumChannelID)+
				". You can pledge a prize now with `/contest prize`.")
	case PhaseSubmit:
		core.RespondInfo(s, i, c.Title,
			"Post your entry in "+core.MentionChannel(c.ForumChannelID)+". One each, closing "+
				discordTS(c.VoteAt.Unix())+".")
	case PhaseVote:
		if !p.worker.Configured() {
			core.RespondWarn(s, i, c.Title, "There's no voting page set up on this bot.")
			return
		}
		core.RespondOK(s, i, c.Title,
			p.worker.PageURL(c.Slug)+"\n\nSign in with Discord on the page and pick up to "+
				strconv.Itoa(c.MaxVotes)+". Voting closes "+discordTS(c.ResultsAt.Unix())+".")
	default:
		if !p.worker.Configured() {
			core.RespondInfo(s, i, c.Title, "This one is finished.")
			return
		}
		core.RespondInfo(s, i, c.Title, "Finished. The gallery is still up:\n"+p.worker.PageURL(c.Slug))
	}
}

// handleClaim is the fallback for a prize DM that never landed, which is
// the common case for anybody with DMs closed to non-friends. An ephemeral
// interaction response reaches them where a DM cannot.
func (p *Plugin) handleClaim(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Every contest this guild has run, not just the newest one. Reading the
	// latest contest meant a winner whose DM bounced had until the next
	// /contest new to collect, after which their prize was unreachable and
	// its ciphertext sat in contest_prizes forever.
	prizes, err := p.store.PrizesAwardedTo(ctx, i.GuildID, actorID(i))
	if err != nil {
		core.RespondErr(s, i, "Couldn't read the prize pool", err)
		return
	}

	var fields []*discordgo.MessageEmbedField
	var toWipe []string
	for _, pr := range prizes {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: core.TruncateEmbedField(pr.Title), Value: prizeClaimBody(p, pr),
		})
		if pr.HasSecret() {
			toWipe = append(toWipe, pr.ID)
		}
	}
	if len(fields) == 0 {
		core.RespondWarn(s, i, "Nothing to claim", "Nothing here is waiting for you.")
		return
	}

	if err := core.RespondEmbed(s, i, core.NewEmbed(core.ColorSuccess, "your prizes",
		"here you go. this message is only visible to you.", fields...)); err != nil {
		// Wiping after a failed send would destroy the only copy, so the
		// wipe below is deliberately downstream of a successful response.
		core.RespondErr(s, i, "Couldn't show that", err)
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

func (p *Plugin) handleStatus(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("contest: defer status", "err", err)
		return
	}
	c, err := p.store.LatestContest(ctx, i.GuildID)
	if err != nil {
		if err := core.FollowUpOK(s, i, "Contests", "Nothing has been run in this server yet."); err != nil {
			p.log.Error("contest: follow up status", "err", err)
		}
		return
	}

	subs, subErr := p.store.Submissions(ctx, c.ID)
	prizes, prizeErr := p.store.Prizes(ctx, c.ID)

	fields := []*discordgo.MessageEmbedField{
		{Name: "phase", Value: string(c.Phase), Inline: true},
		{Name: "entries", Value: countOrErr(len(subs), subErr), Inline: true},
		{Name: "prizes", Value: countOrErr(len(prizes), prizeErr), Inline: true},
		{Name: "forum", Value: core.MentionChannel(c.ForumChannelID), Inline: true},
	}
	if d, has := c.Deadline(); has {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "next", Value: discordTS(d.Unix()), Inline: true,
		})
	}

	// Severity is tracked as the body is built, never recovered by scanning
	// the finished text for warning glyphs: that reads a control signal out
	// of prose merlin does not author, which is the bug /config status had.
	colour := core.ColorInfo
	if p.worker.Configured() {
		voters, votes, err := p.worker.Stats(ctx, c.Slug)
		if err != nil {
			colour = core.ColorWarning
			fields = append(fields, &discordgo.MessageEmbedField{
				Name: "voting page", Value: core.TruncateEmbedField("unreachable: " + err.Error()),
			})
		} else {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name: "votes", Value: strconv.Itoa(votes) + " from " + strconv.Itoa(voters) + " people", Inline: true,
			})
		}
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "gallery", Value: p.worker.PageURL(c.Slug),
		})
	} else {
		colour = core.ColorWarning
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "voting page",
			Value: "not set up. MERLIN_CONTEST_WORKER_URL is empty, so nothing can be browsed or voted on.",
		})
	}

	if c.TallyError != "" {
		colour = core.ColorError
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "last close attempt failed",
			Value: core.TruncateEmbedField(c.TallyError + "\nThe scheduler keeps retrying."),
		})
	}
	if p.sealer == nil {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "prize codes",
			Value: "off. MERLIN_SECRET_KEY is not set, so pledges cannot carry a code.",
		})
	}

	if err := core.FollowUpEmbed(s, i, core.NewEmbed(colour, c.Title, "where this one is up to.", fields...)); err != nil {
		p.log.Error("contest: follow up status", "err", err)
	}
}

func countOrErr(n int, err error) string {
	if err != nil {
		return "unreadable"
	}
	return strconv.Itoa(n)
}

func (p *Plugin) handleAdvance(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("contest: defer advance", "err", err)
		return
	}
	c, err := p.store.LiveContest(ctx, i.GuildID)
	if err != nil {
		followErr(s, i, p, "Nothing to advance", err)
		return
	}
	from := c.Phase
	if err := p.advance(ctx, c); err != nil {
		followErr(s, i, p, "Couldn't advance", err)
		return
	}
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "contest.phase_advanced",
		string(from), "forced early"); err != nil {
		p.log.Error("contest: audit advance", "contest", c.ID, "err", err)
	}
	if err := core.FollowUpOK(s, i, "Moved on", c.Title+" is past "+string(from)+"."); err != nil {
		p.log.Error("contest: follow up advance", "err", err)
	}
}

// handleCancel stops a contest without destroying anything. The forum, the
// posts and the pledges all stay: cancelling is a decision about the
// contest, not about anybody's work, and channel deletion has no undo.
func (p *Plugin) handleCancel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	c, err := p.store.LiveContest(ctx, i.GuildID)
	if err != nil {
		core.RespondWarn(s, i, "Nothing to cancel", "No contest is running.")
		return
	}
	won, err := p.store.AdvancePhase(ctx, c.ID, c.Phase, PhaseCancelled)
	if err != nil {
		core.RespondErr(s, i, "Couldn't cancel", err)
		return
	}
	if !won {
		core.RespondWarn(s, i, "Already moved", "The contest changed phase while you were typing. Try again.")
		return
	}

	c.Phase = PhaseCancelled
	if err := p.setForumOpen(ctx, c, false); err != nil {
		p.log.Error("contest: lock forum on cancel", "contest", c.ID, "err", err)
	}
	p.post(ctx, c, core.NewEmbed(core.ColorWarning, c.Title+" is off",
		"this one is called off. the posts stay where they are."), nil)
	p.pushBestEffort(ctx, c)
	p.afterFinish(ctx, c)

	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "contest.cancelled", c.Title, ""); err != nil {
		p.log.Error("contest: audit cancel", "contest", c.ID, "err", err)
	}
	core.RespondOK(s, i, "Called off",
		"Nothing was deleted. "+core.MentionChannel(c.ForumChannelID)+" is still there.")
}

func (p *Plugin) handleConfigureShow(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg, err := p.store.GetConfig(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Couldn't read the setup", err)
		return
	}
	embed := core.NewEmbed(core.ColorInfo, "Contest setup", "How contests are set up here.",
		&discordgo.MessageEmbedField{Name: "announce channel", Value: orNone(core.MentionChannel(cfg.AnnounceChannelID), "wherever /contest new is run"), Inline: true},
		&discordgo.MessageEmbedField{Name: "forum category", Value: orNone(core.MentionChannel(cfg.ForumCategoryID), "no category"), Inline: true},
		&discordgo.MessageEmbedField{Name: "picks per member", Value: strconv.Itoa(cfg.DefaultMaxVotes), Inline: true},
	)
	if err := core.RespondEmbed(s, i, embed); err != nil {
		p.log.Error("contest: respond configure show", "err", err)
	}
}

func (p *Plugin) handleConfigureSet(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	cfg, err := p.store.GetConfig(ctx, i.GuildID)
	if err != nil {
		core.RespondErr(s, i, "Couldn't read the setup", err)
		return
	}
	args := core.LeafArgs(i)
	old := fmt.Sprintf("announce=%s category=%s picks=%d",
		cfg.AnnounceChannelID, cfg.ForumCategoryID, cfg.DefaultMaxVotes)

	if opt, ok := args["announce-channel"]; ok {
		cfg.AnnounceChannelID = opt.ChannelValue(nil).ID
	}
	if opt, ok := args["forum-category"]; ok {
		cfg.ForumCategoryID = opt.ChannelValue(nil).ID
	}
	if opt, ok := args["picks"]; ok {
		cfg.DefaultMaxVotes = int(opt.IntValue())
	}
	if err := p.store.SetConfig(ctx, cfg); err != nil {
		core.RespondErr(s, i, "Couldn't save the setup", err)
		return
	}
	newVal := fmt.Sprintf("announce=%s category=%s picks=%d",
		core.MentionChannel(cfg.AnnounceChannelID), core.MentionChannel(cfg.ForumCategoryID), cfg.DefaultMaxVotes)
	if err := p.audit.Record(ctx, i.GuildID, actorID(i), "contest.configured", old, newVal); err != nil {
		p.log.Error("contest: audit configure", "guild", i.GuildID, "err", err)
	}
	core.RespondOK(s, i, "Saved", "New contests will use this.")
}

func orNone(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// followErr is the deferred-response half of core.RespondErr, with the
// logging every call site would otherwise repeat.
func followErr(s *discordgo.Session, i *discordgo.InteractionCreate, p *Plugin, title string, err error) {
	if ferr := core.FollowUpErr(s, i, title, err); ferr != nil {
		p.log.Error("contest: follow up error", "title", title, "err", ferr)
	}
}
