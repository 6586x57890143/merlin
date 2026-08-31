package contest

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/scheduler"
	"github.com/6586x57890143/merlin/internal/secret"
	"github.com/6586x57890143/merlin/internal/voice"
	"github.com/bwmarrin/discordgo"
)

const (
	// tickInterval is how often a live contest is looked at. A minute,
	// matching roles-sweep rather than rotation's hourly cadence, because a
	// contest deadline is a moment people are watching for: a phase that
	// opens up to an hour late reads as broken.
	tickInterval = time.Minute

	// refreshInterval is how often a submission's Discord CDN link is
	// re-derived. Signed attachment URLs last about a day, so half a day is
	// one refresh of headroom and costs one REST call per entry.
	refreshInterval = 12 * time.Hour

	// minPhase is the shortest a phase may be. Two minutes rather than
	// something rounder because that is what makes an end-to-end test of all
	// four phases take four minutes instead of an afternoon, and there is no
	// reason to forbid a genuinely quick contest.
	minPhase = 2 * time.Minute

	// maxPhase caps a phase at a month. Not a Discord limit: a contest that
	// runs longer than that is a channel, and it would sit in the scheduler
	// firing every minute for the whole time.
	maxPhase = 30 * 24 * time.Hour

	// channelCapHeadroom keeps contests clear of Discord's 500-channel guild
	// cap, the same self-throttle rotation applies for the same reason
	// (spec.MD §4): the bot must never be the thing that walks a guild into
	// a hard platform limit.
	channelCapHeadroom = 20

	// maxEntryMedia bounds how many attachments one entry contributes to the
	// gallery. Four fits a card without turning it into a scroller, and
	// anything past that is a portfolio rather than a contest entry. The
	// rest stay visible in the forum thread, which the card links to.
	maxEntryMedia = 4

	// maxEntries bounds one contest. Past this the gallery is unusable and
	// the snapshot stops being small, and a server that genuinely needs more
	// wants heats rather than a longer page.
	maxEntries = 200
)

// DiscordOps is the narrow slice of Discord this plugin touches.
// *discordguard.GuildOps satisfies it structurally, which is what keeps
// every destructive call behind the pause/dry-run/rate-limit gate without
// this package importing the guard.
type DiscordOps interface {
	Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error)
	ThreadsActive(channelID string, options ...discordgo.RequestOption) (*discordgo.ThreadsList, error)
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelPermissionSet(channelID, targetID string, targetType discordgo.PermissionOverwriteType, allow, deny int64, options ...discordgo.RequestOption) error
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessagePin(channelID, messageID string, options ...discordgo.RequestOption) error
	UserChannelCreate(recipientID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
}

// OpsProvider hands back the guild-bound Discord view. Mirrors
// rotation.OpsProvider and roles' equivalent.
type OpsProvider func(guildID string) DiscordOps

// Plugin is the contest plugin.
type Plugin struct {
	store   Store
	opsFor  OpsProvider
	speaker voice.Source
	sealer  *secret.Sealer
	worker  *workerClient

	audit    core.AuditWriter
	log      *slog.Logger
	sched    core.Scheduler
	commands *core.CommandRouter

	// now is injected so tests can drive the phase machine without waiting,
	// mirroring the Scheduler's own hook.
	now func() time.Time

	mu             sync.Mutex
	tickRegistered map[string]bool
	lastRefresh    map[string]time.Time // contest ID -> last CDN refresh
}

// New builds the plugin. workerURL may be empty, in which case the gallery
// and voting simply do not exist for this deployment and everything else
// still runs; sealer may be nil, in which case prize codes cannot be stored
// and /contest prize says so instead of storing one in the clear.
func New(store Store, opsFor OpsProvider, speaker voice.Source, sealer *secret.Sealer, workerURL, workerToken, linkKey string) *Plugin {
	return &Plugin{
		store:          store,
		opsFor:         opsFor,
		speaker:        speaker,
		sealer:         sealer,
		worker:         newWorkerClient(workerURL, workerToken, linkKey),
		now:            time.Now,
		tickRegistered: make(map[string]bool),
		lastRefresh:    make(map[string]time.Time),
	}
}

func (p *Plugin) Name() string { return "contest" }

