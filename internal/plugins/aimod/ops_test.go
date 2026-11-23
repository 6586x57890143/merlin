package aimod

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"

	"github.com/6586x57890143/merlin/internal/discordguard"
)

type openGate struct{}

func (openGate) WritesPaused(string) bool { return false }
func (openGate) WritesDryRun(string) bool { return false }

// answers replies to each path with the body of the longest key it
// contains.
type answers map[string]string

func (a answers) RoundTrip(r *http.Request) (*http.Response, error) {
	body, best := "{}", ""
	for k, v := range a {
		if strings.Contains(r.URL.Path, k) && len(k) > len(best) {
			body, best = v, k
		}
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

// GuardOps reads Discord's answers into aimod's views: a member's shown
// name and roles, the guild's owner and role permissions, history lines and
// only the webhooks merlin can post through.
func TestGuardOpsReadsDiscordIntoAimodsViews(t *testing.T) {
	r := rest.New(rest.NewClient("t", rest.WithHTTPClient(&http.Client{Transport: answers{
		"/guilds/100/members/":   `{"nick":"Nick","user":{"id":"4","username":"u"},"roles":["7"],"joined_at":"2026-01-01T00:00:00Z"}`,
		"/guilds/100":            `{"id":"100","name":"G","owner_id":"5","roles":[{"id":"7","name":"mods","permissions":"8","position":1,"color":0}]}`,
		"/channels/2/messages":   `[{"id":"9","channel_id":"2","content":"hi","author":{"id":"4","username":"u"}},{"id":"8","channel_id":"2","content":"","author":{"id":"6","username":"v"}}]`,
		"/channels/2/messages/9": `{"id":"9","channel_id":"2","content":"now","author":{"id":"4","username":"u"}}`,
		"/channels/2":            `{"id":"2","type":0,"guild_id":"100","name":"c"}`,
		"/channels/3":            `{"id":"3","type":11,"guild_id":"100","parent_id":"2","name":"t","thread_metadata":{}}`,
		"/channels/2/webhooks":   `[{"id":"11","type":1,"name":"merlin","token":"tok","channel_id":"2"},{"id":"12","type":2,"name":"follow","channel_id":"2"}]`,
	}})))
	g := discordguard.New(r, openGate{}, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	ops := GuardOps(g.For("100"))

	m, err := ops.Member("100", "4")
	if err != nil || m.Name != "Nick" || len(m.RoleIDs) != 1 || m.RoleIDs[0] != "7" {
		t.Fatalf("member %+v, err %v", m, err)
	}
	gd, err := ops.Guild("100")
	if err != nil || gd.Name != "G" || gd.OwnerID != "5" || !gd.RolePerms["7"].Has(discord.PermissionAdministrator) {
		t.Fatalf("guild %+v, err %v", gd, err)
	}
	one, err := ops.Message("2", "9")
	if err != nil || one.Content != "now" || one.AuthorID != "4" {
		t.Fatalf("message %+v, err %v", one, err)
	}
	lines, err := ops.History("2", "", 10)
	if err != nil || len(lines) != 2 || lines[0].AuthorID != "4" || lines[0].Content != "hi" {
		t.Fatalf("history %+v, err %v", lines, err)
	}
	hooks, err := ops.Webhooks("2")
	if err != nil || len(hooks) != 1 || hooks[0].ID != "11" || hooks[0].Token != "tok" || hooks[0].ThreadID != "" {
		t.Fatalf("webhooks %+v, err %v", hooks, err)
	}
	// A thread's webhooks are its parent's, posted through into the thread.
	if hooks, err = ops.Webhooks("3"); err != nil || len(hooks) != 1 || hooks[0].ThreadID != "3" {
		t.Fatalf("thread webhooks %+v, err %v", hooks, err)
	}
}
