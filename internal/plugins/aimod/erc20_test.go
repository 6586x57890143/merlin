package aimod

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ethServer stands in for a public RPC endpoint, returning one canned body
// and recording the request that asked for it.
// The recorded request is returned as an accessor rather than a pointer, and
// guarded, because a family read now fans its rails out concurrently: several
// handler goroutines write here at once, which is a genuine race however
// briefly the value is wanted afterwards.
func ethServer(t *testing.T, body string) (*ethClient, func() string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = string(raw)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	c := newETHClient(nil, nil)
	// base overrides every rail's endpoint at once, which is exactly what it
	// exists for: a family read now fans out to several chains and a test
	// wants all of them landing on this one server.
	c.base = srv.URL
	return c, func() string {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

// The two rails these tests exercise directly, resolved from the shipped
// table rather than restated here, so a table edit that dropped one fails the
// test instead of quietly testing nothing.
var (
	testRailBase = railByKey("base:USDC")
	testRailTron = railByKey("tron:USDT")
)

// railBalanceOf reads a single rail, which is the unit most of the decoding
// tests below care about. Balances (the whole family, all or nothing) has its
// own tests further down.
func railBalanceOf(c *ethClient, rail *fundingRail, addr string) (float64, error) {
	return c.railBalance(context.Background(), rail, addr)
}

// railServer answers each request from the body that asked for it, so a test
// can make one rail fail while the rest succeed. Returning ok=false answers
// with an HTTP 500, standing in for an endpoint that is simply down.
func railServer(t *testing.T, reply func(body string) (string, bool)) *ethClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body, ok := reply(string(raw))
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	c := newETHClient(nil, nil)
	c.base = srv.URL
	return c
}

func result(hex string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%q}`, hex)
}

const testWallet = "0x4C3f2E391498e2590bd327a7A1CAA68Dd42c4647"

func TestUSDCBalanceDecodesAtSixDecimals(t *testing.T) {
	// 41.20 USDC in base units.
	c, _ := ethServer(t, result(fmt.Sprintf("0x%x", 41_200_000)))

	got, err := railBalanceOf(c, testRailBase, testWallet)
	if err != nil {
		t.Fatalf("USDCBalance: %v", err)
	}
	if math.Abs(got-41.20) > 1e-9 {
		t.Fatalf("balance = %v, want 41.20", got)
	}
}

// An address nobody has ever sent to has no balance, which the node reports
// as an empty or zero word. That is 0 rather than an error: treating it as a
// failure would make a brand new tip jar look broken.
func TestUSDCBalanceEmptyIsZeroNotAnError(t *testing.T) {
	for _, hex := range []string{"0x", "0x0", strings.Repeat("0", 64), "0x" + strings.Repeat("0", 64)} {
		c, _ := ethServer(t, result(hex))
		got, err := railBalanceOf(c, testRailBase, testWallet)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", hex, err)
		}
		if got != 0 {
			t.Fatalf("%q: balance = %v, want 0", hex, got)
		}
	}
}

// The failure that matters most: a JSON-RPC error arrives with HTTP 200, so
// checking the status alone would read a rejected call as a zero balance.
// That is the wrong direction for the call that also validates an address the
// first time somebody sets one.
func TestUSDCBalanceRPCErrorAtHTTP200(t *testing.T) {
	c, _ := ethServer(t, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"execution reverted"}}`)

	if _, err := railBalanceOf(c, testRailBase, testWallet); err == nil {
		t.Fatal("want an error for a JSON-RPC error body, got nil")
	} else if !strings.Contains(err.Error(), "execution reverted") {
		t.Fatalf("error should carry the node's message, got %v", err)
	}
}

