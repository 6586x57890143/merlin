package aimod

// Two model gateways, and how this package tells them apart.
//
// OrcaRouter is the default because its free tier is genuinely free: an
// account that has never topped up can run the whole scan ladder at $0 per
// token, limited by request rate rather than by a balance. OpenRouter stays
// as the fallback the deep rung reaches for when OrcaRouter cannot answer,
// and as the only gateway that lets a request pin its own privacy terms.
//
// A table of two values rather than an interface with two implementations.
// The differences are five fields and a base URL, and the moment this becomes
// a type with methods is the moment somebody has to read two files to find
// out what the second gateway does differently.

// providerSpec is one gateway: where to send a request, how to shape it, and
// which model stack to send when a guild has not chosen its own.
type providerSpec struct {
	// name is the machine-readable identifier, stored and logged.
	name string
	// label is what an admin reads in /aimod status.
	label string
	base  string
	// keyPrefix is how /aimod configure key tells one gateway's credential
	// from the other's without asking. Sniffed, not trusted: an unrecognised
	// prefix falls back to the explicit option rather than being refused,
	// because a gateway is free to change what its keys look like and
	// rejecting a valid key is the worse failure.
	keyPrefix string
	// strictPrefs sends the providerPrefs block.
	//
	// OpenRouter only. It is the one gateway here that accepts per-request
	// privacy terms, and an OpenAI-shaped API handed an unknown top-level
	// object may reject the whole request rather than ignore it. Sending it
	// anyway would be a comfort blanket in the one place this package cannot
	// afford one: see the note on providerPrefs in openrouter.go.
	strictPrefs bool
	// singleModel sends "model" with one entry rather than the "models"
	// fallback array. OpenAI-shaped APIs have no array form, and a request
	// carrying neither field is rejected outright.
	singleModel bool
	// costHeader asks for the billed cost inside the usage block, for a
	// gateway that does not return it unasked. Empty means it always does.
	costHeader string
	// topUpChains names the chains this gateway's crypto checkout was last
	// observed to accept, and topUpVerified is when somebody checked.
	//
	// Deliberately a dated observation rather than a fact, which is the whole
	// difference between this field and a fundingRail. A rail is a property
	// of a blockchain that this bot can verify from here. What a checkout
	// accepts is a property of somebody else's merchant configuration: it can
	// change without notice, nothing here can observe it, and the previous
	// version stating it as certainty is exactly how merlin came to warn
	// donors that Ethereum mainnet USDC would be lost when OpenRouter's
	// checkout takes it perfectly well. Everything rendered from these two
	// fields says "as of" and hedges; everything rendered from a rail does
	// not need to.
	topUpChains   []string
	topUpVerified string

	fastModels []string
	deepModels []string
}

// openRouter is the original gateway, and still the only one that can be
// told to route a request only to endpoints that will not retain it.
var openRouter = &providerSpec{
	name:        "openrouter",
	label:       "OpenRouter",
	base:        openRouterBase,
	keyPrefix:   "sk-or-",
	strictPrefs: true,
	// Their checkout runs on Coinbase Commerce's onchain payment protocol,
	// which takes supported tokens across the Ethereum, Polygon and Base
	// ecosystems and settles them to USDC. Confirmed by a live top-up paid in
	// Ethereum mainnet USDC, which the old single-chain warning here claimed
	// would be lost.
	topUpChains:   []string{"base", "ethereum", "polygon"},
	topUpVerified: topUpCheckedOn,
	fastModels:    defaultFastModels,
	deepModels:    defaultDeepModels,
}

