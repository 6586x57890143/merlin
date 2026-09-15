// Package activity answers one question, for one person: who was talking in
// this server between these two instants, and how much.
//
// It is a break glass diagnostic, so the gate is deliberately narrower than
// anything else in this bot. TierAdmin is only the coarse floor on the leaf;
// the real check is core.Permissions.IsBootstrapAdmin, in the handler,
// because PermSpec cannot express an identity. A guild with five admins would
// otherwise have five accounts able to profile every member of the server on
// demand, and "who spoke when" is exactly the shape of question a weaponized
// reporting campaign wants answered. Same reasoning, and the same shape, as
// /aimod moderate-user and the tip jar's address.
//
// merlin stores no message log: aimod keeps only what it acted on and prunes
// that on the guild's own retention setting, and nothing else records who
// said anything. So this reads Discord's own history over REST for the window
// it was asked about, counts authors, and keeps nothing afterwards. There is
// no table, no cache and no file on disk; the png and the markdown exist for
// as long as it takes to upload them.
//
// It needs no gateway intent. Intents gate the live firehose, not fetching,
// and this reads authors and timestamps rather than message content.
package activity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

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
)

const (
	// interactionTTL is how long Discord honours an interaction token. A
	// scan that outlives it cannot answer through the interaction at all,
	// so the report goes to the operator's DMs (or the channel, when
	// sharing) instead. A minute is shaved off so the last progress edit
	// can still land and say where the report will arrive.
	interactionTTL = 14 * time.Minute
	// progressEvery is how often a running scan updates its placeholder.
	progressEvery = 20 * time.Second
)

// PrivilegeChecker answers the one question this plugin asks about identity.
// Satisfied by *core.Permissions, taken from Deps in Init.
type PrivilegeChecker interface {
	IsBootstrapAdmin(userID string) bool
}

type Plugin struct {
	session   *discordgo.Session
	source    messageSource
	privilege PrivilegeChecker
	client    *http.Client
	now       func() time.Time
	log       *slog.Logger

	// base outlives any one interaction and is cancelled at Shutdown. The
	// router hands handlers a 30 second context, which is where the scan
	// used to die: every report was silently cut at half a minute and
	// labelled "hit its ceiling". A scan runs on this instead, on its own
	// goroutine, for as long as scanBudget allows.
	base context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup
}

func New() *Plugin {
	base, stop := context.WithCancel(context.Background())
	return &Plugin{
		client: &http.Client{Timeout: fetchTTL},
		now:    func() time.Time { return time.Now().UTC() },
		log:    slog.New(slog.DiscardHandler),
		base:   base,
		stop:   stop,
	}
}

func (p *Plugin) Name() string { return "activity" }

func (p *Plugin) Init(deps core.Deps) error {
	p.session = deps.Session
	p.source = deps.Session
	p.privilege = deps.Perms
	if deps.Logger != nil {
		p.log = deps.Logger
	}

	deps.Commands.RegisterCommand(p.Name(), command())
	// TierAdmin is the floor, not the gate: handleActivity refuses anybody
	// but the bootstrap operator. Both are needed, since a guild can lower a
	// tier with /config permissions set-tier but cannot widen this.
	deps.Commands.Handle("activity", "", core.PermSpec{Tier: core.TierAdmin, Action: "activity.report"}, p.handleActivity)
	return nil
}

// command is the /activity definition, kept out of Init so the permission
// decision in it can be pinned by a test.
func command() *discordgo.ApplicationCommand {
	minTop := 1.0
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
	// a replacement for it: handleActivity still refuses everybody but the
	// bootstrap identity, so this cannot widen anything.
	//
	// It can narrow, though, and that is the trade. An operator who is not an
	// administrator of the guild loses the command from their own picker; the
	// way back is the guild's Integrations settings, where an owner can grant
	// an explicit overwrite, which is exactly what a zero here leaves room for.
	adminOnly := int64(0)
	return &discordgo.ApplicationCommand{
		Name:                     "activity",
		Description:              "Who was talking in a window of time",
		DefaultMemberPermissions: &adminOnly,
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "from",
				Description: "Window start, UTC: 2026-09-01, 2026-09-01 14:00, or an RFC3339 timestamp",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "to",
				Description: "Window end, same formats. Defaults to now",
			},
			{
				// A native Channel option rather than a raw ID string
				// (spec.MD §4a), which also gets the picker for free.
				Type:        discordgo.ApplicationCommandOptionChannel,
				Name:        "channel",
				Description: "Only look at this channel",
				ChannelTypes: []discordgo.ChannelType{
					discordgo.ChannelTypeGuildText,
					discordgo.ChannelTypeGuildNews,
					discordgo.ChannelTypeGuildPublicThread,
					discordgo.ChannelTypeGuildPrivateThread,
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "top",
				Description: fmt.Sprintf("How many people to show, 1 to %d. Default %d", maxTop, defaultTop),
				MinValue:    &minTop,
				MaxValue:    maxTop,
			},
			{
				Type:        discordgo.ApplicationCommandOptionBoolean,
				Name:        "share",
				Description: "Post the report in this channel instead of answering only you",
			},
		},
	}
}

