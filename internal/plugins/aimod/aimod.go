package aimod

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/voice"
)

// batchWindow is how long messages in one channel are collected before being
// classified together.
//
// The single biggest cost lever after the free rungs, and it was set too
// tight to work. A classifier prompt is a few hundred tokens of policy plus
// a few dozen of message, so the per-call overhead is most of the bill. At
// a second and a half a real server produced 1.1 messages per call: the
// policy prompt was being paid for almost every message individually, which
// is the shape of not batching at all.
//
// Six seconds is chosen against how conversation actually arrives. Traffic
// worth scanning is bursty (an argument produces a dozen messages in a
// minute, not one every six), and a window has to be wider than the gaps
// inside a burst to collapse one. Three messages per call is already 2.4x
// cheaper per message than one, and five is 3.3x.
//
// The cost is latency on the quiet case: a removal now lands up to six
// seconds after the message rather than two. That is still while the
// message is on screen, which is the property that matters, and a full
// batch flushes immediately regardless, so the busy case gets faster rather
// than slower.
const batchWindow = 6 * time.Second

// batchMax is the ceiling on one call. Past this the prompt starts costing
// more in latency than the batching saves, and a model's index-tracking gets
// less reliable the longer the list.
const batchMax = 20

// queueMax bounds a channel's pending batch during a raid. Past it, new
// messages are dropped from scanning rather than queued: the free rungs have
// already run on them, and an unbounded queue during a flood would spend the
// day's whole budget replaying a raid that was over minutes ago.
const queueMax = 200

// contextLines is how many preceding messages the deep pass sees.
//
// Enough for a threat to be distinguishable from a running joke, few enough
// that the deep prompt stays dominated by the policy text. These are read
// from Discord at escalation time rather than kept in memory, so this plugin
// holds no rolling copy of a server's conversation.
const contextLines = 5

// maxInFlight bounds concurrent model calls per guild.
//
// ponytail: one semaphore per guild, held in a map. A shared worker pool
// across all guilds would be the upgrade if this ever runs hot on a bot in
// many large servers; at that point the budget usually binds first.
const maxInFlight = 4

// maxConcurrentEscalations bounds the deep calls one batch runs at once.
//
// Four, matching maxInFlight rather than being unbounded, for two reasons.
// A batch of twenty could in principle flag twenty messages, and twenty
// simultaneous requests on one guild's API key is how a rate limit gets hit
// by the thing trying to avoid one. And the number that matters for the bug
// this fixes is "more than one": serial escalation meant a batch with six
// hits ran out of its own spawn context and lost the tail, and four at a time
// puts twenty hits comfortably inside it.
const maxConcurrentEscalations = 4

