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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/robfig/cron/v3"

	"github.com/6586x57890143/merlin/internal/core"
)

// CronSpec is an alias for core.CronSpec so Scheduler's Register method
// satisfies the core.Scheduler interface without this package importing a
// second, incompatible definition.
type CronSpec = core.CronSpec

// IntervalSchedule is an alias for core.IntervalSchedule, so tests and
// callers in this package can write IntervalSchedule{...} without an extra
// import of internal/core just for this one type.
type IntervalSchedule = core.IntervalSchedule

// JobFunc is the work a registered job performs. Its error return drives
// retry/backoff and, past the failure threshold, a status-channel alert.
type JobFunc func(ctx context.Context) error

// statusChannelResolver is the narrow view of guild settings Scheduler needs
// for failure alerting, implemented by internal/settings.Store.
type statusChannelResolver interface {
	StatusChannelID(guildID string) string
}

const (
	tickInterval           = 30 * time.Second
	maxConsecutiveFailures = 5
	backoffBase            = 1 * time.Minute
	backoffMax             = 30 * time.Minute
	maxJitter              = 2 * time.Minute

	// jobTimeout bounds a single run. Without it, one wedged call (a REST
	// request that never returns, a query behind a lock) holds the job's
	// per-job lock forever: the job silently never runs again, and never
	// fails either, so it never trips the failure alert: a stall that looks
	// exactly like "nothing was due." Generous enough that no legitimate
	// rotation or sweep comes close.
	jobTimeout = 10 * time.Minute

	// stateWriteTimeout bounds the last-run/failure bookkeeping that follows
	// a run, which deliberately outlives the run's own cancelled context.
	// See execute.
	stateWriteTimeout = 10 * time.Second
)

// JobKey namespaces a job name to a guild, matching the "guild + job name"
// key spec.MD §5 describes for persisted last-run state. Every per-guild job
// (rotation, etc.) should use this convention so alerting (which recovers
// the guild ID from the key) and /scheduler run-now work correctly.
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
	store    JobStateStore
	log      *slog.Logger
	settings statusChannelResolver

	session  *discordgo.Session
	commands *core.CommandRouter

	cron *cron.Cron
	now  func() time.Time
	wg   sync.WaitGroup

	// baseCtx is the parent of every scheduled run, cancelled by Shutdown so
	// in-flight jobs stop promptly instead of holding the process open for
	// their full jobTimeout. Set in Start; nil until then.
	baseCtx    context.Context
	cancelJobs context.CancelFunc

	mu   sync.Mutex
	jobs map[string]*registeredJob

	// alertFunc, if set, replaces the default "post to the guild's
	// status channel" behavior. Tests inject a fake so they don't need a
	// live Discord session.
	alertFunc func(ctx context.Context, jobKey, message string) error
}

func New(store JobStateStore, settings statusChannelResolver, log *slog.Logger) *Scheduler {
	return &Scheduler{
		store:    store,
		settings: settings,
		log:      log,
		jobs:     make(map[string]*registeredJob),
		cron:     cron.New(),
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (s *Scheduler) Name() string { return "scheduler" }

func (s *Scheduler) Init(deps core.Deps) error {
	s.session = deps.Session
	s.commands = deps.Commands

	cmd := &discordgo.ApplicationCommand{
		Name:        "scheduler",
		Description: "Inspect and manually trigger registered background jobs",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "list",
				Description: "List every background job registered for this server, with its status",
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "run-now",
				Description: "Immediately run a job, bypassing its normal interval",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:         discordgo.ApplicationCommandOptionString,
						Name:         "job",
						Description:  "Which job to run",
						Required:     true,
						Autocomplete: true,
					},
				},
			},
		},
	}
	s.commands.RegisterCommand(s.Name(), cmd)
	s.commands.Handle("scheduler", "list", core.PermSpec{Tier: core.TierMod, Action: "scheduler.list"}, s.handleList)
	s.commands.Handle("scheduler", "run-now", core.PermSpec{Tier: core.TierMod, Action: "scheduler.run_now"}, s.handleRunNow)
	s.commands.Autocomplete("scheduler", "run-now", s.autocompleteJob)
	s.commands.HandleComponent(s.Name(), schedulerListComponentPrefix, core.PermSpec{Tier: core.TierMod, Action: "scheduler.list"}, s.handleListPage)
	return nil
}

