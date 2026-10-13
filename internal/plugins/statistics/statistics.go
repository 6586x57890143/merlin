// Package statistics counts what happens in a server, by the hour, and
// answers questions about it: who was talking in this window and how much,
// which channels carry the traffic, whether the server is growing.
//
// The counting is done as messages arrive on the gateway (counter.go), into
// per-hour buckets in Postgres (store.go), so a report over any window is a
// single query rather than a half-hour walk of Discord's history. That walk
// still exists as a backfill (backfill.go) for the time before the bot was
// counting, run as a Scheduler job so a deploy pauses it instead of losing
// it.
//
// What is kept is metadata, never content: message counts per member per
// channel per hour, seconds in voice per member per channel per hour (booked
// from the gateway's voice state changes, since Discord keeps no voice
// history to read back), joins and departures per hour, and the display name
// and avatar a member last posted under. It is still a durable record of who
// was talking where, which is why every row ages out on a per-guild
// retention and why the one command that names people is gated the way it
// was before: TierAdmin is only the coarse floor on the leaf, the real
// check is core.Permissions.IsBootstrapAdmin in the handler, because a
// guild with five admins would otherwise have five accounts able to profile
// every member on demand, and "who spoke when" is exactly the shape of
// question a weaponized reporting campaign wants answered. Same reasoning,
// and the same shape, as /aimod moderate-user and the tip jar's address.
//
// Counting needs the unprivileged GUILD_MESSAGES intent, which
// core.GatewayIntents always requests. Without MESSAGE_CONTENT the events
// carry no text, and nothing here reads any.
package statistics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/omit"
	snow "github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
)

// Attachment names, referenced from the embed by Discord's attachment://
// scheme. The image is shown inline; the list rides along as a file only when
// it does not fit in the embed.
const (
	imageAttachmentName = "activity.png"
	listAttachmentName  = "activity.md"
	imageAttachmentURL  = "attachment://" + imageAttachmentName
)

const (
	defaultTop = 24
	maxTop     = 60
	// pruneEvery is how often expired buckets are dropped. Retention is in
	// days, so an hour is already far finer than anybody can observe.
	pruneEvery = time.Hour
	// maxRetentionDays keeps "forever" off the table: the point of a
	// retention is that this record ends.
	maxRetentionDays = 3650
)

// PrivilegeChecker answers the one question this plugin asks about identity.
// Satisfied by *core.Permissions, taken from Deps in Init.
type PrivilegeChecker interface {
	IsBootstrapAdmin(userID string) bool
}

type Plugin struct {
	commands  *core.CommandRouter
	guilds    guildReader
	source    messageSource
	store     Store
	privilege PrivilegeChecker
	sched     core.Scheduler
	client    *http.Client
	log       *slog.Logger
	now       func() time.Time
	// channelName resolves a channel's current name from the gateway cache,
	// for the live counter; nil in tests. A function rather than an
	// interface for the same reason roles.voiceChannelOf is: it is the one
	// piece of the gateway cache this plugin reads.
	channelName func(guildID, channelID string) string
	// gate is the per-guild plugin switch. The router consults it before
	// any command; the gateway handlers consult it here, so a guild that
	// turned this plugin off is not being counted behind its back.
	gate core.PluginGate

	count *counter

	mu                 sync.Mutex
	backfillRegistered map[string]bool

	// base outlives any one interaction and is cancelled at Shutdown; the
	// flush and prune loops run on it.
	base context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup
}

// New constructs the plugin. store is passed directly rather than through
// core.Deps, the same way rotation and roles take theirs.
func New(store Store, gate core.PluginGate, channelName func(guildID, channelID string) string) *Plugin {
	base, stop := context.WithCancel(context.Background())
	return &Plugin{
		store:              store,
		gate:               gate,
		channelName:        channelName,
		client:             &http.Client{Timeout: fetchTTL},
		log:                slog.New(slog.DiscardHandler),
		now:                func() time.Time { return time.Now().UTC() },
		count:              newCounter(),
		backfillRegistered: map[string]bool{},
		base:               base,
		stop:               stop,
	}
}

