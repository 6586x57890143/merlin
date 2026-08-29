package aimod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpenRouter's base URL, and the two identifying headers it asks clients to
// send. They are cosmetic (they name this bot on the account's activity
// page), but sending them means an operator looking at an unexpected bill
// can see which of their tools spent it.
const (
	openRouterBase = "https://openrouter.ai/api/v1"
	refererHeader  = "https://github.com/6586x57890143/merlin"
	titleHeader    = "merlin"
)

// OrcaRouter's base, and the header that makes it report what a call cost.
//
// The header is not optional bookkeeping. Without it the usage block carries
// token counts and no money, which this package would read as a call that
// cost nothing, and a daily budget that reads every call as free is not a
// budget. See Usage.CostUSD.
const (
	orcaRouterBase = "https://api.orcarouter.ai/v1"
	orcaCostHeader = "X-OrcaRouter-Include-Cost"
)

// httpTimeout bounds one model call.
//
// Short on purpose. This runs on a message someone just posted, and the
// whole feature is worthless if the removal lands a minute later. A model
// that has not answered in this long has effectively failed, and failing
// fast lets the fallback model in the array get a turn.
const httpTimeout = 20 * time.Second

// retryPause is how long one transient failure waits before its single
// retry, when the response did not name a wait of its own.
//
// Short, and bounded to one attempt, because of where this sits: inside a two
// minute batch context with nineteen other messages waiting behind it. The
// Scheduler's backoff machinery is for jobs that own their retry window; this
// is a call somebody is standing in front of.
const retryPause = 750 * time.Millisecond

// maxRetryPause caps what a Retry-After header can ask for. A provider that
// says "come back in ten minutes" is telling this batch it is over, and
// sleeping on it would burn the whole spawn context holding a slot that
// nineteen other messages need.
const maxRetryPause = 5 * time.Second

// Client talks to OpenRouter. One per process, sharing an http.Client so
// connections are reused across guilds; the API key is per call, not per
// client, because it belongs to a guild rather than to this bot.
type Client struct {
	http *http.Client
	base string

	// reasoningMu guards noDisable, which remembers the model stacks that
	// refused to have reasoning switched off. See Chat.
	reasoningMu sync.Mutex
	noDisable   map[string]bool
}

// NewClient builds the production client.
func NewClient() *Client {
	return &Client{
		http:      &http.Client{Timeout: httpTimeout},
		base:      openRouterBase,
		noDisable: make(map[string]bool),
	}
}

