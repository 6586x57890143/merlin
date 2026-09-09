package contest

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
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

	// refreshMargin is how long before a Discord CDN link expires merlin
	// goes and gets a fresh one.
	//
	// The expiry is not guessed: a signed attachment URL carries ?ex=<hex
	// unix seconds>, so every entry says exactly when it dies and the refresh
	// is driven by that rather than by a timer set to half of whatever the
	// lifetime was assumed to be. That matters in both directions. A fixed
	// interval refreshes entries that had eighteen hours left, and it goes on
	// doing so at the same cadence on the day Discord shortens the lifetime,
	// at which point the gallery breaks and nothing in this file is wrong.
	//
	// Six hours of headroom absorbs a failed tick, a restart, and a contest
	// that was paused or rate limited, without being so wide that it refreshes
	// most of a link's life away.
	refreshMargin = 6 * time.Hour

	// refreshFallback is the cadence for a URL whose expiry cannot be read:
	// something that is not a signed Discord attachment, or a signature
	// format that has changed. Half a day, which is what the whole refresh
	// used to run on.
	refreshFallback = 12 * time.Hour

	// refreshFloor stops a contest whose links are somehow always nearly
	// expired from re-reading its forum every single tick. A refresh costs
	// one REST call per entry, and the tick runs every minute.
	refreshFloor = 30 * time.Minute

	// artRefreshWindow is how long after a contest closes merlin keeps
	// re-deriving its entries' CDN links.
	//
	// A Discord attachment URL is signed and lasts about 24 hours, and the
	// snapshot stores it verbatim, so the results gallery -- the one artifact
	// people come back to -- went permanently broken a day after the last
	// refresh. Every finished contest this bot has ever run is currently a
	// page of broken images.
	//
	// Thirty days rather than forever. The cost is one REST call per entry
	// per refresh, and refreshes are need-driven, so a finished contest costs
	// roughly one pass a day; past this nobody is looking and the page falls
	// back to linking the Discord thread the art still lives in, which is
	// honest and free. The bound is the whole reason this is affordable, and
	// it accumulates across every contest a guild has ever run.
	artRefreshWindow = 30 * 24 * time.Hour

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

	// tickerQuips is how many of merlin's lines ride along in a snapshot.
	// Three, because the ticker also carries the phase, the countdown and
	// the entry count, and past that it stops being ambient and starts
	// being something you feel obliged to read.
	tickerQuips = 3

	// maxEntryMedia bounds how many attachments one entry contributes to the
	// gallery. Four fits a card without turning it into a scroller, and
	// anything past that is a portfolio rather than a contest entry. The
	// rest stay visible in the forum thread, which the card links to.
	maxEntryMedia = 4

	// maxEntries bounds one contest. Past this the gallery is unusable and
	// the snapshot stops being small, and a server that genuinely needs more
	// wants heats rather than a longer page.
	maxEntries = 200

	// archivedPageSize is Discord's maximum for one page of archived
	// threads, and maxArchivedPages bounds the walk at more entries than a
	// contest may hold, so a forum that somehow accumulated thousands of
	// archived posts costs a bounded number of calls rather than a loop.
	archivedPageSize = 100
	maxArchivedPages = 4
)