func TestUSDCBalanceRejectsUnusableResults(t *testing.T) {
	cases := map[string]string{
		"not hex":            "0xzzzz",
		"wider than uint256": "0x" + strings.Repeat("f", 66),
	}
	for name, hex := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := ethServer(t, result(hex))
			if _, err := railBalanceOf(c, testRailBase, testWallet); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

// A full uint256 does not fit a uint64. Parsing with strconv would wrap it
// into a small plausible number; math/big keeps it absurd, which is what a
// caller needs to see.
func TestUSDCBalanceHandlesFullUint256(t *testing.T) {
	c, _ := ethServer(t, result("0x"+strings.Repeat("f", 64)))

	got, err := railBalanceOf(c, testRailBase, testWallet)
	if err != nil {
		t.Fatalf("USDCBalance: %v", err)
	}
	if got < 1e60 {
		t.Fatalf("a max uint256 should decode to an enormous number, got %v", got)
	}
}

func TestUSDCBalanceRefusesAMalformedAddress(t *testing.T) {
	c, _ := ethServer(t, result("0x0"))
	for _, addr := range []string{"", "0x", "nonsense", "0x4C3f2E391498e2590bd327a7A1CAA68Dd42c46", testWallet + "ff"} {
		if _, err := railBalanceOf(c, testRailBase, addr); err == nil {
			t.Fatalf("%q should be refused before any request is made", addr)
		}
	}
}

// The padding is the part worth pinning. An unpadded address is still valid
// hex and still gets an answer, just for a different account, so getting this
// wrong fails by silently reading somebody else's zero balance.
func TestBalanceOfCalldataIsSelectorPlusPaddedAddress(t *testing.T) {
	bare, err := bareHexAddress(testWallet)
	if err != nil {
		t.Fatalf("bareHexAddress: %v", err)
	}
	got := balanceOfCalldata(bare)
	want := "0x" + balanceOfSelector +
		"000000000000000000000000" + "4c3f2e391498e2590bd327a7a1caa68dd42c4647"
	if got != want {
		t.Fatalf("calldata =\n %s\nwant\n %s", got, want)
	}
	// 0x, 8 selector characters, 64 argument characters.
	if len(got) != 2+8+64 {
		t.Fatalf("calldata is %d characters, want %d", len(got), 2+8+64)
	}
}

func TestUSDCBalanceCallsTheConfiguredContract(t *testing.T) {
	c, seen := ethServer(t, result("0x0"))
	if _, err := railBalanceOf(c, testRailBase, testWallet); err != nil {
		t.Fatalf("USDCBalance: %v", err)
	}

	var req struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
	}
	if err := json.Unmarshal([]byte(seen()), &req); err != nil {
		t.Fatalf("request was not JSON: %v", err)
	}
	if req.Method != "eth_call" {
		t.Fatalf("method = %q, want eth_call", req.Method)
	}
	call, ok := req.Params[0].(map[string]any)
	if !ok {
		t.Fatalf("first param is not an object: %#v", req.Params[0])
	}
	// Compared case-insensitively: the contract is configured in its EIP-55
	// mixed-case form and goes over the wire lowercased, because the same
	// conversion now serves TRON, where an address arrives base58 and comes
	// out of the decoder as plain hex. RPC treats the two as one address.
	if !strings.EqualFold(fmt.Sprint(call["to"]), testRailBase.defaultContract) {
		t.Fatalf("to = %v, want the USDC contract %s", call["to"], testRailBase.defaultContract)
	}
	if req.Params[1] != "latest" {
		t.Fatalf("block = %v, want latest", req.Params[1])
	}
}

func TestValidAddress(t *testing.T) {
	good := []string{
		testWallet,
		strings.ToLower(testWallet), // a lowercase address is valid; EIP-55 is not checked
		"0x" + strings.Repeat("0", 40),
	}
	for _, a := range good {
		if !validAddress(a) {
			t.Fatalf("%q should be accepted", a)
		}
	}

	bad := []string{"", "0x", testWallet[2:], testWallet + "0", "0x" + strings.Repeat("g", 40)}
	for _, a := range bad {
		if validAddress(a) {
			t.Fatalf("%q should be rejected", a)
		}
	}
}

func TestNewETHClientDefaultsEveryRail(t *testing.T) {
	c := newETHClient(nil, nil)
	for _, rail := range fundingRails {
		got, ok := c.endpoints[rail.key()]
		if !ok {
			t.Fatalf("%s has no endpoint", rail.key())
		}
		// An endpoint on one chain with a token address from another reports
		// a zero balance rather than an error, so a rail that lost either
		// half of its pair fails silently in production.
		if got.rpcURL != rail.defaultRPCURL || got.contract != rail.defaultContract {
			t.Fatalf("%s endpoint = %+v, want %s / %s", rail.key(), got, rail.defaultRPCURL, rail.defaultContract)
		}
		if rail.decimals <= 0 {
			t.Fatalf("%s has no decimals set", rail.key())
		}
		if rail.family == nil || rail.explorer == "" {
			t.Fatalf("%s is missing a family or an explorer link", rail.key())
		}
	}
}

