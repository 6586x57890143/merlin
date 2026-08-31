package aimod

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"github.com/6586x57890143/merlin/internal/secret"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/voice"
)

// In-memory doubles for the whole plugin, in the shape of
// internal/plugins/roles' fakes_test.go: every seam this package defines has
// one here, so the ladder can be exercised end to end with no database, no
// Discord session and no API key.

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- fakeStore ---

type fakeStore struct {
	mu        sync.Mutex
	cfg       map[string]Config
	spend     map[string]Spend
	incidents []Incident
	nextID    int64

	configErr   error
	spendErr    error
	incidentErr error

	funding   map[string]Funding
	triage    map[string]storedTriage
	triageErr error
	// triageSaves counts persistence calls, so a test can assert that the
	// model is written on a cadence rather than on every single message.
	triageSaves int
	fundingErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		cfg: make(map[string]Config), spend: make(map[string]Spend),
		funding: make(map[string]Funding), triage: make(map[string]storedTriage),
	}
}

func (f *fakeStore) setConfig(cfg Config) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cfg.BucketActions == nil {
		cfg.BucketActions = map[Bucket]Action{}
	}
	f.cfg[cfg.GuildID] = cfg
}

func (f *fakeStore) Config(_ context.Context, guildID string) (Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.configErr != nil {
		return Config{}, f.configErr
	}
	if cfg, ok := f.cfg[guildID]; ok {
		return cfg, nil
	}
	return defaultConfig(guildID), nil
}

func (f *fakeStore) mutate(guildID string, fn func(*Config)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg, ok := f.cfg[guildID]
	if !ok {
		cfg = defaultConfig(guildID)
	}
	fn(&cfg)
	f.cfg[guildID] = cfg
	return nil
}

// testState is the budgetState the ladder tests hand to the classifier
// passes: one key on the gateway that pins privacy prefs, which is what
// every test written before there was a second gateway assumed.
func testState() budgetState {
	return budgetState{APIKey: "key", Spec: openRouter}
}

func (f *fakeStore) SetAPIKey(_ context.Context, g, _ string, sealed []byte) error {
	return f.mutate(g, func(c *Config) { c.APIKeySealed = sealed })
}
func (f *fakeStore) SetMode(_ context.Context, g string, m Mode) error {
	return f.mutate(g, func(c *Config) { c.Mode = m })
}
func (f *fakeStore) SetBudget(_ context.Context, g string, usd float64) error {
	return f.mutate(g, func(c *Config) { c.DailyBudgetUSD = usd })
}
func (f *fakeStore) SetEvidenceHours(_ context.Context, g string, h int) error {
	return f.mutate(g, func(c *Config) { c.EvidenceHours = h })
}
func (f *fakeStore) SetModels(_ context.Context, g string, fast, deep []string) error {
	return f.mutate(g, func(c *Config) { c.FastModels, c.DeepModels = fast, deep })
}
func (f *fakeStore) SetBucketAction(_ context.Context, g string, b Bucket, a Action) error {
	return f.mutate(g, func(c *Config) {
		if c.BucketActions == nil {
			c.BucketActions = map[Bucket]Action{}
		}
		c.BucketActions[b] = a
	})
}
func (f *fakeStore) SetExemptChannels(_ context.Context, g string, ids []string) error {
	return f.mutate(g, func(c *Config) { c.ExemptChannelIDs = ids })
}
func (f *fakeStore) SetExemptRoles(_ context.Context, g string, ids []string) error {
	return f.mutate(g, func(c *Config) { c.ExemptRoleIDs = ids })
}
func (f *fakeStore) SetSanctionAction(_ context.Context, g string, a SanctionAction) error {
	return f.mutate(g, func(c *Config) { c.SanctionAction = a })
}
func (f *fakeStore) SetSanctionOptIn(_ context.Context, g string, ids []string) error {
	return f.mutate(g, func(c *Config) { c.SanctionOptInUserIDs = ids })
}

func (f *fakeStore) SetCalibration(_ context.Context, g string, active, pending []CalibrationExample, ranAt time.Time) error {
	return f.mutate(g, func(c *Config) {
		c.Calibration, c.CalibrationPending = active, pending
		if !ranAt.IsZero() {
			c.CalibrationRanAt = ranAt
		}
	})
}

