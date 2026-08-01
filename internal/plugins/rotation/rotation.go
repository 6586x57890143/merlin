// Package rotation implements spec.MD §6's "Refresh" channel rotation:
// periodically renaming a channel into a hidden archive (visible only to
// mods, or mods plus a configured whitelist) and replacing it with a clean
// one, reducing the window of retained history a bad-faith actor can trawl
// through for a retroactive mass-report campaign, while preserving a
// moderation trail and staying transparent to members about the retention
// policy.
package rotation

import (
	"context"
	"log/slog"
	"time"

	"github.com/6586x57890143/merlin/internal/config"
	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/scheduler"
)

// sweepInterval is how often each guild's archive-deletion sweep runs.
// Retention is denominated in days, so hourly slop against a day-scale
// window is a non-issue — retention_days is documented as a minimum, not
// an exact-to-the-second promise.
const sweepInterval = time.Hour

// Plugin implements core.Plugin. It registers, at Init time, one recurring
// Scheduler job per configured rotating channel plus one archive-sweep job
// per guild that has any rotating channels — see internal/scheduler for the
// job-registration contract this relies on. No slash commands of its own:
// /admin run-now (internal/scheduler) already covers manual triggering by
// job key.
type Plugin struct {
	ops      DiscordChannelOps
	archives ArchiveStore
	cfg      *config.Loader
	audit    core.AuditWriter
	bus      *core.EventBus
	log      *slog.Logger
	sched    core.Scheduler
	now      func() time.Time
}

func New() *Plugin {
	return &Plugin{now: func() time.Time { return time.Now().UTC() }}
}

func (p *Plugin) Name() string { return "rotation" }

func (p *Plugin) Init(deps core.Deps) error {
	p.ops = deps.Session
	p.cfg = deps.Config
	p.audit = deps.Audit
	p.bus = deps.Bus
	p.log = deps.Logger
	p.sched = deps.Scheduler
	p.archives = NewPostgresArchiveStore(deps.DB.Pool)

	global := deps.Config.Global()
	for guildID, gc := range global.Guilds {
		for _, rc := range gc.RotatingChannels {
			jobKey := scheduler.JobKey(guildID, "rotation:"+rc.ChannelID)
			spec := core.CronSpec{Interval: time.Duration(rc.IntervalHours) * time.Hour}
			if err := p.sched.Register(jobKey, spec, p.makeRotationJob(guildID, rc)); err != nil {
				return err
			}
		}
		if len(gc.RotatingChannels) > 0 {
			sweepKey := scheduler.JobKey(guildID, "rotation-sweep")
			if err := p.sched.Register(sweepKey, core.CronSpec{Interval: sweepInterval}, p.makeSweepJob(guildID)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Plugin) Start(ctx context.Context) error { return nil }

func (p *Plugin) Shutdown(ctx context.Context) error { return nil }
