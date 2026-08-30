package aimod

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The tip jar reads stablecoin balances over plain JSON-RPC. No web3 library:
// an ERC-20 read is one four byte selector and one left padded argument, which
// is a Sprintf rather than a dependency, and Solana's is one documented method
// returning the amount already parsed.
//
// Conventions here are openrouter.go's rather than new ones: a package level
// timeout, a base field so tests can point at one httptest server, a size
// capped body, and a typed error on a non-2xx.
const (
	// balanceOfSelector is the first four bytes of the keccak256 hash of
	// "balanceOf(address)". Hardcoded rather than computed: it is a constant
	// of the ERC-20 standard, and computing it would mean a keccak
	// dependency to rediscover a number that has not changed since 2015.
	balanceOfSelector = "70a08231"

	ethHTTPTimeout      = 15 * time.Second
	maxETHResponseBytes = 1 << 16
)

// walletFamily is the set of chains one address form can hold funds on.
//
// The unit is a family rather than a chain because an address does not name a
// chain. A 0x address is the same account on Base, Ethereum, Polygon,
// Arbitrum and BNB Chain, since one private key controls all five, so "which
// chain is this address on" has no answer to derive. merlin therefore reads
// every chain in the family and lets the donor send on whichever is cheapest
// for them. Nothing about the chain is stored and nothing is guessed, which is
// a stronger version of the property the old single-chain derivation reached
// for and could not keep once a second EVM chain existed.
type walletFamily struct {
	// name is stored nowhere; it exists for log lines and test names.
	name string
	// label completes "Send ... to" in the public embed.
	label string
	// networks is the one line a donor has to get right.
	networks string
	// note is the rest of the warning. Families fail differently, so this is
	// per family rather than one interpolated sentence.
	note string
	// swap is the how-to-get-here line. Separate strings rather than one with
	// the network substituted in: the cheapest route onto an EVM chain, onto
	// TRON and onto Solana are three different routes, and a sentence that
	// merely swapped the noun would be wrong about two of them.
	swap string
	// explorer is a public block explorer for the whole address, so anybody
	// can audit the jar without trusting this bot's arithmetic. For EVM it is
	// deliberately Etherscan's multi-chain view: a single per-chain explorer
	// would show one of five ledgers and imply the other four were empty.
	explorer string
}

var familyEVM = &walletFamily{
	name:     "evm",
	label:    "USDC or USDT on any of these networks",
	networks: "Base, Ethereum, Polygon, Arbitrum or BNB Chain",
	note: "Send only USDC or USDT, and only on one of those five networks. " +
		"Another token, or the same token on a network not listed, will not be counted here.",
	swap: "Holding SOL, ETH or anything else? Swap it to USDC on an exchange and withdraw on Base, " +
		"which is usually the cheapest way across. Polygon and BNB Chain are close behind.",
	explorer: "https://blockscan.com/address/%s",
}

var familyTron = &walletFamily{
	name:     "tron",
	label:    "USDT on TRON (TRC-20)",
	networks: "TRON",
	note: "TRON, not Ethereum and not BNB Chain. USDT on any other network goes to a different ledger " +
		"and is not counted here.",
	swap: "Most exchanges let you withdraw USDT directly on TRON, which is usually the cheapest transfer " +
		"of the lot. You need a little TRX in the sending wallet for gas.",
	explorer: "https://tronscan.org/#/address/%s",
}

var familySolana = &walletFamily{
	name:     "solana",
	label:    "USDC or USDT on Solana",
	networks: "Solana",
	note: "Solana, and only USDC or USDT. Sending SOL itself, or either token on another network, " +
		"is not counted here.",
	swap: "Most exchanges withdraw USDC on Solana for a few cents. Keep a little SOL in the sending " +
		"wallet for the fee.",
	explorer: "https://solscan.io/account/%s",
}

var walletFamilies = []*walletFamily{familyEVM, familyTron, familySolana}