// Decimals are per deployment and were confirmed live against each contract.
// BNB Chain is the one that bites: its USDT and USDC are 18, and reading them
// at the usual 6 would report a one dollar donation as a trillion.
func TestBNBChainRailsAreEighteenDecimals(t *testing.T) {
	for _, key := range []string{"bsc:USDT", "bsc:USDC"} {
		rail := railByKey(key)
		if rail == nil {
			t.Fatalf("%s is missing from the table", key)
		}
		if rail.decimals != 18 {
			t.Fatalf("%s decimals = %d, want 18", key, rail.decimals)
		}
	}
	// And the rest really are 6, so the constant that used to be shared was
	// not merely renamed away.
	for _, key := range []string{"base:USDC", "ethereum:USDT", "tron:USDT", "solana:USDC"} {
		if rail := railByKey(key); rail == nil || rail.decimals != 6 {
			t.Fatalf("%s should be 6 decimals", key)
		}
	}
}

// A rail override is keyed per rail and an endpoint override per chain, so
// moving one chain off a public node moves every token on it.
func TestNewETHClientAppliesOverrides(t *testing.T) {
	c := newETHClient(
		map[string]string{"ethereum": "https://example.invalid/eth"},
		map[string]string{"ethereum:USDT": "0x" + strings.Repeat("a", 40)},
	)
	if got := c.endpoints["ethereum:USDC"]; got.rpcURL != "https://example.invalid/eth" {
		t.Fatalf("chain override missed a rail on the same chain: %+v", got)
	}
	if got := c.endpoints["ethereum:USDT"]; got.contract != "0x"+strings.Repeat("a", 40) {
		t.Fatalf("token override not applied: %+v", got)
	}
	if got := c.endpoints["base:USDC"]; got.rpcURL != railByKey("base:USDC").defaultRPCURL {
		t.Fatalf("an override on one chain changed another: %+v", got)
	}
}

// A real TRON mainnet address: Tether's own USDT contract, which is also the
// default this file ships. Chosen because it is verifiable from any block
// explorer rather than invented here.
const testTronAddress = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

// The 20 byte hex form of the address above, which is what an eth_call
// carries. TRON's own tooling shows it with the 0x41 version byte in front;
// that byte is the chain's, not the address's, and sending it would shift the
// argument by one byte and read a different account.
const testTronHex = "a614f803b6fd780986a42c78ec9c7f77e6ded13c"

func TestTronAddressDecodesToTwentyBytes(t *testing.T) {
	got, err := tronAddressToHex(testTronAddress)
	if err != nil {
		t.Fatalf("tronAddressToHex: %v", err)
	}
	if got != testTronHex {
		t.Fatalf("hex = %q, want %q", got, testTronHex)
	}
	if len(got) != 40 {
		t.Fatalf("hex is %d characters, want 40: the version byte is still attached", len(got))
	}
}

// Unlike an EVM address, a TRON one carries a checksum, so the typo that
// would otherwise publish an address nobody controls is catchable. This is
// the whole reason set-address can be stricter on this chain.
func TestTronAddressRejectsBadInput(t *testing.T) {
	// One character changed from the real address, which is what a mistyped
	// or truncated-and-repaired paste looks like.
	mangled := "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6u"
	for _, bad := range []string{
		mangled,
		"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6",   // one short
		"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6tt", // one long
		"T0000000000000000000000000000000O0",  // characters outside base58
		"",
	} {
		if _, err := tronAddressToHex(bad); err == nil {
			t.Errorf("tronAddressToHex(%q) succeeded, want an error", bad)
		}
		if familyFor(bad) == familyTron {
			t.Errorf("familyFor(%q) placed it on TRON", bad)
		}
	}
}

