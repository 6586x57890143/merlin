package aimod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// usdc renders a dollar amount the way the contract reports it, so a test can
// say 41.20 and the fake node answers in base units.
func usdc(amount float64) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"0x%x"}`, int64(math.Round(amount*1e6)))
}

// fundedPlugin wires a plugin whose tip jar reads a node reporting balance.
func fundedPlugin(t *testing.T, store *fakeStore, balance float64) *Plugin {
	t.Helper()
	p := testPlugin(t, store, nil, newFakeOps(), &fakeAudit{})
	p.eth, _ = ethServer(t, usdc(balance))
	return p
}

func setFunding(store *fakeStore, f Funding) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.funding[f.GuildID] = f
}

func getFunding(t *testing.T, store *fakeStore, guildID string) Funding {
	t.Helper()
	f, err := store.Funding(context.Background(), guildID)
	if err != nil {
		t.Fatalf("Funding: %v", err)
	}
	return f
}

func TestBarFillsProportionallyAndClamps(t *testing.T) {
	full := strings.Repeat("█", 10)
	empty := strings.Repeat("░", 10)

	cases := []struct {
		frac float64
		want string
	}{
		{0, empty},
		{1, full},
		{0.5, strings.Repeat("█", 5) + strings.Repeat("░", 5)},
		// Out of range in either direction is clamped rather than trusted:
		// strings.Repeat panics on a negative count, and these are a ratio of
		// two numbers that arrive separately from OpenRouter.
		{-1, empty},
		{2, full},
		{math.NaN(), empty},
	}
	for _, c := range cases {
		got := bar(c.frac, 10)
		if got != c.want {
			t.Fatalf("bar(%v) = %q, want %q", c.frac, got, c.want)
		}
		if len([]rune(got)) != 10 {
			t.Fatalf("bar(%v) is %d runes, want 10", c.frac, len([]rune(got)))
		}
	}

	if bar(0.5, 0) != "" {
		t.Fatal("a zero width bar should be empty, not a panic")
	}
}

func TestActualPerDayAveragesReceipts(t *testing.T) {
	if got := actualPerDay(nil); got != 0 {
		t.Fatalf("no history should be 0, got %v", got)
	}
	history := []Spend{{SpentUSD: 1}, {SpentUSD: 2}, {SpentUSD: 3}}
	if got := actualPerDay(history); math.Abs(got-2) > 1e-9 {
		t.Fatalf("actualPerDay = %v, want 2", got)
	}
}

// Members read this number, so it is prose. The audit line for the same
// figure is deliberately core.FormatDuration's compact form instead.
func TestHumanRunwayIsProse(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Minute:    "under an hour",
		time.Hour:           "1 hour",
		6 * time.Hour:       "6 hours",
		72 * time.Hour:      "3 days",
		24 * time.Hour:      "1 day",
		14 * 24 * time.Hour: "14 days",
	}
	for d, want := range cases {
		if got := humanRunway(d); got != want {
			t.Fatalf("humanRunway(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestPollFundingSkipsAGuildWithNoJar(t *testing.T) {
	store := newFakeStore()
	p := fundedPlugin(t, store, 10)

	// A benign skip is nil, not an error: an error here would count toward
	// the Scheduler's consecutive-failure alert for a guild that simply has
	// not opted in.
	if err := p.pollFunding(context.Background(), "g1"); err != nil {
		t.Fatalf("pollFunding with no address should be a no-op, got %v", err)
	}
}

// Pointing the bot at a wallet that already holds money must not report that
// money as a gift on day one.
func TestPollFundingFirstReadIsABaselineNotADonation(t *testing.T) {
	store := newFakeStore()
	setFunding(store, Funding{GuildID: "g1", Address: testWallet, SetBy: "owner", SetAt: testNow})
	p := fundedPlugin(t, store, 500)

	if err := p.pollFunding(context.Background(), "g1"); err != nil {
		t.Fatalf("pollFunding: %v", err)
	}

	f := getFunding(t, store, "g1")
	if f.Donations != 0 {
		t.Fatalf("donations = %d, want 0 on the baseline read", f.Donations)
	}
	if f.ReceivedUSD != 0 {
		t.Fatalf("received = %v, want 0 on the baseline read", f.ReceivedUSD)
	}
	if math.Abs(f.BalanceUSD-500) > 1e-9 {
		t.Fatalf("balance = %v, want the observed 500", f.BalanceUSD)
	}
}

func TestPollFundingCountsAnIncreaseAsOneDonation(t *testing.T) {
	store := newFakeStore()
	setFunding(store, Funding{
		GuildID: "g1", Address: testWallet, SetBy: "owner", SetAt: testNow,
		BalanceUSD: 100, CheckedAt: testNow.Add(-time.Hour),
	})
	p := fundedPlugin(t, store, 141.20)

	if err := p.pollFunding(context.Background(), "g1"); err != nil {
		t.Fatalf("pollFunding: %v", err)
	}

	f := getFunding(t, store, "g1")
	if f.Donations != 1 {
		t.Fatalf("donations = %d, want 1", f.Donations)
	}
	if math.Abs(f.ReceivedUSD-41.20) > 1e-6 {
		t.Fatalf("received = %v, want the 41.20 increase", f.ReceivedUSD)
	}
	if math.Abs(f.BalanceUSD-141.20) > 1e-6 {
		t.Fatalf("balance = %v, want 141.20", f.BalanceUSD)
	}
}

// A fall in balance is the operator moving funds out to buy credits. The new
// balance records it; it is not a negative donation, and it must not reduce
// the running total the server has been thanked for.
func TestPollFundingTreatsAWithdrawalAsNotADonation(t *testing.T) {
	store := newFakeStore()
	setFunding(store, Funding{
		GuildID: "g1", Address: testWallet, SetBy: "owner", SetAt: testNow,
		BalanceUSD: 100, ReceivedUSD: 250, Donations: 7, CheckedAt: testNow.Add(-time.Hour),
	})
	p := fundedPlugin(t, store, 5)

	if err := p.pollFunding(context.Background(), "g1"); err != nil {
		t.Fatalf("pollFunding: %v", err)
	}

	f := getFunding(t, store, "g1")
	if f.Donations != 7 {
		t.Fatalf("donations = %d, want the original 7", f.Donations)
	}
	if math.Abs(f.ReceivedUSD-250) > 1e-9 {
		t.Fatalf("received = %v, want the original 250", f.ReceivedUSD)
	}
	if math.Abs(f.BalanceUSD-5) > 1e-9 {
		t.Fatalf("balance = %v, want the drop to 5 recorded", f.BalanceUSD)
	}
}

func TestPollFundingIgnoresDust(t *testing.T) {
	store := newFakeStore()
	setFunding(store, Funding{
		GuildID: "g1", Address: testWallet, SetBy: "owner", SetAt: testNow,
		BalanceUSD: 100, CheckedAt: testNow.Add(-time.Hour),
	})
	p := fundedPlugin(t, store, 100.005)

	if err := p.pollFunding(context.Background(), "g1"); err != nil {
		t.Fatalf("pollFunding: %v", err)
	}

	if f := getFunding(t, store, "g1"); f.Donations != 0 {
		t.Fatalf("donations = %d, want 0 for a sub-dust change", f.Donations)
	}
}

// Only the guild owner or the bootstrap operator may repoint a payout
// address. An ordinary admin, at any tier, cannot: this is where donated
// money goes and nothing sent on chain can be recovered.
func TestCanSetFundingIsOwnerOrBootstrapOnly(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps() // guild owner is "owner"
	p := testPlugin(t, store, nil, ops, &fakeAudit{})
	p.privilege = fakePrivilege{bootstrapID: "boss"}

	cases := map[string]bool{
		"owner": true,
		"boss":  true,
		"admin": false,
		"":      false,
	}
	for userID, want := range cases {
		got, err := p.canSetFunding("g1", userID)
		if userID == "" {
			if err == nil {
				t.Fatal("an unknown actor should be an error, not a quiet false")
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: unexpected error %v", userID, err)
		}
		if got != want {
			t.Fatalf("canSetFunding(%q) = %v, want %v", userID, got, want)
		}
	}
}

// Fails closed, matching core.Permissions.CanModerate: an unresolvable guild
// refuses the change rather than assuming the actor is the owner.
func TestCanSetFundingFailsClosedWhenTheGuildCannotBeRead(t *testing.T) {
	store := newFakeStore()
	ops := newFakeOps()
	ops.guildErr = errors.New("discord is having a day")
	p := testPlugin(t, store, nil, ops, &fakeAudit{})

	allowed, err := p.canSetFunding("g1", "owner")
	if err == nil {
		t.Fatal("want an error when the guild cannot be read")
	}
	if allowed {
		t.Fatal("must not allow the change when ownership cannot be confirmed")
	}
}

func TestRunwayNeedsReceipts(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, nil, newFakeOps(), &fakeAudit{})

	// No history at all: "cannot say", never "unlimited".
	if _, ok := p.runway(context.Background(), "g1", 100); ok {
		t.Fatal("runway with no spend history should report not-ok")
	}

	store.spend["g1"] = Spend{Day: today(testNow), SpentUSD: 10}
	left, ok := p.runway(context.Background(), "g1", 100)
	if !ok {
		t.Fatal("runway should be known once there are receipts")
	}
	// $100 left at $10 a day is ten days.
	if want := 10 * 24 * time.Hour; left < want-time.Hour || left > want+time.Hour {
		t.Fatalf("runway = %v, want about %v", left, want)
	}
}

func TestReconcileFundingJobRegistersOnlyWhereThereIsAJar(t *testing.T) {
	store := newFakeStore()
	p := testPlugin(t, store, nil, newFakeOps(), &fakeAudit{})
	sched := &fakeScheduler{jobs: map[string]bool{}}
	p.sched = sched

	p.reconcileFundingJob("g1", true)
	if len(sched.jobs) != 1 {
		t.Fatalf("registered = %v, want one job", sched.jobs)
	}
	if _, ok := sched.jobs[fundingJobKey("g1")]; !ok {
		t.Fatalf("registered under %v, want %q", sched.jobs, fundingJobKey("g1"))
	}
	// Deliberately not seeded, unlike the calibration job: a freshly set
	// address should show a balance on the next tick rather than in fifteen
	// minutes, and a job the Scheduler has never seen is immediately due.
	if len(sched.seeded) > 0 {
		t.Fatalf("funding job was seeded %d times, want 0", len(sched.seeded))
	}

	// Idempotent: a second reconcile must not double-register.
	p.reconcileFundingJob("g1", true)
	if len(sched.jobs) != 1 {
		t.Fatalf("re-reconcile changed registrations to %v", sched.jobs)
	}

	p.reconcileFundingJob("g1", false)
	if len(sched.jobs) != 0 {
		t.Fatalf("clearing the jar should unregister, still have %v", sched.jobs)
	}
}

// capturingStub records every Discord request body, so a test can assert what
// the handler actually put on the wire. The shared discordStub throws bodies
// away, which is right for the handlers whose assertions are about stored
// state rather than about what was said.
type capturingStub struct{ bodies []string }

func (c *capturingStub) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		c.bodies = append(c.bodies, string(raw))
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    r,
	}, nil
}

func capturingSession(t *testing.T) (*discordgo.Session, *capturingStub) {
	t.Helper()
	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	stub := &capturingStub{}
	s.Client = &http.Client{Transport: stub}
	return s, stub
}

// showTheJar renders /aimod funding against a fully populated jar and returns
// everything that went to Discord.
func showTheJar(t *testing.T) []string {
	t.Helper()
	store := newFakeStore()
	setFunding(store, Funding{
		GuildID: "g1", Address: testWallet, SetBy: "owner",
		SetAt:      testNow.Add(-30 * 24 * time.Hour),
		CheckedAt:  testNow,
		BalanceUSD: 41.20, ReceivedUSD: 128.60, Donations: 7,
	})
	p := fundedPlugin(t, store, 41.20)

	s, stub := capturingSession(t)
	p.handleFundingShow(context.Background(), s, interaction("g1", "funding", "show"))
	if len(stub.bodies) == 0 {
		t.Fatal("the handler sent nothing to Discord")
	}
	return stub.bodies
}

// The tip jar is a fundraising surface, so it answers the channel rather than
// the person who typed it. Everything else in this plugin is ephemeral, which
// is right for a surface somebody reads to make a decision and wrong for one
// whose whole purpose is to be seen by other people.
//
// Asserted on the acknowledgement specifically: Discord fixes ephemerality
// when the interaction is acked, so this is the request that decides it and
// no later edit can correct it.
func TestFundingShowAnswersPublicly(t *testing.T) {
	if ack := showTheJar(t)[0]; strings.Contains(ack, `"flags"`) {
		t.Fatalf("the tip jar was acknowledged with flags, so only the invoker sees it: %s", ack)
	}
}

// The address is what somebody came for; the caveats are what somebody
// deciding whether to trust it came for. They are not equally urgent, so they
// do not render at the same weight.
func TestFundingShowFormatsTheJar(t *testing.T) {
	all := strings.Join(showTheJar(t), "\n")

	for _, want := range []string{
		testWallet,                      // the address itself
		"**Base network only.**",        // the money-losing mistake, at full weight
		"-# ",                           // Discord's small grey style is in use
		"-# Set by ",                    // provenance, demoted
		"merlin only reads this wallet", // the disclaimer is still present
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("rendered jar is missing %q\n%s", want, all)
		}
	}

	// The disclaimer must never start a line of its own at normal weight,
	// which is the state this test exists to prevent regressing to.
	if strings.Contains(all, `\nmerlin only reads`) {
		t.Fatalf("the disclaimer is rendered at full weight rather than as subtext\n%s", all)
	}
}

// attackerWallet is where a repointed jar would send the money.
const attackerWallet = "0x000000000000000000000000000000000000dEaD"

// The invariant the whole feature turns on: nobody but the guild owner or the
// bootstrap operator can move where donated money goes.
//
// Deliberately driven through the handler rather than through canSetFunding
// alone. The helper being correct is worth nothing if a later edit stops
// calling it, and the *stored address* is what an attacker actually wants, so
// that is what this asserts. The fake node returns a balance happily, so if
// the gate were removed every one of these writes would succeed.
func TestFundingSetAddressIsOwnerOrBootstrapOnly(t *testing.T) {
	// Every identity that is not one of the two, including a tier this
	// command's PermSpec would admit and one that merely looks like the
	// bootstrap ID. /config permissions allow and set-tier can both hand
	// aimod.configure to an ordinary mod, which is exactly why the tier is
	// not the gate here.
	for _, actorID := range []string{"actor", "some-admin", "a-mod", "boss-impostor", "bos"} {
		store := newFakeStore()
		setFunding(store, Funding{
			GuildID: "g1", Address: testWallet, SetBy: "owner",
			SetAt: testNow, CheckedAt: testNow,
		})
		p := fundedPlugin(t, store, 100)
		p.privilege = fakePrivilege{bootstrapID: "boss"}

		i := interaction("g1", "funding", "set-address", strOpt("address", attackerWallet))
		i.Member.User.ID = actorID
		p.handleFundingSetAddress(context.Background(), testSession(t), i)

		if got := getFunding(t, store, "g1").Address; got != testWallet {
			t.Fatalf("%q repointed the tip jar to %s", actorID, got)
		}
	}

	// And exactly the two who may.
	for _, actorID := range []string{"owner", "boss"} {
		store := newFakeStore()
		setFunding(store, Funding{
			GuildID: "g1", Address: testWallet, SetBy: "owner",
			SetAt: testNow, CheckedAt: testNow,
		})
		p := fundedPlugin(t, store, 100)
		p.privilege = fakePrivilege{bootstrapID: "boss"}

		i := interaction("g1", "funding", "set-address", strOpt("address", attackerWallet))
		i.Member.User.ID = actorID
		p.handleFundingSetAddress(context.Background(), testSession(t), i)

		f := getFunding(t, store, "g1")
		if f.Address != attackerWallet {
			t.Fatalf("%q should have been allowed to set the address, it is still %s", actorID, f.Address)
		}
		if f.SetBy != actorID {
			t.Fatalf("set_by = %q, want %q: the audit trail is the point", f.SetBy, actorID)
		}
	}
}

// Fails closed. The real owner is refused when Discord cannot confirm that
// they are the owner, rather than the check being skipped on an error.
func TestFundingSetAddressRefusesWhenOwnershipCannotBeConfirmed(t *testing.T) {
	store := newFakeStore()
	setFunding(store, Funding{
		GuildID: "g1", Address: testWallet, SetBy: "owner",
		SetAt: testNow, CheckedAt: testNow,
	})
	ops := newFakeOps()
	ops.guildErr = errors.New("discord is having a day")
	p := testPlugin(t, store, nil, ops, &fakeAudit{})
	p.eth, _ = ethServer(t, usdc(100))
	p.privilege = fakePrivilege{bootstrapID: "boss"}

	i := interaction("g1", "funding", "set-address", strOpt("address", attackerWallet))
	i.Member.User.ID = "owner"
	p.handleFundingSetAddress(context.Background(), testSession(t), i)

	if got := getFunding(t, store, "g1").Address; got != testWallet {
		t.Fatalf("address changed to %s while ownership was unconfirmable", got)
	}
}

// The bootstrap operator still gets through when Discord is unreachable,
// which is the point of a break-glass identity: it is checked before anything
// that can fail.
func TestFundingSetAddressLetsBootstrapThroughAnOutage(t *testing.T) {
	store := newFakeStore()
	setFunding(store, Funding{
		GuildID: "g1", Address: testWallet, SetBy: "owner",
		SetAt: testNow, CheckedAt: testNow,
	})
	ops := newFakeOps()
	ops.guildErr = errors.New("discord is having a day")
	p := testPlugin(t, store, nil, ops, &fakeAudit{})
	p.eth, _ = ethServer(t, usdc(100))
	p.privilege = fakePrivilege{bootstrapID: "boss"}

	i := interaction("g1", "funding", "set-address", strOpt("address", attackerWallet))
	i.Member.User.ID = "boss"
	p.handleFundingSetAddress(context.Background(), testSession(t), i)

	if got := getFunding(t, store, "g1").Address; got != attackerWallet {
		t.Fatalf("the bootstrap operator was blocked by an unrelated outage, address is %s", got)
	}
}

func TestFundingSetAddressRejectsAMalformedAddressFromTheOwner(t *testing.T) {
	store := newFakeStore()
	setFunding(store, Funding{
		GuildID: "g1", Address: testWallet, SetBy: "owner",
		SetAt: testNow, CheckedAt: testNow,
	})
	p := fundedPlugin(t, store, 100)

	for _, bad := range []string{"", "nonsense", testWallet + "ff", "0x" + strings.Repeat("g", 40)} {
		i := interaction("g1", "funding", "set-address", strOpt("address", bad))
		i.Member.User.ID = "owner"
		p.handleFundingSetAddress(context.Background(), testSession(t), i)

		if got := getFunding(t, store, "g1").Address; got != testWallet {
			t.Fatalf("a malformed address %q was stored as %s", bad, got)
		}
	}
}

// clear is open to any admin on purpose: it can only ever stop donations,
// never redirect them, so it is a kill switch that fails safe.
func TestFundingClearIsOpenToAnyAdmin(t *testing.T) {
	store := newFakeStore()
	setFunding(store, Funding{
		GuildID: "g1", Address: testWallet, SetBy: "owner",
		SetAt: testNow, CheckedAt: testNow,
	})
	p := fundedPlugin(t, store, 100)
	p.privilege = fakePrivilege{bootstrapID: "boss"}

	i := interaction("g1", "funding", "clear")
	i.Member.User.ID = "some-admin"
	p.handleFundingClear(context.Background(), testSession(t), i)

	if f := getFunding(t, store, "g1"); f.Configured() {
		t.Fatalf("an admin could not clear the jar, it still points at %s", f.Address)
	}
}
