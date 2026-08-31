package contest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The Worker is a Cloudflare free-tier Worker with a D1 database behind it,
// and the whole conversation with it is three outbound calls. It holds the
// gallery and the vote ledger and nothing else: merlin owns the schema and
// pushes a complete snapshot on every change rather than diffing, which is
// idempotent, self-healing, and a few KB a push.
//
// Nothing here is a control channel. The Worker cannot make merlin do
// anything; merlin asks it two questions (how is voting going, what was the
// final tally) and tells it everything else.
const (
	// workerTimeout bounds one call. Generous, because a snapshot push
	// happening slowly is fine and a snapshot push failing means the gallery
	// is stale, which is visible and self-corrects on the next tick.
	workerTimeout = 15 * time.Second

	// maxWorkerResponse caps what is read back. A tally for a contest that
	// somehow had ten thousand entries is still well under this.
	maxWorkerResponse = 1 << 20
)

// ErrWorkerUnset reports that no Worker is configured, so the gallery and
// voting do not exist for this deployment. Deliberately not fatal: the
// entire Discord half of a contest (forum, phases, prizes, announcements)
// runs without one, and /contest status says so out loud.
var ErrWorkerUnset = errors.New("contest: MERLIN_CONTEST_WORKER_URL is not set, so there is no gallery or voting")

// snapshot is the whole contest as the Worker sees it. Times are unix
// seconds because the page does its own countdown arithmetic and a string
// date would only be parsed back.
type snapshot struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Theme     string `json:"theme,omitempty"`
	Phase     string `json:"phase"`
	SubmitAt  int64  `json:"submit_at"`
	VoteAt    int64  `json:"vote_at"`
	ResultsAt int64  `json:"results_at"`
	MaxVotes  int    `json:"max_votes"`
	// Guild is what the Worker checks OAuth membership against. It is
	// stripped from the public view before a browser sees it, like by_hash.
	Guild   string       `json:"guild"`
	Forum   string       `json:"forum,omitempty"`
	Entries []entryView  `json:"entries"`
	Prizes  []prizeView  `json:"prizes"`
	Results []resultView `json:"results,omitempty"`
}