func (f *fakeStore) SetCalibrationMode(_ context.Context, g string, mode CalibrationMode) error {
	return f.mutate(g, func(c *Config) { c.CalibrationMode = mode })
}

func (f *fakeStore) IncidentsSince(_ context.Context, g string, since time.Time, limit int) ([]Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.incidentErr != nil {
		return nil, f.incidentErr
	}
	var out []Incident
	for _, inc := range f.incidents {
		if inc.GuildID == g && !inc.CreatedAt.Before(since) {
			out = append(out, inc)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func spendKey(guildID string, day time.Time) string { return guildID + "|" + day.Format("2006-01-02") }

func (f *fakeStore) AddSpend(_ context.Context, g string, day time.Time, u Usage, deep bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sp := f.spend[spendKey(g, day)]
	sp.Day = day
	sp.SpentUSD += u.Cost
	if deep {
		sp.DeepCalls++
		sp.DeepPromptTokens += u.PromptTokens
		sp.DeepCompletionTokens += u.CompletionTokens
	} else {
		sp.FastCalls++
		sp.FastPromptTokens += u.PromptTokens
		sp.FastCompletionTokens += u.CompletionTokens
	}
	// Not split by tier, matching pgStore: reasoning is a property of the
	// endpoint, not of the pass.
	sp.ReasoningTokens += u.CompletionTokensDetails.ReasoningTokens
	f.spend[spendKey(g, day)] = sp
	return nil
}

func (f *fakeStore) AddScanned(_ context.Context, g string, day time.Time, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sp := f.spend[spendKey(g, day)]
	sp.Day = day
	sp.Scanned += n
	f.spend[spendKey(g, day)] = sp
	return nil
}

func (f *fakeStore) SpendToday(_ context.Context, g string, day time.Time) (Spend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spendErr != nil {
		return Spend{}, f.spendErr
	}
	return f.spend[spendKey(g, day)], nil
}

func (f *fakeStore) SpendSince(_ context.Context, g string, since time.Time) ([]Spend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Spend
	for _, sp := range f.spend {
		if !sp.Day.Before(since) {
			out = append(out, sp)
		}
	}
	return out, nil
}

func (f *fakeStore) Funding(_ context.Context, guildID string) (Funding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fundingErr != nil {
		return Funding{}, f.fundingErr
	}
	fu, ok := f.funding[guildID]
	if !ok {
		return Funding{GuildID: guildID}, nil
	}
	return fu, nil
}

func (f *fakeStore) SetFundingAddress(_ context.Context, guildID, address, setBy string, at time.Time, baseline float64, balances map[string]float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fundingErr != nil {
		return f.fundingErr
	}
	// Mirrors the real statement: re-pointing resets the totals, and the
	// baseline is stored with checked_at so the first poll counts nothing.
	f.funding[guildID] = Funding{
		GuildID: guildID, Address: address, SetBy: setBy, SetAt: at,
		BalanceUSD: baseline, CheckedAt: at, Balances: balances,
	}
	return nil
}

func (f *fakeStore) SetTriageMode(_ context.Context, guildID string, mode TriageMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg := f.cfg[guildID]
	cfg.GuildID = guildID
	cfg.TriageMode = mode
	f.cfg[guildID] = cfg
	return nil
}

func (f *fakeStore) TriageModel(_ context.Context, guildID string) ([]byte, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.triageErr != nil {
		return nil, 0, f.triageErr
	}
	m := f.triage[guildID]
	return m.raw, m.examples, nil
}

func (f *fakeStore) SaveTriageModel(_ context.Context, guildID string, raw []byte, examples int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.triageErr != nil {
		return f.triageErr
	}
	if f.triage == nil {
		f.triage = map[string]storedTriage{}
	}
	f.triage[guildID] = storedTriage{raw: raw, examples: examples}
	f.triageSaves++
	return nil
}

func (f *fakeStore) ClearFunding(_ context.Context, guildID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fundingErr != nil {
		return f.fundingErr
	}
	delete(f.funding, guildID)
	return nil
}

func (f *fakeStore) UpdateFundingBalance(_ context.Context, guildID string, balance, donation float64, balances map[string]float64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fundingErr != nil {
		return f.fundingErr
	}
	fu, ok := f.funding[guildID]
	if !ok {
		return nil
	}
	fu.BalanceUSD = balance
	fu.CheckedAt = at
	fu.Balances = balances
	fu.ReceivedUSD += donation
	if donation > 0 {
		fu.Donations++
	}
	f.funding[guildID] = fu
	return nil
}

func (f *fakeStore) RecordIncident(_ context.Context, inc Incident) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.incidentErr != nil {
		return 0, f.incidentErr
	}
	f.nextID++
	inc.ID = f.nextID
	f.incidents = append(f.incidents, inc)
	return inc.ID, nil
}

func (f *fakeStore) IncidentByMessage(_ context.Context, g, messageID string) (Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, inc := range f.incidents {
		if inc.GuildID == g && inc.MessageID == messageID {
			return inc, nil
		}
	}
	return Incident{}, ErrNoIncident
}

func (f *fakeStore) MarkUndone(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.incidents {
		if f.incidents[i].ID == id {
			f.incidents[i].Undone = true
		}
	}
	return nil
}

func (f *fakeStore) CountSanctions(_ context.Context, g, userID string, since time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, inc := range f.incidents {
		if inc.GuildID == g && inc.AuthorID == userID && inc.Action != ActionFlag && !inc.Undone && !inc.CreatedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) PendingFlags(_ context.Context, g, userID string, since time.Time) ([]Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Incident
	for _, inc := range f.incidents {
		if inc.GuildID == g && inc.AuthorID == userID && inc.Action == ActionFlag &&
			!inc.Undone && !inc.CreatedAt.Before(since) {
			out = append(out, inc)
		}
	}
	return out, nil
}

func (f *fakeStore) MarkActioned(_ context.Context, id int64, action Action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.incidents {
		if f.incidents[i].ID == id {
			f.incidents[i].Action = action
		}
	}
	return nil
}

func (f *fakeStore) PruneEvidence(context.Context) (int64, error) { return 0, nil }

func (f *fakeStore) PruneBefore(context.Context, time.Time) (int64, error) { return 0, nil }

func (f *fakeStore) recorded() []Incident {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Incident, len(f.incidents))
	copy(out, f.incidents)
	return out
}

// --- fakeClassifier: a scripted model ---

type fakeClassifier struct {
	mu sync.Mutex
	// fast and deep are the raw JSON bodies returned, in order. A shorter
	// list than the number of calls repeats the last entry, so a test only
	// has to script the answers it cares about.
	fast, deep []string
	// calibration is the weekly review's answer, kept separate because it is
	// a third shape rather than a variant of the deep pass.
	calibration        []string
	calibrationErr     error
	calibrationCalls   int
	lastCalibrationReq chatRequest
	// deepDelay makes a deep call take measurable time, so a test can tell
	// serial escalation from concurrent. inDeep and maxDeepParallel record
	// how many were actually in flight at once: a wall-clock assertion would
	// say the same thing but flake on a loaded machine, and this fails
	// deterministically the moment escalation goes back to being serial.
	deepDelay       time.Duration
	inDeep          int
	maxDeepParallel int
	fastErr         error
	deepErr         error
	// deepErrFor fails a deep call only on one gateway, which is what the
	// deep rung's cross-gateway fallback needs in order to be provable: the
	// first attempt has to fail and the second has to succeed.
	deepErrFor *providerSpec
	// deepSpecs is which gateway each deep call went to, in order.
	deepSpecs   []*providerSpec
	usage       Usage
	fastCalls   int
	deepCalls   int
	lastFastReq chatRequest
	lastDeepReq chatRequest
}

// lastUserMessage returns the user half of the last fast-pass request, which
// is where the numbered batch and its reply context are built.
func (f *fakeClassifier) lastUserMessage() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.lastFastReq.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

// enterDeep marks one deep call as started, records the high-water mark of
// how many were in flight together, and returns how long to sleep.
func (f *fakeClassifier) enterDeep(req chatRequest) time.Duration {
	if !isDeep(req) {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inDeep++
	f.maxDeepParallel = max(f.maxDeepParallel, f.inDeep)
	return f.deepDelay
}

func (f *fakeClassifier) leaveDeep(req chatRequest) {
	if !isDeep(req) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inDeep--
}

func (f *fakeClassifier) peakDeepParallel() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxDeepParallel
}

// isDeep distinguishes the two passes by the schema each one asks for, which
// is the same thing the real client sends and so cannot drift from it.
func isDeep(req chatRequest) bool {
	return schemaName(req) == "verdict"
}

// isCalibration picks out the weekly review, which is neither pass: it uses
// the deep model stack but asks for a different answer entirely, so a fake
// that lumped it in with the deep pass would hand it a verdict to parse.
func isCalibration(req chatRequest) bool {
	return schemaName(req) == "calibration"
}

func schemaName(req chatRequest) string {
	if req.ResponseFormat == nil {
		return ""
	}
	return req.ResponseFormat.JSONSchema.Name
}

func (f *fakeClassifier) Chat(_ context.Context, _ string, req chatRequest) (string, Usage, error) {
	// Outside the lock, deliberately. The delay exists so concurrent
	// escalations actually overlap, and sleeping while holding f.mu would
	// serialize them again and let a serial implementation pass.
	if d := f.enterDeep(req); d > 0 {
		time.Sleep(d)
	}
	defer f.leaveDeep(req)

	f.mu.Lock()
	defer f.mu.Unlock()
	if isCalibration(req) {
		f.calibrationCalls++
		f.lastCalibrationReq = req
		if f.calibrationErr != nil {
			return "", f.usage, f.calibrationErr
		}
		return pick(f.calibration, f.calibrationCalls), f.usage, nil
	}
	if isDeep(req) {
		f.deepCalls++
		f.lastDeepReq = req
		f.deepSpecs = append(f.deepSpecs, req.spec)
		if f.deepErr != nil {
			return "", f.usage, f.deepErr
		}
		if f.deepErrFor != nil && req.spec == f.deepErrFor {
			return "", f.usage, errors.New("that gateway could not answer")
		}
		return pick(f.deep, f.deepCalls), f.usage, nil
	}
	f.fastCalls++
	f.lastFastReq = req
	if f.fastErr != nil {
		return "", f.usage, f.fastErr
	}
	return pick(f.fast, f.fastCalls), f.usage, nil
}

func pick(list []string, nth int) string {
	if len(list) == 0 {
		return `{"v":[]}`
	}
	if nth > len(list) {
		return list[len(list)-1]
	}
	return list[nth-1]
}

func (f *fakeClassifier) counts() (fast, deep int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fastCalls, f.deepCalls
}

// --- fakeOps: in-memory DiscordOps ---

type fakeOps struct {
	mu sync.Mutex

	deleted   []string // message IDs
	posted    []*discordgo.WebhookParams
	timeouts  map[string]time.Duration
	dms       []*discordgo.MessageSend
	webhooks  int
	deleteErr error

	guild      *discordgo.Guild
	guildErr   error
	members    map[string]*discordgo.Member
	history    []*discordgo.Message
	historyErr error
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		timeouts: make(map[string]time.Duration),
		members:  make(map[string]*discordgo.Member),
		guild:    &discordgo.Guild{ID: "g1", Name: "Test Guild", OwnerID: "owner"},
	}
}

func (f *fakeOps) ChannelMessageDelete(_, messageID string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, messageID)
	return nil
}