// fundingRail is one token on one chain: something merlin can read a balance
// from, and a place a donor can send to.
//
// Deliberately separate from what a gateway's checkout accepts, which used to
// be the same struct. The two are facts of completely different kinds. A rail
// is a fact about a blockchain: stable, verifiable from here, and checked live
// against the contract's own symbol() and decimals() before it was added. What
// a checkout accepts is a fact about somebody else's merchant configuration:
// it can change without notice, this bot cannot observe it, and stating it
// with the same confidence as the first kind is how the previous version came
// to tell donors that Ethereum mainnet USDC would be lost when OpenRouter's
// checkout takes it perfectly well. See providerSpec.topUpRails.
type fundingRail struct {
	chain  string
	asset  string
	family *walletFamily
	// decimals is per deployment, never assumed. USDC and USDT are 6 almost
	// everywhere and 18 on BNB Chain, and reading a BNB balance as 6 would
	// have reported a one dollar donation as a trillion.
	decimals int
	// solana marks the rails read with getTokenAccountsByOwner rather than
	// eth_call. One bool rather than a second client type: everything else
	// about the two paths, the timeout, the body cap, the HTTP 200 error
	// handling, is identical.
	solana          bool
	defaultRPCURL   string
	defaultContract string
	// explorer links this one chain's view of the address, used for rails
	// that actually hold something.
	explorer string
	// note is an optional per-rail clarification shown next to a balance.
	note string
}

// key identifies a rail in the stored breakdown and in the endpoint map.
func (r *fundingRail) key() string { return r.chain + ":" + r.asset }

// fundingRails is the whole of what the tip jar can see.
//
// Chosen for cheap transfers and common holdings, and every contract below was
// confirmed live against symbol() and decimals() rather than written from
// memory. That check is not ceremony: it caught BNB Chain's 18 decimals, and
// the codebase already carries a scar from the same class of mistake, since
// Base's USDbC reports a symbol one character off native USDC and is
// unspendable at OpenRouter's checkout. Polygon is worse still, where the
// bridged USDC.e reports the symbol "USDC" identically to the native token, so
// only the address distinguishes them.
//
// ponytail: fixed table of stablecoins, not arbitrary tokens. Reading whatever
// a donor might send needs a token indexer plus a price oracle to value it in
// USD, which is an API key and a dependency for something a donor solves by
// swapping first. Add those only if donations actually start arriving in other
// assets often enough to measure.
var fundingRails = []*fundingRail{
	{
		chain: "base", asset: "USDC", family: familyEVM, decimals: 6,
		defaultRPCURL:   "https://mainnet.base.org",
		defaultContract: "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913",
		explorer:        "https://basescan.org/address/%s",
		note:            "native USDC, not USDbC",
	},
	{
		chain: "ethereum", asset: "USDC", family: familyEVM, decimals: 6,
		defaultRPCURL:   "https://ethereum-rpc.publicnode.com",
		defaultContract: "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
		explorer:        "https://etherscan.io/address/%s",
	},
	{
		chain: "ethereum", asset: "USDT", family: familyEVM, decimals: 6,
		defaultRPCURL:   "https://ethereum-rpc.publicnode.com",
		defaultContract: "0xdAC17F958D2ee523a2206206994597C13D831ec7",
		explorer:        "https://etherscan.io/address/%s",
	},
	{
		chain: "polygon", asset: "USDC", family: familyEVM, decimals: 6,
		defaultRPCURL:   "https://polygon-bor-rpc.publicnode.com",
		defaultContract: "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359",
		explorer:        "https://polygonscan.com/address/%s",
		note:            "native USDC, not the bridged USDC.e",
	},
	{
		chain: "polygon", asset: "USDT", family: familyEVM, decimals: 6,
		defaultRPCURL:   "https://polygon-bor-rpc.publicnode.com",
		defaultContract: "0xc2132D05D31c914a87C6611C10748AEb04B58e8F",
		explorer:        "https://polygonscan.com/address/%s",
		note:            "Tether's omnichain USDT; wallets show it as USDT0",
	},
	{
		chain: "arbitrum", asset: "USDC", family: familyEVM, decimals: 6,
		defaultRPCURL:   "https://arb1.arbitrum.io/rpc",
		defaultContract: "0xaf88d065e77c8cC2239327C5EDb3A432268e5831",
		explorer:        "https://arbiscan.io/address/%s",
	},
	{
		chain: "arbitrum", asset: "USDT", family: familyEVM, decimals: 6,
		defaultRPCURL:   "https://arb1.arbitrum.io/rpc",
		defaultContract: "0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9",
		explorer:        "https://arbiscan.io/address/%s",
		note:            "Tether's omnichain USDT; wallets show it as USD₮0",
	},
	{
		// 18 decimals, not 6. Confirmed live, and the one rail here where
		// assuming the usual stablecoin scale would have been wrong by a
		// factor of a trillion.
		chain: "bsc", asset: "USDT", family: familyEVM, decimals: 18,
		defaultRPCURL:   "https://bsc-dataseed.binance.org",
		defaultContract: "0x55d398326f99059fF775485246999027B3197955",
		explorer:        "https://bscscan.com/address/%s",
	},
	{
		chain: "bsc", asset: "USDC", family: familyEVM, decimals: 18,
		defaultRPCURL:   "https://bsc-dataseed.binance.org",
		defaultContract: "0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d",
		explorer:        "https://bscscan.com/address/%s",
	},
	{
		// TronGrid answers Ethereum's JSON-RPC for reads, which is why this
		// rail needs no separate code path: the eth_call is byte for byte the
		// same request, and only the address encoding differs.
		chain: "tron", asset: "USDT", family: familyTron, decimals: 6,
		defaultRPCURL:   "https://api.trongrid.io/jsonrpc",
		defaultContract: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		explorer:        "https://tronscan.org/#/address/%s",
	},
	{
		chain: "solana", asset: "USDC", family: familySolana, decimals: 6, solana: true,
		defaultRPCURL:   "https://api.mainnet-beta.solana.com",
		defaultContract: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		explorer:        "https://solscan.io/account/%s",
	},
	{
		chain: "solana", asset: "USDT", family: familySolana, decimals: 6, solana: true,
		defaultRPCURL:   "https://api.mainnet-beta.solana.com",
		defaultContract: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
		explorer:        "https://solscan.io/account/%s",
	},
}