// The family is read off the address so the two can never disagree, which is
// what makes the networks printed under an address trustworthy. A family
// rather than a chain, because a 0x address is the same account on all five
// EVM chains and "which chain is this" has no answer to derive.
func TestFamilyForPicksByAddressShape(t *testing.T) {
	cases := map[string]*walletFamily{
		testWallet:                  familyEVM,
		strings.ToLower(testWallet): familyEVM,
		testTronAddress:             familyTron,
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": familySolana,
		"not an address": nil,
		"":               nil,
	}
	for addr, want := range cases {
		if got := familyFor(addr); got != want {
			t.Errorf("familyFor(%q) = %v, want %v", addr, got, want)
		}
	}
}

// The three address forms cannot be confused for one another. A TRON address
// decodes to 25 bytes and a Solana one to 32, which needs at least 43 base58
// characters, so the 34 character TRON form can never reach the Solana branch.
func TestSolanaAddressValidation(t *testing.T) {
	if err := validSolanaAddress("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"); err != nil {
		t.Fatalf("a real mint address was rejected: %v", err)
	}
	for _, bad := range []string{
		testTronAddress, // 25 bytes, not 32
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt",    // truncated
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v0", // 0 is not base58
		"",
	} {
		if err := validSolanaAddress(bad); err == nil {
			t.Errorf("validSolanaAddress(%q) succeeded, want an error", bad)
		}
	}
	// Weaker than the TRON check by nature, and the comment on the function
	// says so: a Solana address is a raw key with no checksum, so a typo that
	// still decodes to 32 bytes is accepted. Pinned so nobody later assumes
	// parity between the two families.
	mangled := "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1u"
	if err := validSolanaAddress(mangled); err != nil {
		t.Fatalf("a one character change should still pass: there is no checksum to catch it (%v)", err)
	}
}

// TronGrid answers Ethereum's JSON-RPC for reads, so the same eth_call serves
// both chains once the address is in hex. Same raw units, same decimals, same
// number out: if that stops being true the jar reports the wrong figure to
// donors rather than failing.
func TestTronBalanceUsesTheSameCallAsEVM(t *testing.T) {
	c, seen := ethServer(t, result("0x0000000000000000000000000000000000000000000000000000000000989680"))
	got, err := railBalanceOf(c, testRailTron, testTronAddress)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if got != 10 {
		t.Fatalf("balance = %v, want 10 at six decimals", got)
	}

	var req struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
	}
	if err := json.Unmarshal([]byte(seen()), &req); err != nil {
		t.Fatalf("request was not JSON: %v", err)
	}
	if req.Method != "eth_call" {
		t.Fatalf("method = %q, want eth_call", req.Method)
	}
	call := req.Params[0].(map[string]any)
	// The token contract goes through the same base58 conversion as the
	// wallet: TronGrid wants Ethereum-shaped addresses in both fields, and an
	// unconverted one reads a different account and returns zero.
	if call["to"] != "0x"+testTronHex {
		t.Errorf("to = %v, want the converted USDT contract 0x%s", call["to"], testTronHex)
	}
	if data, _ := call["data"].(string); !strings.HasSuffix(data, testTronHex) {
		t.Errorf("data = %v, want the wallet left padded into the argument word", call["data"])
	}
}

// Every family has to carry a full set of strings. The renderer prints the
// swap advice and the explorer link conditionally, so a family missing either
// loses a line from the public jar silently rather than failing anywhere a
// test or a log would notice.
func TestEveryWalletFamilyIsComplete(t *testing.T) {
	for _, f := range walletFamilies {
		if f.name == "" || f.label == "" || f.networks == "" || f.note == "" {
			t.Errorf("family %+v is missing a name, label, networks or note", f)
		}
		if f.swap == "" || f.explorer == "" {
			t.Errorf("family %q has no routing advice or no explorer link", f.name)
		}
		if !strings.Contains(f.explorer, "%s") {
			t.Errorf("family %q explorer %q has no address placeholder", f.name, f.explorer)
		}
		// Every family needs at least one rail, or an address in it reads as
		// permanently empty rather than as unsupported.
		if len(railsFor(f)) == 0 {
			t.Errorf("family %q has no rails", f.name)
		}
	}
}