// DiscordOps is this plugin's narrow view of Discord, satisfied structurally
// by *discordguard.GuildOps, so every mutating call is gated by pause,
// dry-run, the per-guild rate cap and the action journal without this
// package knowing any of that exists.
type DiscordOps interface {
	ChannelMessageDelete(channelID, messageID string, options ...discordgo.RequestOption) error
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
	ChannelWebhooks(channelID string, options ...discordgo.RequestOption) ([]*discordgo.Webhook, error)
	WebhookCreate(channelID, name, avatar string, options ...discordgo.RequestOption) (*discordgo.Webhook, error)
	WebhookExecute(webhookID, token string, data *discordgo.WebhookParams, options ...discordgo.RequestOption) error
	GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error)
	GuildMemberTimeout(guildID, userID string, until *time.Time, options ...discordgo.RequestOption) error
	Guild(guildID string, options ...discordgo.RequestOption) (*discordgo.Guild, error)
	UserChannelCreate(recipientID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// OpsProvider yields the Discord view for one guild, bound explicitly for
// the same reason rotation.OpsProvider is.
type OpsProvider func(guildID string) DiscordOps

// PluginGate is the narrow slice of internal/settings.Store that answers
// whether this plugin is switched on for a guild.
//
// Needed here, rather than being covered by the router, because this plugin
// is the only one with an entry point that is not a command:
// core.CommandRouter checks the gate before every dispatch, but HandleMessage
// is called straight from the MessageCreate handler in cmd/bot/main.go and
// never goes near it.
//
// Without this check, /config plugins set aimod false disabled the commands
// and nothing else: messages kept being scanned, deleted and sanctioned,
// while the admin who had just tried to stop it could no longer run
// /aimod configure mode off, because that command was now gated. Turning a
// feature off has to turn it off, and the one case where it left the feature
// running is also the case where it took away the way to reach it.
type PluginGate interface {
	PluginEnabled(guildID, pluginName string) bool
}

// classifier is the seam the tests replace. The real one is *Client; a fake
// lets the whole ladder be exercised without an API key or a network.
type classifier interface {
	Chat(ctx context.Context, apiKey string, req chatRequest) (string, Usage, error)
}

// Plugin implements core.Plugin. See store.go for the package overview.
type Plugin struct {
	store    Store
	client   classifier
	ops      OpsProvider
	sealer   *sealer
	policies map[Bucket]Policy
	voice    voice.Source

	auditWriter core.AuditWriter
	commands    *core.CommandRouter
	// jailer is optional. Nil means no roles plugin is wired into this
	// build, and the sanction ladder falls back to Discord's own timeout.
	jailer Jailer
	// gate reports whether this plugin is enabled for a guild. Optional: a
	// nil gate means enabled, which is what the unit tests run with.
	gate PluginGate
	// privilege answers the one question this plugin asks about identity:
	// whether somebody is the bootstrap operator. Satisfied by
	// *core.Permissions, taken from Deps in Init.
	privilege PrivilegeChecker
	// sched carries the weekly calibration review. Optional: a nil scheduler
	// means no review job is ever registered, which is what the unit tests
	// and any build without one run as, and the plugin is otherwise
	// unaffected. See calibrate.go.
	sched core.Scheduler
	log   *slog.Logger
	now   func() time.Time

	// scanning is false when the operator did not enable the MESSAGE_CONTENT
	// intent. The plugin still registers its commands so /aimod status can
	// explain why nothing is being scanned, which is the whole point: the
	// failure this guards against is a silent one.
	scanning bool

	dedupe *dedupeCache
	// models caches OpenRouter's price list per guild, so autocomplete does
	// not make a network call per keystroke. See models.go.
	models *modelCache
	// meter is the per-member ceiling on how much of a guild's budget any
	// one person can consume. See abuse.go.
	meter     *userMeter
	webhookMu sync.Mutex
	webhooks  map[string]*discordgo.Webhook

	batchMu sync.Mutex
	batches map[string]*batch // channel ID -> pending batch

	inFlightMu sync.Mutex
	inFlight   map[string]chan struct{} // guild ID -> semaphore

	// budgetNoticeMu guards budgetNoticed, which keeps the
	// budget-exhausted audit entry to one per guild per day rather than one
	// per message, which is the difference between a useful warning and a
	// flooded audit channel.
	budgetNoticeMu sync.Mutex
	budgetNoticed  map[string]time.Time

	// calibrateMu guards calibrateRegistered, which mirrors rotation's
	// sweepRegistered: the Scheduler has no "is this job registered" query,
	// so reconcile keeps its own answer. Held across reconcile, so anything
	// it calls must not take it again.
	calibrateMu         sync.Mutex
	calibrateRegistered map[string]bool

	// eth reads the tip jar's wallet balance on whichever chain the guild's
	// address is on. Always non-nil: New defaults both chains, and
	// WithFundingChains overrides their endpoints and token addresses. See
	// funding.go.
	eth *ethClient

	// fundingMu guards fundingRegistered, mirroring calibrateMu above: the
	// Scheduler has no "is this job registered" query, so reconcile keeps its
	// own answer. Held across reconcile, so anything it calls must not take
	// it again.
	fundingMu         sync.Mutex
	fundingRegistered map[string]bool

	// fundingNoticed keeps the low-credit warning to one per guild per day,
	// exactly as budgetNoticed does for the exhausted-budget one. The poll
	// runs every fifteen minutes and the low state persists until somebody
	// tops up, so without this it would be the same warning four times an
	// hour until it stopped meaning anything.
	fundingNoticeMu sync.Mutex
	fundingNoticed  map[string]time.Time

	wg       sync.WaitGroup
	stopOnce sync.Once
	stopped  chan struct{}
}

// batch is one channel's pending messages plus the timer that will flush
// them.
type batch struct {
	guildID    string
	candidates []candidate
	timer      *time.Timer
}

// New constructs the plugin. store, client, ops and speaker are passed
// explicitly rather than through core.Deps, matching roles.New: none of them
// is a service every plugin needs.
func New(store Store, client *Client, ops OpsProvider, secretKey string, speaker voice.Source, scanning bool) (*Plugin, error) {
	seal, err := newSealer(secretKey)
	if err != nil {
		return nil, err
	}
	policies, err := LoadPolicies()
	if err != nil {
		return nil, err
	}
	return &Plugin{
		store:               store,
		client:              client,
		ops:                 ops,
		sealer:              seal,
		policies:            policies,
		voice:               speaker,
		scanning:            scanning,
		now:                 func() time.Time { return time.Now().UTC() },
		dedupe:              newDedupeCache(),
		meter:               newUserMeter(),
		models:              newModelCache(),
		webhooks:            make(map[string]*discordgo.Webhook),
		batches:             make(map[string]*batch),
		inFlight:            make(map[string]chan struct{}),
		budgetNoticed:       make(map[string]time.Time),
		calibrateRegistered: make(map[string]bool),
		fundingRegistered:   make(map[string]bool),
		fundingNoticed:      make(map[string]time.Time),
		eth:                 newETHClient(nil, nil),
		stopped:             make(chan struct{}),
	}, nil
}

// WithGate attaches the per-guild plugin enable/disable check. Wired in
// cmd/bot/main.go from internal/settings.Store, which satisfies it
// structurally, exactly as it satisfies core.PluginGate for the router.
func (p *Plugin) WithGate(g PluginGate) *Plugin {
	p.gate = g
	return p
}

// WithFundingChains points the tip jar's rails at different endpoints.
//
// An endpoint and its token address travel together on purpose: they are what
// a rail is, and an endpoint on one chain with a token address from another
// reports a zero balance rather than an error, which is the worst way for a
// misconfiguration to present. Wired in cmd/bot/main.go from the
// MERLIN_RPC_<CHAIN> and MERLIN_TOKEN_<CHAIN>_<ASSET> environment variables;
// anything unset leaves that rail on its compiled-in default.
func (p *Plugin) WithFundingChains(rpcURLs, contracts map[string]string) *Plugin {
	p.eth = newETHClient(rpcURLs, contracts)
	return p
}

// WithJailer attaches the jail mechanism the sanction ladder prefers.
//
// Wired once in cmd/bot/main.go from the roles plugin, which satisfies
// Jailer structurally, so this package never imports that one. The same
// optional-dependency shape as discordguard.Guard.WithJournal.
func (p *Plugin) WithJailer(j Jailer) *Plugin {
	p.jailer = j
	return p
}

func (p *Plugin) Name() string { return "aimod" }

func (p *Plugin) Init(deps core.Deps) error {
	p.auditWriter = deps.Audit
	p.log = deps.Logger
	p.commands = deps.Commands
	p.privilege = deps.Perms
	p.sched = deps.Scheduler

	p.registerCommands()
	return nil
}

func (p *Plugin) Start(ctx context.Context) error { return nil }

// Shutdown waits for in-flight classifications so a restart does not delete
// a message on the way out and lose the incident row that explains it.
func (p *Plugin) Shutdown(ctx context.Context) error {
	p.stopOnce.Do(func() { close(p.stopped) })

	// Pending batch timers are stopped, and whatever they were holding is
	// dropped rather than classified on the way out. Those messages were
	// never scanned, which is exactly the outcome of the bot having been
	// offline when they were posted, and it is the honest one: classifying
	// them now would mean deleting messages seconds after the process was
	// told to stop, against a context that is about to be cancelled.
	//
	// Work already in flight is a different matter and is waited for below,
	// because that work may have already deleted a message and still owe the
	// incident row that explains it.
	p.batchMu.Lock()
	for id, b := range p.batches {
		b.timer.Stop()
		delete(p.batches, id)
	}
	p.batchMu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// HandleChannelDeleted drops a deleted channel's cached webhook and any
// batch queued against it.
func (p *Plugin) HandleChannelDeleted(channelID string) {
	p.forgetWebhook(channelID)
	p.batchMu.Lock()
	if b, ok := p.batches[channelID]; ok {
		b.timer.Stop()
		delete(p.batches, channelID)
	}
	p.batchMu.Unlock()
}

// ForgetGuild drops per-process state after the bot leaves a guild. Nothing
// in Postgres is touched: being removed is often temporary, and a guild's
// configured key, budget and policy must survive a kick and re-invite, the
// same reasoning as roles.ForgetGuild.
func (p *Plugin) ForgetGuild(guildID string) {
	p.inFlightMu.Lock()
	delete(p.inFlight, guildID)
	p.inFlightMu.Unlock()

	p.budgetNoticeMu.Lock()
	delete(p.budgetNoticed, guildID)
	p.budgetNoticeMu.Unlock()

	p.fundingNoticeMu.Lock()
	delete(p.fundingNoticed, guildID)
	p.fundingNoticeMu.Unlock()

	// The balance poll is stopped, unlike the row behind it, which stays in
	// Postgres so a kick and re-invite keeps the guild's jar. Leaving the job
	// registered would keep making an outbound RPC call every fifteen minutes
	// for a server this bot can no longer see; SyncGuild registers it again
	// on the next GuildCreate.
	p.reconcileFundingJob(guildID, false)

	p.meter.forgetGuild(guildID)
	p.models.forgetGuild(guildID)

	// Cached webhooks are keyed by channel, and the channels of a guild the
	// bot has left are not otherwise reachable: HandleChannelDeleted only
	// fires for a channel deleted while the bot can still see it. Without
	// this they sit in the map for the life of the process.
	p.webhookMu.Lock()
	clear(p.webhooks)
	p.webhookMu.Unlock()

	// The config cache, if one is wrapped around the store. Not required for
	// correctness (the TTL expires it), but a guild the bot has left should
	// not be holding a copy of its API key configuration in memory.
	if f, ok := p.store.(interface{ Forget(string) }); ok {
		f.Forget(guildID)
	}
}

// HandleMessage is the gateway entry point, called from the MessageCreate
// handler in cmd/bot/main.go. It returns immediately: everything past the
// free rungs happens on the batch timer, because discordgo dispatches events
// serially per shard and blocking here would stall every other handler.
// HandleMessageEdit is the MessageUpdate entry point.
//
// Scanning edits is what closes the plugin's most obvious bypass: post
// something innocuous, let it be cleared, then edit it into a violation. The
// classification path is identical, so this is a guard plus a call.
//
// The guard matters more than it looks. MessageUpdate also fires when Discord
// resolves a link preview or pins a message, with the content untouched, and
// those are common enough that scanning them would raise traffic
// substantially for nothing. EditedTimestamp is nil on exactly those and set
// on a real edit.
//
// The payload is partial in a way MessageCreate's never is: Author can be
// nil. shouldSkip returns skipBot for that, so an edit whose author Discord
// did not send is dropped rather than misattributed, which is the right
// direction and is worth not rediscovering.
//
// Cost is bounded by dedupeCache: an edit that changes nothing this plugin
// cares about (whitespace, a typo fixed) hashes to a key that may already
// carry a verdict, and re-editing the same text repeatedly is free.
func (p *Plugin) HandleMessageEdit(m *discordgo.Message) {
	if m == nil || m.EditedTimestamp == nil {
		return
	}
	p.HandleMessage(m)
}

func (p *Plugin) HandleMessage(m *discordgo.Message) {
	if !p.scanning || m == nil || m.GuildID == "" {
		return
	}
	// The checks that need no configuration run first, ahead of the store
	// read, because this is the hot path: every message in every guild
	// arrives here. Bots and webhooks are a large share of traffic on a busy
	// server, and none of them should cost a database round trip to dismiss.
	//
	// The webhook case is also the loop guard (see shouldSkip), so keeping
	// it ahead of everything is worth doing on its own merits.
	if m.WebhookID != "" || m.Author == nil || m.Author.Bot {
		return
	}
	// The gate the router applies to every command, applied to the one entry
	// point that is not one. Reads the same in-memory settings cache, so it
	// costs nothing here. See PluginGate.
	if p.gate != nil && !p.gate.PluginEnabled(m.GuildID, p.Name()) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := p.store.Config(ctx, m.GuildID)
	if err != nil {
		p.log.Error("aimod: load config", "guild", m.GuildID, "err", err)
		return
	}
	if reason := p.shouldSkip(cfg, m, p.now()); reason != skipNone {
		return
	}

	// Resolved once here and carried on the candidate, so every rung below
	// judges the same text. For a forward that text lives in a snapshot
	// rather than in Content; see messageText.
	text, forwarded := messageText(m)
	c := candidate{
		MessageID: m.ID, ChannelID: m.ChannelID, AuthorID: m.Author.ID,
		Content: text, Forwarded: forwarded, ReplyTo: replyContext(m),
	}

	// Rung 1, before anything is queued or paid for. A hard hit is acted on
	// with the bucket's own configured action and no model in the loop, so
	// a token leak or a phishing link is gone in well under a second even on
	// a guild whose budget is spent.
	if bucket, reason, rewrite, hit := hardHit(c.Content); hit {
		action := EffectiveAction(cfg.BucketActions, bucket)
		if action == ActionOff {
			if rewrite == "" {
				return
			}
			// A hard slur is acted on even where hate_speech is off,
			// because off is the default nobody chose and these are the
			// words that get a server terminated with no reading of the
			// sentence required. A guild that deliberately set flag, remove
			// or rewrite still gets exactly what it asked for.
			action = ActionRewrite
		}
		// Never a rewrite with nothing to clean: the patterns above match a
		// credential or a link, and a "cleaned" version of a phishing
		// message is still a phishing message with a hole in it.
		if action == ActionRewrite && rewrite == "" {
			action = ActionRemove
		}
		p.spawn(func(bg context.Context) {
			p.enforce(bg, cfg, c, bucket, action, deepVerdict{
				Violation: true, Bucket: bucket, Confidence: 1,
				Reason: reason, Rewrite: rewrite,
			})
		})
		return
	}

	if len(enforcedBuckets(cfg)) == 0 {
		return
	}

	// A verdict already reached on this exact text is applied again, to this
	// message, for nothing. That is what lets the filter keep up with a flood
	// without a budget to match it: fifty copies of one line cost one
	// classification and produce fifty enforcements, and none of them draw on
	// the member's scan ceiling, which stays for content nobody has judged
	// yet. The bucket's action is re-read from live config rather than taken
	// from the cache, so a policy changed since the verdict still governs.
	if e, ok := p.dedupe.lookup(cfg.GuildID, c.Content, p.now()); ok && e.verdict != nil {
		action := EffectiveAction(cfg.BucketActions, e.verdict.bucket)
		if action == ActionOff {
			return
		}
		p.spawn(func(bg context.Context) {
			p.enforce(bg, cfg, c, e.verdict.bucket, action, e.verdict.deep)
		})
		return
	}
	// The per-member ceiling sits here rather than in shouldSkip, so it
	// gates only the paid rungs: a member who has spent their scan quota is
	// still matched against the free patterns above, which is what stops the
	// meter itself becoming a way to buy immunity by flooding first.
	if !p.meter.allowScan(cfg.GuildID, c.AuthorID, p.now()) {
		return
	}
	p.queue(cfg.GuildID, c)
}

// queue adds a message to its channel's pending batch, starting the flush
// timer if this is the first one.
func (p *Plugin) queue(guildID string, c candidate) {
	p.batchMu.Lock()
	defer p.batchMu.Unlock()

	b, ok := p.batches[c.ChannelID]
	if !ok {
		b = &batch{guildID: guildID}
		channelID := c.ChannelID
		b.timer = time.AfterFunc(batchWindow, func() { p.flush(channelID) })
		p.batches[c.ChannelID] = b
	}
	if len(b.candidates) >= queueMax {
		return
	}
	b.candidates = append(b.candidates, c)

	// Full batches go immediately rather than waiting out the window: a busy
	// channel gets both the cost saving and the lower latency.
	//
	// classify is called directly rather than on a bare goroutine, and that
	// matters: it only acquires a slot (non-blocking) and hands the actual
	// work to spawn, so it returns at once and the work it starts is tracked
	// by p.wg. A `go classify(...)` here would return before spawn had
	// registered anything, leaving Shutdown free to return while a removal
	// was still in flight, which is precisely the case Shutdown exists for.
	if len(b.candidates) >= batchMax {
		b.timer.Stop()
		delete(p.batches, c.ChannelID)
		p.classify(guildID, b.candidates)
	}
}

func (p *Plugin) flush(channelID string) {
	p.batchMu.Lock()
	b, ok := p.batches[channelID]
	if ok {
		delete(p.batches, channelID)
	}
	p.batchMu.Unlock()
	if !ok || len(b.candidates) == 0 {
		return
	}
	p.classify(b.guildID, b.candidates)
}

// spawn runs fn on a tracked goroutine so Shutdown can wait for it.
func (p *Plugin) spawn(fn func(context.Context)) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		fn(ctx)
	}()
}

