package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
)

// A syntactically valid token shape: its head is base64 for
// 123456789012345678.
const testToken = "MTIzNDU2Nzg5MDEyMzQ1Njc4.Xxxxxx.yyyyyyyyyyyyyyyyyyyyyyyyyyy"

// The REST session knows who the bot is before any gateway does, for the
// plugins that ask its state.
func TestNewSessionKnowsTheBotFromItsToken(t *testing.T) {
	s, err := NewSession(testToken)
	if err != nil || s.State.User == nil || s.State.User.ID != "123456789012345678" {
		t.Fatalf("session user %+v, err %v", s.State.User, err)
	}
	if _, err := SelfID("not a token"); err == nil {
		t.Error("a token without an ID read as one")
	}
}

// A portal toggle counts whether it is the full flag or the _LIMITED one a
// bot under 100 servers gets, and an untouched toggle grants nothing.
func TestGrantedReadsThePortalToggles(t *testing.T) {
	for _, c := range []struct {
		flags discord.ApplicationFlags
		want  Intents
	}{
		{0, Intents{}},
		{discord.ApplicationFlagGatewayGuildMemberLimited, Intents{Members: true}},
		{discord.ApplicationFlagGatewayGuildMembers, Intents{Members: true}},
		{discord.ApplicationFlagGatewayMessageContentLimited, Intents{MessageContent: true}},
		{discord.ApplicationFlagGatewayMessageContent | discord.ApplicationFlagGatewayGuildMembers, Intents{Members: true, MessageContent: true}},
		{discord.ApplicationFlagGatewayPresence, Intents{}},
	} {
		if got := Granted(c.flags); got != c.want {
			t.Errorf("Granted(%b) = %+v, want %+v", c.flags, got, c.want)
		}
	}
}

// A connection Discord refuses fails startup, naming the toggles.
func TestOpenGatewayFailsWithAnActionableError(t *testing.T) {
	err := openWithin(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "Developer Portal") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if err := openWithin(context.Background(), func(context.Context) error { return nil }, time.Second); err != nil {
		t.Fatalf("a healthy open failed: %v", err)
	}
}

func TestGatewayIntentsRequestGuildMembersOnlyWhenAsked(t *testing.T) {
	with := GatewayIntents(Intents{Members: true})
	if with&gateway.IntentGuildMembers == 0 {
		t.Error("GUILD_MEMBERS was requested but is missing from the identify intents")
	}

	without := GatewayIntents(Intents{})
	if without&gateway.IntentGuildMembers != 0 {
		t.Error("GUILD_MEMBERS was declined but is still being requested")
	}

	for name, s := range map[string]gateway.Intents{"with": with, "without": without} {
		if s&gateway.IntentGuilds == 0 {
			t.Errorf("%s members: GUILDS is required for the bot to see any guild at all", name)
		}
	}
}

// MESSAGE_CONTENT is the largest ask this bot makes of a server: every
// message in it. Nothing but an explicit request may turn it on. The
// unprivileged GUILD_MESSAGES is always on, since statistics counts every
// message's author and hour and needs no text to do it.
func TestGatewayIntentsRequestMessageContentOnlyWhenAsked(t *testing.T) {
	off := GatewayIntents(Intents{Members: true})
	if off&gateway.IntentMessageContent != 0 {
		t.Error("MESSAGE_CONTENT was not asked for but is being requested")
	}
	if off&gateway.IntentGuildMessages == 0 {
		t.Error("GUILD_MESSAGES must always be requested: statistics counts from it")
	}

	on := GatewayIntents(Intents{MessageContent: true})
	if on&gateway.IntentMessageContent == 0 {
		t.Error("MESSAGE_CONTENT was asked for but is missing")
	}
	if on&gateway.IntentGuildMessages == 0 {
		t.Error("MESSAGE_CONTENT without GUILD_MESSAGES delivers no message events to read")
	}
	if on&gateway.IntentGuildMembers != 0 {
		t.Error("asking for MESSAGE_CONTENT must not drag GUILD_MEMBERS along with it")
	}
}
