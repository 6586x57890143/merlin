package core

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fakeGateway stands in for *discordgo.Session's handler registration so the
// READY paths can be exercised without a websocket.
type fakeGateway struct {
	mu       sync.Mutex
	handler  func(*discordgo.Session, *discordgo.Ready)
	removals int
}

func (f *fakeGateway) AddHandler(handler any) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = handler.(func(*discordgo.Session, *discordgo.Ready))
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.removals++
	}
}

func (f *fakeGateway) fireReady() {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	h(nil, &discordgo.Ready{})
}

func TestWatchReadyReturnsOnceDiscordAcceptsTheConnection(t *testing.T) {
	gw := &fakeGateway{}
	wait := watchReady(gw, 5*time.Second)

	go gw.fireReady()

	if err := wait(); err != nil {
		t.Fatalf("READY arrived but the watch reported %v", err)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.removals != 1 {
		t.Errorf("handler removals = %d, want 1; the watch must not leak its subscription", gw.removals)
	}
}

// The handler has to be registered by the time watchReady returns, not when
// the returned function is called. discordgo starts dispatching inside Open,
// so a watch that only subscribed at Wait() time could miss READY entirely
// and fail startup on a perfectly healthy connection.
func TestWatchReadySubscribesBeforeItIsWaitedOn(t *testing.T) {
	gw := &fakeGateway{}
	wait := watchReady(gw, 5*time.Second)

	gw.mu.Lock()
	subscribed := gw.handler != nil
	gw.mu.Unlock()
	if !subscribed {
		t.Fatal("no READY handler registered before waiting; the event can be missed")
	}

	// Fire before anyone waits: the result must still be observed.
	gw.fireReady()
	if err := wait(); err != nil {
		t.Fatalf("READY fired before the wait began and was lost: %v", err)
	}
}

// The failure this exists to catch: Discord silently refusing the identify
// because a privileged intent was requested but never granted. discordgo
// reports nothing above debug level and just reconnect-loops, so the error
// text is the only thing an operator will have to go on.
func TestWatchReadyTimesOutWithAnActionableError(t *testing.T) {
	gw := &fakeGateway{}
	wait := watchReady(gw, 10*time.Millisecond)

	err := wait()
	if err == nil {
		t.Fatal("a gateway that never sends READY must not look like a successful start")
	}
	for _, want := range []string{"Server Members Intent", "MERLIN_DISABLE_GUILD_MEMBERS_INTENT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, leaving the operator no way to act on it: %v", want, err)
		}
	}
}

// discordgo re-dispatches READY on every reconnect. Closing an already-closed
// channel panics, and this handler outlives startup.
func TestWatchReadyToleratesRepeatedReadyEvents(t *testing.T) {
	gw := &fakeGateway{}
	wait := watchReady(gw, 5*time.Second)

	gw.fireReady()
	gw.fireReady()
	gw.fireReady()

	if err := wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

// A syntactically valid token shape; discordgo.New does no network I/O.
const testToken = "MTIzNDU2Nzg5MDEyMzQ1Njc4.Xxxxxx.yyyyyyyyyyyyyyyyyyyyyyyyyyy"

func TestNewSessionRequestsGuildMembersOnlyWhenAsked(t *testing.T) {
	with, err := NewSession(testToken, Intents{Members: true})
	if err != nil {
		t.Fatalf("NewSession(Members=true): %v", err)
	}
	if with.Identify.Intents&discordgo.IntentsGuildMembers == 0 {
		t.Error("GUILD_MEMBERS was requested but is missing from the identify intents")
	}

	without, err := NewSession(testToken, Intents{})
	if err != nil {
		t.Fatalf("NewSession(Members=false): %v", err)
	}
	if without.Identify.Intents&discordgo.IntentsGuildMembers != 0 {
		t.Error("GUILD_MEMBERS was declined but is still being requested")
	}

	for name, s := range map[string]*discordgo.Session{"with": with, "without": without} {
		if s.Identify.Intents&discordgo.IntentsGuilds == 0 {
			t.Errorf("%s members: GUILDS is required for the bot to see any guild at all", name)
		}
	}
}

// MESSAGE_CONTENT is the largest ask this bot makes of a server: every
// message in it. Nothing but an explicit request may turn it on, and asking
// for it must also ask for GUILD_MESSAGES, since MESSAGE_CONTENT only fills
// in the content field of message events rather than delivering them.
func TestNewSessionRequestsMessageContentOnlyWhenAsked(t *testing.T) {
	off, err := NewSession(testToken, Intents{Members: true})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if off.Identify.Intents&discordgo.IntentsMessageContent != 0 {
		t.Error("MESSAGE_CONTENT was not asked for but is being requested")
	}
	if off.Identify.Intents&discordgo.IntentsGuildMessages != 0 {
		t.Error("GUILD_MESSAGES is being requested with nothing to read")
	}

	on, err := NewSession(testToken, Intents{MessageContent: true})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if on.Identify.Intents&discordgo.IntentsMessageContent == 0 {
		t.Error("MESSAGE_CONTENT was asked for but is missing")
	}
	if on.Identify.Intents&discordgo.IntentsGuildMessages == 0 {
		t.Error("MESSAGE_CONTENT without GUILD_MESSAGES delivers no message events to read")
	}
	if on.Identify.Intents&discordgo.IntentsGuildMembers != 0 {
		t.Error("asking for MESSAGE_CONTENT must not drag GUILD_MEMBERS along with it")
	}
}
