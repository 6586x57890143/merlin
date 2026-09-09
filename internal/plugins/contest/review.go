package contest

import (
	"context"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// The prize review queue.
//
// /contest prize is TierPublic, and it used to publish on submission: the row
// was written, the snapshot pushed and merlin's "prize pledged" line posted,
// all before anybody had read a word of it. So the gallery and the announce
// channel carried whatever any member typed, under their own name.
//
// A mod now rules on each pledge first, and does it **without being shown the
// code**. That is not a nicety. The sealing exists so a prize code is seen
// once, by the person who won it, and a moderator is not a smaller exception
// to that than anybody else; a review flow that decrypted would quietly turn
// every mod into somebody who could pocket a key and reject the pledge.
//
// What a mod gets instead is the donor, the age of their account, the title,
// the details, and one line describing the code's *shape* (codeShape, derived
// at pledge time). That is enough to throw out "lol get rekt idiot" and not
// enough to redeem anything. It is deliberately not enough to catch a
// plausible fake: nothing short of redeeming a code can, and pretending
// otherwise would be the worse failure, because it would make the queue look
// like a guarantee.
const reviewPrefix = "contest:review:"

// pendingPrizes is the queue, oldest first (Prizes already orders by
// created_at), so the thing somebody has been waiting longest on is the thing
// a mod is shown.
func pendingPrizes(prizes []Prize) []Prize {
	out := make([]Prize, 0, len(prizes))
	for _, pr := range prizes {
		if pr.Pending() {
			out = append(out, pr)
		}
	}
	return out
}

func (p *Plugin) handleReview(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	c, err := p.store.LiveContest(ctx, i.GuildID)
	if err != nil {
		core.RespondWarn(s, i, "Nothing running", "There's no contest taking pledges right now.")
		return
	}
	prizes, err := p.store.Prizes(ctx, c.ID)
	if err != nil {
		core.RespondErr(s, i, "Couldn't read the prize pool", err)
		return
	}
	pending := pendingPrizes(prizes)
	if len(pending) == 0 {
		core.RespondOK(s, i, "Nothing waiting", "Every pledge on "+c.Title+" has been looked at.")
		return
	}
	embed, components := reviewView(pending, 0)
	if err := core.RespondEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("contest: respond review", "err", err)
	}
}

// handleReviewButton is Approve, Reject and Skip on one prefix.
//
// Every click re-reads the queue rather than trusting what the message it
// arrived on was showing, the same stateless choice /config setup makes:
// there is no session to expire, two mods can work the queue at once, and the
// prize id in the CustomID is checked against live rows rather than taken as
// an instruction. A pledge somebody else already ruled on loses the claim in
// the store, and the click just re-renders.
func (p *Plugin) handleReviewButton(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	action, prizeID, _ := strings.Cut(strings.TrimPrefix(customID, reviewPrefix), ":")
	if err := core.DeferUpdate(s, i); err != nil {
		p.log.Error("contest: defer review click", "err", err)
		return
	}

	c, err := p.store.LiveContest(ctx, i.GuildID)
	if err != nil {
		p.reviewDone(s, i, "That contest has moved on", "It isn't taking pledges any more.")
		return
	}

	switch action {
	case "approve":
		p.approvePledge(ctx, i, c, prizeID)
	case "reject":
		p.rejectPledge(ctx, i, c, prizeID)
	}

	prizes, err := p.store.Prizes(ctx, c.ID)
	if err != nil {
		p.log.Error("contest: re-read prizes after review", "contest", c.ID, "err", err)
		return
	}
	pending := pendingPrizes(prizes)
	if len(pending) == 0 {
		p.reviewDone(s, i, "Nothing waiting", "Every pledge on "+c.Title+" has been looked at.")
		return
	}

	// Skip moves past the one on screen; a decision leaves the cursor where
	// it was, which is now the next pledge because the old one left the
	// queue. Anything unfindable lands at the top, which is the right answer
	// for a queue that changed underneath the click.
	at := 0
	if action == "skip" {
		at = indexOfPrize(pending, prizeID) + 1
	}
	embed, components := reviewView(pending, at)
	if err := core.UpdateEmbedWithComponents(s, i, embed, components); err != nil {
		p.log.Error("contest: update review", "err", err)
	}
}

func (p *Plugin) approvePledge(ctx context.Context, i *discordgo.InteractionCreate, c Contest, prizeID string) {
	pr, ok := p.pendingPrize(ctx, c, prizeID)
	if !ok {
		return
	}
	won, err := p.store.ApprovePrize(ctx, prizeID, actorID(i), p.now())
	if err != nil {
		p.log.Error("contest: approve prize", "prize", prizeID, "err", err)
		return
	}
	if !won {
		return
	}

	// The public half of pledging, moved here from the pledge itself. This is
	// the first moment anything about this prize is allowed to be seen.
	p.pushBestEffort(ctx, c)
	line := p.speak(ctx, c.GuildID, voice.KeyContestPrizePledged, map[string]string{
		"donor": pr.DonorName,
		"prize": pr.Title,
	}, pr.DonorName+" put up "+pr.Title+".")
	p.post(ctx, c, core.NewEmbed(core.ColorSuccess, "prize pledged", line), nil)

	if err := p.audit.Record(ctx, c.GuildID, actorID(i), "contest.prize_approved", "", pr.Title); err != nil {
		p.log.Error("contest: audit prize approval", "prize", prizeID, "err", err)
	}
}

