// Package scheduler implements spec.MD §5's cron core: a generic internal
// service other plugins register recurring jobs with. It persists last-run
// state per job key (guild + job name) so "every N hours" survives a
// process restart without resetting its clock or double-firing.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/robfig/cron/v3"

	"github.com/6586x57890143/merlin/internal/config"
	"github.com/6586x57890143/merlin/internal/core"
)

// CronSpec is an alias for core.CronSpec so Scheduler's Register method
// satisfies the core.Scheduler interface without this package importing a
// second, incompatible definition.
type CronSpec = core.CronSpec

// JobFunc is the work a registered job performs. Its error return drives
// retry/backoff and, past the failure threshold, a status-channel alert.
type JobFunc func(ctx context.Context) error

const (
	tickInterval           = 30 * time.Second
	maxConsecutiveFailures = 5
	backoffBase            = 1 * time.Minute
	backoffMax             = 30 * time.Minute
	maxJitter              = 2 * time.Minute
)

// JobKey namespaces a job name to a guild, matching the "guild + job name"
// key spec.MD §5 describes for persisted last-run state. Every per-guild job
// (rotation, etc.) should use this convention so alerting (which recovers
// the guild ID from the key) and /admin run-now work correctly.
func JobKey(guildID, name string) string {
	return guildID + ":" + name
}

type registeredJob struct {
	key    string
	spec   CronSpec
	fn     JobFunc
	jitter time.Duration

	mu      sync.Mutex
	running bool
}

func (j *registeredJob) tryLock() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running {
		return false
	}
	j.running = true
	return true
}

func (j *registeredJob) unlock() {
	j.mu.Lock()
	j.running = false
	j.mu.Unlock()
}

// Scheduler implements both core.Plugin (so the Registry manages its
// lifecycle) and core.Scheduler (so other plugins can register jobs against
// it via Deps.Scheduler). Construct with New, wire into Deps.Scheduler, and
// register it with the Registry like any other plugin.
type Scheduler struct {
	store JobStateStore
	log   *slog.Logger

	session *discordgo.Session
	cfg     *config.Loader
	perms   *core.Permissions

	cron *cron.Cron
	now  func() time.Time
	wg   sync.WaitGroup

	mu   sync.Mutex
	jobs map[string]*registeredJob

	// alertFunc, if set, replaces the default "post to the guild's
	// status channel" behavior — tests inject a fake so they don't need a
	// live Discord session.
	alertFunc func(ctx context.Context, jobKey, message string) error
}

func New(store JobStateStore, log *slog.Logger) *Scheduler {
	return &Scheduler{
		store: store,
		log:   log,
		jobs:  make(map[string]*registeredJob),
		cron:  cron.New(),
		now:   func() time.Time { return time.Now().UTC() },
	}
}

func (s *Scheduler) Name() string { return "scheduler" }

func (s *Scheduler) Init(deps core.Deps) error {
	s.session = deps.Session
	s.perms = deps.Perms
	s.cfg = deps.Config
	deps.Session.AddHandler(s.handleInteraction)
	return nil
}

func (s *Scheduler) Start(ctx context.Context) error {
	cmd := &discordgo.ApplicationCommand{
		Name:                     "admin",
		Description:              "Admin operations",
		DefaultMemberPermissions: permPtr(discordgo.PermissionManageGuild),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "run-now",
				Description: "Immediately run a scheduled job, bypassing its normal interval",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionString,
						Name:        "job",
						Description: "Job name, as registered (without the guild prefix)",
						Required:    true,
					},
				},
			},
		},
	}
	appID := s.cfg.Global().Discord.AppID
	if err := core.RegisterCommands(s.session, appID, "", []*discordgo.ApplicationCommand{cmd}); err != nil {
		return fmt.Errorf("scheduler: register admin commands: %w", err)
	}

	if _, err := s.cron.AddFunc("@every "+tickInterval.String(), func() { s.tick(context.Background()) }); err != nil {
		return fmt.Errorf("scheduler: schedule tick: %w", err)
	}
	s.cron.Start()
	return nil
}