// acquire takes one of a guild's in-flight slots, or reports that the guild
// is already at its ceiling.
func (p *Plugin) acquire(guildID string) (release func(), ok bool) {
	p.inFlightMu.Lock()
	sem, exists := p.inFlight[guildID]
	if !exists {
		sem = make(chan struct{}, maxInFlight)
		p.inFlight[guildID] = sem
	}
	p.inFlightMu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
		return nil, false
	}
}

// classify runs the paid rungs for one batch.
func (p *Plugin) classify(guildID string, batch []candidate) {
	release, ok := p.acquire(guildID)
	if !ok {
		p.log.Warn("aimod: at the concurrent-scan ceiling, batch not classified",
			"guild", guildID, "messages", len(batch))
		return
	}

	p.spawn(func(ctx context.Context) {
		defer release()

		cfg, err := p.store.Config(ctx, guildID)
		if err != nil {
			p.log.Error("aimod: load config", "guild", guildID, "err", err)
			return
		}
		state, err := p.checkBudget(ctx, cfg)
		if err != nil {
			p.log.Error("aimod: budget check", "guild", guildID, "err", err)
			return
		}
		if state.Exhausted {
			p.noticeBudget(ctx, guildID, state)
			return
		}

		hits, usage, err := p.classifyFast(ctx, state, cfg, batch)
		// Booked before the error is handled: a call that returned usage was
		// billed whether or not its body parsed.
		//
		// The scanned count rides the same condition, and that is the point
		// of moving it here from above the call. Scanned is the denominator
		// every cost-per-message figure in /aimod models show divides the
		// booked tokens by, so the two have to agree on which batches count.
		// Counted unconditionally, a batch that failed before reaching a
		// model (a 429, an outage) added twenty to the denominator and
		// nothing to the numerator, reporting the guild's scanning as cheaper
		// than it is, and worst exactly when the model was failing most.
		// Counted only on success, a billed-but-unparseable call would add
		// tokens with no messages under them, which is the same error the
		// other way and is what the budget tests pin down.
		if usage.Cost > 0 || usage.TotalTokens > 0 {
			p.recordUsage(ctx, guildID, usage, false)
			if err := p.store.AddScanned(ctx, guildID, today(p.now()), len(batch)); err != nil {
				p.log.Error("aimod: count scanned", "guild", guildID, "err", err)
			}
		}
		if err != nil {
			p.log.Error("aimod: fast pass", "guild", guildID, "messages", len(batch), "err", err)
			return
		}

		// Only what the fast pass cleared is remembered, so an identical
		// repeat of a *flagged* message is scanned and flagged again rather
		// than silently swallowed by the cache. See dedupeCache.markClean.
		flagged := make(map[int]bool, len(hits))
		for _, hit := range hits {
			flagged[hit.Index] = true
		}
		for i, c := range batch {
			if !flagged[i+1] {
				p.dedupe.markClean(guildID, c.Content, p.now())
			}
		}

		// Concurrently, bounded, and that is a correctness fix rather than a
		// speed one. Serially, each escalation is a deep call bounded by the
		// client's 20 second timeout inside this spawn's 2 minute context, so
		// six flagged messages in one batch was exactly the budget: past that
		// the tail was cancelled with no verdict, no incident row and no
		// audit line saying it had happened. batchMax is 20, and six hits in
		// twenty messages is a heated argument, not a raid.
		//
		// It also fixes the latency the batching design was justified on: the
		// last flagged message in a batch of five used to stay live for the
		// better part of two minutes, against a promise that removals land
		// while the message is still on screen.
		//
		// Nothing escalate touches is unguarded (dedupe, meter and webhooks
		// all hold their own mutexes), and maxInFlight still bounds a guild's
		// total concurrent model calls, so this removes a cliff rather than
		// widening the guild's cost or rate-limit exposure.
		var wg sync.WaitGroup
		slots := make(chan struct{}, maxConcurrentEscalations)
		for _, hit := range hits {
			select {
			case <-p.stopped:
				// Checked per hit, as before: a shutdown mid-batch stops
				// starting new work, and the wait below still lets what is
				// already running finish recording what it did.
				wg.Wait()
				return
			default:
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				p.escalate(ctx, cfg, state, batch[hit.Index-1], hit)
			}()
		}
		wg.Wait()
	})
}

