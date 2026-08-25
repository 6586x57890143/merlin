package aimod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// OpenRouter's base URL, and the two identifying headers it asks clients to
// send. They are cosmetic (they name this bot on the account's activity
// page), but sending them means an operator looking at an unexpected bill
// can see which of their tools spent it.
const (
	openRouterBase = "https://openrouter.ai/api/v1"
	refererHeader  = "https://github.com/6586x57890143/merlin"
	titleHeader    = "Merlin"
)

// httpTimeout bounds one model call.
//
// Short on purpose. This runs on a message someone just posted, and the
// whole feature is worthless if the removal lands a minute later. A model
// that has not answered in this long has effectively failed, and failing
// fast lets the fallback model in the array get a turn.
const httpTimeout = 20 * time.Second

// Client talks to OpenRouter. One per process, sharing an http.Client so
// connections are reused across guilds; the API key is per call, not per
// client, because it belongs to a guild rather than to this bot.
type Client struct {
	http *http.Client
	base string
}

// NewClient builds the production client.
func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: httpTimeout}, base: openRouterBase}
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

func strictProvider() providerPrefs {
	return providerPrefs{ZDR: true, DataCollection: "deny", RequireParameters: true}
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
	Models         []string        `json:"models"`
	Messages       []chatMessage   `json:"messages"`
	Provider       providerPrefs   `json:"provider"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	// Zero, always. This is a classifier: the same message twice must get
	// the same verdict twice, or a mod comparing two identical incidents
	// has no way to reason about the filter at all.
	Temperature float64 `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage Usage  `json:"usage"`
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
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openrouter: HTTP %d: %s", e.Status, e.Message)
}

// Chat runs one completion and returns the assistant text alongside the
// usage. The usage is returned even on a parse failure upstream, because the
// call was still billed and the budget has to know.
func (c *Client) Chat(ctx context.Context, apiKey string, req chatRequest) (string, Usage, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("openrouter: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, fmt.Errorf("openrouter: build request: %w", err)
	}
	c.setHeaders(httpReq, apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

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
		return "", Usage{}, &APIError{Status: resp.StatusCode, Message: errorMessage(raw)}
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", Usage{}, fmt.Errorf("openrouter: decode chat response: %w", err)
	}
	if parsed.Error != nil {
		return "", parsed.Usage, &APIError{Status: parsed.Error.Code, Message: parsed.Error.Message}
	}
	if len(parsed.Choices) == 0 {
		return "", parsed.Usage, fmt.Errorf("openrouter: no choices in response")
	}
	return parsed.Choices[0].Message.Content, parsed.Usage, nil
}

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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
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
