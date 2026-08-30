package aimod

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"

	"github.com/bwmarrin/discordgo"
)

// The tip jar. A guild points this at a wallet, members donate USDC to it,
// and /aimod funding shows that balance next to how much OpenRouter credit is
// actually left.
//
// The bot holds no key and moves no money, which is not so much a limitation
// as the only honest shape available: OpenRouter's programmatic crypto
// purchase endpoint returns 410 Gone and their auto top-up charges a saved
// card rather than a wallet, so buying the credits is a human clicking
// through their checkout either way. Given that, custodying donations would
// buy nothing and would put them behind a Discord bot's threat model.
const (
	// fundingPollInterval is how often a configured wallet is read.
	//
	// A minute, matching roles-sweep rather than the hourly sweeps, because
	// the quarter hour this started at was answering the wrong question. It
	// was set on the grounds that donations are not urgent, which is true of
	// the money and false of the acknowledgement: somebody who has just sent
	// a tip watches for it, and a jar that takes a quarter of an hour to
	// notice reads as one that did not receive it. Balances is also all or
	// nothing across a family's rails, so one flaky public endpoint costs a
	// whole cycle, and at fifteen minutes that compounds into the gap this
	// was reported as.
	//
	// The cost is one eth_call per rail per minute per guild with a wallet
	// set, spread over five providers for an EVM family, and the job is only
	// registered where there is an address to read. Scheduler jitter is
	// bounded at interval/10, so a minute here stays a minute rather than
	// becoming three.
	//
	// ponytail: polling, and a minute is roughly its floor. Real-time needs
	// either an eth_subscribe websocket per chain or an indexer webhook, and
	// a webhook means an inbound HTTP surface this bot deliberately does not
	// have. An operator who wants more headroom points MERLIN_RPC_<CHAIN> at
	// a paid endpoint, which already works and needs no code.
	fundingPollInterval = 1 * time.Minute

	// donationDust is the smallest balance increase counted as a donation.
	// Below this it is rounding in decimal handling rather than somebody
	// being generous.
	donationDust = 0.01

	// lowCreditRunway is when the warning starts. Three days is enough for an
	// operator to notice, decide, and get through a checkout without rushing,
	// which is the whole job of the warning.
	lowCreditRunway = 72 * time.Hour

	// addressChangeWarning is how long the public view leads with the fact
	// that the payout address moved. A repointed jar is the one way this
	// feature can cost somebody money, and nothing on chain can be undone, so
	// the warning is loud and the window is generous.
	addressChangeWarning = 24 * time.Hour

	// barWidth is in characters, sized for a phone: an embed field wraps
	// under about 24 on a narrow screen, and a wrapped progress bar reads
	// worse than a short one.
	barWidth = 20

	// spendHistoryDays is the window the burn rate is averaged over.
	spendHistoryDays = 7
)