func (f *fakeOps) ChannelMessages(string, int, string, string, string, ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.history, f.historyErr
}

func (f *fakeOps) ChannelWebhooks(string, ...discordgo.RequestOption) ([]*discordgo.Webhook, error) {
	return nil, nil
}

func (f *fakeOps) WebhookCreate(string, string, string, ...discordgo.RequestOption) (*discordgo.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.webhooks++
	return &discordgo.Webhook{ID: "w1", Token: "t1"}, nil
}

func (f *fakeOps) WebhookExecute(_, _ string, data *discordgo.WebhookParams, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posted = append(f.posted, data)
	return nil
}

func (f *fakeOps) GuildMember(_, userID string, _ ...discordgo.RequestOption) (*discordgo.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.members[userID]; ok {
		return m, nil
	}
	return &discordgo.Member{User: &discordgo.User{ID: userID, Username: "member" + userID}}, nil
}

func (f *fakeOps) GuildMemberTimeout(_, userID string, until *time.Time, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if until == nil {
		delete(f.timeouts, userID)
		return nil
	}
	f.timeouts[userID] = time.Until(*until)
	return nil
}

func (f *fakeOps) Guild(string, ...discordgo.RequestOption) (*discordgo.Guild, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.guildErr != nil {
		return nil, f.guildErr
	}
	return f.guild, nil
}

