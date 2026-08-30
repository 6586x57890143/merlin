//go:build js && wasm

// Command lab is merlin's logic, compiled to WebAssembly and handed to a
// browser.
//
// It exists because two of this codebase's own promises were only true with a
// Go toolchain in front of you. internal/voice is built so its lines are data
// that can be reviewed as writing, and reviewing one meant running the bot.
// The rotation notice is a guild's published retention policy, and the only
// way to see what a channel would actually be told was to configure it in a
// live server and wait for the rotation.
//
// A JavaScript reimplementation would answer both and would be wrong, sooner
// or later, about a deletion window somebody had published. Compiling the real
// packages is the only way to put this in a browser with one source of truth,
// which is the entire argument for the binary being several megabytes.
//
// Every function here is a marshalling shim: JSON in, JSON out, no decisions.
// The decisions are in internal/lab, which has no build tag and is tested
// natively.
package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"syscall/js"

	"github.com/6586x57890143/merlin/internal/lab"
)

func main() {
	l, err := lab.New(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		// The catalog failed to validate, which is precisely the condition the
		// bot refuses to boot on. Reporting it into the page rather than
		// panicking silently is the difference between "the lab is broken" and
		// "the lines you are about to review would not boot".
		js.Global().Set("merlinLabError", js.ValueOf(err.Error()))
		fail()
		return
	}

	js.Global().Set("merlinLab", js.ValueOf(map[string]any{
		"rotation": bind(func(raw string) (any, error) {
			var req lab.RotationRequest
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				return nil, err
			}
			return l.Rotation(req), nil
		}),
		"keys": bind(func(string) (any, error) { return l.Keys(), nil }),
		"roll": bind(func(raw string) (any, error) {
			var req struct {
				Key  string            `json:"key"`
				Vars map[string]string `json:"vars"`
				N    int               `json:"n"`
			}
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				return nil, err
			}
			return l.Roll(req.Key, req.Vars, req.N), nil
		}),
		"lint": bind(func(raw string) (any, error) {
			var req struct {
				Key  string `json:"key"`
				Line string `json:"line"`
			}
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				return nil, err
			}
			return l.Lint(req.Key, req.Line), nil
		}),
	}))

	ready()
	// Blocks forever so the callbacks above stay callable. A wasm main that
	// returns tears down the Go runtime, and every js.Func it registered stops
	// working, which presents as a page whose buttons quietly do nothing.
	select {}
}

// bind wraps one function as a JS callback taking a JSON string and returning
// one.
//
// JSON on both sides rather than js.Value marshalling by hand: it is the same
// shape for every function, it costs nothing at these sizes, and hand-built
// js.Value trees are exactly the kind of glue that grows logic in it. An error
// comes back as {"error": ...} rather than throwing, so the page renders it
// like any other answer.
func bind(fn func(string) (any, error)) js.Func {
	return js.FuncOf(func(_ js.Value, args []js.Value) any {
		var in string
		if len(args) > 0 && args[0].Type() == js.TypeString {
			in = args[0].String()
		}
		out, err := fn(in)
		if err != nil {
			return errJSON(err.Error())
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return errJSON(err.Error())
		}
		return string(encoded)
	})
}

func errJSON(msg string) string {
	encoded, err := json.Marshal(map[string]string{"error": msg})
	if err != nil {
		// Nothing left to marshal with, so hand back something the page can
		// still parse rather than an empty string it would treat as success.
		return `{"error":"the lab could not encode its own error"}`
	}
	return string(encoded)
}

// ready and fail tell the page the runtime has finished starting, either way.
//
// An event rather than leaving the page to poll for the global: the wasm blob
// takes a moment to instantiate, and a poll loop is a race that shows up as an
// occasionally dead page on a slow connection.
func ready() { dispatch("merlin-lab-ready") }
func fail()  { dispatch("merlin-lab-failed") }

func dispatch(name string) {
	event := js.Global().Get("Event").New(name)
	js.Global().Call("dispatchEvent", event)
}