// entryView is one submission. ByHash, not a Discord ID: the Worker needs to
// refuse a self-vote and that is the only thing it needs identity for, so it
// gets the same HMAC the voting token carries and never the snowflake.
type entryView struct {
	ID     string `json:"id"`
	By     string `json:"by"`
	ByHash string `json:"by_hash"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	URL    string `json:"url,omitempty"`
	Link   string `json:"link,omitempty"`
	Body   string `json:"body,omitempty"`
	Thread string `json:"thread,omitempty"`
}

// prizeView is a pledge as the public page shows it. There is no field here
// for the sealed code, and there is deliberately no way to add one: the
// ciphertext lives in merlin's Postgres and the plaintext exists for the
// length of one DM.
type prizeView struct {
	By      string `json:"by"`
	Title   string `json:"title"`
	Details string `json:"details,omitempty"`
}

// resultView is the tally merlin computed, pushed back so the page renders a
// number that was already decided rather than counting anything itself.
type resultView struct {
	ID    string `json:"id"`
	Votes int    `json:"votes"`
	Rank  int    `json:"rank"`
}

// Tally is what comes back from a close: entry ID to vote count. The Worker
// freezes voting in the same request, so this is final by construction and
// there is no window where a late vote lands after the count.
type Tally map[string]int

// workerClient talks to the Worker. A nil client is a valid state, returned
// when no URL is configured, and every method on it fails with
// ErrWorkerUnset rather than panicking. Same shape as a nil secret.Sealer.
type workerClient struct {
	base    string
	token   string
	linkKey []byte
	http    *http.Client
}

// newWorkerClient returns nil when baseURL is empty. Callers check
// Configured() rather than the nil, so the degraded path reads as a state
// and not as a missing pointer.
func newWorkerClient(baseURL, botToken, linkKey string) *workerClient {
	if baseURL == "" {
		return nil
	}
	return &workerClient{
		base:    strings.TrimRight(baseURL, "/"),
		token:   botToken,
		linkKey: []byte(linkKey),
		// No Timeout on the client: every call bounds itself with a context
		// so a retry gets a fresh budget, matching aimod's openrouter client.
		http: &http.Client{},
	}
}

// Configured reports whether there is a Worker to talk to at all.
func (w *workerClient) Configured() bool { return w != nil && w.base != "" }

// Hash is the only identity that crosses to Cloudflare: HMAC-SHA256 of a
// Discord ID under the link key, truncated to 128 bits. It has to be stable
// (the same member voting twice must collide) and one-way (the vote ledger
// must not be a list of who voted), and an HMAC under a key that never
// leaves merlin is both. Truncation is fine here: this is a collision
// domain of a few hundred members, not a signature.
//
// The Worker computes this same value from the Discord ID that OAuth just
// proved, and compares it against the by_hash on each entry to refuse a
// self-vote. So the two implementations have to agree byte for byte, which
// is what TestTokenGoldenVector and scripts/check-contest.mjs pin between
// them.
func (w *workerClient) Hash(userID string) string {
	mac := hmac.New(sha256.New, w.linkKey)
	mac.Write([]byte("id:" + userID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// PageURL is the gallery. One link for everybody, safe to paste in the
// announcement, because who you are is established by Discord OAuth on the
// page rather than by anything carried in the URL. That is what makes one
// ballot per member enforceable rather than merely encouraged: a forwarded
// link gets the recipient their own vote, not somebody else's.
func (w *workerClient) PageURL(slug string) string {
	if !w.Configured() {
		return ""
	}
	return w.base + "/c/" + slug
}

func (w *workerClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	if !w.Configured() {
		return nil, ErrWorkerUnset
	}
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("contest worker: encode %s: %w", path, err)
		}
		rdr = bytes.NewReader(buf)
	}
	ctx, cancel := context.WithTimeout(ctx, workerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, w.base+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("contest worker: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contest worker: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxWorkerResponse))
	if err != nil {
		return nil, fmt.Errorf("contest worker: read %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("contest worker: %s %s: %s: %s",
			method, path, resp.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Push replaces the Worker's copy of a contest. Full snapshot, never a
// diff: the Worker is storage, merlin owns the schema, and a push that
// lands after a missed one is still correct.
func (w *workerClient) Push(ctx context.Context, snap snapshot) error {
	_, err := w.do(ctx, http.MethodPut, "/api/c/"+snap.Slug, snap)
	return err
}

// Close freezes voting and returns the final counts in one request. One
// call rather than a freeze followed by a read, so there is no window in
// which a vote lands between the two.
func (w *workerClient) Close(ctx context.Context, slug string) (Tally, error) {
	out, err := w.do(ctx, http.MethodPost, "/api/c/"+slug+"/close", nil)
	if err != nil {
		return nil, err
	}
	var t Tally
	if err := json.Unmarshal(out, &t); err != nil {
		return nil, fmt.Errorf("contest worker: decode tally: %w", err)
	}
	return t, nil
}

// Stats is the live picture /contest status reports mid-vote.
func (w *workerClient) Stats(ctx context.Context, slug string) (voters, votes int, err error) {
	out, err := w.do(ctx, http.MethodGet, "/api/c/"+slug+"/stats", nil)
	if err != nil {
		return 0, 0, err
	}
	var s struct {
		Voters int `json:"voters"`
		Votes  int `json:"votes"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return 0, 0, fmt.Errorf("contest worker: decode stats: %w", err)
	}
	return s.Voters, s.Votes, nil
}

// rank turns a tally into the ordered result list merlin announces and the
// page renders. Ties share a rank, and the tiebreak for display order is
// entry ID rather than anything derived from time or votes, so two runs of
// this over the same tally produce the same list.
func rank(subs []Submission, t Tally) []resultView {
	out := make([]resultView, 0, len(subs))
	for _, s := range subs {
		out = append(out, resultView{ID: s.ID, Votes: t[s.ID]})
	}
	sortResults(out)
	for i := range out {
		if i > 0 && out[i].Votes == out[i-1].Votes {
			out[i].Rank = out[i-1].Rank
			continue
		}
		out[i].Rank = i + 1
	}
	return out
}

func sortResults(rs []resultView) {
	// Insertion sort. The list is one contest's entries, so tens of items at
	// most, and the alternative is importing sort for a comparison that is
	// two fields long.
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0; j-- {
			if rs[j].Votes < rs[j-1].Votes ||
				(rs[j].Votes == rs[j-1].Votes && rs[j].ID >= rs[j-1].ID) {
				break
			}
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}
