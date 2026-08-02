package core

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/config"
	"github.com/6586x57890143/merlin/internal/storage"
)

// AuditWriter is the narrow interface plugins depend on for audit logging.
// The concrete implementation (DB write + #bird-audit-log embed) lands in a
// later milestone; this shape lets code written now compile against it
// without a hard package dependency.
type AuditWriter interface {
	Record(ctx context.Context, guildID, actorID, action, oldValue, newValue string) error
}

// CronSpec describes a recurring job's schedule via a Schedule (see
// schedule.go) — restart-safety comes from persisting last-run state and
// computing next-due = Schedule.Next(last-run) ourselves, which a bare cron
// expression can't do on its own (spec.MD §5).
type CronSpec struct {
	Schedule Schedule
}

// Scheduler is the narrow interface plugins depend on to register recurring
// jobs. The concrete implementation lives in internal/scheduler, which is
// itself a Plugin — it's referenced here only as an interface so this
// package (which internal/scheduler must import for Plugin/Deps) never
// imports internal/scheduler back, avoiding a cycle.
type Scheduler interface {
	// Register adds a job under jobKey (by convention "guildID:name" for
	// per-guild jobs). Returns an error if jobKey is already registered.
	Register(jobKey string, spec CronSpec, fn func(ctx context.Context) error) error
	// Unregister removes jobKey so it never fires again. A no-op if jobKey
	// isn't registered. Exists so plugins whose set of jobs can change at
	// runtime (rotation channels added/removed via /rotation configure, not
	// just at startup) can keep the Scheduler in sync with current settings
	// instead of leaving stale jobs behind.
	Unregister(jobKey string) error
	// RunNow executes the named job immediately, bypassing its normal
	// schedule, and updates its persisted state as a normal run would.
	RunNow(ctx context.Context, jobKey string) error
	// Seed marks jobKey as having just completed successfully at "at",
	// without actually invoking its function. A job the Scheduler has never
	// seen before is otherwise treated as immediately due (correct for a job
	// like rotation's archive sweep, which should start working right away);
	// Seed lets a caller that just registered a job representing something a
	// user freshly configured — e.g. rotation's /rotation configure add —
	// defer its first real fire by one full schedule period instead.
	Seed(ctx context.Context, jobKey string, at time.Time) error
}

// Plugin is the interface every feature module implements. Plugins are
// compiled into the binary and registered at startup — never dynamically
// loaded (spec.MD Design Principle 1). Modularity here is for code
// organization and blast-radius containment, not runtime extensibility.
type Plugin interface {
	// Name is a unique, stable identifier used in logs, audit entries, and
	// panic attribution.
	Name() string

	// Init runs once, before any plugin's Start, in registration order.
	// Plugins register slash command definitions and event subscriptions
	// here. The Discord session may not be connected yet — no gateway or
	// REST calls in Init.
	Init(deps Deps) error

	// Start runs once, after all plugins have Init'd successfully and the
	// session is open. Long-running work is launched here and must respect
	// ctx cancellation.
	Start(ctx context.Context) error

	// Shutdown runs in reverse start order on process exit. Must be
	// idempotent and bounded by ctx's deadline.
	Shutdown(ctx context.Context) error
}

// Deps is the fixed set of shared core services injected into every plugin
// at Init time, so plugins reach for nothing global and never call each
// other directly — they only publish/subscribe on Bus.
type Deps struct {
	Session   *discordgo.Session
	Bus       *EventBus
	Config    *config.Loader
	Perms     *Permissions
	Commands  *CommandRouter
	Audit     AuditWriter
	Logger    *slog.Logger
	DB        *storage.Store
	Scheduler Scheduler
}

// Registry owns plugin lifecycle: Init, Start, and reverse-order Shutdown.
type Registry struct {
	mu      sync.Mutex
	plugins []Plugin
	started []Plugin
	deps    Deps
	log     *slog.Logger
}

func NewRegistry(deps Deps, log *slog.Logger) *Registry {
	return &Registry{deps: deps, log: log}
}

func (r *Registry) Register(p Plugin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins = append(r.plugins, p)
}

// InitAll calls Init on every registered plugin, in registration order,
// under panic recovery. A failure or panic in any plugin's Init aborts the
// whole startup — fail safe, not fail silent (spec.MD Principle 2).
func (r *Registry) InitAll() error {
	for _, p := range r.plugins {
		if err := safeCall(func() error { return p.Init(r.deps) }); err != nil {
			return fmt.Errorf("init plugin %q: %w", p.Name(), err)
		}
	}
	return nil
}

// startupFailureShutdownTimeout bounds the ShutdownAll called from StartAll's
// own failure path (see below) — matching the deadline main.go gives normal
// shutdown, so a plugin with a hung Shutdown can't block a startup failure
// from ever returning.
const startupFailureShutdownTimeout = 10 * time.Second

// StartAll starts plugins in registration order. If any Start fails, it
// shuts down everything already started (reverse order) before returning,
// so the process never limps along half-initialized.
func (r *Registry) StartAll(ctx context.Context) error {
	for _, p := range r.plugins {
		if err := safeCall(func() error { return p.Start(ctx) }); err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), startupFailureShutdownTimeout)
			r.ShutdownAll(shutdownCtx)
			cancel()
			return fmt.Errorf("start plugin %q: %w", p.Name(), err)
		}
		r.started = append(r.started, p)
	}
	return nil
}

// ShutdownAll shuts down every started plugin in reverse start order. Each
// call is panic-isolated so one plugin's broken Shutdown never blocks the
// others from running.
func (r *Registry) ShutdownAll(ctx context.Context) {
	for i := len(r.started) - 1; i >= 0; i-- {
		p := r.started[i]
		if err := safeCall(func() error { return p.Shutdown(ctx) }); err != nil {
			r.log.Error("plugin shutdown error", "plugin", p.Name(), "err", err)
		}
	}
	r.started = nil
}

func safeCall(fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return fn()
}
