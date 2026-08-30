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
	"strings"
	"time"
)

// The tip jar reads an ERC-20 balance over plain JSON-RPC. No web3 library:
// this is one four byte selector and one left padded argument, which is a
// Sprintf rather than a dependency, and one response field parsed as hex.
//
// Conventions here are openrouter.go's rather than new ones: a package level
// timeout, a base field so tests can point at an httptest server, a size
// capped body, and a typed error on a non-2xx.
const (
	// Base (chain 8453) is what OpenRouter's own Coinbase checkout settles
	// USDC on, so a jar for an OpenRouter guild takes the same asset on the
	// same chain the credits are bought with: no bridge, no swap, and gas
	// measured in cents rather than the dollars an Ethereum mainnet tip
	// would burn.
	defaultETHRPCURL    = "https://mainnet.base.org"
	defaultUSDCContract = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"

	// TRON is the other half of the same argument, for the other gateway.
	// OrcaRouter's crypto checkout runs through NOWPayments and settles USDT
	// on TRON, so an OrcaRouter guild's jar has to be a TRON jar or every
	// donation needs a swap and a bridge before it buys a single token.
	//
	// TronGrid speaks Ethereum's JSON-RPC for reads, which is why this file
	// needed a base58 decoder and nothing else: the eth_call below is
	// byte-for-byte the same request on both chains.
	defaultTronRPCURL   = "https://api.trongrid.io/jsonrpc"
	defaultUSDTContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

	// Fixed by the token contracts, not by the chains. USDC and USDT are
	// both 6 decimals everywhere they are deployed, which is why the amount
	// parsing below needs no per-chain branch.
	stablecoinDecimals = 6

	// balanceOfSelector is the first four bytes of the keccak256 hash of
	// "balanceOf(address)". Hardcoded rather than computed: it is a constant
	// of the ERC-20 standard, and computing it would mean a keccak
	// dependency to rediscover a number that has not changed since 2015.
	balanceOfSelector = "70a08231"

	ethHTTPTimeout      = 15 * time.Second
	maxETHResponseBytes = 1 << 16
)

// fundingChain is one asset on one chain: what a jar collects, and what the
// member reading /aimod funding has to be told to send.
//
// Two values, like providerSpec, and for the same reason. A guild's chain is
// never stored: it is read back off the address it holds, so the two cannot
// disagree, and the warning printed under an address is derived from that
// same address rather than written once as a constant. That last part is the
// whole point. Sending on the wrong chain is the mistake people actually
// make, the money does not come back, and a hardcoded "USDC on Base" line
// under a TRON address would be this bot causing it.
type fundingChain struct {
	name  string
	asset string
	// label is the one line a donor has to get right.
	label string
	// note is the rest of the warning, chain specific because the failure is.
	note            string
	defaultRPCURL   string
	defaultContract string
	decimals        int
}

var baseUSDC = &fundingChain{
	name:            "base",
	asset:           "USDC",
	label:           "USDC on Base",
	note:            "Base, not Ethereum mainnet, and USDC rather than USDbC. Anything else sent here is lost.",
	defaultRPCURL:   defaultETHRPCURL,
	defaultContract: defaultUSDCContract,
	decimals:        stablecoinDecimals,
}

var tronUSDT = &fundingChain{
	name:            "tron",
	asset:           "USDT",
	label:           "USDT on TRON (TRC-20)",
	note:            "TRON, not Ethereum and not BNB Chain. USDT on any other network is lost.",
	defaultRPCURL:   defaultTronRPCURL,
	defaultContract: defaultUSDTContract,
	decimals:        stablecoinDecimals,
}

var fundingChains = []*fundingChain{baseUSDC, tronUSDT}

// chainFor identifies the chain from the address itself. Nil when it is not
// a well formed address on either.
func chainFor(address string) *fundingChain {
	switch {
	case addressPattern.MatchString(address):
		return baseUSDC
	case tronAddressPattern.MatchString(address):
		if _, err := tronAddressToHex(address); err == nil {
			return tronUSDT
		}
	}
	return nil
}

// addressPattern is the whole of address validation, on purpose.
//
// EIP-55 checksum validation is deliberately not done: a fully lowercase
// address is perfectly valid, so rejecting on a failed checksum would refuse
// correct input, and verifying one needs keccak256. This catches the typo
// that actually happens, which is a truncated or over-long paste.
var addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// tronAddressPattern is a first sieve only: it fixes the alphabet, the
// version character and the length, and tronAddressToHex then verifies the
// checksum. Unlike an EVM address, a TRON one carries one, so a typo here is
// genuinely catchable rather than merely improbable.
var tronAddressPattern = regexp.MustCompile(`^T[1-9A-HJ-NP-Za-km-z]{33}$`)

// validAddress reports whether s is well formed enough to be worth sending to
// an RPC, on either chain. It says nothing about whether anybody controls it.
func validAddress(s string) bool { return chainFor(s) != nil }

