package core

import (
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// NewSession builds the single shared *discordgo.Session used by every
// plugin. Intents are the minimum this binary needs: GUILDS always, plus
// GUILD_MEMBERS unless the operator has turned it off. MESSAGE_CONTENT is
// never requested at all.
//
// withMembers controls GUILD_MEMBERS, which is what lets the roles plugin
// react to a rejoin the instant it happens rather than on the next sweep
// (roles.reapplyEvadedJails remains the fallback either way — the intent
// narrows the window, it does not create the protection). It is privileged:
// a Developer Portal toggle below 100 guilds, Discord approval above. If the
// portal has not granted it, Discord rejects the connection outright, which
// is why callers must pair this with a readiness check rather than trusting
// Open to report the problem.
func NewSession(token string, withMembers bool) (*discordgo.Session, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	intents := discordgo.IntentsGuilds
	if withMembers {
		intents |= discordgo.IntentsGuildMembers
	}
	s.Identify.Intents = intents
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
// dispatched the moment Open returns — registering the handler afterwards
// races against it, and losing that race would mean waiting the full timeout
// and then failing startup on a connection that was perfectly healthy.
//
// The reason to check at all: Open returns as soon as the identify payload has
// been *sent*, never that Discord accepted it. When the gateway rejects the
// identify (close code 4014, "disallowed intents" — the bot asked for a
// privileged intent its application has not been granted), discordgo neither
// surfaces the close code nor gives up; it reconnects, is rejected again, and
// loops. Open having returned nil, startup carries on and logs that the bot is
// running. The result is a process that looks healthy, holds no gateway
// connection, receives no events, and silently does nothing — jails never
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
// timeout and dispatch paths are testable without a gateway connection —
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
			return fmt.Errorf("no READY from Discord within %s: the gateway is refusing this connection. "+
				"The usual cause is a privileged intent the application has not been granted — enable "+
				"\"Server Members Intent\" under Bot in the Discord Developer Portal, or set "+
				"MERLIN_DISABLE_GUILD_MEMBERS_INTENT=1 to run without it", timeout)
		}
	}
}