func (p *Plugin) Name() string { return "statistics" }

// guildReader is the slice of disgo's REST client guildName uses.
type guildReader interface {
	GetGuild(guildID snow.ID, withCounts bool, opts ...rest.RequestOpt) (*discord.RestGuild, error)
}

func (p *Plugin) Init(deps core.Deps) error {
	if deps.Rest != nil {
		p.guilds = deps.Rest
		p.source = restSource{deps.Rest}
	}
	p.privilege = deps.Perms
	p.sched = deps.Scheduler
	if deps.Logger != nil {
		p.log = deps.Logger
	}

	deps.Commands.RegisterCommand(p.Name(), command())
	// TierAdmin is the floor, not the gate, on the two leaves that name
	// people: handleReport and handleBackfill refuse anybody but the
	// bootstrap operator. Both are needed, since a guild can lower a tier
	// with /config permissions set-tier but cannot widen this.
	deps.Commands.HandleIx("statistics", "report", core.PermSpec{Tier: core.TierAdmin, Action: "statistics.report"}, p.handleReport)
	deps.Commands.HandleIx("statistics", "backfill", core.PermSpec{Tier: core.TierAdmin, Action: "statistics.backfill"}, p.handleBackfill)
	deps.Commands.HandleIx("statistics", "channels", core.PermSpec{Tier: core.TierAdmin, Action: "statistics.view"}, p.handleChannels)
	deps.Commands.HandleIx("statistics", "members", core.PermSpec{Tier: core.TierAdmin, Action: "statistics.view"}, p.handleMembers)
	deps.Commands.HandleIx("statistics", "status", core.PermSpec{Tier: core.TierAdmin, Action: "statistics.view"}, p.handleStatus)
	p.registerPanel(deps.Commands)
	return nil
}

// command is the /statistics definition, kept out of Init so the permission
// decision in it can be pinned by a test.
func command() discord.SlashCommandCreate {
	window := []discord.ApplicationCommandOption{
		discord.ApplicationCommandOptionString{
			Name:        "from",
			Description: "Window start, UTC: 2026-09-01, 2026-09-01 14:00, or an RFC3339 timestamp",
			Required:    true,
		},
		discord.ApplicationCommandOptionString{
			Name:        "to",
			Description: "Window end, same formats. Defaults to now",
		},
	}
	return discord.SlashCommandCreate{
		Name:        "statistics",
		Description: "Activity and membership statistics for this server",
		// Discord's own default_member_permissions is left unset everywhere else
		// in this bot (spec.MD §4a), so the internal checks are the sole gate and
		// cannot be bypassed by a mismatched permission bit. That reasoning is
		// about a command being *reachable*; this one is about it being *listed*.
		//
		// Every registered command shows in every member's picker regardless of
		// who may run it, so without this the whole server sees that somebody can
		// ask merlin who was talking and when. That is a fact about the server's
		// surveillance surface, published to the people it is about, for a command
		// none of them can run. Zero means nobody but a holder of Discord's
		// Administrator bit, which is a floor under the operator check rather than
		// a replacement for it: the handlers still refuse everybody but the
		// bootstrap identity where it matters, so this cannot widen anything.
		//
		// It can narrow, though, and that is the trade. An operator who is not an
		// administrator of the guild loses the command from their own picker; the
		// way back is the guild's Integrations settings, where an owner can grant
		// an explicit overwrite, which is exactly what a zero here leaves room for.
		DefaultMemberPermissions: omit.NewPtr(discord.Permissions(0)),
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionSubCommand{
				Name:        "report",
				Description: "Who was talking in a window of time",
				Options: append(append([]discord.ApplicationCommandOption{}, window...),
					discord.ApplicationCommandOptionChannel{
						// A native Channel option rather than a raw ID string
						// (spec.MD §4a), which also gets the picker for free.
						Name:        "channel",
						Description: "Only look at this channel",
						ChannelTypes: []discord.ChannelType{
							discord.ChannelTypeGuildText,
							discord.ChannelTypeGuildNews,
							discord.ChannelTypeGuildPublicThread,
							discord.ChannelTypeGuildPrivateThread,
						},
					},
					discord.ApplicationCommandOptionInt{
						Name:        "top",
						Description: fmt.Sprintf("How many people to show, 1 to %d. Default %d", maxTop, defaultTop),
						MinValue:    omit.Ptr(1),
						MaxValue:    omit.Ptr(maxTop),
					},
					discord.ApplicationCommandOptionBool{
						Name:        "share",
						Description: "Post the report in this channel instead of answering only you",
					},
					discord.ApplicationCommandOptionBool{
						Name:        "show-channels",
						Description: "Name the channels each person posted or sat in. Off by default, so private channels stay off it",
					},
				),
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "channels",
				Description: "Which channels carried the traffic in a window of time",
				Options:     window,
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "members",
				Description: "Joins and departures over a window of time",
				Options:     window,
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "backfill",
				Description: "Count history from before merlin was counting, from this date",
				Options:     window[:1],
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "status",
				Description: "What is being counted, since when, and for how long it is kept (and change that)",
			},
		},
	}
}