// railsFor lists the rails merlin reads for one family, in table order.
func railsFor(family *walletFamily) []*fundingRail {
	var out []*fundingRail
	for _, r := range fundingRails {
		if r.family == family {
			out = append(out, r)
		}
	}
	return out
}

// railByKey resolves a stored breakdown key back to its rail. Nil for a key
// written by a version that listed a rail this one does not, which the
// renderer skips rather than guessing at.
func railByKey(key string) *fundingRail {
	for _, r := range fundingRails {
		if r.key() == key {
			return r
		}
	}
	return nil
}

// familyFor identifies which family an address belongs to, from its form
// alone. Nil when it is not a well formed address in any of them.
//
// The three forms cannot collide. An EVM address is the only one starting 0x;
// a TRON address is 34 base58 characters carrying a checksum; and a Solana
// address decodes to 32 bytes, which needs at least 43 base58 characters and
// so can never be mistaken for TRON's 25.
func familyFor(address string) *walletFamily {
	switch {
	case evmAddressPattern.MatchString(address):
		return familyEVM
	case tronAddressPattern.MatchString(address):
		if _, err := tronAddressToHex(address); err == nil {
			return familyTron
		}
	case solanaAddressPattern.MatchString(address):
		if err := validSolanaAddress(address); err == nil {
			return familySolana
		}
	}
	return nil
}

// evmAddressPattern is the whole of EVM address validation, on purpose.
//
// EIP-55 checksum validation is deliberately not done: a fully lowercase
// address is perfectly valid, so rejecting on a failed checksum would refuse
// correct input, and verifying one needs keccak256. This catches the typo that
// actually happens, which is a truncated or over-long paste.
var evmAddressPattern = regexp.MustCompile("^0x[0-9a-fA-F]{40}$")

// tronAddressPattern is a first sieve only: it fixes the alphabet, the version
// character and the length, and tronAddressToHex then verifies the checksum.
var tronAddressPattern = regexp.MustCompile("^T[1-9A-HJ-NP-Za-km-z]{33}$")

// solanaAddressPattern is likewise a sieve, and validSolanaAddress then checks
// the decoded length.
var solanaAddressPattern = regexp.MustCompile("^[1-9A-HJ-NP-Za-km-z]{32,44}$")

// validAddress reports whether s is well formed enough to be worth sending to
// an RPC. It says nothing about whether anybody controls it.
func validAddress(s string) bool { return familyFor(s) != nil }