func (p *Plugin) Init(deps core.Deps) error {
	p.audit = deps.Audit
	p.log = deps.Logger
	p.sched = deps.Scheduler
	p.commands = deps.Commands
	p.registerCommands()
	return nil
}

func (p *Plugin) Start(context.Context) error    { return nil }
func (p *Plugin) Shutdown(context.Context) error { return nil }

// SyncGuild is called from cmd/bot/main.go on every GuildCreate.
func (p *Plugin) SyncGuild(ctx context.Context, guildID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reconcileTickJob(ctx, guildID)
}

// ForgetGuild drops bookkeeping when merlin leaves a guild.
func (p *Plugin) ForgetGuild(guildID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tickRegistered, guildID)
	if err := p.sched.Unregister(scheduler.JobKey(guildID, "contest-tick")); err != nil {
		p.log.Error("contest: unregister tick job", "guild", guildID, "err", err)
	}
}

// reconcileTickJob registers the per-guild tick only where there is a live
// contest to tick, and unregisters it when there is not. Same "a job exists
// only where it has work" rule as rotation's sweep and notice jobs, and for
// the same reason: a job armed in a guild that never ran a contest is a
// minute-by-minute database query forever.
//
// Assumes the caller holds p.mu, exactly as rotation.reconcileNoticeJob
// does. Taking the lock again here deadlocks.
func (p *Plugin) reconcileTickJob(ctx context.Context, guildID string) {
	_, err := p.store.LiveContest(ctx, guildID)
	live := err == nil
	if err != nil && err != ErrNoLiveContest {
		// A failed lookup leaves the current registration untouched rather
		// than guessing in either direction: unregistering on a transient
		// database error would silently stop a running contest, and
		// registering would arm a job with nothing to do.
		p.log.Error("contest: reconcile tick job", "guild", guildID, "err", err)
		return
	}

	key := scheduler.JobKey(guildID, "contest-tick")
	switch {
	case live && !p.tickRegistered[guildID]:
		if err := p.sched.Register(key,
			core.CronSpec{Schedule: core.IntervalSchedule{Interval: tickInterval}},
			func(ctx context.Context) error { return p.tick(ctx, guildID) },
		); err != nil {
			p.log.Error("contest: register tick job", "guild", guildID, "err", err)
			return
		}
		p.tickRegistered[guildID] = true
	case !live && p.tickRegistered[guildID]:
		if err := p.sched.Unregister(key); err != nil {
			p.log.Error("contest: unregister tick job", "guild", guildID, "err", err)
			return
		}
		delete(p.tickRegistered, guildID)
	}
}

// tick is the whole scheduled half of this plugin: pull in whatever the
// forum has, advance the phase if its deadline has passed, and push the
// result to the Worker.
//
// Ordering matters. Submissions are synced before the phase check so that a
// tick which closes submissions has already seen everything posted in the
// final minute, and so the snapshot the vote phase opens with is complete.
func (p *Plugin) tick(ctx context.Context, guildID string) error {
	c, err := p.store.LiveContest(ctx, guildID)
	if err == ErrNoLiveContest {
		p.mu.Lock()
		p.reconcileTickJob(ctx, guildID)
		p.mu.Unlock()
		return nil
	}
	if err != nil {
		return err
	}

	if c.Phase == PhaseSubmit || c.Phase == PhaseVote {
		if err := p.syncSubmissions(ctx, c); err != nil {
			// Not fatal to the tick: a forum read failing must not stop a
			// deadline from being enforced, or a Discord blip could hold a
			// contest in its submission phase indefinitely.
			p.log.Error("contest: sync submissions", "guild", guildID, "contest", c.ID, "err", err)
		}
	}

	deadline, has := c.Deadline()
	if has && !p.now().Before(deadline) {
		return p.advance(ctx, c)
	}

	// Nothing changed phase, so the only reason to push is a refreshed set
	// of CDN links. Rate-limited to refreshInterval because the push itself
	// is cheap but the REST reads behind it are one per entry.
	if c.Phase == PhaseVote && p.dueForRefresh(c.ID) {
		if err := p.pushSnapshot(ctx, c); err != nil {
			p.log.Error("contest: refresh push", "contest", c.ID, "err", err)
		}
	}
	return nil
}

