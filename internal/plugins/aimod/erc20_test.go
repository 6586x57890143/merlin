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
	"testing"
)

// ethServer stands in for a public RPC endpoint, returning one canned body
// and recording the request that asked for it.
func ethServer(t *testing.T, body string) (*ethClient, *string) {
	t.Helper()
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	c := newETHClient(srv.URL, defaultUSDCContract, srv.URL, defaultUSDTContract)
	return c, &seen
}

func result(hex string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%q}`, hex)
}

const testWallet = "0x4C3f2E391498e2590bd327a7A1CAA68Dd42c4647"

func TestUSDCBalanceDecodesAtSixDecimals(t *testing.T) {
	// 41.20 USDC in base units.
	c, _ := ethServer(t, result(fmt.Sprintf("0x%x", 41_200_000)))

	got, err := c.Balance(context.Background(), baseUSDC, testWallet)
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
		got, err := c.Balance(context.Background(), baseUSDC, testWallet)
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

	if _, err := c.Balance(context.Background(), baseUSDC, testWallet); err == nil {
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
			if _, err := c.Balance(context.Background(), baseUSDC, testWallet); err == nil {
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

	got, err := c.Balance(context.Background(), baseUSDC, testWallet)
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
		if _, err := c.Balance(context.Background(), baseUSDC, addr); err == nil {
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
	if _, err := c.Balance(context.Background(), baseUSDC, testWallet); err != nil {
		t.Fatalf("USDCBalance: %v", err)
	}

	var req struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
	}
	if err := json.Unmarshal([]byte(*seen), &req); err != nil {
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
	if !strings.EqualFold(fmt.Sprint(call["to"]), defaultUSDCContract) {
		t.Fatalf("to = %v, want the USDC contract %s", call["to"], defaultUSDCContract)
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

func TestNewETHClientDefaultsEveryChain(t *testing.T) {
	c := newETHClient("", "", "", "")
	// The token addresses are the ones each gateway's own checkout settles
	// in. Getting either wrong points donors at a different token on the
	// right chain, which reports a zero balance rather than an error.
	want := map[string]chainEndpoint{
		baseUSDC.name: {rpcURL: defaultETHRPCURL, contract: defaultUSDCContract},
		tronUSDT.name: {rpcURL: defaultTronRPCURL, contract: defaultUSDTContract},
	}
	for name, w := range want {
		if got := c.endpoints[name]; got != w {
			t.Fatalf("%s endpoint = %+v, want %+v", name, got, w)
		}
	}
	if stablecoinDecimals != 6 {
		t.Fatalf("USDC and USDT are both 6 decimals, got %d", stablecoinDecimals)
	}
}

// A real TRON mainnet address: Tether's own USDT contract, which is also the
// default this file ships. Chosen because it is verifiable from any block
// explorer rather than invented here.
const testTronAddress = defaultUSDTContract

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
		if chainFor(bad) == tronUSDT {
			t.Errorf("chainFor(%q) placed it on TRON", bad)
		}
	}
}

// The chain is read off the address so that the two can never disagree, which
// is what makes the warning printed under an address trustworthy.
func TestChainForPicksByAddressShape(t *testing.T) {
	if got := chainFor(testWallet); got != baseUSDC {
		t.Errorf("chainFor(EVM address) = %v, want Base", got)
	}
	if got := chainFor(testTronAddress); got != tronUSDT {
		t.Errorf("chainFor(TRON address) = %v, want TRON", got)
	}
	if got := chainFor("not an address"); got != nil {
		t.Errorf("chainFor(junk) = %v, want nil", got)
	}
}

// TronGrid answers Ethereum's JSON-RPC for reads, so the same eth_call serves
// both chains once the address is in hex. Same raw units, same decimals, same
// number out: if that stops being true the jar reports the wrong figure to
// donors rather than failing.
func TestTronBalanceUsesTheSameCallAsBase(t *testing.T) {
	c, seen := ethServer(t, result("0x0000000000000000000000000000000000000000000000000000000000989680"))
	got, err := c.Balance(context.Background(), tronUSDT, testTronAddress)
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
	if err := json.Unmarshal([]byte(*seen), &req); err != nil {
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