// escalate runs rung 3 on one flagged message and acts on the result.
//
// The rule this function exists to enforce: a fast-pass hit on its own can
// only ever flag. Nothing is deleted or rewritten without the deep tier
// having read the message against the full policy text and returned above
// actThreshold. A cheap model is being used to decide what is worth looking
// at, never what is worth acting on.
func (p *Plugin) escalate(ctx context.Context, cfg Config, state budgetState, c candidate, hit Verdict) {
	action := EffectiveAction(cfg.BucketActions, hit.Bucket)
	if action == ActionOff {
		return
	}

	if action == ActionFlag || cfg.Mode == ModeFlag {
		// Nothing is going to be touched, so there is nothing the deep pass
		// could change and no reason to pay for it. The recorded confidence
		// is the fast tier's own, and /aimod why says so.
		p.enforce(ctx, cfg, c, hit.Bucket, ActionFlag, deepVerdict{
			Violation: true, Bucket: hit.Bucket, Confidence: hit.Confidence,
			Reason: "flagged by the first-pass filter, not confirmed",
		})
		return
	}

	allowed, justCrossed := p.meter.allowDeep(cfg.GuildID, c.AuthorID, p.now())
	if !allowed {
		// Never a silent drop: the flag is still recorded, so a real
		// violation buried in a flood is still visible to a moderator even
		// though nothing was paid to confirm it.
		p.enforce(ctx, cfg, c, hit.Bucket, ActionFlag, deepVerdict{
			Violation: true, Bucket: hit.Bucket, Confidence: hit.Confidence,
			Reason: "flagged by the first-pass filter, not confirmed: this member is over their scan ceiling",
		})
		if justCrossed {
			p.handleAbuse(ctx, cfg, c)
		}
		return
	}

	priorLines, self := p.recentContext(cfg.GuildID, c)
	v, usage, err := p.classifyDeep(ctx, state, cfg, hit.Bucket, c,
		priorLines, self, action == ActionRewrite)
	if usage.Cost > 0 || usage.TotalTokens > 0 {
		p.recordUsage(ctx, cfg.GuildID, usage, true)
	}
	// One retry on the other gateway, for the deep rung only.
	//
	// This is the rung whose verdict can delete or rewrite, and its default
	// gateway is a free tier: rate limited on a bucket shared with every
	// other call this workspace makes, and with no way to require that the
	// endpoint answering honours the JSON schema it was handed. Both of
	// those arrive here as an error, and an unparseable answer is one of
	// them, so no attempt is made to tell them apart. Nothing is acted on
	// either way, which is the safe direction and also the useless one: a
	// message that tripped the first pass goes unjudged.
	//
	// Deliberately not extended to the fast rung. That rung can only flag,
	// everything it flags is re-read here, and re-running a twenty message
	// batch at OpenRouter prices is the bill this gateway exists to avoid.
	if err != nil && state.FallbackKey != "" && worthFallback(err) {
		p.log.Info("aimod: deep pass falling back",
			"guild", cfg.GuildID, "from", providerName(state.Spec), "err", err)
		fb := state
		fb.APIKey, fb.Spec = state.FallbackKey, state.fallback()
		v, usage, err = p.classifyDeep(ctx, fb, cfg, hit.Bucket, c,
			priorLines, self, action == ActionRewrite)
		if usage.Cost > 0 || usage.TotalTokens > 0 {
			p.recordUsage(ctx, cfg.GuildID, usage, true)
		}
	}
	if err != nil {
		// No action. A deep pass that failed is not a verdict, and treating
		// an unreachable model as a confirmation would let an outage delete
		// messages.
		p.log.Error("aimod: deep pass", "guild", cfg.GuildID, "message", c.MessageID, "err", err)
		return
	}
	if !v.Violation || v.Confidence < actThreshold {
		// Cleared by the deep pass, so identical text is genuinely clean and
		// later copies should not pay to be told so a second time.
		p.dedupe.markCleanFor(cfg.GuildID, c.AuthorID, c.Content, p.now())
		return
	}
	// Remembered before acting, so a flood arriving while this one is still
	// in flight finds the verdict rather than queueing more classifications.
	p.dedupe.remember(cfg.GuildID, c.Content, p.now(), &cachedVerdict{
		bucket: hit.Bucket, action: action, deep: v,
	})
	p.enforce(ctx, cfg, c, hit.Bucket, action, v)
}