// DiscordOps is the narrow slice of Discord this plugin touches.
// *discordguard.GuildOps satisfies it structurally, which is what keeps
// every destructive call behind the pause/dry-run/rate-limit gate without
// this package importing the guard.
type DiscordOps interface {
	Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error)
	GuildRoles(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Role, error)
	User(userID string, options ...discordgo.RequestOption) (*discordgo.User, error)
	GuildThreadsActive(guildID string, options ...discordgo.RequestOption) (*discordgo.ThreadsList, error)
	ThreadsArchived(channelID string, before *time.Time, limit int, options ...discordgo.RequestOption) (*discordgo.ThreadsList, error)
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelPermissionSet(channelID, targetID string, targetType discordgo.PermissionOverwriteType, allow, deny int64, options ...discordgo.RequestOption) error
	ChannelEditComplex(channelID string, data *discordgo.ChannelEdit, options ...discordgo.RequestOption) (*discordgo.Channel, error)
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
	botID          string // merlin's own user ID, resolved once (forumperms.go)
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
	if err == ErrNoLiveContest {
		// A contest that has finished is not live, but its gallery still has
		// links that expire, so the job stays until the refresh window is up.
		if _, ok := p.closedNeedingArt(ctx, guildID); ok {
			live = true
		}
	}
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
		if done, ok := p.closedNeedingArt(ctx, guildID); ok {
			return p.refreshArt(ctx, done)
		}
		p.mu.Lock()
		p.reconcileTickJob(ctx, guildID)
		p.mu.Unlock()
		return nil
	}
	if err != nil {
		return err
	}

	// While submissions are open the sync runs every tick, so a new entry
	// shows up within a minute. Once voting starts the entry list is frozen
	// and the only reason to re-read the forum is that the CDN links go
	// stale, which costs one REST call per entry: that goes at
	// need, not every minute. The rate limit used to sit on the
	// push alone, which is the cheap half, leaving a 200-entry contest
	// making 200 REST calls a minute for a link that needs refreshing twice
	// a day.
	refresh := c.Phase == PhaseVote && p.dueForRefresh(ctx, c)
	synced := c.Phase == PhaseSubmit || refresh
	if synced {
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

	// A sync that ran is a snapshot that may have changed, so the push
	// follows the sync rather than the refresh.
	//
	// Gating it on refresh alone meant the gallery was never pushed once
	// during the submission window: syncSubmissions wrote every new entry
	// into Postgres a minute after it was posted, and the Worker went on
	// serving the snapshot from the announce phase until the vote
	// transition pushed the finished list. Entrants watched a page that
	// said nothing had been entered, for the entire time entering was
	// open, which is the one phase where the page is meant to be filling
	// up in front of people.
	//
	// Unconditional rather than diffed. The reads behind the sync are
	// already one REST call per entry per minute; the push is one call
	// with a few KB in it, and a full replace landing after a missed one
	// is still correct with nothing to reconcile.
	if synced {
		if err := p.pushSnapshot(ctx, c); err != nil {
			p.log.Error("contest: push after sync", "contest", c.ID, "err", err)
		}
	}
	return nil
}

// dueForRefresh reports whether this contest's art is close enough to
// expiring to be worth re-deriving.
//
// It asks the URLs rather than the clock. Every signed Discord attachment
// link carries its own expiry, so the soonest one across the whole contest is
// the only deadline that matters, and refreshing is what happens when it
// comes within refreshMargin. A link with no readable expiry falls back to a
// fixed cadence, which is what this used to do for every link.
func (p *Plugin) dueForRefresh(ctx context.Context, c Contest) bool {
	p.mu.Lock()
	last := p.lastRefresh[c.ID]
	p.mu.Unlock()

	now := p.now()
	// However urgent the URLs look, never more than twice an hour: a refresh
	// is one REST call per entry and the tick runs every minute.
	if !last.IsZero() && now.Sub(last) < refreshFloor {
		return false
	}

	subs, err := p.store.Submissions(ctx, c.ID)
	if err != nil {
		// Fail toward refreshing, on the fallback cadence. The cost of being
		// wrong here is one extra pass over the forum; the cost the other way
		// is a gallery of broken images.
		p.log.Error("contest: read entries to check link expiry", "contest", c.ID, "err", err)
		return p.claimRefresh(c.ID, now, last, refreshFallback)
	}

	deadline, ok := soonestExpiry(subs)
	if !ok {
		return p.claimRefresh(c.ID, now, last, refreshFallback)
	}
	if now.Add(refreshMargin).Before(deadline) {
		return false
	}
	return p.claimRefresh(c.ID, now, last, 0)
}

// claimRefresh records that a refresh is happening now, or declines when the
// fallback cadence has not elapsed. Separate from the decision above so both
// paths mark the attempt exactly once.
func (p *Plugin) claimRefresh(contestID string, now, last time.Time, every time.Duration) bool {
	if every > 0 && !last.IsZero() && now.Sub(last) < every {
		return false
	}
	p.mu.Lock()
	p.lastRefresh[contestID] = now
	p.mu.Unlock()
	return true
}

// soonestExpiry is the earliest moment any of this contest's art stops
// resolving, and whether any of it said so at all.
func soonestExpiry(subs []Submission) (time.Time, bool) {
	var soonest time.Time
	for _, s := range subs {
		for _, u := range s.MediaURLs {
			at, ok := urlExpiry(u)
			if !ok {
				continue
			}
			if soonest.IsZero() || at.Before(soonest) {
				soonest = at
			}
		}
	}
	return soonest, !soonest.IsZero()
}