func (p *Plugin) rejectPledge(ctx context.Context, i *discordgo.InteractionCreate, c Contest, prizeID string) {
	pr, ok := p.pendingPrize(ctx, c, prizeID)
	if !ok {
		return
	}
	won, err := p.store.RejectPrize(ctx, prizeID, actorID(i), p.now())
	if err != nil {
		p.log.Error("contest: reject prize", "prize", prizeID, "err", err)
		return
	}
	if !won {
		return
	}

	// Telling the donor is what keeps this from being a silent
	// disappearance, and a bounced DM is the common case rather than a
	// failure: it must not undo a decision that is already recorded.
	if err := p.tellDonorRejected(c, pr); err != nil {
		p.log.Error("contest: dm rejected donor", "prize", prizeID, "err", err)
	}
	// The title, never the code, which by now does not exist anyway:
	// RejectPrize wiped the ciphertext in the same statement.
	if err := p.audit.Record(ctx, c.GuildID, actorID(i), "contest.prize_rejected", "", pr.Title); err != nil {
		p.log.Error("contest: audit prize rejection", "prize", prizeID, "err", err)
	}
}

// pendingPrize re-reads one pledge and confirms it is still pending. The
// store's conditional update is the real guard against a double decision;
// this is what gives the caller the title and donor it needs for the
// announcement and the DM.
func (p *Plugin) pendingPrize(ctx context.Context, c Contest, prizeID string) (Prize, bool) {
	prizes, err := p.store.Prizes(ctx, c.ID)
	if err != nil {
		p.log.Error("contest: read prizes for review", "contest", c.ID, "err", err)
		return Prize{}, false
	}
	for _, pr := range prizes {
		if pr.ID == prizeID && pr.Pending() {
			return pr, true
		}
	}
	return Prize{}, false
}

func (p *Plugin) tellDonorRejected(c Contest, pr Prize) error {
	ops := p.opsFor(c.GuildID)
	dm, err := ops.UserChannelCreate(pr.DonorID)
	if err != nil {
		return err
	}
	// Code-authored rather than a voice line, matching deliverPrize's DM in
	// announce.go: this is a moderation outcome addressed to one person, and
	// voice.Line selects at random and falls back silently, which is right
	// for a greeting and wrong for the sentence explaining why somebody's
	// prize is not going up.
	embed := core.NewEmbed(core.ColorWarning, "your pledge wasn't taken",
		"a mod turned down what you put up for "+c.Title+". nothing of yours was published.",
		&discordgo.MessageEmbedField{Name: "what", Value: core.TruncateEmbedField(pr.Title)},
		&discordgo.MessageEmbedField{
			Name: "if that's a mistake",
			Value: "talk to a mod in " + core.MentionChannel(c.AnnounceChannelID) +
				". you can pledge again with /contest prize.",
		})
	_, err = ops.ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		Files:  core.EmbedFiles(embed),
	})
	return err
}

// reviewDone replaces the queue with a closing note and takes the buttons
// away, so a stale Approve cannot be clicked on an empty queue.
func (p *Plugin) reviewDone(s *discordgo.Session, i *discordgo.InteractionCreate, title, body string) {
	if err := core.UpdateEmbedWithComponents(s, i, core.NewEmbed(core.ColorSuccess, title, body), nil); err != nil {
		p.log.Error("contest: close review", "err", err)
	}
}

// reviewView renders one pledge out of the queue. at is clamped rather than
// rejected, because it comes off a button on a message that may be older than
// the queue it is describing.
func reviewView(pending []Prize, at int) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	if at < 0 || at >= len(pending) {
		at = 0
	}
	pr := pending[at]

	code := "no code"
	if pr.HasSecret() {
		code = pr.CodeShape
		if code == "" {
			// A pledge from before code_shape existed, or one whose shape
			// came back empty. Say that rather than leaving the field blank,
			// which reads as "there is no code" and is the opposite of true.
			code = "a code, shape not recorded"
		}
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "from", Value: core.TruncateEmbedField(donorLine(pr))},
		{Name: "what", Value: core.TruncateEmbedField(pr.Title)},
	}
	if pr.Details != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "details", Value: core.TruncateEmbedField(pr.Details),
		})
	}
	fields = append(fields,
		&discordgo.MessageEmbedField{Name: "code", Value: code, Inline: true},
		&discordgo.MessageEmbedField{Name: "pledged", Value: discordTS(pr.CreatedAt.Unix()), Inline: true},
	)

	embed := core.NewEmbed(core.ColorPrimary,
		"pledge review "+strconv.Itoa(at+1)+" of "+strconv.Itoa(len(pending))+" waiting",
		"merlin can't show you the code, and won't. judge the shape, the title and who sent it.",
		fields...)

	row := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Approve", Style: discordgo.SuccessButton, CustomID: reviewPrefix + "approve:" + pr.ID},
		discordgo.Button{Label: "Reject", Style: discordgo.DangerButton, CustomID: reviewPrefix + "reject:" + pr.ID},
	}}
	if len(pending) > 1 {
		row.Components = append(row.Components, discordgo.Button{
			Label: "Skip", Style: discordgo.SecondaryButton, CustomID: reviewPrefix + "skip:" + pr.ID,
		})
	}
	return embed, []discordgo.MessageComponent{row}
}

// donorLine is who pledged, plus how old their account is.
//
// The age comes out of the snowflake (discordgo.SnowflakeTimestamp), so it
// costs no API call, and it is account age rather than join date on purpose:
// a throwaway made this morning is the signal worth having, while somebody
// who joined the server last week may well have been on Discord for years.
func donorLine(pr Prize) string {
	who := core.MentionUser(pr.DonorID)
	made, err := discordgo.SnowflakeTimestamp(pr.DonorID)
	if err != nil {
		return who
	}
	return who + " · account made " + discordTS(made.Unix())
}

func indexOfPrize(prizes []Prize, id string) int {
	for i, pr := range prizes {
		if pr.ID == id {
			return i
		}
	}
	return -1
}
