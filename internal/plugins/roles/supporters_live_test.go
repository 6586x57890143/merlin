package roles

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
	supportplugin "github.com/6586x57890143/merlin/internal/plugins/support"
)

// These settings exist only in this process. No production settings or ledger
// rows are read or written by the test. The operator preflights write controls.
type liveSupporterSettings struct{ guild, channel string }

func (s liveSupporterSettings) PluginEnabled(guild, plugin string) bool {
	return guild == s.guild && plugin == "support"
}
func (s liveSupporterSettings) SupporterChannelID(guild string) string {
	if guild == s.guild {
		return s.channel
	}
	return ""
}
func (s liveSupporterSettings) WritesPaused(guild string) bool { return guild != s.guild }
func (liveSupporterSettings) WritesDryRun(string) bool         { return false }

type liveSupporterSender struct {
	ops     *discordguard.GuildOps
	channel snowflake.ID
	count   atomic.Int32
	done    chan snowflake.ID
}

func (s *liveSupporterSender) CreateMessage(channel snowflake.ID, data discord.MessageCreate) (*discord.Message, error) {
	if channel != s.channel || !s.count.CompareAndSwap(0, 1) {
		return nil, errors.New("live smoke: only one message in the selected test channel is allowed")
	}
	data.Components = append([]discord.LayoutComponent{discord.NewTextDisplay("-# One-time supporter integration test. No payment was recorded.")}, data.Components...)
	message, err := s.ops.CreateMessage(channel, data)
	if err == nil && message != nil {
		s.done <- message.ID
	}
	return message, err
}

// Opt-in only, using a token on stdin rather than argv or environment. This
// exercises the real role sync, event bus, welcome plugin, guard and REST calls
// against the bot's own membership. The grant store and channel override are
// isolated fixtures; this does not claim funded settlement or fleet coverage.
func TestLiveSupporterSelfBotLifecycle(t *testing.T) {
	if os.Getenv("MERLIN_LIVE_SUPPORTER_SMOKE") != "self-bot-one-time" {
		t.Skip("requires explicitly authorized live self-bot smoke")
	}
	var input struct{ Token, Guild, Channel, Role, Bot string }
	if json.NewDecoder(io.LimitReader(os.Stdin, 8192)).Decode(&input) != nil || input.Token == "" {
		t.Fatal("missing private smoke input")
	}
	for _, value := range []string{input.Guild, input.Channel, input.Role, input.Bot} {
		if core.ID(value) == 0 {
			t.Fatal("invalid target ID")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := core.NewRest(input.Token, &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	var me discord.User
	if client.Do(rest.GetUser.Compile(nil, "@me"), nil, &me) != nil || !me.Bot || me.ID != core.ID(input.Bot) {
		t.Fatal("live smoke may target only the authenticated bot itself")
	}
	log := slog.New(slog.DiscardHandler)
	settings := liveSupporterSettings{guild: input.Guild, channel: input.Channel}
	guard := discordguard.New(client, settings, log, false)
	ops := guard.For(input.Guild)
	member, err := ops.GetMember(core.ID(input.Guild), me.ID)
	if err != nil || slices.Contains(member.RoleIDs, core.ID(input.Role)) {
		t.Fatal("bot must be present and must not already hold the test supporter role")
	}
	roles, err := ops.GetRoles(core.ID(input.Guild))
	if err != nil {
		t.Fatal("could not verify role safety")
	}
	top, found := 0, false
	var permissions discord.Permissions
	for _, role := range roles {
		if slices.Contains(member.RoleIDs, role.ID) {
			top = max(top, role.Position)
			permissions |= role.Permissions
		}
		if role.ID == core.ID(input.Guild) {
			permissions |= role.Permissions
		}
	}
	for _, role := range roles {
		if role.ID == core.ID(input.Role) {
			found = !role.Managed && role.Permissions == discord.PermissionsNone && role.Position < top
		}
	}
	if !found || permissions&(discord.PermissionManageRoles|discord.PermissionAdministrator) == 0 {
		t.Fatal("test role must be unmanaged, permissionless and below the bot")
	}
	// Even a failed grant response may have applied. Cleanup checks live state
	// and removes only the selected role, which was absent before this test.
	t.Cleanup(func() {
		current, err := ops.GetMember(core.ID(input.Guild), me.ID)
		if err != nil {
			t.Error("could not verify final bot role state")
			return
		}
		if slices.Contains(current.RoleIDs, core.ID(input.Role)) {
			if ops.RemoveMemberRole(core.ID(input.Guild), me.ID, core.ID(input.Role)) != nil {
				t.Error("cleanup failed: temporary supporter role must be removed from the bot")
			}
		}
	})
	p := newTestPlugin(newFakeOps(), newFakeStore(), newFakeSettings(), newFakeAudit(), newFakePerms(), newFakeScheduler())
	p.ops = func(guild string) DiscordMemberOps { return guard.For(guild) }
	now := time.Now().UTC()
	p.now = func() time.Time { return now }
	defer func() { _ = p.Shutdown(context.Background()) }()
	p.bus = core.NewEventBus(log)
	sender := &liveSupporterSender{ops: ops, channel: core.ID(input.Channel), done: make(chan snowflake.ID, 1)}
	welcome := supportplugin.New(func(string) supportplugin.Sender { return sender }, settings, p.voice, "")
	if welcome.Init(core.Deps{Logger: log, Bus: p.bus, Commands: core.NewCommandRouter(nil, nil, log)}) != nil {
		t.Fatal("could not initialize the real welcome plugin")
	}
	until := now.Add(30 * 24 * time.Hour)
	p.WithSupporters(SupportersFunc(func(context.Context) ([]Backer, error) {
		return []Backer{{UserID: input.Bot, Until: until}}, nil
	}))
	if p.syncSupporters(t.Context(), input.Guild, input.Role) != nil {
		t.Fatal("real supporter grant failed")
	}
	member, err = ops.GetMember(core.ID(input.Guild), me.ID)
	if err != nil || !slices.Contains(member.RoleIDs, core.ID(input.Role)) {
		t.Fatal("Discord did not confirm the supporter role")
	}
	select {
	case message := <-sender.done:
		t.Logf("one-time thank-you: https://discord.com/channels/%s/%s/%s", input.Guild, input.Channel, message)
	case <-time.After(20 * time.Second):
		t.Fatal("the real welcome plugin did not confirm its single test message")
	}
	until = until.Add(30 * 24 * time.Hour)
	if p.syncSupporters(t.Context(), input.Guild, input.Role) != nil || sender.count.Load() != 1 {
		t.Fatal("renewal failed or sent another welcome message")
	}
	now = until.Add(time.Second)
	if p.revokeGrantChecked(t.Context(), input.Guild, input.Bot, input.Role, core.ActorSystem, true) != nil {
		t.Fatal("expired test grant was not revoked")
	}
	member, err = ops.GetMember(core.ID(input.Guild), me.ID)
	if err != nil || slices.Contains(member.RoleIDs, core.ID(input.Role)) {
		t.Fatal("Discord did not confirm removal of the temporary supporter role")
	}
	t.Log("real self-bot grant, one welcome, renewal and expiry/revoke passed; production settings and grant ledger were not used")
}