func (s *Scheduler) Shutdown(ctx context.Context) error {
	stopCtx := s.cron.Stop()
	select {
	case <-stopCtx.Done():
	case <-ctx.Done():
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// Register adds a job under jobKey. Returns an error if spec.Interval isn't
// positive or jobKey is already registered.
func (s *Scheduler) Register(jobKey string, spec CronSpec, fn func(ctx context.Context) error) error {
	if jobKey == "" {
		return errors.New("scheduler: job key must not be empty")
	}
	if spec.Interval <= 0 {
		return errors.New("scheduler: interval must be positive")
	}
	if fn == nil {
		return errors.New("scheduler: job function must not be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.jobs[jobKey]; exists {
		return fmt.Errorf("scheduler: job %q already registered", jobKey)
	}
	s.jobs[jobKey] = &registeredJob{
		key:    jobKey,
		spec:   spec,
		fn:     fn,
		jitter: jitterFor(jobKey, spec.Interval),
	}
	return nil
}

// RunNow executes jobKey immediately, regardless of due-ness, and persists
// its result exactly like a normal scheduled run. Fails if the job is
// already running (per-job lock) or unknown.
func (s *Scheduler) RunNow(ctx context.Context, jobKey string) error {
	s.mu.Lock()
	j, ok := s.jobs[jobKey]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("scheduler: unknown job %q", jobKey)
	}
	if !j.tryLock() {
		return fmt.Errorf("scheduler: job %q is already running", jobKey)
	}
	defer j.unlock()
	return s.execute(ctx, j)
}

func (s *Scheduler) tick(ctx context.Context) {
	s.mu.Lock()
	jobs := make([]*registeredJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.Unlock()

	for _, j := range jobs {
		due, err := s.isDue(ctx, j)
		if err != nil {
			s.log.Error("scheduler: check due", "job", j.key, "err", err)
			continue
		}
		if !due || !j.tryLock() {
			continue
		}
		s.wg.Add(1)
		go func(j *registeredJob) {
			defer s.wg.Done()
			defer j.unlock()
			_ = s.execute(ctx, j) // errors already logged inside execute
		}(j)
	}
}

// isDue decides whether j should run now, given its persisted state:
//   - never attempted: due immediately.
//   - failing, below the alert threshold: backoff since the last attempt.
//   - otherwise (healthy, or failing at/above threshold): the normal
//     interval+jitter since the last success, or since the last attempt if
//     there has never been a success (avoids a hot loop at the threshold).
func (s *Scheduler) isDue(ctx context.Context, j *registeredJob) (bool, error) {
	st, err := s.store.Get(ctx, j.key)
	if err != nil {
		return false, err
	}
	now := s.now()

	switch {
	case st.ConsecutiveFailures == 0 && !st.HasLastRun:
		return true, nil
	case st.ConsecutiveFailures > 0 && st.ConsecutiveFailures < maxConsecutiveFailures:
		return !now.Before(st.LastAttempt.Add(backoffFor(st.ConsecutiveFailures))), nil
	default:
		anchor := st.LastAttempt
		if st.HasLastRun {
			anchor = st.LastRun
		}
		return !now.Before(anchor.Add(j.spec.Interval).Add(j.jitter)), nil
	}
}

func (s *Scheduler) execute(ctx context.Context, j *registeredJob) error {
	err := safeRun(ctx, j.fn)
	now := s.now()
	if err != nil {
		s.log.Error("scheduler: job failed", "job", j.key, "err", err)
		count, ferr := s.store.RecordFailure(ctx, j.key, now)
		if ferr != nil {
			s.log.Error("scheduler: record failure", "job", j.key, "err", ferr)
		}
		if count >= maxConsecutiveFailures {
			s.alert(ctx, j.key, fmt.Sprintf("job %q has failed %d consecutive times: %v", j.key, count, err))
		}
		return err
	}
	if serr := s.store.RecordSuccess(ctx, j.key, now); serr != nil {
		s.log.Error("scheduler: record success", "job", j.key, "err", serr)
	}
	return nil
}

func (s *Scheduler) alert(ctx context.Context, jobKey, msg string) {
	if s.alertFunc != nil {
		if err := s.alertFunc(ctx, jobKey, msg); err != nil {
			s.log.Error("scheduler: alert failed", "job", jobKey, "err", err)
		}
		return
	}
	guildID, _, ok := strings.Cut(jobKey, ":")
	if !ok {
		s.log.Error("scheduler: alert: job key has no guild prefix, cannot route alert", "job", jobKey)
		return
	}
	gc, err := s.cfg.Guild(guildID)
	if err != nil {
		s.log.Error("scheduler: alert: no guild config", "job", jobKey, "err", err)
		return
	}
	if _, err := s.session.ChannelMessageSend(gc.StatusChannelID, msg); err != nil {
		s.log.Error("scheduler: alert: send failed", "job", jobKey, "err", err)
	}
}

func (s *Scheduler) handleInteraction(sess *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	if data.Name != "admin" || len(data.Options) == 0 || data.Options[0].Name != "run-now" {
		return
	}

	var jobName string
	for _, opt := range data.Options[0].Options {
		if opt.Name == "job" {
			jobName = opt.StringValue()
		}
	}

	if err := s.perms.Authorize(i, core.PermCheck{Required: discordgo.PermissionManageGuild, Action: "admin.run_now"}); err != nil {
		respond(sess, i, "You are not allowed to run this command.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	jobKey := JobKey(i.GuildID, jobName)
	if err := s.RunNow(ctx, jobKey); err != nil {
		respond(sess, i, fmt.Sprintf("Failed to run %q: %v", jobName, err))
		return
	}
	respond(sess, i, fmt.Sprintf("Ran %q.", jobName))
}

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func safeRun(ctx context.Context, fn JobFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(ctx)
}

// jitterFor deterministically derives a small, stable per-job offset from
// jobKey so many guilds sharing the same interval don't all fire on the same
// tick — computed once at Register time, never re-randomized.
func jitterFor(jobKey string, interval time.Duration) time.Duration {
	max := min(interval/10, maxJitter)
	if max <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(jobKey))
	return time.Duration(h.Sum32() % uint32(max))
}

func backoffFor(consecutiveFailures int) time.Duration {
	d := backoffBase << (consecutiveFailures - 1)
	if d <= 0 || d > backoffMax {
		return backoffMax
	}
	return d
}

func permPtr(p int64) *int64 { return &p }
