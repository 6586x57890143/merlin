package discordguard

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

// ErrBraked is a request refused before it went out: this process has had
// too many 401, 403 and 429 answers lately.
var ErrBraked = errors.New("discordguard: holding off discord, too many refused requests")

// Discord bans an IP from its API once it draws 10,000 401, 403 and 429
// answers in 10 minutes, and every bot on that IP goes with it. The guild
// caps can't see that: a 403 is the request's fault, not Discord
// struggling, and a member can make one on demand. Brake counts them for
// the whole process and stops sending once BrakeBudget is reached, so this
// bot alone can never spend the IP's allowance. Four bots share foundry's
// IP, so each keeps well under a quarter.
const (
	BrakeBudget = 2000
	brakeWindow = 10 * time.Minute
	brakeSlots  = 10
)

// Brake wraps next with the process-wide count. tripped is called once
// each time the brake engages; it may be nil.
func Brake(next http.RoundTripper, tripped func(count int)) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &brake{next: next, budget: BrakeBudget, now: time.Now, tripped: tripped}
}

type brake struct {
	next    http.RoundTripper
	budget  int
	now     func() time.Time
	tripped func(int)

	mu      sync.Mutex
	slots   [brakeSlots]int
	minutes [brakeSlots]int64
	engaged bool
}

func (b *brake) RoundTrip(req *http.Request) (*http.Response, error) {
	if n := b.count(); n >= b.budget {
		b.mu.Lock()
		first := !b.engaged
		b.engaged = true
		b.mu.Unlock()
		if first && b.tripped != nil {
			b.tripped(n)
		}
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, ErrBraked
	}
	resp, err := b.next.RoundTrip(req)
	if resp != nil && invalid(resp) {
		b.add()
	}
	return resp, err
}

// invalid is an answer Discord counts toward the ban: 401, 403, and a 429
// that isn't a shared limit.
func invalid(r *http.Response) bool {
	switch r.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	case http.StatusTooManyRequests:
		return r.Header.Get("X-RateLimit-Scope") != "shared"
	}
	return false
}

func (b *brake) add() {
	m := b.now().Unix() / 60
	i := m % brakeSlots
	b.mu.Lock()
	if b.minutes[i] != m {
		b.minutes[i], b.slots[i] = m, 0
	}
	b.slots[i]++
	b.mu.Unlock()
}

// count is the refused answers in the last brakeWindow, to the minute.
func (b *brake) count() int {
	m := b.now().Unix() / 60
	n := 0
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.slots {
		if m-b.minutes[i] < brakeSlots {
			n += b.slots[i]
		}
	}
	if n < b.budget {
		b.engaged = false
	}
	return n
}
