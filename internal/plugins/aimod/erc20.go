package aimod

import (
	"bytes"
	"context"
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
	// USDC on, so the jar takes the same asset on the same chain the credits
	// are bought with: no bridge, no swap, and gas measured in cents rather
	// than the dollars an Ethereum mainnet tip would burn.
	defaultETHRPCURL    = "https://mainnet.base.org"
	defaultUSDCContract = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"

	// usdcDecimals is fixed by the token contract, not by the chain. USDC is
	// 6 decimals everywhere it is deployed.
	usdcDecimals = 6

	// balanceOfSelector is the first four bytes of the keccak256 hash of
	// "balanceOf(address)". Hardcoded rather than computed: it is a constant
	// of the ERC-20 standard, and computing it would mean a keccak
	// dependency to rediscover a number that has not changed since 2015.
	balanceOfSelector = "70a08231"

	ethHTTPTimeout      = 15 * time.Second
	maxETHResponseBytes = 1 << 16
)

// addressPattern is the whole of address validation, on purpose.
//
// EIP-55 checksum validation is deliberately not done: a fully lowercase
// address is perfectly valid, so rejecting on a failed checksum would refuse
// correct input, and verifying one needs keccak256. This catches the typo
// that actually happens, which is a truncated or over-long paste.
var addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// validAddress reports whether s is well formed enough to be worth sending to
// an RPC. It says nothing about whether anybody controls it.
func validAddress(s string) bool { return addressPattern.MatchString(s) }

type ethClient struct {
	http     *http.Client
	base     string
	contract string
}

// newETHClient binds one RPC endpoint and one token contract.
//
// Both are settable (MERLIN_ETH_RPC_URL, MERLIN_USDC_CONTRACT) because they
// have to exist as values either way, and an endpoint and a token address are
// what a different EVM chain is: the eth_call below is identical everywhere.
// They travel together for the same reason, since an endpoint on one chain
// with a token address from another reports a zero balance rather than an
// error, which is the worst way for a misconfiguration to present.
func newETHClient(rpcURL, contract string) *ethClient {
	if rpcURL == "" {
		rpcURL = defaultETHRPCURL
	}
	if contract == "" {
		contract = defaultUSDCContract
	}
	return &ethClient{
		http:     &http.Client{Timeout: ethHTTPTimeout},
		base:     rpcURL,
		contract: contract,
	}
}

// balanceOfCalldata builds the eth_call data for balanceOf(owner): the
// selector followed by the address left padded to a full 32 byte word.
//
// The padding is the part worth testing. An address dropped in unpadded is
// still valid hex and still gets an answer, just for the wrong account, so
// this fails by silently reading a zero balance rather than by erroring.
func balanceOfCalldata(owner string) string {
	bare := strings.ToLower(strings.TrimPrefix(owner, "0x"))
	return "0x" + balanceOfSelector + strings.Repeat("0", 64-len(bare)) + bare
}

type ethRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *ethRPCError) Error() string {
	return fmt.Sprintf("eth rpc error %d: %s", e.Code, e.Message)
}

// USDCBalance returns the owner's token balance in whole USDC.
func (c *ethClient) USDCBalance(ctx context.Context, owner string) (float64, error) {
	if !validAddress(owner) {
		return 0, fmt.Errorf("eth: %q is not a wallet address", owner)
	}

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params": []any{
			map[string]string{"to": c.contract, "data": balanceOfCalldata(owner)},
			"latest",
		},
	})
	if err != nil {
		return 0, fmt.Errorf("eth: build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("eth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("eth: call %s: %w", c.base, err)
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

	return parseTokenAmount(envelope.Result, usdcDecimals)
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
