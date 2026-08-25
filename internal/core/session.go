package core

import (
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Intents is which privileged gateway intents this process asks Discord for.
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

// NewSession builds the single shared *discordgo.Session used by every
// plugin. Intents are the minimum this binary needs: GUILDS and
// GUILD_VOICE_STATES always, plus the two privileged ones the caller asks
// for.
//
// GUILD_VOICE_STATES is unprivileged (no Developer Portal toggle, no
// approval process, unlike GUILD_MEMBERS below), so it is always requested.
// It is what keeps session.State's voice-connection cache populated, which
// roles uses to name the channel a jailed member was disconnected from in
// its log line; the disconnect itself is a plain REST call and works
// without this intent, so nothing depends on the cache being complete.
//
// intents.MessageContent controls MESSAGE_CONTENT, which is what lets
// internal/plugins/aimod read message text. Without it that plugin registers
// its commands and scans nothing, and says so in /aimod status rather than
// appearing to work. It is the larger of the two asks by a wide margin (it
// is every message in every server, and Discord reviews it above 100
// guilds), which is why it is off unless MERLIN_ENABLE_MESSAGE_CONTENT_INTENT
// says otherwise while GUILD_MEMBERS below is on unless told not to. GUILD_MESSAGES
// rides along with it: MESSAGE_CONTENT only fills in the content field of
// message events, it does not deliver the events themselves.
//
// intents.Members controls GUILD_MEMBERS, which is what lets the roles plugin
// react to a rejoin the instant it happens rather than on the next sweep
// (roles.reapplyEvadedJails remains the fallback either way; the intent
// narrows the window, it does not create the protection). It is privileged:
// a Developer Portal toggle below 100 guilds, Discord approval above. If the
// portal has not granted it, Discord rejects the connection outright, which
// is why callers must pair this with a readiness check rather than trusting
// Open to report the problem.
func NewSession(token string, intents Intents) (*discordgo.Session, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	want := discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates
	if intents.Members {
		want |= discordgo.IntentsGuildMembers
	}
	if intents.MessageContent {
		want |= discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent
	}
	s.Identify.Intents = want
	return s, nil
}

// readyTimeout bounds how long WatchReady's waiter blocks on Discord's READY.
// Generous relative to a healthy connect (a second or two) because the cost
// of being wrong in the impatient direction is a crash loop on a slow
// network, while the cost of waiting is a slower failure on a broken one.
const readyTimeout = 45 * time.Second

// WatchReady arms a one-shot watch for Discord's READY event and returns a
// function that blocks until it arrives, reporting a diagnosable error if it
// never does.
//
// It is split from the waiting deliberately, and must be called *before*
// Open. discordgo starts its listen goroutine inside Open, so READY can be
// dispatched the moment Open returns, so registering the handler afterwards
// races against it, and losing that race would mean waiting the full timeout
// and then failing startup on a connection that was perfectly healthy.
//
// The reason to check at all: Open returns as soon as the identify payload has
// been *sent*, never that Discord accepted it. When the gateway rejects the
// identify (close code 4014, "disallowed intents": the bot asked for a
// privileged intent its application has not been granted), discordgo neither
// surfaces the close code nor gives up; it reconnects, is rejected again, and
// loops. Open having returned nil, startup carries on and logs that the bot is
// running. The result is a process that looks healthy, holds no gateway
// connection, receives no events, and silently does nothing: jails never
// released, rotations never fired, and no error anywhere above debug level.
//
// Waiting for READY is what separates "connected" from "has not failed loudly
// yet", so the error is meant to be fatal at startup rather than a warning. A
// bot that cannot connect has nothing useful to do, and a container that exits
// with an explanation is far easier to diagnose than one claiming success.
func WatchReady(s *discordgo.Session) func() error {
	return watchReady(s, readyTimeout)
}

// readyWatcher is the sliver of *discordgo.Session watchReady needs, so the
// timeout and dispatch paths are testable without a gateway connection,
// the same narrow-seam pattern the plugins use for their Discord ops.
type readyWatcher interface {
	AddHandler(handler any) func()
}

func watchReady(s readyWatcher, timeout time.Duration) func() error {
	ready := make(chan struct{})
	var once sync.Once
	remove := s.AddHandler(func(*discordgo.Session, *discordgo.Ready) {
		// discordgo re-dispatches READY on every successful reconnect, and
		// closing a closed channel panics. Once, not a bool, because the
		// dispatch goroutine and the waiter below are different goroutines.
		once.Do(func() { close(ready) })
	})

	return func() error {
		defer remove()
		select {
		case <-ready:
			return nil
		case <-time.After(timeout):
			// Both privileged intents are named. Only one of them was ever
			// requested when this message was written, and an operator who
			// has just enabled the other and hit close code 4014 would
			// otherwise be sent to check a toggle that was already correct.
			return fmt.Errorf("no READY from Discord within %s: the gateway is refusing this connection. "+
				"The usual cause is a privileged intent the application has not been granted. Under Bot in the "+
				"Discord Developer Portal, enable \"Server Members Intent\" (or set "+
				"MERLIN_DISABLE_GUILD_MEMBERS_INTENT=1 to run without it) and, if "+
				"MERLIN_ENABLE_MESSAGE_CONTENT_INTENT is set, \"Message Content Intent\" as well", timeout)
		}
	}
}