func (f *fakeOps) UserChannelCreate(recipientID string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: "dm-" + recipientID}, nil
}

func (f *fakeOps) ChannelMessageSendComplex(_ string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dms = append(f.dms, data)
	return &discordgo.Message{}, nil
}

func (f *fakeOps) snapshot() (deleted []string, posted []*discordgo.WebhookParams) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...), append([]*discordgo.WebhookParams(nil), f.posted...)
}

// --- fakeAudit ---

type fakeAudit struct {
	mu      sync.Mutex
	entries []auditEntry
	err     error
}

type auditEntry struct {
	guildID, actorID, action, oldValue, newValue string
}

func (f *fakeAudit) Record(_ context.Context, guildID, actorID, action, oldValue, newValue string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, auditEntry{guildID, actorID, action, oldValue, newValue})
	return f.err
}

func (f *fakeAudit) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e.action)
	}
	return out
}

// --- fakeJailer ---

type fakeJailer struct {
	mu        sync.Mutex
	calls     []jailCall
	err       error
	refuseAll bool
}

type jailCall struct {
	userID    string
	duration  time.Duration
	consented bool
}

func (f *fakeJailer) JailAutomatic(_ context.Context, _, userID string, d time.Duration, _ string, consented bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuseAll && !consented {
		return errors.New("roles: automatic jail refused: target is an admin")
	}
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, jailCall{userID: userID, duration: d, consented: consented})
	return nil
}

