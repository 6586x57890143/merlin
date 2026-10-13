package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/disgoorg/disgo/rest"

	"github.com/6586x57890143/merlin/internal/core"
)

// intentsEvery is how often the Developer Portal's toggles are read again.
const intentsEvery = 10 * time.Minute

var errIntentsChanged = errors.New("privileged intents changed in the Developer Portal; restarting to identify with them")

// watchIntents closes changed when the portal grants something other than
// granted. A probe that fails is retried on the next tick: an API blip is
// not a change.
// ponytail: a restart rather than a new connection in place, as skua does;
// in-process re-identify is the upgrade if restarts ever hurt.
func watchIntents(ctx context.Context, r rest.Rest, granted core.Intents, every time.Duration, changed chan<- struct{}, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now, err := core.Probe(r)
		if err != nil {
			log.Warn("re-reading the Developer Portal's intents", "err", err)
			continue
		}
		if now != granted {
			log.Warn("privileged intents changed in the Developer Portal, restarting",
				"members", now.Members, "message_content", now.MessageContent)
			close(changed)
			return
		}
	}
}