// Usage is the cost and token accounting OpenRouter returns on every
// response. Cost is in USD and arrives without being asked for, which is
// what makes the daily budget an exact figure rather than an estimate from
// token counts and a price list that may have changed.
type Usage struct {
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Cost             float64 `json:"cost"`
	// CostUSD is the same figure under the name OrcaRouter gives it, and
	// only when asked for with orcaCostHeader. Folded into Cost immediately
	// after the response is parsed, so that every reader downstream of here
	// (the budget, the projections, /aimod status) keeps seeing one field
	// and cannot be taught about a gateway.
	CostUSD float64 `json:"cost_usd"`
	// CompletionTokensDetails breaks out how much of the completion was the
	// model thinking rather than answering. Reasoning tokens are billed at
	// the completion rate and are already inside CompletionTokens, so this is
	// the only way to see what thinking is costing.
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// providerPrefs pins every request to endpoints that will not retain the
// text being sent.
//
// This is the privacy requirement from the package doc, enforced at the
// request rather than delegated to whichever model a guild happened to
// configure. ZDR restricts routing to endpoints with a zero data retention
// policy; DataCollection "deny" excludes providers that may store what they
// are sent for their own purposes. RequireParameters keeps routing on
// endpoints that actually honour the JSON schema below, rather than one
// that silently ignores it and returns prose this code then fails to parse.
type providerPrefs struct {
	ZDR               bool   `json:"zdr"`
	DataCollection    string `json:"data_collection"`
	RequireParameters bool   `json:"require_parameters"`
}

// Sent only where it means something. providerSpec.strictPrefs gates it, and
// chatOnce strips it for a gateway that does not accept it: a block that is
// silently ignored is worse than no block, because the code around it goes on
// reading as though the guarantee held.
func strictProvider() *providerPrefs {
	return &providerPrefs{ZDR: true, DataCollection: "deny", RequireParameters: true}
}

type jsonSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type chatRequest struct {
	// Models, not Model. OpenRouter treats the array as a fallback chain and
	// tries the next entry when one errors, which is exactly the behaviour
	// this plugin would otherwise have to write: a free model that has hit
	// its 20-per-minute cap returns 429 and the paid one behind it answers,
	// with no code here knowing it happened.
	//
	// It is not an escalation mechanism. The array only moves on failure,
	// never on a low-confidence answer, which is why the fast-to-deep step
	// in classify.go is a second call rather than a longer array.
	Models []string `json:"models,omitempty"`
	// Model is the OpenAI-shaped singular form, for a gateway with no array
	// form. Set by chatOnce from Models[0], never by a caller: which of the
	// two fields goes over the wire is a property of the gateway, and a
	// caller choosing it would be a caller that has to know about gateways.
	Model          string          `json:"model,omitempty"`
	Messages       []chatMessage   `json:"messages"`
	Provider       *providerPrefs  `json:"provider,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	// Zero, always. This is a classifier: the same message twice must get
	// the same verdict twice, or a mod comparing two identical incidents
	// has no way to reason about the filter at all.
	Temperature float64 `json:"temperature"`
	// Reasoning is set by Chat rather than by callers, because whether it can
	// be sent at all is a property of the endpoint that answers. Nil omits it.
	Reasoning *reasoningPrefs `json:"reasoning,omitempty"`

	// spec is which gateway this goes to. Unexported, so it never reaches
	// the wire. Nil means OpenRouter on c.base, which is what every request
	// meant before there was a second gateway and is what keeps the tests
	// that predate one honest.
	spec *providerSpec `json:"-"`
}

type reasoningPrefs struct {
	Enabled bool `json:"enabled"`
}

// Reasoning is decided per model stack, by trying and remembering.
//
// Both passes answer a strict JSON schema, so chain-of-thought buys nothing
// the schema does not already pin down, while reasoning tokens are billed at
// the completion rate and count against max_tokens. Switching it off is
// therefore worth real money.
//
// It cannot simply be switched off everywhere. Some endpoints reject the
// attempt outright:
//
//	HTTP 400: Reasoning is mandatory for this endpoint and cannot be disabled.
//
// which took the fast pass down for a guild when this was sent
// unconditionally. Nor can it be decided from the model list: a model can
// accept the reasoning parameter for raising effort while refusing to have
// it disabled, and /models does not distinguish those.
//
// So Chat asks, once, and remembers the answer. The first request for a
// stack carries reasoning disabled; if that specific rejection comes back,
// the stack is marked and the request is retried immediately without the
// field, and every later request for that stack omits it. Self-correcting,
// costs one wasted call per stack per process, and needs no metadata that
// might be wrong.

// reasoningRejected reports whether an API error is the endpoint refusing to
// have reasoning disabled, as opposed to any other 400. Matched on the text
// because there is no distinct code for it; kept narrow so an unrelated bad
// request is never silently retried.
func reasoningRejected(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		return false
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "reasoning") &&
		(strings.Contains(msg, "mandatory") || strings.Contains(msg, "cannot be disabled"))
}

// ReasoningDisabled reports whether this client is currently able to switch
// reasoning off for a stack. Surfaced by /aimod models show, because it is
// the difference between paying for thinking on every scanned message and
// not, and an admin reading a cost figure should be able to see which.
func (c *Client) ReasoningDisabled(spec *providerSpec, models []string) bool {
	c.reasoningMu.Lock()
	defer c.reasoningMu.Unlock()
	return !c.noDisable[stackKey(spec, models)]
}

func (c *Client) markReasoningMandatory(spec *providerSpec, models []string) {
	c.reasoningMu.Lock()
	defer c.reasoningMu.Unlock()
	c.noDisable[stackKey(spec, models)] = true
}

// stackKey names one model stack on one gateway. The gateway is part of the
// key because the same model ID reached through two routers is two
// endpoints, and one of them refusing to have reasoning switched off says
// nothing about the other.
func stackKey(spec *providerSpec, models []string) string {
	return providerName(spec) + ":" + strings.Join(models, ",")
}

// providerName is the gateway's name, treating nil as OpenRouter for the
// same reason chatRequest.spec does.
func providerName(spec *providerSpec) string {
	if spec == nil {
		return openRouter.name
	}
	return spec.name
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
		// FinishReason is what separates "the model had nothing to say" from
		// "the model was cut off mid-answer". Without it an empty completion
		// is indistinguishable from a truncated one, and the two have
		// opposite fixes.
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
	// Model is which model actually answered, which is not necessarily the
	// first entry of the request's fallback array.
	Model string `json:"model"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// APIError is a non-2xx response, carrying the status so callers can tell a
// bad key (401) from a rate limit (429) from an outage (5xx). That
// distinction matters here for the same reason core.IsUnknownResource
// matters elsewhere: this plugin must not treat "temporarily unavailable" as
// "nothing to moderate".
type APIError struct {
	Status  int
	Message string
	// RetryAfter is the Retry-After header, when the response carried one.
	// Zero otherwise. Read by retryAfter, which clamps it: a provider asking
	// for ten minutes is telling this batch it is over.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openrouter: HTTP %d: %s", e.Status, e.Message)
}

// Chat runs one completion and returns the assistant text alongside the
// usage. The usage is returned even on a parse failure upstream, because the
// call was still billed and the budget has to know.
func (c *Client) Chat(ctx context.Context, apiKey string, req chatRequest) (string, Usage, error) {
	if c.ReasoningDisabled(req.spec, req.Models) {
		req.Reasoning = &reasoningPrefs{Enabled: false}
	}
	out, usage, err := c.chatOnce(ctx, apiKey, req)
	if err != nil && req.Reasoning != nil && reasoningRejected(err) {
		// This endpoint will not have reasoning switched off. Remember it, so
		// this costs one wasted call per stack per process rather than one
		// per message, and answer the caller from the retry.
		c.markReasoningMandatory(req.spec, req.Models)
		req.Reasoning = nil
		out, usage, err = c.chatOnce(ctx, apiKey, req)
	}
	if err != nil && transient(err) && !freeRateLimited(err) {
		// The distinction APIError was built to carry, finally acted on. A
		// 429 or a 5xx used to lose the whole batch: one log line, and twenty
		// messages that nothing ever judged. The Models fallback array does
		// not cover this, because it moves between models on a model error,
		// not on an account-level rate limit or an OpenRouter outage.
		//
		// Free to retry: a response that failed this way carries no usage
		// block, so nothing is booked twice.
		if !pause(ctx, retryAfter(err)) {
			return out, usage, err
		}
		return c.chatOnce(ctx, apiKey, req)
	}
	return out, usage, err
}

// transient reports whether err is worth exactly one more attempt.
//
// Rate limits and server errors are; 400, 401 and 403 are not. A bad key or a
// malformed request fails identically the second time, so retrying those
// doubles a pointless bill and delays the log line that says what is actually
// wrong. A cancelled context is not transient either: that is the batch's own
// deadline, and retrying past it is work nobody is waiting for.
func transient(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
	}
	// A transport error: connection reset, DNS blip, the timeout above. The
	// call never reached a model, so there is nothing to be consistent with.
	return true
}

// freeRateLimited reports whether err is OrcaRouter's free tier saying no.
//
// Worth its own check because a free window does not ease back, it refills
// completely at its boundary: the minute bucket at the next minute, the day
// bucket at 00:00 UTC. So the Retry-After can be hours, and the one thing
// that is certainly wrong is this package's ordinary answer to a 429, which
// is to wait a moment and try the same gateway again.
//
// It also covers the case that would otherwise never terminate. The lowest
// free tier caps the size of a single request and refuses an oversized one
// with this same error, and that refusal never passes on a retry however
// long the wait: the prompt has to get smaller. Since the two are reported
// identically, neither is retried here, and the deep rung falls over to the
// other gateway instead.
func freeRateLimited(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.Message), "free_rate_limited")
}