func (p *Plugin) Start(context.Context) error { return nil }

// Shutdown cancels any running scan and waits for it to notice, so the
// process never exits mid-page with a goroutine still paging Discord.
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

// operator reports whether userID is the bootstrap identity.
//
// Fails closed in every direction: an unknown actor is refused, and a nil
// checker loses the escape hatch rather than granting it to everybody. There
// is deliberately no widening branch here at all.
func (p *Plugin) operator(userID string) bool {
	return userID != "" && p.privilege != nil && p.privilege.IsBootstrapAdmin(userID)
}

// options are the parsed arguments, validated before anything is deferred so
// a typo comes back in three seconds rather than after a full scan.
type options struct {
	from, to  time.Time
	channelID string
	top       int
	share     bool
}

func parseOptions(args map[string]*discordgo.ApplicationCommandInteractionDataOption, now time.Time) (options, error) {
	opts := options{to: now, top: defaultTop}

	arg, ok := args["from"]
	if !ok {
		return opts, errors.New("i need a window start")
	}
	from, err := parseWhen(arg.StringValue())
	if err != nil {
		return opts, err
	}
	opts.from = from

	if arg, ok := args["to"]; ok {
		to, err := parseWhen(arg.StringValue())
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
		opts.channelID = arg.ChannelValue(nil).ID
	}
	if arg, ok := args["top"]; ok {
		opts.top = min(maxTop, max(1, int(arg.IntValue())))
	}
	if arg, ok := args["share"]; ok {
		opts.share = arg.BoolValue()
	}
	return opts, nil
}

func (p *Plugin) handleActivity(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	var actor string
	if i.Member != nil && i.Member.User != nil {
		actor = i.Member.User.ID
	}
	if !p.operator(actor) {
		core.RespondWarn(s, i, "Not yours to run",
			"This one is the operator's alone. It answers who was talking and when, for every member of the server, "+
				"which is not a question admin should be able to ask on its own.")
		return
	}

	opts, err := parseOptions(core.LeafArgs(i), p.now())
	if err != nil {
		core.RespondErr(s, i, "That window doesn't work", err)
		return
	}

	// A scan walks channels and pages history, which is far past Discord's
	// three seconds. Deferring publicly when sharing is not optional:
	// ephemerality is fixed at acknowledgement, so a private defer cannot be
	// edited into a channel-visible answer later.
	ack := core.DeferResponse
	if opts.share {
		ack = core.DeferResponsePublic
	}
	if err := ack(s, i); err != nil {
		return
	}

	// Off the router's goroutine and its 30 second context: a scan over
	// months runs for as long as it takes. The router's recover() does not
	// reach a goroutine it did not start, so this one carries its own.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				_ = core.FollowUpErr(s, i, "Could not read the history", fmt.Errorf("scan panicked: %v", r))
			}
		}()
		p.run(s, i, actor, opts)
	}()
}