func (f *fakeJailer) jailed() []jailCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]jailCall(nil), f.calls...)
}

// The real voice catalogue, not a fake one. It is cheap to load and it is
// also the thing that would break if a required placeholder went missing, so
// exercising it here costs nothing and catches something.
func testVoice(t testingT) voice.Source {
	t.Helper()
	speaker, err := voice.New(testLogger())
	if err != nil {
		t.Fatalf("voice.New: %v", err)
	}
	return speaker
}

// --- fakePrivilege ---

type fakePrivilege struct{ bootstrapID string }

func (f fakePrivilege) IsBootstrapAdmin(userID string) bool {
	return f.bootstrapID != "" && userID == f.bootstrapID
}

// testPlugin assembles a Plugin over the fakes above, with the real policy
// catalogue loaded (a fake one would let a test pass against definitions the
// production build does not have).
func testPlugin(t testingT, store *fakeStore, client classifier, ops *fakeOps, audit *fakeAudit) *Plugin {
	t.Helper()
	policies, err := LoadPolicies()
	if err != nil {
		t.Fatalf("LoadPolicies: %v", err)
	}
	return &Plugin{
		store:         store,
		client:        client,
		ops:           func(string) DiscordOps { return ops },
		policies:      policies,
		voice:         testVoice(t),
		auditWriter:   audit,
		log:           testLogger(),
		now:           func() time.Time { return testNow },
		scanning:      true,
		dedupe:        newDedupeCache(),
		meter:         newUserMeter(),
		models:        newModelCache(),
		webhooks:      make(map[string]*discordgo.Webhook),
		batches:       make(map[string]*batch),
		inFlight:      make(map[string]chan struct{}),
		budgetNoticed: make(map[string]time.Time),
		// The funding maps are made here for the same reason budgetNoticed
		// is: noticeFunding writes to fundingNoticed, and a write to a nil
		// map panics rather than failing the assertion under test.
		fundingNoticed:    make(map[string]time.Time),
		fundingRegistered: make(map[string]bool),
		stopped:           make(chan struct{}),
	}
}

// testNow is fixed so spend day keys and sliding windows are deterministic.
var testNow = time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)

type testingT interface {
	Helper()
	Fatalf(format string, args ...any)
}

func (f *fakeClassifier) specsSeen() []*providerSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*providerSpec(nil), f.deepSpecs...)
}

func specNames(specs []*providerSpec) []string {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, providerName(s))
	}
	return names
}

// storedTriage is one guild's persisted model in the fake store.
type storedTriage struct {
	raw      []byte
	examples int64
}

// testSecretKey stands in for MERLIN_SECRET_KEY. Generated per process
// rather than written down, for the same reason internal/secret's own test
// does it: a fixed base64 string of exactly the right length is
// indistinguishable from a leaked key to a secret scanner.
var testSecretKey = func() string {
	key := make([]byte, secret.KeyBytes)
	if _, err := rand.Read(key); err != nil {
		panic("aimod: generate test secret key: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(key)
}()