// bar draws a proportion as a run of filled and empty blocks.
//
// Local to this package rather than in internal/core/embeds.go, because it
// has exactly one consumer. Promote it if a second surface wants one; an
// exported helper with a single caller is an abstraction that has not yet
// earned itself.
func bar(frac float64, width int) string {
	if width <= 0 {
		return ""
	}
	// Clamped rather than trusted. frac is a ratio of two numbers that arrive
	// separately from OpenRouter, and strings.Repeat panics on a negative
	// count.
	if frac < 0 || math.IsNaN(frac) {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(math.Round(frac * float64(width)))
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// actualPerDay averages what was really billed across the days there are
// receipts for. Zero when there is no history, which callers must treat as
// "cannot say" rather than as "free".
func actualPerDay(history []Spend) float64 {
	if len(history) == 0 {
		return 0
	}
	var total float64
	for _, sp := range history {
		total += sp.SpentUSD
	}
	return total / float64(len(history))
}

// fundingJobKey names a guild's balance poll.
func fundingJobKey(guildID string) string { return guildID + ":aimod-funding" }

var fundingSchedule = func() core.CronSpec {
	return core.CronSpec{Schedule: core.IntervalSchedule{Interval: fundingPollInterval}}
}

// reconcileFundingJob registers the poll only where there is a wallet to
// poll, the same rule as rotation.reconcileSweepJob and the calibration job
// next door.
//
// Deliberately NOT Seeded, which is the opposite of reconcileCalibrationJob.
// A job the Scheduler has never seen is immediately due, and that is what is
// wanted here: an admin who has just set an address should watch the balance
// appear on the next tick rather than wonder for fifteen minutes whether they
// typed it wrong. Calibration seeds because its first fire spends money
// against a guild with no history; this one costs one public RPC call.
//
// Assumes the caller does not already hold p.fundingMu.
func (p *Plugin) reconcileFundingJob(guildID string, configured bool) {
	if p.sched == nil {
		return
	}
	p.fundingMu.Lock()
	defer p.fundingMu.Unlock()
	if p.fundingRegistered == nil {
		// New fills this in, but the plugin is also built field-wise in
		// tests, where a nil map would panic on the first registration rather
		// than on the path under test.
		p.fundingRegistered = map[string]bool{}
	}

	key := fundingJobKey(guildID)
	registered := p.fundingRegistered[guildID]

	switch {
	case configured && !registered:
		if err := p.sched.Register(key, fundingSchedule(), p.makeFundingJob(guildID)); err != nil {
			p.log.Error("aimod: register funding job", "guild", guildID, "err", err)
			return
		}
		p.fundingRegistered[guildID] = true
	case !configured && registered:
		if err := p.sched.Unregister(key); err != nil {
			p.log.Error("aimod: unregister funding job", "guild", guildID, "err", err)
			return
		}
		delete(p.fundingRegistered, guildID)
	}
}

// makeFundingJob re-reads the row on every tick rather than closing over it,
// so an address changed since registration is picked up without touching the
// Scheduler.
func (p *Plugin) makeFundingJob(guildID string) func(context.Context) error {
	return func(ctx context.Context) error { return p.pollFunding(ctx, guildID) }
}

// pollFunding reads the wallet once and books any increase as a donation.
func (p *Plugin) pollFunding(ctx context.Context, guildID string) error {
	f, err := p.store.Funding(ctx, guildID)
	if err != nil {
		return fmt.Errorf("aimod: read funding: %w", err)
	}
	// A benign skip returns nil rather than an error, so it never counts
	// toward the Scheduler's consecutive-failure alert.
	if !f.Configured() || p.eth == nil {
		return nil
	}

	// The family comes from the address, so a guild that repointed its jar
	// from one family to another is read on the right rails from the very
	// next poll with nothing to migrate.
	//
	// Balances is all or nothing on purpose. A sum missing one unreachable
	// rail looks exactly like a withdrawal, and the branch below reads a fall
	// as the operator buying credits, so a partial read would book a
	// withdrawal that never happened and then book the recovery as a donation
	// nobody made. Failing costs one fifteen minute retry.
	balances, balance, err := p.eth.Balances(ctx, f.Address)
	if err != nil {
		return fmt.Errorf("aimod: read tip jar balance: %w", err)
	}

	// The first poll after an address is set is a baseline, never a donation.
	// CheckedAt is written together with the address for exactly this reason,
	// so in practice this branch only guards a hand-edited row.
	var donation float64
	if !f.CheckedAt.IsZero() {
		// A fall in balance is the operator moving funds out to buy credits.
		// The new balance records that; it is not a negative donation.
		if delta := balance - f.BalanceUSD; delta >= donationDust {
			donation = delta
		}
	}

	if err := p.store.UpdateFundingBalance(ctx, guildID, balance, donation, balances, p.now()); err != nil {
		return fmt.Errorf("aimod: record tip jar balance: %w", err)
	}

	// Announced after the write, for the same reason enforce records before
	// it touches a message: the booked donation is the durable half, and an
	// audit post that failed must not cost it. Hence log-and-continue rather
	// than a returned error, which would also have the Scheduler retry a
	// balance read that already succeeded and book the difference twice.
	if donation > 0 {
		p.noticeDonation(ctx, guildID, f, donation, balance)
	}

	// The credit check is the other half of the gauge and rides along on top
	// of the poll: a failure there must not fail the job and retry a balance
	// read that already succeeded.
	p.checkCredit(ctx, guildID)
	return nil
}

// noticeDonation records that the tip jar grew.
//
// The audit log rather than a channel of its own, because it is the one
// surface every configured guild already has and this needs no new setting to
// reach it. It is also the right reader: the money only becomes scanning when
// somebody clicks a checkout, so the entry says what arrived and what the jar
// now holds, which is what the operator needs to decide whether to go and do
// that.
//
// Not rate limited the way noticeFunding is. That one repeats a state that
// persists until somebody tops up, so without a daily cap it would say the
// same thing four times an hour; this one fires on an edge that somebody
// deliberately caused, and swallowing a second gift because a first arrived
// today would be the one direction this must not fail in.
func (p *Plugin) noticeDonation(ctx context.Context, guildID string, f Funding, donation, balance float64) {
	detail := fmt.Sprintf("%s arrived in the tip jar at `%s`. It now holds %s across %d %s. "+
		"Nothing is automatic from here: the credit is bought by hand at the gateway checkout. "+
		"/aimod funding shows the jar.",
		formatUSD(donation), f.Address, formatUSD(balance), f.Donations+1,
		plural(f.Donations+1, "donation", "donations"))
	if err := p.auditWriter.Record(ctx, guildID, core.ActorSystem, "aimod.funding_received", "", detail); err != nil {
		p.log.Error("aimod: audit donation", "guild", guildID, "err", err)
	}
}

// checkCredit warns once a day when the gateway balance is nearly gone.
func (p *Plugin) checkCredit(ctx context.Context, guildID string) {
	cfg, err := p.store.Config(ctx, guildID)
	if err != nil || cfg.Mode == ModeOff {
		return
	}
	spec, sealed := route(cfg)
	if len(sealed) == 0 {
		return
	}
	plain, err := p.sealer.open(sealed)
	if err != nil {
		return
	}
	info, err := p.keyInfo(ctx, spec, plain)
	// A key with no limit set reports LimitRemaining nil, and there is then
	// no balance to warn about. /aimod funding says so and nudges the admin
	// to set one; a warning cannot be invented from an unknown.
	if err != nil || info.LimitRemaining == nil {
		return
	}
	left, ok := p.runway(ctx, guildID, *info.LimitRemaining)
	if !ok || left > lowCreditRunway {
		return
	}
	p.noticeFunding(ctx, guildID, spec, *info.LimitRemaining, left)
}

// runway estimates how long the remaining credit lasts at the recent burn
// rate. ok is false when there are no receipts to divide by, which callers
// render as no line at all rather than as an unlimited runway.
func (p *Plugin) runway(ctx context.Context, guildID string, remaining float64) (time.Duration, bool) {
	since := today(p.now()).AddDate(0, 0, -spendHistoryDays)
	history, err := p.store.SpendSince(ctx, guildID, since)
	if err != nil {
		return 0, false
	}
	perDay := actualPerDay(history)
	if perDay <= 0 {
		return 0, false
	}
	if remaining <= 0 {
		return 0, true
	}
	return time.Duration(remaining / perDay * float64(24*time.Hour)), true
}

// noticeFunding records that a guild is nearly out of credit, once per day.
// The same dedupe as noticeBudget, and for the same reason: this runs on a
// timer, and a warning repeated every fifteen minutes is one nobody reads.
func (p *Plugin) noticeFunding(ctx context.Context, guildID string, spec *providerSpec, remaining float64, left time.Duration) {
	day := today(p.now())
	p.fundingNoticeMu.Lock()
	last, seen := p.fundingNoticed[guildID]
	if seen && !last.Before(day) {
		p.fundingNoticeMu.Unlock()
		return
	}
	p.fundingNoticed[guildID] = day
	p.fundingNoticeMu.Unlock()

	detail := fmt.Sprintf("%s of "+spec.label+" credit left, about %s at the last week's rate. "+
		"Below this the plugin falls back to pattern checks only. /aimod funding shows the tip jar.",
		formatUSD(remaining), core.FormatDuration(left))
	if err := p.auditWriter.Record(ctx, guildID, core.ActorSystem, "aimod.funding_low", "", detail); err != nil {
		p.log.Error("aimod: audit funding notice", "guild", guildID, "err", err)
	}
}

// creditUnknown explains an ungaugeable balance, per gateway.
//
// The two have different reasons for it and different fixes, and saying
// "set a limit on the key" to somebody on a free tier would be advice for a
// problem they do not have.
func creditUnknown(spec *providerSpec) string {
	if spec == orcaRouter {
		return "Scanning runs on free models, so there is no balance to run down.\n" +
			subtext("What limits it there is a request rate rather than money. The tip jar below still "+
				"pays for the paid fallback, which confirms anything the free pass flags.")
	}
	return "Not shown: this server's key has no credit limit set.\n" +
		subtext("Setting a limit on the key at openrouter.ai turns this into a gauge, "+
			"and stops a leaked key draining the account.")
}

// acceptedChains lists the chains in this family that the gateway's checkout
// was last seen taking, in rail-table order and without repeats.
//
// The answer drives a hedge, never a claim. What a checkout accepts is an
// observation with a date on it (providerSpec.topUpChains), so an empty result
// means "we have not seen it take any of these", not "these are worthless".
func acceptedChains(spec *providerSpec, family *walletFamily) []string {
	var out []string
	seen := map[string]bool{}
	for _, rail := range railsFor(family) {
		if seen[rail.chain] || !spec.acceptsChain(rail.chain) {
			continue
		}
		seen[rail.chain] = true
		out = append(out, chainLabels[rail.chain])
	}
	return out
}

// chainLabels renders a chain key the way a wallet's network picker does, so
// the name on screen matches the one somebody is about to select.
var chainLabels = map[string]string{
	"base":     "Base",
	"ethereum": "Ethereum",
	"polygon":  "Polygon",
	"arbitrum": "Arbitrum",
	"bsc":      "BNB Chain",
	"tron":     "TRON",
	"solana":   "Solana",
}

// railBreakdown renders where the money in a jar actually sits, one line per
// rail holding something, each linked to that chain's own explorer.
//
// Only non-empty rails are listed. A family has up to nine rails and a jar
// usually has money on one, so listing them all would bury the useful line in
// eight zeroes. An empty result means the jar is empty, which the caller says
// in words instead.
func railBreakdown(f Funding) []string {
	var out []string
	for _, key := range sortedRailKeys(f.Balances) {
		amount := f.Balances[key]
		if amount < donationDust {
			continue
		}
		rail := railByKey(key)
		if rail == nil {
			// A rail this build no longer lists. Skipped rather than guessed
			// at: the total already counts it, and inventing a name for a
			// chain nobody here knows about would be the one kind of wrong
			// this field cannot afford.
			continue
		}
		out = append(out, fmt.Sprintf("%s in %s on [%s](%s)", formatUSD(amount), rail.asset,
			chainLabels[rail.chain], fmt.Sprintf(rail.explorer, f.Address)))
	}
	return out
}

// humanRunway renders a countdown as prose, not as core.FormatDuration's
// compact form. Every member-facing duration in this codebase is prose and
// every admin or audit one is compact; this is the member-facing side, and
// the audit line in noticeFunding above is deliberately the other.
func humanRunway(d time.Duration) string {
	switch {
	case d < time.Hour:
		return "under an hour"
	case d < 24*time.Hour:
		h := int(d.Round(time.Hour).Hours())
		if h == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", h)
	default:
		days := int(d.Round(24*time.Hour).Hours() / 24)
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	}
}

// agoWords renders how long ago something happened, in prose.
func agoWords(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	return humanRunway(d) + " ago"
}

// subtext renders a line in Discord's small grey style.
//
// The tip jar is read by two people with different questions. Somebody who
// came to donate wants the address and nothing else; somebody deciding
// whether to trust it wants who set it, which chain it is on, and what merlin
// can and cannot do with the wallet. Putting the second group's answers in
// subtext keeps them on the screen without making the first group read past
// four sentences to reach a hex string.
//
// Discord's marker applies to the line it starts, not to a block, so every
// line gets one. Rewriting the newlines here rather than documenting that as
// a rule for callers is shorter than the comment explaining the rule would
// have been, and it cannot be forgotten at a call site.
func subtext(s string) string { return "-# " + strings.ReplaceAll(s, "\n", "\n-# ") }

// joinWords renders a short list the way a sentence does, so a network list
// reads as prose rather than as CSV in the middle of a warning.
func joinWords(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// chainNames maps chain keys to the names a wallet's network picker shows,
// falling back to the key so an unmapped chain reads as itself rather than
// vanishing from a sentence about where money can be sent.
func chainNames(chains []string) []string {
	out := make([]string, 0, len(chains))
	for _, c := range chains {
		if label, ok := chainLabels[c]; ok {
			out = append(out, label)
			continue
		}
		out = append(out, c)
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// canSetFunding answers whether userID may point this guild's jar at a
// wallet: the guild owner, or the bootstrap operator.
//
// Not a tier. PermSpec cannot express either identity, so TierAdmin is the
// coarse floor on the command and this is the real gate, exactly as
// /aimod moderate-user does for its own bootstrap-only rule. A guild with
// five admins otherwise has five accounts that can silently repoint a payout
// address, and nothing sent on chain can be recovered.
//
// Fails closed: an unresolvable guild refuses the change rather than assuming
// the actor is the owner, matching core.Permissions.CanModerate's own rule.
func (p *Plugin) canSetFunding(guildID, userID string) (bool, error) {
	// Every branch below either returns a hard error or compares against a
	// single identity. There is deliberately no path here that widens on a
	// missing dependency: a nil privilege checker loses the bootstrap escape
	// hatch rather than granting anybody else the right.
	if guildID == "" || userID == "" {
		return false, fmt.Errorf("could not tell who you are, or which server this is")
	}
	if p.privilege != nil && p.privilege.IsBootstrapAdmin(userID) {
		return true, nil
	}
	guild, err := p.ops(guildID).Guild(guildID)
	if err != nil {
		return false, fmt.Errorf("could not read this server to check who owns it: %w", err)
	}
	// A nil guild with no error should not happen, and if it ever does the
	// answer is no. Dereferencing it would panic into the router's recover(),
	// which reads to the actor as a transient glitch worth retrying.
	if guild == nil {
		return false, fmt.Errorf("this server could not be read, so ownership cannot be confirmed")
	}
	return guild.OwnerID == userID, nil
}

// handleFundingShow is the public face of the jar.
//
// TierPublic on purpose, and it is the only reason the feature works: the
// members being asked to chip in have to be able to see the address and
// whether it is needed. It shows two balances and a runway, and nothing about
// what is being moderated or whom.
//
// Answered publicly, unlike every other command in this plugin, and for the
// same reason it is TierPublic: a tip jar shown only to whoever typed the
// command is shown to the one person who already knew about it. Somebody
// running this in a channel is usually pointing other people at it. That also
// makes it the one aimod surface that must carry nothing private, which is
// why the mod-facing figures stay on /aimod status.
func (p *Plugin) handleFundingShow(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.DeferResponsePublic(s, i); err != nil {
		p.log.Error("aimod: defer funding show", "guild", i.GuildID, "err", err)
		return
	}

	f, err := p.store.Funding(ctx, i.GuildID)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Failed to read the tip jar", err)
		return
	}
	cfg, err := p.store.Config(ctx, i.GuildID)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Failed to read the configuration", err)
		return
	}

	color := core.ColorSuccess
	var fields []*discordgo.MessageEmbedField

	// remaining is nil whenever the balance cannot be known, which is a real
	// and common case (a key with no limit set on it). The difference between
	// "cannot know" and "zero" is the whole reason this is a pointer.
	var remaining, limit *float64
	spec, sealed := route(cfg)
	if len(sealed) > 0 {
		if plain, err := p.sealer.open(sealed); err == nil {
			if info, err := p.keyInfo(ctx, spec, plain); err == nil {
				remaining, limit = info.LimitRemaining, info.Limit
			}
		}
	}

	var left time.Duration
	var haveRunway bool
	if remaining == nil {
		fields = append(fields, &discordgo.MessageEmbedField{
			Value: core.TruncateEmbedField(creditUnknown(spec)),
		})
	} else {
		left, haveRunway = p.runway(ctx, i.GuildID, *remaining)
		value := formatUSD(*remaining) + " left"
		if limit != nil && *limit > 0 {
			value = bar(*remaining/(*limit), barWidth) + "\n" + formatUSD(*remaining) + " of " + formatUSD(*limit)
		}
		if haveRunway {
			value += "\n" + subtext("about "+humanRunway(left)+" at the last week's rate")
		}
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Scanning credit", Value: core.TruncateEmbedField(value)})
		switch {
		case *remaining <= 0:
			color = core.ColorError
		case haveRunway && left <= lowCreditRunway:
			color = core.ColorWarning
		}
	}

	if !f.Configured() {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "Tip jar",
			Value: "Not set up yet.\n" +
				subtext("The server owner can point it at a wallet with /aimod funding set-address."),
		})
	} else {
		jar := "**" + formatUSD(f.BalanceUSD) + "** waiting to be loaded"
		// Where it sits, not just how much. A donor who sent on Polygon can
		// confirm their own transfer landed, and anybody can follow a link and
		// check the figure above against the chain rather than taking this
		// bot's word for it.
		if lines := railBreakdown(f); len(lines) > 0 {
			jar += "\n" + subtext(strings.Join(lines, "\n"))
		}
		if f.Donations > 0 {
			jar += "\n" + subtext(fmt.Sprintf("%s raised from %d %s so far", formatUSD(f.ReceivedUSD), f.Donations,
				plural(f.Donations, "donation", "donations")))
		}
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "In the tip jar",
			Value: core.TruncateEmbedField(jar),
		})

		// The address, who set it, and the standing warning. Never routed
		// through internal/voice: line selection there is random and falls
		// back silently, which is right for a greeting and wrong for the one
		// sentence telling somebody where their money is about to go.
		//
		// Three weights, because these lines are not equally urgent. The
		// address is a fenced block so it survives a phone keyboard and can be
		// copied in one gesture. The chain is bold because getting it wrong is
		// the mistake people actually make: chains are separate ledgers, so
		// USDC sent on Ethereum mainnet lands on that ledger instead, where
		// the same key still controls it but this gauge cannot see it and
		// OpenRouter's Base checkout cannot spend it. Everything below that is
		// subtext, which is Discord's own small grey style: still on the
		// screen for whoever is deciding, out of the way of whoever just
		// wanted the address.
		family := familyFor(f.Address)
		if family == nil {
			// Only reachable from a hand-edited row: set-address refuses
			// anything familyFor cannot place. Naming no network at all is the
			// one safe answer, since guessing would print a chain somebody
			// might send on.
			family = &walletFamily{
				label:    "an unrecognised address",
				networks: "unknown",
				note: "merlin cannot tell which network this address belongs to. Do not send anything " +
					"until the server owner sets it again.",
			}
		}
		where := "```\n" + f.Address + "\n```" +
			"**Networks: " + family.networks + "**\n" +
			family.note + "\n"
		if family.swap != "" {
			where += subtext(family.swap) + "\n"
		}
		// Anybody can audit the jar without trusting the arithmetic above.
		// For an EVM address this is deliberately the multi-chain view: one
		// per-chain explorer would show a single ledger and imply the other
		// four were empty.
		if family.explorer != "" {
			where += subtext("[Check this wallet yourself]("+fmt.Sprintf(family.explorer, f.Address)+")") + "\n"
		}

		// Whether donations can buy credit without a detour. Hedged and dated,
		// unlike everything above it, because what a checkout accepts is an
		// observation of somebody else's merchant configuration rather than a
		// property of a chain. The previous version stated it flatly and was
		// wrong: it told donors Ethereum mainnet USDC would be lost, which
		// OpenRouter's checkout takes perfectly well.
		if accepted := acceptedChains(spec, family); len(accepted) == 0 {
			where += subtext("Note for the operator: as of "+spec.topUpVerified+", "+spec.label+
				"'s checkout was taking "+joinWords(chainNames(spec.topUpChains))+
				". Donations here will probably need swapping or bridging first.") + "\n"
			if color == core.ColorSuccess {
				color = core.ColorWarning
			}
		} else {
			where += subtext("As of "+spec.topUpVerified+", "+spec.label+"'s checkout took "+
				joinWords(accepted)+", so donations on "+plural(len(accepted), "that network", "those networks")+
				" buy scanning credit directly.") + "\n"
		}

		provenance := "merlin only reads this wallet. Funds go to whoever controls it."
		if f.SetBy != "" {
			provenance = "Set by " + core.MentionUser(f.SetBy) + " " + agoWords(p.now().Sub(f.SetAt)) + ". " + provenance
		}
		where += subtext(provenance)

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "Send " + family.label + " to",
			Value: core.TruncateEmbedField(where),
		})
	}

	body := p.fundingWords(ctx, i.GuildID, f, remaining, left, haveRunway)

	// A freshly changed payout address leads, and raises the severity rather
	// than lowering it: a jar that is both nearly empty and just repointed is
	// still an error, not a warning.
	if f.Configured() && p.now().Sub(f.SetAt) < addressChangeWarning {
		if color == core.ColorSuccess {
			color = core.ColorWarning
		}
		body = "**This address changed " + agoWords(p.now().Sub(f.SetAt)) +
			".** Check with your server owner before sending.\n\n" + body
	}

	embed := core.NewEmbed(color, "merlin's tip jar", core.TruncateEmbedDescription(body), fields...)
	if err := core.FollowUpEmbed(s, i, embed); err != nil {
		p.log.Error("aimod: respond funding show", "guild", i.GuildID, "err", err)
	}
}

