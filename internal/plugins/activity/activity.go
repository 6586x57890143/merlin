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
	"net/http"
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
}

func New() *Plugin {
	return &Plugin{
		client: &http.Client{Timeout: fetchTTL},
		now:    func() time.Time { return time.Now().UTC() },
	}
}

func (p *Plugin) Name() string { return "activity" }

func (p *Plugin) Init(deps core.Deps) error {
	p.session = deps.Session
	p.source = deps.Session
	p.privilege = deps.Perms

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

func (p *Plugin) Start(context.Context) error    { return nil }
func (p *Plugin) Shutdown(context.Context) error { return nil }

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

	rep, err := scan(ctx, p.source, i.GuildID, opts.channelID, opts.from, opts.to)
	if err != nil {
		_ = core.FollowUpErr(s, i, "Could not read the history", err)
		return
	}

	guild := p.guildName(i.GuildID)
	shown := markdown(rep, guild, opts.from, opts.to, opts.top)
	full := markdown(rep, guild, opts.from, opts.to, 0)

	colour := core.ColorInfo
	if rep.truncated() || rep.skipped > 0 {
		colour = core.ColorWarning
	}
	embed := core.NewEmbed(colour, "", core.TruncateEmbedDescription(shown))

	var files []*discordgo.File
	if image, err := renderPNG(p.client, rep, guild, opts.from, opts.to, opts.top); err == nil {
		embed.Image = &discordgo.MessageEmbedImage{URL: imageAttachmentURL}
		files = append(files, fileFrom(imageAttachmentName, "image/png", image))
	}
	// The full list rides along whenever the embed is not carrying all of it,
	// so a report never silently drops the tail of the very thing it is for.
	if full != shown {
		files = append(files, fileFrom(listAttachmentName, "text/markdown", []byte(full)))
	}

	if err := core.FollowUpEmbedWithFiles(s, i, embed, files...); err != nil {
		_ = core.FollowUpErr(s, i, "Could not post the report", err)
	}
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
