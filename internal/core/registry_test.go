package core

import (
	"context"
	"errors"
	"testing"
)

type recordingPlugin struct {
	name       string
	initErr    error
	startErr   error
	startPanic bool
	events     *[]string
}

func (p *recordingPlugin) Name() string { return p.name }

func (p *recordingPlugin) Init(deps Deps) error {
	*p.events = append(*p.events, p.name+":init")
	return p.initErr
}

func (p *recordingPlugin) Start(ctx context.Context) error {
	*p.events = append(*p.events, p.name+":start")
	if p.startPanic {
		panic("start panic in " + p.name)
	}
	return p.startErr
}

func (p *recordingPlugin) Shutdown(ctx context.Context) error {
	*p.events = append(*p.events, p.name+":shutdown")
	return nil
}

func TestRegistryLifecycleOrder(t *testing.T) {
	var events []string
	r := NewRegistry(Deps{}, testLogger())
	r.Register(&recordingPlugin{name: "a", events: &events})
	r.Register(&recordingPlugin{name: "b", events: &events})

	if err := r.InitAll(); err != nil {
		t.Fatalf("InitAll: %v", err)
	}
	if err := r.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	r.ShutdownAll(context.Background())

	want := []string{"a:init", "b:init", "a:start", "b:start", "b:shutdown", "a:shutdown"}
	if len(events) != len(want) {
		t.Fatalf("got %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("got %v, want %v", events, want)
		}
	}
}

func TestRegistryInitFailureAbortsStartup(t *testing.T) {
	var events []string
	r := NewRegistry(Deps{}, testLogger())
	r.Register(&recordingPlugin{name: "a", events: &events})
	r.Register(&recordingPlugin{name: "b", events: &events, initErr: errors.New("boom")})

	if err := r.InitAll(); err == nil {
		t.Fatal("expected InitAll to fail")
	}
}

func TestRegistryPartialStartRollsBack(t *testing.T) {
	var events []string
	r := NewRegistry(Deps{}, testLogger())
	r.Register(&recordingPlugin{name: "a", events: &events})
	r.Register(&recordingPlugin{name: "b", events: &events, startErr: errors.New("boom")})

	if err := r.InitAll(); err != nil {
		t.Fatalf("InitAll: %v", err)
	}
	if err := r.StartAll(context.Background()); err == nil {
		t.Fatal("expected StartAll to fail")
	}

	// "a" started successfully before "b" failed, so it must be the last
	// event: it was rolled back via ShutdownAll before StartAll returned.
	if events[len(events)-1] != "a:shutdown" {
		t.Fatalf("expected a:shutdown to be rolled back, got %v", events)
	}
}

func TestRegistryStartPanicIsolation(t *testing.T) {
	var events []string
	r := NewRegistry(Deps{}, testLogger())
	r.Register(&recordingPlugin{name: "a", events: &events, startPanic: true})

	if err := r.InitAll(); err != nil {
		t.Fatalf("InitAll: %v", err)
	}
	err := r.StartAll(context.Background())
	if err == nil {
		t.Fatal("expected StartAll to return an error when a plugin panics")
	}
}