func (p *Plugin) dueForRefresh(contestID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	last := p.lastRefresh[contestID]
	if p.now().Sub(last) < refreshInterval {
		return false
	}
	p.lastRefresh[contestID] = p.now()
	return true
}

// advance moves a contest to its next phase. Every transition claims the
// move with a conditional update before doing anything visible, so two
// overlapping ticks cannot both announce it: exactly one wins the claim and
// the other returns having done nothing. Same shape as rotation's
// claim-before-posting rule for pre-rotation notices, and the direction is
// chosen the same way: a missed announcement is invisible, a doubled one
// reads as a broken bot.
func (p *Plugin) advance(ctx context.Context, c Contest) error {
	switch c.Phase {
	case PhaseAnnounce:
		won, err := p.store.AdvancePhase(ctx, c.ID, PhaseAnnounce, PhaseSubmit)
		if err != nil || !won {
			return err
		}
		c.Phase = PhaseSubmit
		if err := p.setForumOpen(ctx, c, true); err != nil {
			p.log.Error("contest: open forum", "contest", c.ID, "err", err)
		}
		p.announceSubmissionsOpen(ctx, c)
		p.pushBestEffort(ctx, c)
		return nil

	case PhaseSubmit:
		won, err := p.store.AdvancePhase(ctx, c.ID, PhaseSubmit, PhaseVote)
		if err != nil || !won {
			return err
		}
		c.Phase = PhaseVote
		if err := p.setForumOpen(ctx, c, false); err != nil {
			p.log.Error("contest: lock forum", "contest", c.ID, "err", err)
		}
		// Push before announcing: the announcement carries a link to a page
		// that has to already show the entries when somebody clicks it.
		if err := p.pushSnapshot(ctx, c); err != nil {
			p.log.Error("contest: push at vote open", "contest", c.ID, "err", err)
		}
		p.announceVotingOpen(ctx, c)
		return nil

	case PhaseVote:
		return p.finish(ctx, c)
	}
	return nil
}

// finish closes voting, computes the tally, announces the winners and hands
// out prizes.
//
// The phase claim happens last here, unlike every other transition, and that
// is deliberate: the tally comes from the Worker, and if the Worker is
// unreachable there is no result to announce. Claiming first would leave a
// contest sitting in results with no winner and nothing scheduled to try
// again. Returning the error instead leaves it in vote, records why on the
// contest so /contest status can say it out loud, and lets the Scheduler's
// own backoff retry.
func (p *Plugin) finish(ctx context.Context, c Contest) error {
	subs, err := p.store.Submissions(ctx, c.ID)
	if err != nil {
		return err
	}

	if len(subs) == 0 {
		won, err := p.store.AdvancePhase(ctx, c.ID, PhaseVote, PhaseResults)
		if err != nil || !won {
			return err
		}
		c.Phase = PhaseResults
		p.announceNoEntries(ctx, c)
		p.afterFinish(ctx, c)
		return nil
	}

	var results []resultView
	if p.worker.Configured() {
		tally, err := p.worker.Close(ctx, c.Slug)
		if err != nil {
			if serr := p.store.SetTallyError(ctx, c.ID, err.Error()); serr != nil {
				p.log.Error("contest: record tally error", "contest", c.ID, "err", serr)
			}
			return fmt.Errorf("contest: close voting: %w", err)
		}
		results = rank(subs, tally)
	} else {
		// No Worker means nobody could vote, so every entry is tied at zero
		// and there is no winner to declare. Say that rather than crowning
		// whoever posted first.
		results = rank(subs, Tally{})
	}

	blob, err := marshalResults(results)
	if err != nil {
		return err
	}
	won, err := p.store.AdvancePhase(ctx, c.ID, PhaseVote, PhaseResults)
	if err != nil || !won {
		return err
	}
	c.Phase = PhaseResults
	if err := p.store.SetResults(ctx, c.ID, blob, p.now()); err != nil {
		p.log.Error("contest: store results", "contest", c.ID, "err", err)
	}
	c.Results = blob

	p.pushBestEffort(ctx, c)
	p.announceWinners(ctx, c, subs, results)
	p.awardPrizes(ctx, c, subs, results)
	p.afterFinish(ctx, c)
	return nil
}