// Start runs the flush and prune loops for the life of the process.
func (p *Plugin) Start(context.Context) error {
	p.wg.Add(2)
	go func() { defer p.wg.Done(); p.flushLoop(p.base) }()
	go func() { defer p.wg.Done(); p.pruneLoop(p.base) }()
	return nil
}

// Shutdown stops the loops and waits for the final flush, so a restart
// loses no counts.
func (p *Plugin) Shutdown(ctx context.Context) error {
	p.stop()
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// SyncGuild records when counting began for a guild (once, ever) and
// reconciles its backfill job. Called from the GuildCreate handler.
func (p *Plugin) SyncGuild(ctx context.Context, guildID string) {
	if err := p.store.MarkLive(ctx, guildID, p.now()); err != nil {
		p.log.Error("statistics: mark live", "guild", guildID, "err", err)
	}
	p.reconcileBackfillJob(ctx, guildID)
}

// ForgetGuild drops the backfill registration bookkeeping after the bot has
// been removed; the Scheduler has already dropped the job itself. Buckets
// stay: they age out on retention like everything else here.
func (p *Plugin) ForgetGuild(guildID string) {
	p.mu.Lock()
	delete(p.backfillRegistered, guildID)
	p.mu.Unlock()
}

func (p *Plugin) pruneLoop(ctx context.Context) {
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		opCtx, cancel := context.WithTimeout(ctx, flushTimeout)
		n, err := p.store.Prune(opCtx, p.now())
		cancel()
		if err != nil {
			p.log.Error("statistics: prune", "err", err)
		} else if n > 0 {
			p.log.Info("statistics: pruned expired buckets", "rows", n)
		}
	}
}

// operator reports whether userID is the bootstrap identity.
//
// Fails closed in every direction: an unknown actor is refused, and a nil
// checker loses the escape hatch rather than granting it to everybody. There
// is deliberately no widening branch here at all.
// enabled reports whether the guild has this plugin switched on. No gate
// (tests) means on.
func (p *Plugin) enabled(guildID string) bool {
	return p.gate == nil || p.gate.PluginEnabled(guildID, p.Name())
}

func (p *Plugin) operator(userID string) bool {
	return userID != "" && p.privilege != nil && p.privilege.IsBootstrapAdmin(userID)
}

// options are the parsed arguments, validated before anything is deferred so
// a typo comes back in three seconds rather than after a query.
type options struct {
	from, to  time.Time
	channelID string
	top       int
	share     bool
	// showChannels names where each person was. Off by default: the names
	// come from every channel merlin can read, ticket and mod channels
	// included, and a shared report would publish them.
	showChannels bool
}

