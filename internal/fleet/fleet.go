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
	cards "github.com/6586x57890143/cygnet/support"

	"github.com/6586x57890143/merlin/internal/core"
)

// Link is merlin on the hub: nil when it isn't configured.
type Link struct {
	c        *link.Client
	log      *slog.Logger
	manifest atomic.Pointer[link.Manifest]
	serves   []string
}

// Dial makes the link from url and token, or nil when either is empty. It
// doesn't connect until Run. attest is what CI signed for this build
// (MERLIN_ATTEST), shown to cygnet at hello; a bad one is dropped and merlin
// links unsigned, which a cygnet that pins her key turns away.
func Dial(url, token, attest string, log *slog.Logger) *Link {
	if url == "" || token == "" {
		return nil
	}
	l := &Link{log: log}
	var a *link.Attestation
	if attest != "" {
		if got, err := link.DecodeAttestation(attest); err != nil {
			log.Warn("MERLIN_ATTEST doesn't decode; linking unsigned", "err", err)
		} else {
			a = &got
		}
	}
	l.c = link.New(link.Config{URL: url, Token: token, Build: Build(), Attest: a, Log: log})
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

// Serve answers merlin.<verb> with h. Call it before Run; without the link
// it does nothing.
func (l *Link) Serve(verb string, h link.Handler) {
	if l == nil {
		return
	}
	l.c.Handle(verb, h)
	l.serves = append(l.serves, verb)
}

// Here asks cygnet whether she is a member of guildID (cygnet.here).
func (l *Link) Here(ctx context.Context, guildID string) (bool, error) {
	if l == nil {
		return false, nil
	}
	var a struct {
		Here bool `json:"here"`
	}
	err := l.c.Call(ctx, "cygnet.here", map[string]string{"guild": guildID}, &a)
	return a.Here, err
}

// Backer is one supporter as cygnet.supporters lists them.
type Backer struct {
	User  string `json:"user"`
	Gold  bool   `json:"gold"`
	Until int64  `json:"until"` // Unix milliseconds
}

// Supporters asks cygnet who backs the birds now (cygnet.supporters).
func (l *Link) Supporters(ctx context.Context) ([]Backer, error) {
	var a struct {
		Supporters []Backer `json:"supporters"`
	}
	err := l.c.Call(ctx, "cygnet.supporters", nil, &a)
	return a.Supporters, err
}

// PaymentMethods is the public rails on offer, without checkout or user data.
func (l *Link) PaymentMethods(ctx context.Context) ([]cards.Method, error) {
	var result struct {
		Rails []cards.Method `json:"rails"`
	}
	err := l.c.Call(ctx, "cygnet.payment_methods", nil, &result)
	return result.Rails, err
}

// Run keeps the link up until ctx ends, then says goaway.
func (l *Link) Run(ctx context.Context) {
	if l != nil {
		_ = l.c.Run(ctx)
	}
}

// Describe is what merlin.manifest answers from now on: her app, the
// intents she identifies with (what the Developer Portal grants), the
// permissions her install asks for (core.Install) and each command with
// the lowest tier under it.
func (l *Link) Describe(app string, intents, perms uint64, tiers map[string]core.PermTier) {
	if l == nil {
		return
	}
	m := link.Manifest{Build: Build(), App: app, Perms: strconv.FormatUint(perms, 10), Intents: intents, Wants: intents, Serves: append([]string{"ping"}, l.serves...), Commands: []link.Command{}}
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
