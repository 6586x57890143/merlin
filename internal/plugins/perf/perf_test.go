package perf

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/obs"
)

type auth struct{}

func (auth) ModRoleIDs(string) []string                    { return nil }
func (auth) AdminUserIDs(string) []string                  { return nil }
func (auth) ActionPolicy(string, string) core.ActionPolicy { return core.ActionPolicy{} }
func (auth) PluginEnabled(string, string) bool             { return true }

func setup(t *testing.T) *Plugin {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	perms := core.NewPermissions(nil, auth{}, operator)
	router := core.NewCommandRouter(perms, auth{}, log)
	rec := obs.New()
	rec.Add("rotation", obs.Run, 40*time.Millisecond)
	rec.Add("a-route-with-a-very-long-name channels/messages", obs.HTTP, 2*time.Second)
	p := New(rec)
	p.runtime = func() (time.Duration, time.Duration) { return 50 * time.Microsecond, 0 }
	if err := p.Init(core.Deps{Commands: router, Perms: perms, Logger: log}); err != nil {
		t.Fatal(err)
	}
	if err := router.Finalize(); err != nil {
		t.Fatal(err)
	}
	return p
}

// A code block line wider than 40 wraps on a phone and the grid stops being
// a grid, so every line, long route names included, stays within it.
func TestReadoutFitsItsGrid(t *testing.T) {
	out := setup(t).readout()
	for _, line := range strings.Split(out, "\n") {
		if n := utf8.RuneCountInString(line); n > 40 {
			t.Errorf("%d wide: %q", n, line)
		}
	}
	for _, want := range []string{"rotation run", " http", "go sched", "50 µs", "go gc pause", "-"} {
		if !strings.Contains(out, want) {
			t.Errorf("readout missing %q:\n%s", want, out)
		}
	}
}

// operator and admin are the bootstrap identity and a guild admin.
const operator, admin = "900", "901"

func TestOnlyTheOperatorReadsIt(t *testing.T) {
	p := setup(t)
	ix, wire := paneltest.IxCommand(t, admin, "perf", nil)
	p.handlePerf(context.Background(), ix)
	if strings.Contains(wire.All(), "go sched") || !strings.Contains(wire.All(), "Operator only") {
		t.Error("a guild admin read process-wide numbers")
	}
	ix, wire = paneltest.IxCommand(t, operator, "perf", nil)
	p.handlePerf(context.Background(), ix)
	if !strings.Contains(wire.All(), "go sched") || !strings.Contains(wire.All(), `"flags":32832`) {
		t.Errorf("the operator was refused, or the panel was not private and V2: %s", wire.All())
	}
	ix, wire = paneltest.IxClick(t, operator, refreshID, nil)
	p.handleRefresh(context.Background(), ix, refreshID)
	if !strings.Contains(wire.All(), `"type":7`) {
		t.Error("refresh did not redraw in place")
	}
	ix, wire = paneltest.IxClick(t, admin, refreshID, nil)
	p.handleRefresh(context.Background(), ix, refreshID)
	if strings.Contains(wire.All(), "go sched") {
		t.Error("a guild admin refreshed it")
	}
	if p.Help().Title != "Perf" {
		t.Error("no help page")
	}
}

func TestNumbersReadAsNumbers(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "-", 900 * time.Microsecond: "900 µs", 4500 * time.Microsecond: "4.5 ms", 250 * time.Millisecond: "250 ms", 15 * time.Second: "15s"} {
		if got := dur(d); got != want {
			t.Errorf("dur(%v) = %q, want %q", d, got, want)
		}
	}
	for n, want := range map[uint64]string{7: "7", 1500: "1.5k", 42000: "42k"} {
		if got := count(n); got != want {
			t.Errorf("count(%d) = %q", n, got)
		}
	}
	if bar(0, 0, 1) != "" || bar(time.Second, time.Second, 1) != "█" {
		t.Error("bar ends wrong")
	}
}