// recentContext reads the handful of messages before this one, each labelled
// with who said it, and returns the label the candidate's own author got.
//
// Read at escalation time, from Discord, for about one message in a hundred,
// rather than kept in a rolling in-memory buffer of every channel. That
// choice costs one REST call on the rare path and saves holding a copy of
// the server's conversation in this process, which is the trade this
// plugin's privacy property asks for everywhere else too.
//
// The labels are the context half of accuracy. Bare lines with the speakers
// stripped read as one undifferentiated blob, and most of what context is
// for cannot be answered from that: whether this is two people trading
// insults or one person escalating at a third, whether the flagged author is
// answering the person they are talking about, and whether a slur is being
// quoted back at whoever just used it. hate_speech's reclaimed-language
// exception in particular is a question about the speaker, and the model was
// being asked to apply it with every speaker erased.
//
// Letters rather than usernames or IDs, on purpose. The model needs identity
// relations, not identities: "A said this, B replied" carries the whole
// signal. A username would put a member's chosen name in a third party's
// prompt for no gain, and would hand anyone who wanted it a second injection
// surface next to the message content we are already forced to send.
//
// Failure is not an error: the deep pass works without context, just less
// well, and refusing to judge because the history was unreadable would turn
// a permissions gap into a filter that never acts.
func (p *Plugin) recentContext(guildID string, c candidate) (lines []string, self string) {
	labels := newSpeakerLabels()
	// The candidate's author is labelled first so it is always "A", whether
	// or not they also wrote any of the preceding lines.
	self = labels.of(c.AuthorID)

	msgs, err := p.ops(guildID).ChannelMessages(c.ChannelID, contextLines, c.MessageID, "", "")
	if err != nil {
		return nil, self
	}
	lines = make([]string, 0, len(msgs))
	// Discord returns newest-first; reverse so the model reads them in the
	// order they were said.
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i] == nil || msgs[i].Content == "" || msgs[i].Author == nil {
			continue
		}
		lines = append(lines, labels.of(msgs[i].Author.ID)+": "+msgs[i].Content)
	}
	return lines, self
}

