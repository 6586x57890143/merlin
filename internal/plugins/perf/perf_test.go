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

// Every route is a field of its own, its name short enough for one line of
// a column, and nothing is a code block: a record is read by its fields.
func TestRecordIsOneFieldARoute(t *testing.T) {
	e := setup(t).record()
	var out []string
	for _, f := range e.Fields {
		if n := utf8.RuneCountInString(f.Name); n > nameW+5 {
			t.Errorf("%d wide: %q", n, f.Name)
		}
		out = append(out, f.Name+": "+f.Value)
	}
	all := strings.Join(out, "\n")
	for _, want := range []string{"rotation run", " http", "go sched", "50 µs p99", "go gc pause", "- p99", "p50 · 1"} {
		if !strings.Contains(all, want) {
			t.Errorf("record missing %q:\n%s", want, all)
		}
	}
	if strings.Contains(all+e.Description, "```") {
		t.Errorf("a record is fields, not a code block:\n%s", all)
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
	if !strings.Contains(wire.All(), "go sched") || !strings.Contains(wire.All(), `"flags":64`) || strings.Contains(wire.All(), "32832") {
		t.Errorf("the operator was refused, or the record was not a private embed: %s", wire.All())
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