func (s *Scheduler) Start(ctx context.Context) error {
	// Deliberately not derived from ctx: Start's ctx bounds startup, not the
	// scheduler's whole lifetime, and a job inheriting it would be cancelled
	// the instant startup finished.
	s.baseCtx, s.cancelJobs = context.WithCancel(context.Background())
	if _, err := s.cron.AddFunc("@every "+tickInterval.String(), func() { s.tick(s.baseCtx) }); err != nil {
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
	if s.cancelJobs != nil {
		s.cancelJobs()
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

// Register adds a job under jobKey. Returns an error if spec.Schedule is nil
// or fails its own Validate, or jobKey is already registered.
func (s *Scheduler) Register(jobKey string, spec CronSpec, fn func(ctx context.Context) error) error {
	if jobKey == "" {
		return errors.New("scheduler: job key must not be empty")
	}
	if spec.Schedule == nil {
		return errors.New("scheduler: schedule must not be nil")
	}
	if err := spec.Schedule.Validate(); err != nil {
		return fmt.Errorf("scheduler: invalid schedule: %w", err)
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
		jitter: jitterFor(jobKey, spec.Schedule.TypicalPeriod()),
	}
	return nil
}

// Unregister removes jobKey so it never fires again. A no-op if jobKey
// isn't registered; safe to call even while the job is mid-run (its
// in-flight execution finishes normally, it just won't be picked up by any
// later tick).
func (s *Scheduler) Unregister(jobKey string) error {
	s.mu.Lock()
	delete(s.jobs, jobKey)
	s.mu.Unlock()
	return nil
}

// UnregisterGuild removes every job belonging to guildID and returns how
// many were dropped. Called when the bot is removed from a guild: without
// it, that guild's rotation and sweep jobs keep ticking forever against a
// server the bot can no longer see, failing every REST call until they trip
// the consecutive-failure alert, which then tries to post to a status
// channel in the same unreachable guild.
//
// Persisted last-run state is deliberately left in Postgres. It is small,
// and keeping it means a guild that re-adds the bot resumes its old
// schedule instead of treating every job as never-run and therefore
// immediately due, which for rotation would mean rotating on the first
// tick after rejoining.
func (s *Scheduler) UnregisterGuild(guildID string) int {
	prefix := JobKey(guildID, "")
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for key := range s.jobs {
		if strings.HasPrefix(key, prefix) {
			delete(s.jobs, key)
			n++
		}
	}
	return n
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

// Seed marks jobKey as having just completed successfully at "at" (see
// core.Scheduler's doc comment for why a caller would want this). A plain
// passthrough to the same store.RecordSuccess a normal run would call, so a
// job seeded this way is indistinguishable from one that just genuinely ran.
func (s *Scheduler) Seed(ctx context.Context, jobKey string, at time.Time) error {
	if err := s.store.RecordSuccess(ctx, jobKey, at); err != nil {
		return fmt.Errorf("scheduler: seed job %q: %w", jobKey, err)
	}
	return nil
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
			runCtx, cancel := context.WithTimeout(ctx, jobTimeout)
			defer cancel()
			_ = s.execute(runCtx, j) // errors already logged inside execute
		}(j)
	}
}

// isDue decides whether j should run now, given its persisted state:
//   - never attempted: due immediately.
//   - failing, below the alert threshold: backoff since the last attempt.
//   - otherwise (healthy, or failing at/above threshold): the schedule's own
//     next-due instant (plus jitter) since the last success, or since the
//     last attempt if there has never been a success (avoids a hot loop at
//     the threshold).
func (s *Scheduler) isDue(ctx context.Context, j *registeredJob) (bool, error) {
	st, err := s.store.Get(ctx, j.key)
	if err != nil {
		return false, err
	}
	return jobIsDue(st, j.spec.Schedule, j.jitter, s.now()), nil
}

func jobIsDue(st JobState, sched core.Schedule, jitter time.Duration, now time.Time) bool {
	switch {
	case st.ConsecutiveFailures == 0 && !st.HasLastRun:
		return true
	case st.ConsecutiveFailures > 0 && st.ConsecutiveFailures < maxConsecutiveFailures:
		return !now.Before(st.LastAttempt.Add(backoffFor(st.ConsecutiveFailures)))
	default:
		anchor := st.LastAttempt
		if st.HasLastRun {
			anchor = st.LastRun
		}
		return !now.Before(sched.Next(anchor).Add(jitter))
	}
}

// nextDue estimates when j will next become due, for /scheduler list's
// benefit. An estimate, not a promise: a currently-failing job's real next
// attempt depends on backoff, which resets on the next success.
func nextDue(st JobState, sched core.Schedule, jitter time.Duration) (time.Time, bool) {
	if st.ConsecutiveFailures == 0 && !st.HasLastRun {
		return time.Time{}, false // due now
	}
	if st.ConsecutiveFailures > 0 && st.ConsecutiveFailures < maxConsecutiveFailures {
		return st.LastAttempt.Add(backoffFor(st.ConsecutiveFailures)), true
	}
	anchor := st.LastAttempt
	if st.HasLastRun {
		anchor = st.LastRun
	}
	return sched.Next(anchor).Add(jitter), true
}

func (s *Scheduler) execute(ctx context.Context, j *registeredJob) error {
	err := safeRun(ctx, j.fn)
	now := s.now()

	// The outcome has to be persisted even when ctx is exactly what ended
	// the run (jobTimeout expiring, shutdown cancelling it). Writing
	// through the dead context would drop the failure, so the job would keep
	// looking healthy, never back off, and never alert. Detached, with its
	// own bound so a wedged database can't hold the run open indefinitely.
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateWriteTimeout)
	defer cancel()

	if err != nil {
		s.log.Error("scheduler: job failed", "job", j.key, "err", err)
		count, ferr := s.store.RecordFailure(stateCtx, j.key, now)
		if ferr != nil {
			s.log.Error("scheduler: record failure", "job", j.key, "err", ferr)
		}
		if count >= maxConsecutiveFailures {
			s.alert(stateCtx, j.key, fmt.Sprintf("job %q has failed %d consecutive times: %v", j.key, count, err))
		}
		return err
	}
	if serr := s.store.RecordSuccess(stateCtx, j.key, now); serr != nil {
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
	channelID := s.settings.StatusChannelID(guildID)
	if channelID == "" {
		s.log.Error("scheduler: alert: no status channel configured", "job", jobKey)
		return
	}
	if _, err := s.session.ChannelMessageSend(channelID, msg); err != nil {
		s.log.Error("scheduler: alert: send failed", "job", jobKey, "err", err)
	}
}

// jobsForGuild returns guildID's registered jobs (bare name, guild prefix
// stripped), sorted for stable output.
func (s *Scheduler) jobsForGuild(guildID string) []*registeredJob {
	prefix := guildID + ":"
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*registeredJob
	for key, j := range s.jobs {
		if strings.HasPrefix(key, prefix) {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].key < out[k].key })
	return out
}

// schedulerListComponentPrefix namespaces this plugin's pagination buttons
// (core.HandleComponent, spec.MD §4a) so they can't collide with another
// plugin's. See reconcile's job-key comment for why the fully-qualified
// scheduler job keys themselves aren't reused here for anything but display.
const schedulerListComponentPrefix = "scheduler:list:page:"

func (s *Scheduler) handleList(ctx context.Context, sess *discordgo.Session, i *discordgo.InteractionCreate) {
	lines := s.jobLines(ctx, i.GuildID)
	if len(lines) == 0 {
		core.RespondInfo(sess, i, "Registered jobs", "No background jobs are registered for this server.")
		return
	}
	embed, components := s.renderJobsPage(lines, 0)
	if err := core.RespondEmbedWithComponents(sess, i, embed, components); err != nil {
		s.log.Error("scheduler: list response failed", "err", err)
	}
}

// handleListPage re-renders handleList's embed for the page encoded in a
// Prev/Next button's CustomID and edits the message in place. jobLines is
// re-queried fresh rather than reused from the original response, since
// nothing survives in memory between the two interactions.
func (s *Scheduler) handleListPage(ctx context.Context, sess *discordgo.Session, i *discordgo.InteractionCreate, customID string) {
	page, err := core.ParsePaginationPage(customID, schedulerListComponentPrefix)
	if err != nil {
		s.log.Error("scheduler: parse pagination page", "custom_id", customID, "err", err)
		page = 0
	}
	embed, components := s.renderJobsPage(s.jobLines(ctx, i.GuildID), page)
	if err := core.UpdateEmbedWithComponents(sess, i, embed, components); err != nil {
		s.log.Error("scheduler: list page update failed", "err", err)
	}
}

// JobHealth summarizes guildID's registered jobs for /config status: how
// many exist, and how many have failed at least once in a row.
//
// A separate, deliberately coarse view from jobLines, which is for a mod
// reading job-by-job detail. This answers the only question an operator has
// during an incident ("is anything wedged?") in one number, so it can sit
// alongside database reachability and pause state in a single embed.
func (s *Scheduler) JobHealth(ctx context.Context, guildID string) (total, failing int, err error) {
	jobs := s.jobsForGuild(guildID)
	for _, j := range jobs {
		total++
		st, stErr := s.store.Get(ctx, j.key)
		if stErr != nil {
			// Can't read the state, so can't claim the job is healthy.
			// Counting it as failing is the fail-closed answer for a
			// health check.
			failing++
			err = stErr
			continue
		}
		if st.ConsecutiveFailures > 0 {
			failing++
		}
	}
	return total, failing, err
}

// jobLines formats one line per guildID job, newest logic unchanged from
// before pagination existed, just split out so both handleList and
// handleListPage build from the same up-to-date source.
func (s *Scheduler) jobLines(ctx context.Context, guildID string) []string {
	jobs := s.jobsForGuild(guildID)
	prefix := guildID + ":"
	lines := make([]string, 0, len(jobs))
	for _, j := range jobs {
		name := strings.TrimPrefix(j.key, prefix)
		st, err := s.store.Get(ctx, j.key)
		if err != nil {
			lines = append(lines, fmt.Sprintf("`%s` · error reading state: %v", name, err))
			continue
		}
		last := "never"
		if st.HasLastRun {
			last = st.LastRun.Format(time.RFC3339)
		}
		next := "due now"
		if due, ok := nextDue(st, j.spec.Schedule, j.jitter); ok {
			next = due.Format(time.RFC3339)
		}
		lines = append(lines, fmt.Sprintf("`%s` · last run: %s, next due: %s, consecutive failures: %d", name, last, next, st.ConsecutiveFailures))
	}
	return lines
}

func (s *Scheduler) renderJobsPage(lines []string, page int) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	pageLines, clampedPage, totalPages := core.Paginate(lines, page)
	embed := core.NewEmbed(core.ColorInfo, "Registered jobs", strings.Join(pageLines, "\n"))
	return embed, core.PaginationRow(schedulerListComponentPrefix, clampedPage, totalPages)
}