func parseOptions(args map[string]discord.SlashCommandOption, now time.Time) (options, error) {
	opts := options{to: now, top: defaultTop}

	arg, ok := args["from"]
	if !ok {
		return opts, errors.New("i need a window start")
	}
	from, err := parseWhen(arg.String())
	if err != nil {
		return opts, err
	}
	opts.from = from

	if arg, ok := args["to"]; ok {
		to, err := parseWhen(arg.String())
		if err != nil {
			return opts, err
		}
		opts.to = to
	}
	if !opts.to.After(opts.from) {
		return opts, errors.New("the window ends before it starts")
	}
	if opts.from.After(now) {
		return opts, errors.New("that window has not happened yet")
	}
	if arg, ok := args["channel"]; ok {
		opts.channelID = arg.Snowflake().String()
	}
	if arg, ok := args["top"]; ok {
		opts.top = min(maxTop, max(1, arg.Int()))
	}
	if arg, ok := args["share"]; ok {
		opts.share = arg.Bool()
	}
	if arg, ok := args["show-channels"]; ok {
		opts.showChannels = arg.Bool()
	}
	return opts, nil
}

func (p *Plugin) refuseUnlessOperator(ix *core.Ix) bool {
	if p.operator(ix.UserID) {
		return true
	}
	ix.RespondWarn("Not yours to run",
		"This one is the operator's alone. It answers who was talking and when, for every member of the server, "+
			"which is not a question admin should be able to ask on its own.")
	return false
}

func (p *Plugin) handleReport(ctx context.Context, ix *core.Ix) {
	if !p.refuseUnlessOperator(ix) {
		return
	}
	opts, err := parseOptions(ix.Args(), p.now())
	if err != nil {
		ix.RespondErr("That window doesn't work", err)
		return
	}
	// The query is quick; the png is not always, and the avatar fetches
	// behind it can take longer than Discord's three seconds. Deferring
	// publicly when sharing is not optional: ephemerality is fixed at
	// acknowledgement, so a private defer cannot be edited into a
	// channel-visible answer later.
	ack := ix.Defer
	if opts.share {
		ack = ix.DeferPublic
	}
	if err := ack(); err != nil {
		return
	}

	rep, err := p.build(ctx, ix.GuildID, opts)
	if err != nil {
		_ = ix.FollowUpErr("Could not read the statistics", err)
		return
	}
	guild := p.guildName(ix.GuildID)
	shown := markdown(rep, guild, opts.from, opts.to, opts.top)
	full := markdown(rep, guild, opts.from, opts.to, 0)

	colour := core.ColorInfo
	if rep.partial() {
		colour = core.ColorWarning
	}
	embed := core.Embed(colour, "", core.TruncateEmbedDescription(shown))
	var files []*discord.File
	if png, err := renderPNG(p.client, rep, guild, opts.from, opts.to, opts.top); err == nil {
		embed.Image = &discord.EmbedResource{URL: imageAttachmentURL}
		files = append(files, fileFrom(imageAttachmentName, png))
	}
	// The full list rides along whenever the embed is not carrying all of it,
	// so a report never silently drops the tail of the very thing it is for.
	if full != shown {
		files = append(files, fileFrom(listAttachmentName, []byte(full)))
	}
	if err := ix.FollowUpEmbedWithFiles(embed, files...); err != nil {
		_ = ix.FollowUpErr("Could not post the report", err)
	}
}

// build turns the buckets for a window into the renderer's report.
func (p *Plugin) build(ctx context.Context, guildID string, opts options) (report, error) {
	rows, err := p.store.Report(ctx, guildID, opts.channelID, opts.from, opts.to)
	if err != nil {
		return report{}, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.UserID)
	}
	users, err := p.store.Users(ctx, guildID, ids)
	if err != nil {
		return report{}, err
	}
	channels, err := p.store.Channels(ctx, guildID)
	if err != nil {
		return report{}, err
	}
	rep := report{from: opts.from, coveredFrom: p.coveredFrom(ctx, guildID)}
	people := make(map[string]*person, len(rows))
	busy := map[string]bool{}
	for _, r := range rows {
		u := users[r.UserID]
		name := u.Name
		if name == "" {
			name = r.UserID
		}
		per := &person{id: r.UserID, name: name, avatar: u.Avatar, count: r.Messages,
			voice: time.Duration(r.VoiceSeconds) * time.Second, channels: map[string]bool{}, rooms: map[string]bool{}, last: r.Last}
		for _, ch := range r.Channels {
			if opts.showChannels {
				per.channels[channelLabel(channels, ch)] = true
			}
			busy[ch] = true
		}
		if opts.showChannels {
			for _, ch := range r.VoiceChannels {
				per.rooms[channelLabel(channels, ch)] = true
			}
		}
		people[r.UserID] = per
		rep.messages += r.Messages
		rep.voice += per.voice
	}
	rep.people = rank(people)
	rep.channels = len(busy)
	rep.hourly = opts.to.Sub(opts.from) <= hourlyHeatMax
	series := p.store.Days
	if rep.hourly {
		series = p.store.Hours
	}
	if rep.days, err = series(ctx, guildID, opts.channelID, opts.from, opts.to); err != nil {
		return report{}, err
	}
	return rep, nil
}