// speakerLabels hands out a short stable letter per author within one
// prompt. Shared by recentContext and the calibration review, which both need
// the same thing: who said what, without saying who anybody is.
type speakerLabels struct{ seen map[string]string }

func newSpeakerLabels() *speakerLabels { return &speakerLabels{seen: map[string]string{}} }

func (l *speakerLabels) of(authorID string) string {
	if s, ok := l.seen[authorID]; ok {
		return s
	}
	// Wraps past 26 distinct speakers, which cannot happen inside the five
	// lines recentContext asks for and only costs a merged label in a long
	// calibration transcript. Not worth a second character everywhere else.
	s := string(rune('A' + len(l.seen)%26))
	l.seen[authorID] = s
	return s
}

// noticeBudget records that a guild stopped scanning, once per day.
func (p *Plugin) noticeBudget(ctx context.Context, guildID string, state budgetState) {
	day := today(p.now())
	p.budgetNoticeMu.Lock()
	last, seen := p.budgetNoticed[guildID]
	if seen && !last.Before(day) {
		p.budgetNoticeMu.Unlock()
		return
	}
	p.budgetNoticed[guildID] = day
	p.budgetNoticeMu.Unlock()

	detail := "no OpenRouter key is configured"
	if state.Budget > 0 && state.Spent > 0 {
		detail = "spent " + formatUSD(state.Spent) + " of the " + formatUSD(state.Budget) + " daily budget"
	}
	if err := p.auditWriter.Record(ctx, guildID, core.ActorSystem, "aimod.budget_exhausted", "",
		detail+". Pattern-based checks still run; nothing is being sent to a model until the budget resets at midnight UTC."); err != nil {
		p.log.Error("aimod: audit budget notice", "guild", guildID, "err", err)
	}
}