// handleRunNow defers before running: a rotation walks a guild's channels and
// posts several messages, which comfortably outlives Discord's 3-second
// response deadline. Responding only after the job finished meant a job that
// ran perfectly still surfaced as "the application did not respond."
func (s *Scheduler) handleRunNow(ctx context.Context, sess *discordgo.Session, i *discordgo.InteractionCreate) {
	var jobName string
	if opt, ok := core.LeafArgs(i)["job"]; ok {
		jobName = opt.StringValue()
	}

	if err := core.DeferResponse(sess, i); err != nil {
		s.log.Error("scheduler: defer run-now response failed", "job", jobName, "err", err)
		return
	}

	// Deliberately not the interaction's own ctx: that's cancelled as soon
	// as this handler returns, which would kill a job the moment it was
	// handed off. jobTimeout bounds it instead, same as a scheduled run.
	runCtx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()

	var err error
	if runErr := s.RunNow(runCtx, JobKey(i.GuildID, jobName)); runErr != nil {
		err = core.FollowUpErr(sess, i, fmt.Sprintf("Failed to run %q", jobName), runErr)
	} else {
		err = core.FollowUpOK(sess, i, "Job run", fmt.Sprintf("Ran `%s`.", jobName))
	}
	if err != nil {
		s.log.Error("scheduler: run-now follow-up failed", "job", jobName, "err", err)
	}
}