// base58Alphabet is Bitcoin's, which both TRON and Solana reuse. No 0, O, I
// or l.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Decode converts a base58 string to its bytes, preserving leading
// zeroes.
//
// Hand rolled on math/big rather than pulled in as a dependency, matching the
// note at the top of this file: base58 is a base conversion, and the whole of
// it is shorter than the go.mod line that would replace it.
func base58Decode(s string) ([]byte, error) {
	num := new(big.Int)
	radix := big.NewInt(58)
	for _, r := range s {
		idx := strings.IndexRune(base58Alphabet, r)
		if idx < 0 {
			return nil, fmt.Errorf("%q is not base58", s)
		}
		num.Mul(num, radix)
		num.Add(num, big.NewInt(int64(idx)))
	}
	decoded := num.Bytes()
	// Every leading '1' is a leading zero byte that Bytes() cannot know about.
	// Dropping this rule would leave a decoder that is quietly wrong for any
	// input that has one, and Solana keys legitimately do.
	for i := 0; i < len(s) && s[i] == '1'; i++ {
		decoded = append([]byte{0}, decoded...)
	}
	return decoded, nil
}

// tronAddressToHex turns a base58check TRON address into the bare 20 byte hex
// an eth_call wants, verifying the checksum on the way.
func tronAddressToHex(address string) (string, error) {
	decoded, err := base58Decode(address)
	if err != nil {
		return "", fmt.Errorf("tron: %w", err)
	}
	if len(decoded) != 25 {
		return "", fmt.Errorf("tron: %q decodes to %d bytes, not 25", address, len(decoded))
	}

	payload, want := decoded[:21], decoded[21:]
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	if !bytes.Equal(second[:4], want) {
		// The reason TRON gets a real check and the others do not: a mistyped
		// TRON address is almost always caught here, before an address nobody
		// controls is published as somewhere to send money.
		return "", fmt.Errorf("tron: %q has a bad checksum", address)
	}
	if payload[0] != 0x41 {
		return "", fmt.Errorf("tron: %q is not a mainnet address", address)
	}
	return hex.EncodeToString(payload[1:]), nil
}

// validSolanaAddress checks that an address decodes to a 32 byte public key.
//
// That is genuinely all there is to check, and it is weaker than the TRON
// check above rather than equivalent to it: a Solana address is a raw ed25519
// public key with no checksum, so a typo that still decodes to 32 bytes is
// accepted here and cannot be caught by anything short of asking the chain.
// Worth stating plainly, because the set-address confirmation reads the same
// on both families and the guarantee behind it does not.
func validSolanaAddress(address string) error {
	decoded, err := base58Decode(address)
	if err != nil {
		return fmt.Errorf("solana: %w", err)
	}
	if len(decoded) != 32 {
		return fmt.Errorf("solana: %q decodes to %d bytes, not 32", address, len(decoded))
	}
	return nil
}

// bareHexAddress is the 20 byte hex form of an EVM or TRON address, with no
// 0x. One function because everything downstream of it, the calldata padding
// and the eth_call "to" field, is identical on both.
func bareHexAddress(address string) (string, error) {
	if tronAddressPattern.MatchString(address) {
		return tronAddressToHex(address)
	}
	if !evmAddressPattern.MatchString(address) {
		return "", fmt.Errorf("eth: %q is not a wallet address", address)
	}
	return strings.ToLower(strings.TrimPrefix(address, "0x")), nil
}

// chainEndpoint is where one rail is read and which token is read there. They
// travel together because an endpoint on one chain with a token address from
// another reports a zero balance rather than an error, which is the worst way
// for a misconfiguration to present.
type chainEndpoint struct {
	rpcURL   string
	contract string
}

type ethClient struct {
	http *http.Client
	// base overrides every endpoint, for tests pointing at one httptest
	// server. Empty in production.
	base      string
	endpoints map[string]chainEndpoint
}

// newETHClient binds an endpoint and a token contract per rail, each falling
// back to that rail's own default.
//
// Overrides arrive as maps rather than as a fixed argument list, which is what
// lets the rail table grow without this signature growing with it. RPC
// overrides are keyed by chain, since every rail on one chain shares an
// endpoint; token overrides are keyed by rail. See config.GlobalConfig for the
// environment variables they come from.
func newETHClient(rpcOverrides, tokenOverrides map[string]string) *ethClient {
	endpoints := make(map[string]chainEndpoint, len(fundingRails))
	for _, rail := range fundingRails {
		e := chainEndpoint{rpcURL: rail.defaultRPCURL, contract: rail.defaultContract}
		if v := rpcOverrides[rail.chain]; v != "" {
			e.rpcURL = v
		}
		if v := tokenOverrides[rail.key()]; v != "" {
			e.contract = v
		}
		endpoints[rail.key()] = e
	}
	return &ethClient{
		http:      &http.Client{Timeout: ethHTTPTimeout},
		endpoints: endpoints,
	}
}