// MessagesPerDay is the server's measured traffic over the last few days:
// the average over the whole days that have anything counted, and false
// when nothing has been. It is what other plugins ask this one, aimod for
// pricing a model stack against the traffic it would actually see rather
// than a compiled-in guess.
func (p *Plugin) MessagesPerDay(ctx context.Context, guildID string, days int) (float64, bool) {
	now := p.now()
	to := now.Truncate(24 * time.Hour)
	from := to.AddDate(0, 0, -max(1, days))
	stats, err := p.store.Days(ctx, guildID, "", from, to)
	if err != nil {
		p.log.Error("statistics: messages per day", "guild", guildID, "err", err)
		return 0, false
	}
	var total float64
	var counted int
	for _, d := range stats {
		if d.Messages > 0 {
			total += float64(d.Messages)
			counted++
		}
	}
	if counted == 0 {
		return 0, false
	}
	return total / float64(counted), true
}

// coveredFrom is the earliest instant the buckets speak for: the oldest
// hour held, or when counting began if nothing is stored yet. Zero when
// neither is known, which reads as "no caveat" rather than a wrong one.
func (p *Plugin) coveredFrom(ctx context.Context, guildID string) time.Time {
	if oldest, err := p.store.OldestHour(ctx, guildID); err == nil && !oldest.IsZero() {
		return oldest
	}
	if cfg, err := p.store.Config(ctx, guildID); err == nil {
		return cfg.LiveSince
	}
	return time.Time{}
}

// channelLabel names a channel from the recorded names, falling back to
// the id: a report that says "#1234567890" is ugly and unambiguous, where
// a blank would be a row with no room on it.
func channelLabel(names map[string]string, id string) string {
	if n := names[id]; n != "" {
		return n
	}
	return id
}

func (p *Plugin) handleChannels(ctx context.Context, ix *core.Ix) {
	opts, err := parseOptions(ix.Args(), p.now())
	if err != nil {
		ix.RespondErr("That window doesn't work", err)
		return
	}
	totals, err := p.store.ChannelTotals(ctx, ix.GuildID, opts.from, opts.to)
	if err != nil {
		ix.RespondErr("Could not read the statistics", err)
		return
	}
	names, err := p.store.Channels(ctx, ix.GuildID)
	if err != nil {
		ix.RespondErr("Could not read the statistics", err)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` to `%s` utc\n\n", opts.from.Format("2006-01-02 15:04"), opts.to.Format("2006-01-02 15:04"))
	if len(totals) == 0 {
		b.WriteString("nothing was counted in that window.")
	}
	all := 0
	for _, t := range totals {
		all += t.Messages
	}
	for n, t := range totals {
		if n == core.PageSize {
			fmt.Fprintf(&b, "\nand `%d` more", len(totals)-n)
			break
		}
		fmt.Fprintf(&b, "`%6d` in `#%s` from `%d` people\n", t.Messages, escape(channelLabel(names, t.ChannelID)), t.People)
	}
	if all > 0 {
		fmt.Fprintf(&b, "\n`%d` messages over `%d` channels", all, len(totals))
	}
	ix.RespondInfo("Where the traffic went", core.TruncateEmbedDescription(b.String()))
}