// base58Alphabet is Bitcoin's, which TRON reuses. No 0, O, I or l.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// tronAddressToHex turns a base58check TRON address into the bare 20 byte
// hex an eth_call wants.
//
// Hand rolled on math/big and crypto/sha256 rather than pulled in as a
// dependency, matching the note at the top of this file: base58check is a
// base conversion and two hashes, and the whole of it is shorter than the
// go.mod line that would replace it.
func tronAddressToHex(address string) (string, error) {
	num := new(big.Int)
	radix := big.NewInt(58)
	for _, r := range address {
		idx := strings.IndexRune(base58Alphabet, r)
		if idx < 0 {
			return "", fmt.Errorf("tron: %q is not a base58 address", address)
		}
		num.Mul(num, radix)
		num.Add(num, big.NewInt(int64(idx)))
	}
	decoded := num.Bytes()
	// Every leading '1' is a leading zero byte that Bytes() cannot know
	// about. A TRON address never starts with one, since it starts with the
	// 0x41 version byte, but dropping the rule here would leave a decoder
	// that is quietly wrong for any other base58check input it ever meets.
	for i := 0; i < len(address) && address[i] == '1'; i++ {
		decoded = append([]byte{0}, decoded...)
	}
	if len(decoded) != 25 {
		return "", fmt.Errorf("tron: %q decodes to %d bytes, not 25", address, len(decoded))
	}

	payload, want := decoded[:21], decoded[21:]
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	if !bytes.Equal(second[:4], want) {
		// The reason this chain gets a real check and Base does not: a
		// mistyped TRON address is almost always caught here, before an
		// address nobody controls is published as somewhere to send money.
		return "", fmt.Errorf("tron: %q has a bad checksum", address)
	}
	if payload[0] != 0x41 {
		return "", fmt.Errorf("tron: %q is not a mainnet address", address)
	}
	return hex.EncodeToString(payload[1:]), nil
}

// bareHexAddress is the 20 byte hex form of an address on either chain, with
// no 0x. One function because everything downstream of it, the calldata
// padding and the eth_call "to" field, is identical on both.
func bareHexAddress(address string) (string, error) {
	if tronAddressPattern.MatchString(address) {
		return tronAddressToHex(address)
	}
	if !addressPattern.MatchString(address) {
		return "", fmt.Errorf("eth: %q is not a wallet address", address)
	}
	return strings.ToLower(strings.TrimPrefix(address, "0x")), nil
}

// chainEndpoint is where one chain is read and which token is read there.
// They travel together because an endpoint on one chain with a token address
// from another reports a zero balance rather than an error, which is the
// worst way for a misconfiguration to present.
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

// newETHClient binds an endpoint and a token contract per chain, each
// falling back to the chain's own default.
//
// All four are settable (MERLIN_ETH_RPC_URL, MERLIN_USDC_CONTRACT,
// MERLIN_TRON_RPC_URL, MERLIN_USDT_CONTRACT) because they have to exist as
// values either way, and an endpoint plus a token address is what a chain is
// here: the eth_call below is identical on both.
func newETHClient(ethRPC, usdc, tronRPC, usdt string) *ethClient {
	endpoints := map[string]chainEndpoint{
		baseUSDC.name: {rpcURL: ethRPC, contract: usdc},
		tronUSDT.name: {rpcURL: tronRPC, contract: usdt},
	}
	for _, chain := range fundingChains {
		e := endpoints[chain.name]
		if e.rpcURL == "" {
			e.rpcURL = chain.defaultRPCURL
		}
		if e.contract == "" {
			e.contract = chain.defaultContract
		}
		endpoints[chain.name] = e
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

// Balance returns the owner's token balance on one chain, in whole tokens.
func (c *ethClient) Balance(ctx context.Context, chain *fundingChain, owner string) (float64, error) {
	if chain == nil {
		return 0, fmt.Errorf("eth: no chain for %q", owner)
	}
	bare, err := bareHexAddress(owner)
	if err != nil {
		return 0, err
	}
	endpoint, ok := c.endpoints[chain.name]
	if !ok {
		return 0, fmt.Errorf("eth: no endpoint configured for %s", chain.name)
	}
	// The token contract goes through the same conversion as the wallet.
	// TronGrid wants Ethereum-shaped addresses in both fields, and the
	// contract is configured in the base58 form an operator would copy off
	// an explorer.
	to, err := bareHexAddress(endpoint.contract)
	if err != nil {
		return 0, fmt.Errorf("eth: token contract for %s: %w", chain.name, err)
	}
	url := endpoint.rpcURL
	if c.base != "" {
		url = c.base
	}

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params": []any{
			map[string]string{"to": "0x" + to, "data": balanceOfCalldata(bare)},
			"latest",
		},
	})
	if err != nil {
		return 0, fmt.Errorf("eth: build request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("eth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("eth: call %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxETHResponseBytes))
	if err != nil {
		return 0, fmt.Errorf("eth: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, fmt.Errorf("eth: call returned %d", resp.StatusCode)
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

	return parseTokenAmount(envelope.Result, chain.decimals)
}

// parseTokenAmount converts a hex quantity to whole tokens.
//
// math/big rather than strconv.ParseUint, because a uint256 does not fit a
// uint64 and a hostile or buggy contract should not silently wrap into a
// small number. The float64 conversion happens once, at the end, and only
// because the result is going to be displayed as dollars.
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