// orcaRouter is the default gateway.
//
// Both stacks are single entries because singleModel sends only the first,
// and a stack whose later entries are silently dropped is worse than one
// that never had them. The fallback they would have provided is covered
// twice over: orcarouter/free is itself a router across the free lineup, and
// the deep rung falls over to OpenRouter when this gateway cannot answer.
var orcaRouter = &providerSpec{
	name:        "orcarouter",
	label:       "OrcaRouter",
	base:        orcaRouterBase,
	keyPrefix:   "sk-orca-",
	singleModel: true,
	costHeader:  orcaCostHeader,
	// Their crypto top-up is a NOWPayments checkout, which as a platform
	// settles roughly 350 assets across Ethereum, Tron, BSC, Solana and
	// Polygon. Which of those reach OrcaRouter's own checkout is OrcaRouter's
	// merchant setting, not a platform limit, and it cannot be read from
	// here. Only TRON has actually been seen offered, so only TRON is
	// claimed.
	topUpChains:   []string{"tron"},
	topUpVerified: topUpCheckedOn,
	// orcarouter/free routes by difficulty across the free lineup, which is
	// the same job the fallback array did on the other gateway.
	fastModels: []string{"orcarouter/free"},
	// Named directly rather than via the free router: the deep rung is the
	// only one whose verdict can delete or rewrite, and letting a difficulty
	// classifier decide it gets the light model would put the cheapest tier
	// behind the most consequential call.
	deepModels: []string{"deepseek/deepseek-v4-pro-free"},
}

// topUpCheckedOn is when a human last opened both checkouts and read the
// networks off them. One shared constant rather than a date per gateway,
// because they are checked in the same sitting and two dates drifting apart
// would imply a rigour this process does not have. Bump it when somebody
// actually looks again.
const topUpCheckedOn = "2026-08-30"

// acceptsChain reports whether this gateway's checkout was last seen taking
// the given chain.
func (s *providerSpec) acceptsChain(chain string) bool {
	for _, c := range s.topUpChains {
		if c == chain {
			return true
		}
	}
	return false
}

// providers lists every gateway, newest default first. Used by the key
// prefix sniff and by /aimod status.
var providers = []*providerSpec{orcaRouter, openRouter}

// route picks the gateway for a guild's next model call, and returns the
// sealed credential it needs.
//
// Derived from which keys the guild holds rather than from a stored setting.
// A provider column and a key column can disagree, and what that produces is
// a guild whose scanning silently stopped pointing at a gateway it has no
// credential for. Here the two cannot drift, because there is only one fact.
func route(cfg Config) (*providerSpec, []byte) {
	if len(cfg.OrcaKeySealed) > 0 {
		return orcaRouter, cfg.OrcaKeySealed
	}
	return openRouter, cfg.APIKeySealed
}

// sealedKeyFor returns the guild's stored credential for one gateway.
func sealedKeyFor(cfg Config, spec *providerSpec) []byte {
	if spec == orcaRouter {
		return cfg.OrcaKeySealed
	}
	return cfg.APIKeySealed
}

// providerForKey guesses which gateway a pasted key belongs to. Nil when
// nothing matches, which the caller turns into "say which" rather than into
// a refusal.
func providerForKey(key string) *providerSpec {
	for _, spec := range providers {
		if len(key) > len(spec.keyPrefix) && key[:len(spec.keyPrefix)] == spec.keyPrefix {
			return spec
		}
	}
	return nil
}

// privacyLine says what this gateway can and cannot be told about the text
// sent to it.
//
// Spelled out on /aimod status rather than left in the docs, because it is
// the one promise this plugin makes about member messages and the default
// gateway is the weaker of the two on it. An admin choosing between them is
// entitled to read that in the place they are already looking, and a
// guarantee that quietly stopped applying is worse than one that was never
// claimed.
func privacyLine(spec *providerSpec) string {
	if spec.strictPrefs {
		return "Every request pins zero data retention and refuses providers that collect what they are sent."
	}
	return "No per-request retention control: this gateway has no equivalent of that setting, so what the " +
		"model provider does with a scanned message is governed by their own policy, not by this bot. " +
		"Store an OpenRouter key and clear the OrcaRouter one to move back."
}

// providerByName resolves the stored/option form. Nil when unknown.
func providerByName(name string) *providerSpec {
	for _, spec := range providers {
		if spec.name == name {
			return spec
		}
	}
	return nil
}