func (p *Plugin) handleMembers(ctx context.Context, ix *core.Ix) {
	opts, err := parseOptions(ix.Args(), p.now())
	if err != nil {
		ix.RespondErr("That window doesn't work", err)
		return
	}
	days, err := p.store.MemberDays(ctx, ix.GuildID, opts.from, opts.to)
	if err != nil {
		ix.RespondErr("Could not read the statistics", err)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` to `%s` utc\n\n", opts.from.Format("2006-01-02 15:04"), opts.to.Format("2006-01-02 15:04"))
	joined, departed := 0, 0
	for _, d := range days {
		joined += d.Joined
		departed += d.Departed
	}
	if len(days) == 0 {
		b.WriteString("no joins or departures were counted in that window.")
	} else {
		fmt.Fprintf(&b, "`%d` joined, `%d` left, net `%+d`\n\n", joined, departed, joined-departed)
		// Newest first, since the question is usually "what happened this
		// week" and the tail of a long window is the part that fits.
		for n := len(days) - 1; n >= 0 && len(days)-1-n < core.PageSize; n-- {
			d := days[n]
			fmt.Fprintf(&b, "`%s` `+%d` `-%d`\n", d.Day.Format("2006-01-02"), d.Joined, d.Departed)
		}
	}
	ix.RespondInfo("Members", core.TruncateEmbedDescription(b.String()))
}

func (p *Plugin) handleBackfill(ctx context.Context, ix *core.Ix) {
	if !p.refuseUnlessOperator(ix) {
		return
	}
	arg, ok := ix.Args()["from"]
	if !ok {
		ix.RespondErr("That window doesn't work", errors.New("i need a start date"))
		return
	}
	from, err := parseWhen(arg.String())
	if err != nil {
		ix.RespondErr("That window doesn't work", err)
		return
	}
	cfg, err := p.store.Config(ctx, ix.GuildID)
	if err != nil {
		ix.RespondErr("Could not read the statistics", err)
		return
	}
	if cfg.LiveSince.IsZero() {
		ix.RespondErr("Nothing to fill up to", errors.New("counting has not started for this server yet; try again in a minute"))
		return
	}
	// Up to the top of the hour counting began. The partial hour between
	// that and live_since belongs to neither source and stays uncounted, one
	// time, rather than being counted by both.
	until := cfg.LiveSince.Truncate(time.Hour)
	if !from.Before(until) {
		ix.RespondErr("Already counted", fmt.Errorf("live counting has covered everything since `%s` utc", until.Format("2006-01-02 15:04")))
		return
	}
	// Listing channels is one call; the walk itself is the job's.
	if err := ix.Defer(); err != nil {
		return
	}
	n, err := p.queueBackfill(ctx, ix.GuildID, from, until)
	if err != nil {
		_ = ix.FollowUpErr("Could not start the backfill", err)
		return
	}
	_ = ix.FollowUpOK("Backfill queued",
		fmt.Sprintf("`%d` channels will be read from `%s` to `%s` utc, about a page of a hundred messages a second each. "+
			"it carries on across restarts; `/statistics status` shows how far it has got.",
			n, from.Format("2006-01-02"), until.Format("2006-01-02 15:04")))
}

// fileFrom is an upload; Discord types it from the name.
func fileFrom(name string, body []byte) *discord.File {
	return discord.NewFile(name, "", bytes.NewReader(body))
}

// guildName resolves the server's display name for the headline, falling back
// to "this server" rather than failing the report. A saved png of "who was
// active" with no server on it is unidentifiable a week later, but not
// knowing the name is a reason to be vague and not a reason to give up.
func (p *Plugin) guildName(guildID string) string {
	gid, err := snow.Parse(guildID)
	if p.guilds == nil || err != nil {
		return "this server"
	}
	g, err := p.guilds.GetGuild(gid, false)
	if err != nil || g == nil || g.Name == "" {
		return "this server"
	}
	return g.Name
}

// Perms is what the backfill does as merlin: read a channel's history.
func (p *Plugin) Perms(string) discord.Permissions {
	return discord.PermissionViewChannel | discord.PermissionReadMessageHistory
}
