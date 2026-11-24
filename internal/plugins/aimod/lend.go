package aimod

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
)

// What another plugin's calls need besides Ask: which models it may name and
// what they cost, and the triage model. dragoman's ladder (dragoman/rungs.go)
// is built from these.

const pricesTTL = 6 * time.Hour

// priceCache is the last read of the models that can be named, the same for
// every guild.
type priceCache struct {
	mu     sync.Mutex
	prices map[string]core.ModelPrice
	at     time.Time
}

// openRouterKey is the guild's OpenRouter key whichever gateway its scanning
// uses, or "": only OpenRouter can be told to keep nothing.
func (s budgetState) openRouterKey() string {
	if s.Spec == openRouter {
		return s.APIKey
	}
	return s.FallbackKey
}

// HasOpenRouter reports whether the guild can name its own models, which are
// OpenRouter's and pinned to zero data retention there.
func (p *Plugin) HasOpenRouter(ctx context.Context, guildID string) bool {
	cfg, err := p.store.Config(ctx, guildID)
	return err == nil && len(cfg.APIKeySealed) > 0
}

// Prices is what each model that can be named in a Prompt costs: those with
// an OpenRouter endpoint that keeps nothing and takes everything a request
// here sends. A model missing from it would never be routed. The price is
// the middle one of its endpoints, since OpenRouter doesn't always pick the
// cheapest.
func (p *Plugin) Prices(ctx context.Context, guildID string) (map[string]core.ModelPrice, error) {
	now := p.now()
	p.prices.mu.Lock()
	prices, fresh := p.prices.prices, now.Sub(p.prices.at) < pricesTTL
	p.prices.mu.Unlock()
	if prices != nil && fresh {
		return prices, nil
	}
	cfg, err := p.store.Config(ctx, guildID)
	if err != nil {
		return nil, fmt.Errorf("aimod: read config: %w", err)
	}
	client, ok := p.client.(*Client)
	if !ok || len(cfg.APIKeySealed) == 0 {
		return nil, ErrNoKey
	}
	key, err := p.sealer.Open(cfg.APIKeySealed)
	if err != nil {
		return nil, fmt.Errorf("aimod: open key: %w", err)
	}
	fetched, err := client.ZDRPrices(ctx, key)
	if err != nil {
		if prices != nil {
			return prices, nil
		}
		return nil, err
	}
	p.prices.mu.Lock()
	defer p.prices.mu.Unlock()
	p.prices.prices, p.prices.at = fetched, now
	return fetched, nil
}

// zdrParams is everything askOnce sends, which require_parameters holds an
// endpoint to.
var zdrParams = []string{"structured_outputs", "response_format", "max_tokens", "temperature"}

// ZDRPrices reads OpenRouter's list of zero-retention endpoints.
func (c *Client) ZDRPrices(ctx context.Context, apiKey string) (map[string]core.ModelPrice, error) {
	var envelope struct {
		Data []struct {
			ModelID string `json:"model_id"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			Params []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := c.get(ctx, apiKey, "/endpoints/zdr", &envelope); err != nil {
		return nil, err
	}
	all := map[string][]core.ModelPrice{}
	for _, e := range envelope.Data {
		if slices.ContainsFunc(zdrParams, func(s string) bool { return !slices.Contains(e.Params, s) }) {
			continue
		}
		all[e.ModelID] = append(all[e.ModelID], core.ModelPrice{Prompt: perMillion(e.Pricing.Prompt), Completion: perMillion(e.Pricing.Completion)})
	}
	out := make(map[string]core.ModelPrice, len(all))
	for id, ps := range all {
		slices.SortFunc(ps, func(a, b core.ModelPrice) int { return cmp.Compare(a.Prompt+a.Completion, b.Prompt+b.Completion) })
		out[id] = ps[len(ps)/2]
	}
	return out, nil
}

// The triage model (triage.go), lent out: whether a text is like the ones
// another plugin has been told aren't worth a call. Kept beside aimod's own
// under "use:guild", so neither is taught by the other. Weights only, as
// there.

const (
	// lentWarmup is how many answers a use needs before it skips anything,
	// and lentThreshold how sure it has to be: a call saved is worth much
	// less than a message lost.
	lentWarmup    = 200
	lentThreshold = 0.05
	// lentSaveEvery is sooner than triageSaveEvery: these arrive far more
	// slowly.
	lentSaveEvery = 25
)

// Unlike reports whether text can be skipped. A share of what could be is
// not, so the model keeps hearing about what it would skip.
func (p *Plugin) Unlike(ctx context.Context, guildID, use, text string) bool {
	feats := triageFeatures(text)
	if len(feats) == 0 {
		return false
	}
	m := p.triageFor(ctx, use+":"+guildID)
	return m.Stats().Examples >= lentWarmup && m.Score(feats) < lentThreshold && !p.sampleTriage()
}

// Teach records whether a call about text was worth making.
func (p *Plugin) Teach(ctx context.Context, guildID, use, text string, worth bool) {
	p.triageTeach(ctx, use+":"+guildID, text, worth, lentSaveEvery)
}