// balanceOfCalldata builds the eth_call data for balanceOf(owner): the
// selector followed by the address left padded to a full 32 byte word.
//
// The padding is the part worth testing. An address dropped in unpadded is
// still valid hex and still gets an answer, just for the wrong account, so
// this fails by silently reading a zero balance rather than by erroring.
func balanceOfCalldata(bare string) string {
	return "0x" + balanceOfSelector + strings.Repeat("0", 64-len(bare)) + bare
}

type ethRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *ethRPCError) Error() string {
	return fmt.Sprintf("eth rpc error %d: %s", e.Code, e.Message)
}

// Balances reads every rail in the address's family and returns the per-rail
// amounts plus their total, in whole tokens.
//
// All or nothing, and that is the single most important property here. A
// partial sum is indistinguishable from a smaller balance, and pollFunding
// reads a fall in balance as the operator withdrawing to buy credits. So one
// unreachable RPC would book a withdrawal that never happened, and that same
// RPC recovering would then book the difference as a donation nobody made. An
// error costs a fifteen minute retry; the alternative silently corrupts the
// running total this feature exists to publish.
//
// The rails run concurrently on a plain WaitGroup, the same shape as the reads
// at the top of rotation.rotate(). Twelve requests every fifteen minutes per
// guild does not justify JSON-RPC batching, which several public endpoints
// refuse anyway.
func (c *ethClient) Balances(ctx context.Context, address string) (map[string]float64, float64, error) {
	family := familyFor(address)
	if family == nil {
		return nil, 0, fmt.Errorf("eth: %q is not a wallet address on any supported network", address)
	}

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		amounts = make(map[string]float64)
		firstEr error
	)
	for _, rail := range railsFor(family) {
		wg.Add(1)
		go func(rail *fundingRail) {
			defer wg.Done()
			amount, err := c.railBalance(ctx, rail, address)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstEr == nil {
					firstEr = fmt.Errorf("%s: %w", rail.key(), err)
				}
				return
			}
			amounts[rail.key()] = amount
		}(rail)
	}
	wg.Wait()
	if firstEr != nil {
		return nil, 0, firstEr
	}

	var total float64
	for _, v := range amounts {
		total += v
	}
	return amounts, total, nil
}

// railBalance reads one token on one chain.
func (c *ethClient) railBalance(ctx context.Context, rail *fundingRail, owner string) (float64, error) {
	endpoint, ok := c.endpoints[rail.key()]
	if !ok {
		return 0, fmt.Errorf("eth: no endpoint configured for %s", rail.key())
	}
	url := endpoint.rpcURL
	if c.base != "" {
		url = c.base
	}
	if rail.solana {
		return c.solanaBalance(ctx, url, endpoint.contract, owner)
	}
	return c.evmBalance(ctx, url, endpoint.contract, owner, rail.decimals)
}