// fundingWords picks the prose around the numbers. The numbers themselves are
// built by the caller, in code.
func (p *Plugin) fundingWords(ctx context.Context, guildID string, f Funding, remaining *float64, left time.Duration, haveRunway bool) string {
	key := voice.KeyFundingAsk
	vars := map[string]string{}
	switch {
	case remaining != nil && *remaining <= 0:
		key = voice.KeyFundingDry
	case remaining != nil && haveRunway && left <= lowCreditRunway:
		key = voice.KeyFundingLow
		vars["runway"] = humanRunway(left)
	}

	body := p.voice.Line(ctx, guildID, key, vars)
	if f.Donations > 0 {
		thanks := p.voice.Line(ctx, guildID, voice.KeyFundingThanks,
			map[string]string{"raised": formatUSD(f.ReceivedUSD)})
		if thanks != "" {
			body += "\n\n" + thanks
		}
	}
	return body
}

// handleFundingSetAddress points the jar at a wallet.
func (p *Plugin) handleFundingSetAddress(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := core.DeferResponse(s, i); err != nil {
		p.log.Error("aimod: defer funding set-address", "guild", i.GuildID, "err", err)
		return
	}

	userID := ""
	if i.Member != nil && i.Member.User != nil {
		userID = i.Member.User.ID
	}
	allowed, err := p.canSetFunding(i.GuildID, userID)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Could not check who owns this server", err)
		return
	}
	if !allowed {
		_ = core.FollowUpErr(s, i, "Only the server owner can change this",
			fmt.Errorf("this is where donated money goes, so it is deliberately not something every admin can repoint"))
		return
	}

	address := strings.TrimSpace(core.LeafArgs(i)["address"].StringValue())
	family := familyFor(address)
	if family == nil {
		_ = core.FollowUpErr(s, i, "That is not a wallet address",
			fmt.Errorf("give an EVM address (`0x` and 40 hex characters, read on %s), "+
				"a TRON one (`T` and 33 more characters, read on %s), "+
				"or a Solana one (read on %s)",
				familyEVM.networks, familyTron.networks, familySolana.networks))
		return
	}
	if p.eth == nil {
		_ = core.FollowUpErr(s, i, "The funding chain is not configured",
			fmt.Errorf("no RPC endpoint is wired up"))
		return
	}

	// The chains are the authority on whether this address can be read at all,
	// and the balances they report become the baseline in the same write, so
	// the first poll cannot report an existing balance as a donation. Every
	// rail in the family has to answer: an address accepted while one chain
	// was unreachable would bank an incomplete baseline and then report that
	// chain's existing holdings as a gift on the next poll.
	balances, balance, err := p.eth.Balances(ctx, address)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Could not read that wallet", err)
		return
	}

	previous, _ := p.store.Funding(ctx, i.GuildID)
	if err := p.store.SetFundingAddress(ctx, i.GuildID, address, userID, p.now(), balance, balances); err != nil {
		_ = core.FollowUpErr(s, i, "Failed to save the address", err)
		return
	}
	p.reconcileFundingJob(i.GuildID, true)

	detail := "tip jar set to " + address
	if previous.Configured() && previous.Address != address {
		detail = "tip jar moved from " + previous.Address + " to " + address
	}
	if err := p.auditWriter.Record(ctx, i.GuildID, userID, "aimod.funding_address_changed", "", detail); err != nil {
		p.log.Error("aimod: audit funding address", "guild", i.GuildID, "err", err)
	}

	_ = core.FollowUpOK(s, i, "Tip jar set",
		"Donations of "+family.label+" to `"+address+"` show up on `/aimod funding` within "+
			core.FormatDuration(fundingPollInterval)+". Networks read: "+family.networks+
			". It currently holds "+formatUSD(balance)+".\n\n"+
			"merlin only reads this wallet and can never spend from it. Buying credits is still a human on the gateway's checkout.")
}

// handleFundingClear takes the jar down.
//
// Deliberately open to any admin, unlike set-address, and the asymmetry is
// the point: clearing only ever stops donations, it can never redirect them,
// so it is a kill switch that fails safe. An admin who suspects a bad address
// can shut the jar immediately without waiting for the owner.
func (p *Plugin) handleFundingClear(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if err := p.store.ClearFunding(ctx, i.GuildID); err != nil {
		core.RespondErr(s, i, "Failed to clear the tip jar", err)
		return
	}
	p.reconcileFundingJob(i.GuildID, false)

	actor := ""
	if i.Member != nil && i.Member.User != nil {
		actor = i.Member.User.ID
	}
	if err := p.auditWriter.Record(ctx, i.GuildID, actor, "aimod.funding_cleared", "", "tip jar removed"); err != nil {
		p.log.Error("aimod: audit funding cleared", "guild", i.GuildID, "err", err)
	}

	core.RespondOK(s, i, "Tip jar removed",
		"`/aimod funding` no longer shows an address, and the balance poll has stopped.")
}