// maxAutocompleteChoices is Discord's hard limit on an autocomplete
// response. Exceeding it doesn't truncate: the whole response is rejected
// and the user sees no suggestions at all, so a guild with many rotating
// channels would lose autocomplete entirely.
const maxAutocompleteChoices = 25

func (s *Scheduler) autocompleteJob(ctx context.Context, i *discordgo.InteractionCreate, focusedOption, focusedValue string) []*discordgo.ApplicationCommandOptionChoice {
	var choices []*discordgo.ApplicationCommandOptionChoice
	for _, j := range s.jobsForGuild(i.GuildID) {
		name := strings.TrimPrefix(j.key, i.GuildID+":")
		if focusedValue != "" && !strings.Contains(name, focusedValue) {
			continue
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: name, Value: name})
		if len(choices) == maxAutocompleteChoices {
			break
		}
	}
	return choices
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
// tick, computed once at Register time, never re-randomized.
//
// The hash must be 64-bit: a duration in nanoseconds outgrows uint32 at just
// 4.3 seconds, so folding a 32-bit hash into the bound silently capped every
// job's jitter at ~4s regardless of maxJitter, leaving the thundering-herd
// spread this exists to provide almost entirely absent.
func jitterFor(jobKey string, interval time.Duration) time.Duration {
	bound := min(interval/10, maxJitter)
	if bound <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(jobKey))
	return time.Duration(h.Sum64() % uint64(bound))
}

func backoffFor(consecutiveFailures int) time.Duration {
	d := backoffBase << (consecutiveFailures - 1)
	if d <= 0 || d > backoffMax {
		return backoffMax
	}
	return d
}
