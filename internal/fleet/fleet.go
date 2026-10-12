// Package fleet is merlin's link to cygnet, the hub the birds (skua, merlin,
// peregrine) call each other through. merlin answers merlin.ping and
// merlin.manifest, which is what cygnet's fleet card and install links read,
// and tells cygnet when Discord refuses her a permission, so cygnet can grant
// it. Without MERLIN_CYGNET_URL and MERLIN_CYGNET_TOKEN merlin runs as before.
package fleet

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/6586x57890143/cygnet/link"
	"github.com/6586x57890143/cygnet/link/perms"

	"github.com/6586x57890143/merlin/internal/core"
)

// Perms is what merlin's plugins use between them: README's widest install
// link (the base three, AI moderation's and rapsheet's), so cygnet can grant
// the lot to a server she runs.
//
// ponytail: one constant, as merlin's plugins don't declare what they use;
// a Perms per plugin, unioned here, once one needs to differ.
const Perms = 1100333786262

// Link is merlin on the hub: nil when it isn't configured.
type Link struct {
	c        *link.Client
	log      *slog.Logger
	manifest atomic.Pointer[link.Manifest]
}

// Dial makes the link from url and token, or nil when either is empty. It
// doesn't connect until Run.
func Dial(url, token string, log *slog.Logger) *Link {
	if url == "" || token == "" {
		return nil
	}
	l := &Link{log: log}
	l.c = link.New(link.Config{URL: url, Token: token, Build: Build(), Log: log})
	l.c.Handle("ping", func(context.Context, link.Call) (any, error) { return nil, nil })
	// Read once the commands are finalised; a call before then is refused
	// busy, as the hub would for a full link.
	l.c.Handle(link.ManifestMethod, func(context.Context, link.Call) (any, error) {
		if m := l.manifest.Load(); m != nil {
			return m, nil
		}
		return nil, link.Err(link.Busy, "starting")
	})
	return l
}

// Run keeps the link up until ctx ends, then says goaway.
func (l *Link) Run(ctx context.Context) {
	if l != nil {
		_ = l.c.Run(ctx)
	}
}

// Describe is what merlin.manifest answers from now on: her app, the
// intents she identifies with (which are what she asks for: Discord turns a
// connection down outright for a privileged one the portal hasn't granted)
// and each command with the lowest tier under it.
func (l *Link) Describe(app string, intents uint64, tiers map[string]core.PermTier) {
	if l == nil {
		return
	}
	m := link.Manifest{Build: Build(), App: app, Perms: strconv.FormatUint(Perms, 10), Intents: intents, Wants: intents, Commands: []link.Command{}}
	for name, t := range tiers {
		m.Commands = append(m.Commands, link.Command{Name: name, Tier: tier(t)})
	}
	slices.SortFunc(m.Commands, func(a, b link.Command) int {
		if a.Name < b.Name {
			return -1
		}
		return 1
	})
	l.manifest.Store(&m)
}

// tier is merlin's tier in the fleet's shared terms; a mod tier is its own.
func tier(t core.PermTier) link.Tier {
	switch t {
	case core.TierPublic:
		return link.Public
	case core.TierMod:
		return "mod"
	}
	return link.Admin
}

// Transport is base reporting every request Discord refuses for want of a
// permission: to cygnet when linked, to the log either way.
func (l *Link) Transport(base http.RoundTripper, log *slog.Logger) http.RoundTripper {
	return perms.Watch(base, func(m perms.Missing) {
		log.Warn("discord refused merlin for want of a permission", "op", m.Op, "guild", m.Guild, "channel", m.Channel)
		if l == nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := l.c.Call(ctx, perms.MissingMethod, m, nil); err != nil {
				log.Warn("telling cygnet about a refusal", "op", m.Op, "err", err)
			}
		}()
	})
}

// revision is the commit the image was built from, set at link time by the
// Dockerfile (-X .../internal/fleet.revision=$REVISION, which CI passes as
// github.sha). The Docker build context has no .git, so without it Go
// records no vcs.revision and every deployed merlin reported "dev".
var revision string

// Build is the commit merlin was built from: the one the build stamped in,
// else the one Go recorded from a local checkout, else "dev".
func Build() string { return build(revision) }

func build(stamped string) string {
	if len(stamped) >= 7 {
		return stamped[:7]
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return s.Value[:7]
			}
		}
	}
	return "dev"
}
