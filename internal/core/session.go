package core

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// Intents is which privileged gateway intents this process identifies with:
// the ones the Developer Portal has granted (Granted).
//
// A struct rather than two positional bools, because the call site is one
// line in main.go and "NewSession(token, true, false)" is unreadable at
// exactly the place where getting it wrong means either a bot that silently
// cannot do its job or one asking to read every message in every server it
// is in.
type Intents struct {
	// Members enables the roles plugin's instant re-jail on rejoin.
	Members bool
	// MessageContent enables the aimod plugin to read messages at all.
	MessageContent bool
}

// NewSession builds the *discordgo.Session every plugin still sends REST
// through. It never connects: the gateway is disgo's (NewGateway). Its
// state carries one thing, the bot's own user, read from the token, for the
// plugins that still ask session.State who the bot is.
func NewSession(token string) (*discordgo.Session, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	self, err := SelfID(token)
	if err != nil {
		return nil, err
	}
	s.State.User = &discordgo.User{ID: self.String(), Bot: true}
	return s, nil
}

// SelfID is the bot's own user ID, which a bot token begins with.
func SelfID(token string) (snowflake.ID, error) {
	head, _, _ := strings.Cut(token, ".")
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(head, "="))
	if err != nil {
		return 0, fmt.Errorf("core: the bot token does not start with the bot's ID: %w", err)
	}
	return snowflake.Parse(string(raw))
}

// GatewayIntents is what merlin identifies with: GUILDS,
// GUILD_VOICE_STATES, GUILD_MESSAGES and GUILD_MODERATION always, plus the
// two privileged ones the caller asks for.
//
// GUILD_MESSAGES is unprivileged and delivers message events with no text
// in them: author, channel and timestamp, which is what
// internal/plugins/statistics counts. MESSAGE_CONTENT below is what fills in
// the content field, and nothing but aimod reads it.
//
// GUILD_VOICE_STATES is unprivileged (no Developer Portal toggle, no
// approval process, unlike GUILD_MEMBERS below), so it is always requested.
// It keeps the gateway cache's voice states, which roles uses to name the
// channel a jailed member was disconnected from in its log line (the
// disconnect itself is a plain REST call and works without it) and
// statistics uses to tell a move from a mute.
//
// GUILD_MODERATION carries GUILD_AUDIT_LOG_ENTRY_CREATE, which is how the
// rapsheet learns about a ban, kick or timeout done through the Discord
// client rather than through merlin. Unprivileged, so no portal toggle; the
// bot does still need View Audit Log in the guild or Discord delivers
// nothing, silently.
//
// intents.MessageContent controls MESSAGE_CONTENT, which is what lets
// internal/plugins/aimod read message text. Without it that plugin registers
// its commands and scans nothing, and says so in /aimod status rather than
// appearing to work. It is the larger of the two asks by a wide margin (it
// is every message in every server, and Discord reviews it above 100
// guilds); its portal toggle is the opt-in, and nothing else reads it.
//
// intents.Members controls GUILD_MEMBERS, which is what lets the roles plugin
// react to a rejoin the instant it happens rather than on the next sweep
// (roles.reapplyEvadedJails remains the fallback either way; the intent
// narrows the window, it does not create the protection). It is privileged:
// a Developer Portal toggle below 100 guilds, Discord approval above.
//
// Both come from Granted, so merlin never asks for one the portal has not
// switched on, which is what Discord answers with close code 4014.
func GatewayIntents(intents Intents) gateway.Intents {
	want := gateway.IntentGuilds | gateway.IntentGuildVoiceStates | gateway.IntentGuildMessages | gateway.IntentGuildModeration
	if intents.Members {
		want |= gateway.IntentGuildMembers
	}
	if intents.MessageContent {
		want |= gateway.IntentMessageContent
	}
	return want
}

// NewGateway builds merlin's one gateway connection, unopened: disgo, with
// zstd transport compression (its default), no raw events (each would be
// a second dispatch of every gateway event), and every listener run on its
// own goroutine, as discordgo ran them, so a slow
// handler never holds up the connection.
//
// The cache keeps what merlin reads: guilds and their roles (authorization
// and the role hierarchy), channels (statistics' channel names), voice
// states, and of members only the bot's own, for its top role. discordgo's
// state kept every member it saw.
func NewGateway(token string, intents Intents, log *slog.Logger, listeners ...bot.EventListener) (*bot.Client, error) {
	self, err := SelfID(token)
	if err != nil {
		return nil, err
	}
	return disgo.New(token,
		bot.WithLogger(log),
		bot.WithGatewayConfigOpts(gateway.WithIntents(GatewayIntents(intents))),
		bot.WithCacheConfigOpts(
			cache.WithCaches(cache.FlagGuilds, cache.FlagRoles, cache.FlagChannels, cache.FlagVoiceStates, cache.FlagMembers),
			cache.WithMemberCachePolicy(func(m discord.Member) bool { return m.User.ID == self }),
		),
		bot.WithEventManagerConfigOpts(bot.WithAsyncEventsEnabled()),
		bot.WithEventListeners(listeners...),
	)
}

// Granted is the privileged intents the Developer Portal has switched on,
// read from the application's flags. The _LIMITED flags are what a bot
// under 100 servers gets for the same toggle, so they count. skua's
// internal/intents, for the two merlin can use.
func Granted(flags discord.ApplicationFlags) Intents {
	return Intents{
		Members:        flags&(discord.ApplicationFlagGatewayGuildMembers|discord.ApplicationFlagGatewayGuildMemberLimited) != 0,
		MessageContent: flags&(discord.ApplicationFlagGatewayMessageContent|discord.ApplicationFlagGatewayMessageContentLimited) != 0,
	}
}

// Probe reads what the Developer Portal grants merlin now.
func Probe(r rest.Rest) (Intents, error) {
	app, err := r.GetCurrentApplication()
	if err != nil {
		return Intents{}, fmt.Errorf("core: reading the application's intents: %w", err)
	}
	return Granted(app.Flags), nil
}

// readyTimeout bounds how long OpenGateway waits for Discord's READY.
// Generous relative to a healthy connect (a second or two) because the cost
// of being wrong in the impatient direction is a crash loop on a slow
// network, while the cost of waiting is a slower failure on a broken one.
const readyTimeout = 45 * time.Second

// OpenGateway connects and waits for Discord to accept the identify.
//
// Intents come from Granted, so close code 4014 ("disallowed intents")
// should not happen; this is the backstop for a toggle switched off in the
// moment between the probe and the identify, or a connection that fails for
// any other reason. A bot that cannot connect has nothing useful to do, so
// the error is fatal at startup rather than a process that looks healthy and
// holds no connection.
func OpenGateway(ctx context.Context, c *bot.Client) error {
	return openWithin(ctx, c.OpenGateway, readyTimeout)
}

func openWithin(ctx context.Context, open func(context.Context) error, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := open(ctx); err != nil {
		return fmt.Errorf("no READY from Discord within %s (%w): the gateway is refusing this connection. "+
			"If a privileged intent was switched off in the Developer Portal just now, restart merlin and "+
			"she identifies without it", timeout, err)
	}
	return nil
}
