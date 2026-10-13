// Package paneltest drives a core.Panel from a test the way Discord would:
// a click or a form submission from a member, against a panel as Discord
// hands it back, through a session whose requests are recorded rather than
// sent.
package paneltest

import (
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// Wire records every request a session makes.
type Wire struct {
	mu     sync.Mutex
	Bodies []string
	Paths  []string
}

func (w *Wire) RoundTrip(r *http.Request) (*http.Response, error) {
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	w.mu.Lock()
	w.Bodies, w.Paths = append(w.Bodies, body), append(w.Paths, r.Method+" "+r.URL.Path)
	w.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

// All is every body sent, joined.
func (w *Wire) All() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.Bodies, "\n")
}

// Answered is whether the interaction was responded to at all.
func (w *Wire) Answered() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.Paths {
		if strings.HasSuffix(p, "/callback") {
			return true
		}
	}
	return false
}

// Session is a discordgo session that sends nothing anywhere.
func Session() (*discordgo.Session, *Wire) {
	w := &Wire{}
	s, _ := discordgo.New("Bot test")
	s.Client = &http.Client{Transport: w}
	s.State.User = &discordgo.User{ID: "bot"}
	return s, w
}