// evmBalance reads an ERC-20 balance with eth_call, on any EVM chain and on
// TRON, whose node speaks the same JSON-RPC.
func (c *ethClient) evmBalance(ctx context.Context, url, contract, owner string, decimals int) (float64, error) {
	bare, err := bareHexAddress(owner)
	if err != nil {
		return 0, err
	}
	// The token contract goes through the same conversion as the wallet.
	// TronGrid wants Ethereum-shaped addresses in both fields, and the
	// contract is configured in the base58 form an operator would copy off an
	// explorer.
	to, err := bareHexAddress(contract)
	if err != nil {
		return 0, fmt.Errorf("eth: token contract %q: %w", contract, err)
	}

	raw, err := c.post(ctx, url, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params": []any{
			map[string]string{"to": "0x" + to, "data": balanceOfCalldata(bare)},
			"latest",
		},
	})
	if err != nil {
		return 0, err
	}

	var envelope struct {
		Result string       `json:"result"`
		Error  *ethRPCError `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return 0, fmt.Errorf("eth: decode response: %w", err)
	}
	// A JSON-RPC error arrives with HTTP 200. Checking the status alone would
	// read a rejected request as a zero balance, which is the wrong direction
	// for the call that also validates an address when one is first set.
	if envelope.Error != nil {
		return 0, envelope.Error
	}
	return parseTokenAmount(envelope.Result, decimals)
}

// solanaBalance sums the owner's token accounts for one mint.
//
// Summed rather than read from the first account, because an owner can hold
// several token accounts for the same mint (an associated one, plus anything
// an exchange or a program opened), and reading only the first would under
// report a balance somebody had genuinely sent.
//
// The node's own decimals are used rather than this rail's, because the node
// has already read them off the mint: taking its answer removes the one place
// a hardcoded scale could disagree with the chain.
func (c *ethClient) solanaBalance(ctx context.Context, url, mint, owner string) (float64, error) {
	if err := validSolanaAddress(owner); err != nil {
		return 0, err
	}
	raw, err := c.post(ctx, url, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "getTokenAccountsByOwner",
		"params": []any{
			owner,
			map[string]string{"mint": mint},
			map[string]string{"encoding": "jsonParsed"},
		},
	})
	if err != nil {
		return 0, err
	}

	var envelope struct {
		Result struct {
			Value []struct {
				Account struct {
					Data struct {
						Parsed struct {
							Info struct {
								TokenAmount struct {
									Amount   string `json:"amount"`
									Decimals int    `json:"decimals"`
								} `json:"tokenAmount"`
							} `json:"info"`
						} `json:"parsed"`
					} `json:"data"`
				} `json:"account"`
			} `json:"value"`
		} `json:"result"`
		Error *ethRPCError `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return 0, fmt.Errorf("solana: decode response: %w", err)
	}
	// Same trap as eth_call above: Solana's JSON-RPC also reports errors with
	// HTTP 200, so an owner the node rejected would otherwise read as empty.
	if envelope.Error != nil {
		return 0, envelope.Error
	}

	var total float64
	for _, acct := range envelope.Result.Value {
		amount := acct.Account.Data.Parsed.Info.TokenAmount
		v, ok := new(big.Int).SetString(amount.Amount, 10)
		if !ok {
			return 0, fmt.Errorf("solana: %q is not an amount", amount.Amount)
		}
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(amount.Decimals)), nil)
		f, _ := new(big.Rat).SetFrac(v, scale).Float64()
		total += f
	}
	return total, nil
}

// post sends one JSON-RPC request and returns the size capped body.
func (c *ethClient) post(ctx context.Context, url string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("eth: build request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("eth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("eth: call %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxETHResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("eth: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("eth: call returned %d", resp.StatusCode)
	}
	return raw, nil
}

// parseTokenAmount converts a hex quantity to whole tokens.
//
// math/big rather than strconv.ParseUint, because a uint256 does not fit a
// uint64 and a hostile or buggy contract should not silently wrap into a small
// number. The float64 conversion happens once, at the end, and only because
// the result is going to be displayed as dollars.
func parseTokenAmount(hex string, decimals int) (float64, error) {
	bare := strings.TrimPrefix(strings.TrimSpace(hex), "0x")
	// An empty result is 0 rather than an error: it is what a call against a
	// wallet nobody has donated to yet returns.
	if bare == "" {
		return 0, nil
	}
	if len(bare) > 64 {
		return 0, fmt.Errorf("eth: balance is %d hex digits, wider than a uint256", len(bare))
	}
	v, ok := new(big.Int).SetString(bare, 16)
	if !ok {
		return 0, fmt.Errorf("eth: %q is not a hex quantity", hex)
	}

	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	amount, _ := new(big.Rat).SetFrac(v, scale).Float64()
	return amount, nil
}

// sortedRailKeys orders a stored breakdown by the rail table rather than by
// map iteration, so the public embed does not reshuffle itself between polls.
func sortedRailKeys(balances map[string]float64) []string {
	order := make(map[string]int, len(fundingRails))
	for i, r := range fundingRails {
		order[r.key()] = i
	}
	keys := make([]string, 0, len(balances))
	for k := range balances {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		oi, oki := order[keys[i]]
		oj, okj := order[keys[j]]
		if oki != okj {
			// A key no longer in the table sorts last rather than being
			// dropped here; the renderer decides what to do with it.
			return oki
		}
		if oki {
			return oi < oj
		}
		return keys[i] < keys[j]
	})
	return keys
}