// afterFinish drops the now-idle tick job. Separate from finish so both the
// entries and the no-entries path get it.
func (p *Plugin) afterFinish(ctx context.Context, c Contest) {
	p.mu.Lock()
	delete(p.lastRefresh, c.ID)
	p.reconcileTickJob(ctx, c.GuildID)
	p.mu.Unlock()
}

// pushBestEffort pushes and logs, for the call sites where a stale gallery
// is not worth failing an operation that already succeeded. Same
// log-and-continue policy as an audit write failure.
func (p *Plugin) pushBestEffort(ctx context.Context, c Contest) {
	if err := p.pushSnapshot(ctx, c); err != nil {
		p.log.Error("contest: push snapshot", "contest", c.ID, "err", err)
	}
}

// pushSnapshot rebuilds the whole contest as the Worker sees it and sends
// it. Deliberately a full replace rather than a diff: merlin owns the
// schema, the payload is a few KB, and a push landing after a missed one is
// still correct with no reconciliation to write.
func (p *Plugin) pushSnapshot(ctx context.Context, c Contest) error {
	if !p.worker.Configured() {
		return nil
	}
	subs, err := p.store.Submissions(ctx, c.ID)
	if err != nil {
		return err
	}
	prizes, err := p.store.Prizes(ctx, c.ID)
	if err != nil {
		return err
	}
	return p.worker.Push(ctx, p.snapshotOf(c, subs, prizes))
}

func (p *Plugin) snapshotOf(c Contest, subs []Submission, prizes []Prize) snapshot {
	snap := snapshot{
		Slug:      c.Slug,
		Title:     c.Title,
		Theme:     c.Theme,
		Phase:     string(c.Phase),
		SubmitAt:  c.SubmitAt.Unix(),
		VoteAt:    c.VoteAt.Unix(),
		ResultsAt: c.ResultsAt.Unix(),
		MaxVotes:  c.MaxVotes,
		Guild:     c.GuildID,
		Entries:   make([]entryView, 0, len(subs)),
		Prizes:    make([]prizeView, 0, len(prizes)),
	}
	if c.ForumChannelID != "" {
		snap.Forum = channelLink(c.GuildID, c.ForumChannelID)
	}
	for _, s := range subs {
		snap.Entries = append(snap.Entries, entryView{
			ID:     s.ID,
			By:     s.Author,
			ByHash: p.worker.Hash(s.UserID),
			Title:  s.Title,
			Kind:   s.Kind,
			URL:    s.MediaURL,
			URLs:   s.MediaURLs,
			Link:   s.Link,
			Body:   s.Body,
			Thread: channelLink(c.GuildID, s.ThreadID),
		})
	}
	for _, pr := range prizes {
		// Title and details only. There is no field on prizeView for the
		// sealed code and adding one would be the whole point of this
		// design going out the window.
		snap.Prizes = append(snap.Prizes, prizeView{By: pr.DonorName, Title: pr.Title, Details: pr.Details})
	}
	if len(c.Results) > 0 {
		if rs, err := unmarshalResults(c.Results); err == nil {
			snap.Results = rs
		}
	}
	return snap
}

func channelLink(guildID, channelID string) string {
	return "https://discord.com/channels/" + guildID + "/" + channelID
}

// newSlug is 128 bits of base32, lowercased. Unguessable rather than
// sequential on purpose: the gallery is a public page showing members' work
// under their display names, on a server whose threat model is mass
// reporting, so anyone with the link can browse and nobody enumerates their
// way in. The Worker also serves it noindex.
func newSlug() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("contest: generate slug: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)), nil
}

// newID is a random 96-bit base32 string. Not a UUID, because nothing here
// needs one: these ids are only ever compared for equality and handed to the
// Worker, and a dependency for that is a dependency for nothing.
func newID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is not a condition this process can continue
		// through: every id, slug and nonce in the binary comes from it.
		panic("contest: crypto/rand: " + err.Error())
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
}

// speak returns merlin's wording, or the plain fallback if the catalog is
// unreachable. Every member-facing sentence in this plugin goes through
// here; admin surfaces do not, per PERSONA.md.
func (p *Plugin) speak(ctx context.Context, guildID string, key voice.Key, vars map[string]string, plain string) string {
	if p.speaker == nil {
		return plain
	}
	if line := p.speaker.Line(ctx, guildID, key, vars); line != "" {
		return line
	}
	return plain
}
