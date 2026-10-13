// Package obs times merlin from the gateway to Discord's API, the way skua
// does, in four stages: in (Discord creating an interaction to merlin
// dispatching it: the gateway, decoding and the scheduler), run (a plugin's
// handler or scheduled job), http (a REST round trip, by route) and wait
// (how long a 429 told merlin to hold off, by route). Runtime adds what the
// Go runtime itself costs, and Flight keeps the last seconds of execution
// trace for when a number alone does not say why.
//
// A sample costs one read lock, a map lookup and a few atomic adds and
// allocates nothing (BenchmarkAdd), so it is always on.
//
// Wait is the time a request spent queued behind its rate limit bucket,
// recorded by Limiter around disgo's limiter. A 429's Retry-After is held
// there too, so it is counted once, as the wait it caused.
package obs

import (
	"context"
	"fmt"
	"math"
	"math/bits"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime/metrics"
	"runtime/trace"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Stage is one leg of a call's way from the gateway to the API and back.
type Stage uint8

const (
	In Stage = iota
	Run
	Wait
	HTTP
)

func (s Stage) String() string { return [...]string{"in", "run", "wait", "http"}[s] }

// buckets are half octaves of nanoseconds: bucket i holds durations whose
// top two bits put them in [2^(i/2), 2^(i/2+1)), so a quantile is good to
// within about 20%, which is enough to say which part is slow.
const buckets = 128

type hist struct {
	b   [buckets]atomic.Uint64
	n   atomic.Uint64
	max atomic.Int64
}

func bucket(ns int64) int {
	if ns < 2 {
		return int(max(ns, 0))
	}
	top := bits.Len64(uint64(ns)) - 1
	half := int(uint64(ns)>>(top-1)) & 1
	return min(2*top+half, buckets-1)
}

// lower is the smallest duration bucket i holds.
func lower(i int) time.Duration {
	if i < 2 {
		return time.Duration(i)
	}
	top := i / 2
	return time.Duration(1<<top + (i%2)<<(top-1))
}

func (h *hist) add(d time.Duration) {
	h.b[bucket(int64(d))].Add(1)
	h.n.Add(1)
	for m := h.max.Load(); int64(d) > m && !h.max.CompareAndSwap(m, int64(d)); m = h.max.Load() {
	}
}

// quantile is the midpoint of the bucket holding q of the samples.
func (h *hist) quantile(q float64) time.Duration {
	n := h.n.Load()
	if n == 0 {
		return 0
	}
	rank := uint64(math.Ceil(q * float64(n)))
	var seen uint64
	for i := range buckets {
		if seen += h.b[i].Load(); seen >= rank {
			return min((lower(i)+lower(i+1))/2, time.Duration(h.max.Load()))
		}
	}
	return time.Duration(h.max.Load())
}

type key struct {
	who   string
	stage Stage
}

// Recorder is safe for concurrent use. The zero value is not; use New.
type Recorder struct {
	mu sync.RWMutex
	h  map[key]*hist
}

func New() *Recorder { return &Recorder{h: map[key]*hist{}} }

// Default is the process's recorder.
var Default = New()

// Add records that who spent d in stage.
func (r *Recorder) Add(who string, s Stage, d time.Duration) {
	k := key{who, s}
	r.mu.RLock()
	h := r.h[k]
	r.mu.RUnlock()
	if h == nil {
		r.mu.Lock()
		if h = r.h[k]; h == nil {
			h = new(hist)
			r.h[k] = h
		}
		r.mu.Unlock()
	}
	h.add(d)
}

// Row is one plugin, job or route's stage since boot.
type Row struct {
	Who      string
	Stage    Stage
	N        uint64
	P50, P99 time.Duration
	Max      time.Duration
}

// Rows is every stage seen, slowest p99 first.
func (r *Recorder) Rows() []Row {
	r.mu.RLock()
	rows := make([]Row, 0, len(r.h))
	for k, h := range r.h {
		rows = append(rows, Row{k.who, k.stage, h.n.Load(), h.quantile(0.5), h.quantile(0.99), time.Duration(h.max.Load())})
	}
	r.mu.RUnlock()
	slices.SortFunc(rows, func(a, b Row) int {
		if a.P99 != b.P99 {
			return int(b.P99 - a.P99)
		}
		return strings.Compare(a.Who+a.Stage.String(), b.Who+b.Stage.String())
	})
	return rows
}

// Region times fn as a region named who in the execution trace, so a
// flight recording shows which plugin a goroutine was in. It costs next to
// nothing while no trace is being taken.
func Region(who string, fn func()) { trace.WithRegion(context.Background(), who, fn) }

// Since snowflake is when Discord made the object an ID names: the top 42
// bits are milliseconds since Discord's epoch.
func Since(snowflake string) (time.Duration, bool) {
	id, err := strconv.ParseUint(snowflake, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	at := time.UnixMilli(int64(id>>22) + 1420070400000)
	return time.Since(at), true
}

// transport times every REST call merlin makes, by route.
type transport struct {
	inner http.RoundTripper
	rec   *Recorder
}

// Transport wraps inner, recording each request's round trip as http
// against its route.
func Transport(inner http.RoundTripper, rec *Recorder) http.RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &transport{inner: inner, rec: rec}
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.inner.RoundTrip(req)
	t.rec.Add(Route(req.Method, req.URL.Path), HTTP, time.Since(start))
	return resp, err
}

// Route is a short name for an endpoint: its method and the last two fixed
// parts of its path, "put reactions/@me" or "post channels/messages". IDs
// and interaction tokens are dropped, so one route is one row however many
// channels it touched.
func Route(method, path string) string {
	var fixed []string
	for p := range strings.SplitSeq(path, "/") {
		if p == "" || p == "api" || isVersion(p) || isID(p) {
			continue
		}
		fixed = append(fixed, p)
	}
	return strings.ToLower(method) + " " + strings.Join(fixed[max(0, len(fixed)-2):], "/")
}

func isVersion(p string) bool { return len(p) > 1 && p[0] == 'v' && isID(p[1:]) }

// isID is a snowflake, or a token: anything with a digit and longer than a
// word Discord puts in a path.
func isID(p string) bool {
	if p == "" {
		return false
	}
	digits := 0
	for _, c := range p {
		if c >= '0' && c <= '9' {
			digits++
		}
	}
	return digits == len(p) || (digits > 0 && len(p) > 24)
}

// Runtime is what the Go runtime itself costs: the p99 of how long a
// runnable goroutine waits to run, and of GC pauses, since boot.
func Runtime() (sched, gc time.Duration) {
	s := []metrics.Sample{{Name: "/sched/latencies:seconds"}, {Name: "/sched/pauses/total/gc:seconds"}}
	metrics.Read(s)
	return p99(s[0].Value), p99(s[1].Value)
}

func p99(v metrics.Value) time.Duration {
	if v.Kind() != metrics.KindFloat64Histogram {
		return 0
	}
	h := v.Float64Histogram()
	var n uint64
	for _, c := range h.Counts {
		n += c
	}
	rank := uint64(math.Ceil(0.99 * float64(n)))
	var seen uint64
	for i, c := range h.Counts {
		if seen += c; n > 0 && seen >= rank {
			hi := h.Buckets[i+1]
			if math.IsInf(hi, 1) {
				hi = h.Buckets[i]
			}
			return time.Duration(hi * float64(time.Second))
		}
	}
	return 0
}

// Flight starts a flight recorder holding about the last ten seconds of
// execution trace, and returns a handler that writes it out for go tool
// trace. Only one can run in a process.
func Flight() (http.HandlerFunc, error) {
	fr := trace.NewFlightRecorder(trace.FlightRecorderConfig{MinAge: 10 * time.Second})
	if err := fr.Start(); err != nil {
		return nil, err
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="merlin.trace"`)
		_, _ = fr.WriteTo(w)
	}, nil
}

// Serve starts the debug listener on addr: pprof, and the flight recorder
// at /debug/merlin/flight. merlin is outbound-only by design, so this is
// off unless asked for and refuses any address that is not loopback: the
// profiles carry message content in flight, and a listener reachable from
// outside the host would be a door the threat model says does not exist.
// Reach it over SSH: ssh -L 6060:localhost:6060 the-host.
func Serve(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("debug address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("debug address %q is not loopback; it would expose profiles off the host", addr)
	}
	flight, err := Flight()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/merlin/flight", flight)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() { _ = srv.Serve(ln) }()
	return nil
}
