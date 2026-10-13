package fleet

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/6586x57890143/cygnet/link"

	"github.com/6586x57890143/merlin/internal/core"
)

var quiet = slog.New(slog.DiscardHandler)

func TestUnconfigured(t *testing.T) {
	for _, c := range [][2]string{{"", ""}, {"ws://x", ""}, {"", "t"}} {
		if Dial(c[0], c[1], quiet) != nil {
			t.Errorf("%v: linked", c)
		}
	}
	// Unlinked, everything is a no-op and refusals are only logged.
	var l *Link
	l.Describe("1", 1, nil)
	l.Run(context.Background())
	srv := refusing(t)
	c := &http.Client{Transport: l.Transport(nil, quiet)}
	if res, err := c.Post(srv.URL+"/api/v10/channels/42/messages", "application/json", nil); err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("%v %v", res, err)
	}
}

func refusing(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code": 50013}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDescribe(t *testing.T) {
	l := Dial("ws://127.0.0.1:1/v1/link", "t", quiet)
	if l.manifest.Load() != nil {
		t.Fatal("a manifest before the commands are finalised")
	}
	l.Describe("1533094679560847460", 1<<1, map[string]core.PermTier{"rapsheet": core.TierMod, "ping": core.TierPublic, "config": core.TierAdmin})
	m := l.manifest.Load()
	if m.App != "1533094679560847460" || m.Perms != "1100333786262" || m.Intents != 2 || m.Wants != 2 || len(m.Serves) != 1 || m.Build == "" {
		t.Fatalf("manifest %+v", m)
	}
	want := []link.Command{{Name: "config", Tier: link.Admin}, {Name: "ping", Tier: link.Public}, {Name: "rapsheet", Tier: "mod"}}
	if len(m.Commands) != 3 {
		t.Fatalf("commands %+v", m.Commands)
	}
	for i, c := range want {
		if m.Commands[i] != c {
			t.Errorf("command %d: %+v, want %+v", i, m.Commands[i], c)
		}
	}

	// Linked but unreachable, a refusal is still logged, and the call to
	// cygnet fails on its own goroutine without holding the request.
	srv := refusing(t)
	c := &http.Client{Transport: l.Transport(nil, quiet)}
	start := time.Now()
	if res, err := c.Post(srv.URL+"/channels/42/messages", "application/json", nil); err != nil || res.StatusCode != http.StatusForbidden || time.Since(start) > 2*time.Second {
		t.Fatalf("%v %v", res, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	l.Run(ctx)
}

func TestBuild(t *testing.T) {
	if b := Build(); b == "" {
		t.Fatal("no build")
	}
}

// A stamped revision wins over whatever Go recorded, and is cut to seven.
func TestBuildPrefersTheStampedRevision(t *testing.T) {
	if got := build("2db258f0123456789abcdef"); got != "2db258f" {
		t.Errorf("build = %q", got)
	}
	if got := build(""); got == "" {
		t.Error("an unstamped build must still name something")
	}
}