// urlExpiry reads the expiry Discord signs into an attachment URL.
//
// The ex parameter is hex unix seconds and is documented as "hex timestamp
// indicating when an attachment CDN URL will expire". Reading it is what lets
// this bot refresh before a link dies rather than on a guess about how long
// they last, and it keeps working if that lifetime ever changes.
//
// Anything unparseable is reported as "no expiry" rather than as expired: a
// URL merlin cannot read is not necessarily one that has died, and treating
// it as dead would re-read the forum on every tick forever.
func urlExpiry(raw string) (time.Time, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return time.Time{}, false
	}
	ex := u.Query().Get("ex")
	if ex == "" {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(ex, 16, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0).UTC(), true
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
		if err := p.setForumOpen(c, true); err != nil {
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
		if err := p.setForumOpen(c, false); err != nil {
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
		// Push, like every other terminal path. Without it the Worker goes
		// on serving phase "vote" forever, so the public gallery keeps
		// inviting votes on a contest that ended.
		p.pushBestEffort(ctx, c)
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
		// whoever posted first: the noVotes branch below is what makes that
		// true, and it used to fall straight through to announceWinners.
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
	if noVotes(results) {
		// Nobody voted, so there is no winner. results[0] here is whichever
		// entry sorted first on a random entry ID, and awardPrizes would DM
		// that person a sealed prize code and then wipe it: the one
		// irreversible thing this plugin does, decided by a coin flip. This
		// is the no-Worker case by definition and a real contest that
		// nobody turned out for in practice.
		p.announceNoVotes(ctx, c)
	} else {
		p.announceWinners(ctx, c, subs, results)
		p.awardPrizes(ctx, c, subs, results)
	}
	p.afterFinish(ctx, c)
	return nil
}

// noVotes reports that nothing was voted for at all. rank leaves every entry
// on zero when the Worker is not configured, and a genuine tally can come
// back empty too, so this is one check rather than two.
func noVotes(rs []resultView) bool {
	for _, r := range rs {
		if r.Votes > 0 {
			return false
		}
	}
	return true
}

// closedNeedingArt is the guild's most recent contest when it has finished
// but its gallery art has not yet been left to expire.
//
// Cancelled contests are excluded: nothing points at their gallery and
// nobody is coming back to it. A contest with no ClosedAt has not been
// through SetResults, so there is no window to measure from.
func (p *Plugin) closedNeedingArt(ctx context.Context, guildID string) (Contest, bool) {
	c, err := p.store.LatestContest(ctx, guildID)
	if err != nil {
		// A failed lookup is not a reason to decide the window is over: that
		// direction abandons a gallery for good, and the other costs one
		// tick. Same "only untrack on gone, never on failed" rule the sweeps
		// follow.
		if err != ErrNoLiveContest {
			p.log.Error("contest: look for a gallery to refresh", "guild", guildID, "err", err)
		}
		return Contest{}, false
	}
	if c.Phase != PhaseResults || c.ClosedAt == nil {
		return Contest{}, false
	}
	if p.now().Sub(*c.ClosedAt) >= artRefreshWindow {
		return Contest{}, false
	}
	return c, true
}

// refreshArt re-derives a finished contest's CDN links and pushes them.
//
// The entry list is frozen at this point, so this only ever updates the URL
// on entries that already exist; syncSubmissions skips its withdraw pass
// outside PhaseSubmit for exactly that reason.
func (p *Plugin) refreshArt(ctx context.Context, c Contest) error {
	if !p.dueForRefresh(ctx, c) {
		return nil
	}
	if err := p.syncSubmissions(ctx, c); err != nil {
		p.log.Error("contest: refresh finished gallery", "contest", c.ID, "err", err)
		return nil
	}
	p.pushBestEffort(ctx, c)
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
	// Approved only. A pledge nobody has ruled on must not reach the public
	// gallery, which is the whole point of the review queue: /contest prize
	// is TierPublic, so without this the page carries whatever any member
	// typed, under their own name.
	return p.worker.Push(ctx, p.snapshotOf(c, subs, approvedPrizes(prizes)))
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
		Quips:     p.quips(c.GuildID),
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

// quips picks a few of merlin's lines for the gallery ticker. Separate
// calls rather than one, because voice.Line guarantees no immediate repeat
// per guild and key, so asking three times is what spreads them out.
func (p *Plugin) quips(guildID string) []string {
	if p.speaker == nil {
		return nil
	}
	out := make([]string, 0, tickerQuips)
	seen := make(map[string]bool, tickerQuips)
	for i := 0; i < tickerQuips; i++ {
		line := p.speaker.Line(context.Background(), guildID, voice.KeyContestTicker, nil)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
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