// run is the whole scan-render-deliver path for one report, on its own
// goroutine. Where the answer lands depends on how long it took: through the
// interaction while its token lives, and otherwise as a DM to the operator
// (or a post in the channel, when sharing), because a report that took an
// hour to count is not one to lose to a fifteen minute token.
func (p *Plugin) run(s *discordgo.Session, i *discordgo.InteractionCreate, actor string, opts options) {
	ctx, cancel := context.WithTimeout(p.base, scanBudget)
	defer cancel()

	prog := &progress{}
	started := p.now()
	done := make(chan struct{})
	go p.narrate(s, i, opts, prog, started, done)

	rep, err := scan(ctx, p.source, i.GuildID, opts.channelID, opts.from, opts.to, prog)
	close(done)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Could not read the history", err)
		return
	}

	guild := p.guildName(i.GuildID)
	shown := markdown(rep, guild, opts.from, opts.to, opts.top)
	full := markdown(rep, guild, opts.from, opts.to, 0)

	colour := core.ColorInfo
	if rep.truncated || rep.skipped > 0 {
		colour = core.ColorWarning
	}
	embed := core.NewEmbed(colour, "", core.TruncateEmbedDescription(shown))

	// Attachments are built fresh per attempt: discordgo drains the readers
	// into the multipart body, so a file sent through an expired token
	// would arrive empty on the second try.
	var image []byte
	if png, err := renderPNG(p.client, rep, guild, opts.from, opts.to, opts.top); err == nil {
		embed.Image = &discordgo.MessageEmbedImage{URL: imageAttachmentURL}
		image = png
	}
	files := func() []*discordgo.File {
		var out []*discordgo.File
		if image != nil {
			out = append(out, fileFrom(imageAttachmentName, "image/png", image))
		}
		// The full list rides along whenever the embed is not carrying all
		// of it, so a report never silently drops the tail of the very
		// thing it is for.
		if full != shown {
			out = append(out, fileFrom(listAttachmentName, "text/markdown", []byte(full)))
		}
		return out
	}

	if p.now().Sub(started) < interactionTTL {
		if err := core.FollowUpEmbedWithFiles(s, i, embed, files()...); err == nil {
			return
		}
	}
	if err := p.deliverLate(s, i, actor, opts.share, embed, files()); err != nil {
		_ = core.FollowUpErr(s, i, "Could not post the report", err)
	}
}

// narrate keeps the placeholder honest while the scan runs: what it has
// counted so far, and where the report will land if it outlives the token.
// It stops editing at interactionTTL, since nothing after that can land.
func (p *Plugin) narrate(s *discordgo.Session, i *discordgo.InteractionCreate, opts options, prog *progress, started time.Time, done <-chan struct{}) {
	t := time.NewTicker(progressEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		if p.now().Sub(started) >= interactionTTL {
			return
		}
		where := "your DMs"
		if opts.share {
			where = "this channel"
		}
		body := fmt.Sprintf("`%d` messages over `%d` channels so far, `%s` in.\n-# if this outlives Discord's fifteen minute window the report lands in %s instead",
			prog.messages.Load(), prog.channels.Load(), humanSpan(p.now().Sub(started)), where)
		// Logged rather than dropped: a progress edit that fails on every
		// tick looks, from Discord, like a scan that stopped, and the
		// attachment cap did exactly that for three weeks with nothing to
		// show for it.
		if err := core.FollowUpEmbed(s, i, core.NewEmbed(core.ColorInfo, "Still counting", body)); err != nil {
			p.log.Warn("activity: progress edit failed", "guild", i.GuildID, "err", err)
		}
	}
}

// deliverLate posts the finished report without the interaction: to the
// channel it was asked in when sharing, otherwise to the operator's DMs. On
// the raw session, like scheduler.alert, so mentions are zeroed here.
func (p *Plugin) deliverLate(s *discordgo.Session, i *discordgo.InteractionCreate, actor string, share bool, embed *discordgo.MessageEmbed, files []*discordgo.File) error {
	channelID := i.ChannelID
	if !share {
		dm, err := s.UserChannelCreate(actor)
		if err != nil {
			return fmt.Errorf("open a DM: %w", err)
		}
		channelID = dm.ID
	}
	_, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{embed},
		Files:           append(core.EmbedFiles(embed), files...),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	return err
}

func fileFrom(name, contentType string, body []byte) *discordgo.File {
	return &discordgo.File{Name: name, ContentType: contentType, Reader: bytes.NewReader(body)}
}

// guildName resolves the server's display name for the headline, falling back
// to "this server" rather than failing the report. A saved png of "who was
// active" with no server on it is unidentifiable a week later, but not
// knowing the name is a reason to be vague and not a reason to give up.
func (p *Plugin) guildName(guildID string) string {
	if p.session == nil || guildID == "" {
		return "this server"
	}
	g, err := p.session.Guild(guildID)
	if err != nil || g == nil || g.Name == "" {
		return "this server"
	}
	return g.Name
}