// worthFallback reports whether a failed call is worth re-running on the
// other gateway.
//
// Everything except the batch's own deadline. A rate limit, an outage, a
// refused key, a model that ignored its schema: all of those are this
// gateway failing to answer, and the other one has not been asked. A
// cancelled context is the opposite, the caller has stopped waiting, and
// spending a second call on an answer nobody will read is the one retry that
// is certainly pointless.
func worthFallback(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// retryAfter is how long the failure asked to be left alone, clamped.
func retryAfter(err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return min(apiErr.RetryAfter, maxRetryPause)
	}
	return retryPause
}

// parseRetryAfter reads the delay-seconds form of the header. The HTTP-date
// form is not handled: OpenRouter sends seconds, and a wrong date parse would
// produce a wait this code then has to defend against anyway. Anything
// unreadable is zero, which retryAfter turns into the ordinary pause.
func parseRetryAfter(h string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// pause waits, and reports whether it got to finish. False means the context
// ended first, in which case the caller must not spend another call.
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Client) chatOnce(ctx context.Context, apiKey string, req chatRequest) (string, Usage, error) {
	// req is a value, so shaping it for the gateway cannot leak back to the
	// caller, which matters because Chat may call this twice.
	base := c.base
	if spec := req.spec; spec != nil {
		if spec.base != "" {
			base = spec.base
		}
		if spec.singleModel && len(req.Models) > 0 {
			req.Model, req.Models = req.Models[0], nil
		}
		if !spec.strictPrefs {
			req.Provider = nil
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("openrouter: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, fmt.Errorf("openrouter: build request: %w", err)
	}
	c.setHeaders(httpReq, apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	if req.spec != nil && req.spec.costHeader != "" {
		httpReq.Header.Set(req.spec.costHeader, "true")
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", Usage{}, fmt.Errorf("openrouter: chat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", Usage{}, fmt.Errorf("openrouter: read chat response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", Usage{}, &APIError{
			Status:     resp.StatusCode,
			Message:    errorMessage(raw),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", Usage{}, fmt.Errorf("openrouter: decode chat response: %w", err)
	}
	// One name for money from here on. Done before any return, including the
	// error returns below, because those still carry usage the budget has to
	// book: a call that was billed and then failed to parse spent real money.
	if parsed.Usage.Cost == 0 {
		parsed.Usage.Cost = parsed.Usage.CostUSD
	}
	if parsed.Error != nil {
		return "", parsed.Usage, &APIError{Status: parsed.Error.Code, Message: parsed.Error.Message}
	}
	if len(parsed.Choices) == 0 {
		return "", parsed.Usage, fmt.Errorf("openrouter: no choices in response")
	}

	// An empty completion is a 200 with nothing in it, so OpenRouter's own
	// fallback array does not treat it as a failure and never tries the next
	// model. Reporting it here, named and with the model and finish reason
	// attached, is the difference between a log line somebody can act on and
	// "unexpected end of JSON input", which says only that the caller tried
	// to parse an empty string and tells nobody why it was empty.
	choice := parsed.Choices[0]
	if strings.TrimSpace(choice.Message.Content) == "" {
		return "", parsed.Usage, fmt.Errorf("%w: %s answered with %d completion tokens and finish_reason %q",
			ErrEmptyCompletion, parsed.Model, parsed.Usage.CompletionTokens, choice.FinishReason)
	}
	return choice.Message.Content, parsed.Usage, nil
}

// ErrEmptyCompletion reports that a model returned a successful response
// carrying no content.
//
// Transient in practice (roughly one call in a hundred when it was first
// seen), and it fails in the safe direction: the batch is simply not
// classified, so nothing is acted on. It is worth its own error rather than
// a parse failure because the two have different causes and different fixes,
// and because a parse failure implies the model said something malformed
// when in fact it said nothing at all.
var ErrEmptyCompletion = errors.New("openrouter: model returned an empty completion")

// maxResponseBytes caps what is read back. The classifier's replies are a
// few hundred bytes; anything approaching this is a misrouted request or a
// model ignoring its schema, and neither is worth buffering.
const maxResponseBytes = 1 << 20

// errorMessage digs the human half out of an error body, falling back to the
// raw text. An admin pasting a bad key wants to read "invalid api key", not
// a JSON blob.
func errorMessage(raw []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	const maxInline = 300
	if len(raw) > maxInline {
		raw = raw[:maxInline]
	}
	return string(raw)
}

func (c *Client) setHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("HTTP-Referer", refererHeader)
	req.Header.Set("X-Title", titleHeader)
}

// KeyInfo is what GET /key reports about a guild's own OpenRouter account:
// the limits and usage OpenRouter itself is tracking, as opposed to the
// per-guild spend this plugin tracks in Postgres. /aimod status shows both,
// because they answer different questions ("is the key about to run dry"
// versus "is this server about to hit the cap its admin set").
type KeyInfo struct {
	Label          string   `json:"label"`
	Limit          *float64 `json:"limit"`
	LimitRemaining *float64 `json:"limit_remaining"`
	Usage          float64  `json:"usage"`
	UsageDaily     float64  `json:"usage_daily"`
	IsFreeTier     bool     `json:"is_free_tier"`
}

func (c *Client) KeyInfo(ctx context.Context, apiKey string) (KeyInfo, error) {
	var envelope struct {
		Data KeyInfo `json:"data"`
	}
	if err := c.get(ctx, apiKey, "/key", &envelope); err != nil {
		return KeyInfo{}, err
	}
	return envelope.Data, nil
}

// OrcaBalance is the same answer from the other gateway, mapped onto the
// same struct so that everything reading it (the fuel gauge, the runway, the
// low-credit audit entry, /aimod status) stays gateway-agnostic. Two
// requests rather than one, because OrcaRouter reports the ceiling and the
// spend from separate OpenAI-shaped endpoints and the remaining balance is
// the subtraction.
//
// total_usage is in cents, which is what OpenAI's shape means by that field.
// Getting the factor wrong here renders a gauge that is confidently off by a
// hundred, so it is worth checking against a live account rather than
// trusting this comment.
func (c *Client) OrcaBalance(ctx context.Context, spec *providerSpec, apiKey string) (KeyInfo, error) {
	base := orcaRouterBase
	if spec != nil && spec.base != "" {
		base = spec.base
	}

	var sub struct {
		HardLimitUSD float64 `json:"hard_limit_usd"`
	}
	if err := c.getFrom(ctx, base, apiKey, "/dashboard/billing/subscription", &sub); err != nil {
		return KeyInfo{}, err
	}
	var usage struct {
		TotalUsage float64 `json:"total_usage"`
	}
	if err := c.getFrom(ctx, base, apiKey, "/dashboard/billing/usage", &usage); err != nil {
		return KeyInfo{}, err
	}

	spent := usage.TotalUsage / 100
	info := KeyInfo{Label: orcaRouter.label, Usage: spent}
	// No ceiling means no balance to gauge, which is the ordinary state of a
	// free-tier account and is reported as nil rather than as zero: the
	// difference is "nothing is known" against "nothing is left", and the
	// renderers already branch on it.
	if sub.HardLimitUSD > 0 {
		limit := sub.HardLimitUSD
		left := max(limit-spent, 0)
		info.Limit, info.LimitRemaining = &limit, &left
	}
	return info, nil
}

// Model is one entry from GET /models, trimmed to what this plugin shows.
//
// Prices come back as strings holding USD per single token, which is a
// hostile unit for a human: a nano model reads as "0.00000005". PromptPerM
// and CompletionPerM convert once, here, so nothing downstream multiplies by
// a million and gets it wrong somewhere else.
type Model struct {
	ID             string
	Name           string
	ContextLength  int
	PromptPerM     float64
	CompletionPerM float64
	Free           bool
}

type rawModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	Pricing       struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
}

// Models lists everything the key can reach, with pricing. Used by the
// autocomplete on /aimod models set-fast|set-deep and by the cost table in
// /aimod models show.
func (c *Client) Models(ctx context.Context, apiKey string) ([]Model, error) {
	var envelope struct {
		Data []rawModel `json:"data"`
	}
	if err := c.get(ctx, apiKey, "/models", &envelope); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(envelope.Data))
	for _, m := range envelope.Data {
		prompt := perMillion(m.Pricing.Prompt)
		completion := perMillion(m.Pricing.Completion)
		out = append(out, Model{
			ID:             m.ID,
			Name:           m.Name,
			ContextLength:  m.ContextLength,
			PromptPerM:     prompt,
			CompletionPerM: completion,
			Free:           prompt == 0 && completion == 0,
		})
	}
	return out, nil
}

// perMillion converts OpenRouter's per-token price string to USD per million
// tokens. An unparseable price yields 0, which reads as free; that is the
// wrong direction to be wrong in for a cost estimate, so callers showing a
// price alongside it should say where the number came from rather than
// treating a zero as authoritative. It is only ever used for display.
func perMillion(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v * 1e6
}

func (c *Client) get(ctx context.Context, apiKey, path string, into any) error {
	return c.getFrom(ctx, c.base, apiKey, path, into)
}

func (c *Client) getFrom(ctx context.Context, base, apiKey, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return fmt.Errorf("openrouter: build request: %w", err)
	}
	c.setHeaders(req, apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openrouter: get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsResponseBytes))
	if err != nil {
		return fmt.Errorf("openrouter: read %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Message: errorMessage(raw)}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("openrouter: decode %s: %w", path, err)
	}
	return nil
}

// maxModelsResponseBytes is larger than maxResponseBytes because /models
// legitimately returns several hundred entries.
const maxModelsResponseBytes = 16 << 20
