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

	c := newETHClient(srv.URL, defaultUSDCContract)
	return c, &seen
}

func result(hex string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%q}`, hex)
}

const testWallet = "0x4C3f2E391498e2590bd327a7A1CAA68Dd42c4647"

func TestUSDCBalanceDecodesAtSixDecimals(t *testing.T) {
	// 41.20 USDC in base units.
	c, _ := ethServer(t, result(fmt.Sprintf("0x%x", 41_200_000)))

	got, err := c.USDCBalance(context.Background(), testWallet)
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
		got, err := c.USDCBalance(context.Background(), testWallet)
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

	if _, err := c.USDCBalance(context.Background(), testWallet); err == nil {
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
			if _, err := c.USDCBalance(context.Background(), testWallet); err == nil {
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

	got, err := c.USDCBalance(context.Background(), testWallet)
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
		if _, err := c.USDCBalance(context.Background(), addr); err == nil {
			t.Fatalf("%q should be refused before any request is made", addr)
		}
	}
}

// The padding is the part worth pinning. An unpadded address is still valid
// hex and still gets an answer, just for a different account, so getting this
// wrong fails by silently reading somebody else's zero balance.
func TestBalanceOfCalldataIsSelectorPlusPaddedAddress(t *testing.T) {
	got := balanceOfCalldata(testWallet)
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
	if _, err := c.USDCBalance(context.Background(), testWallet); err != nil {
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
	if call["to"] != defaultUSDCContract {
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

func TestNewETHClientDefaultsToBase(t *testing.T) {
	c := newETHClient("", "")
	if c.base != defaultETHRPCURL {
		t.Fatalf("base = %q, want the Base default", c.base)
	}
	// The token address is the one OpenRouter's own checkout settles in.
	// Getting it wrong points donors at a different token on the same chain.
	if c.contract != defaultUSDCContract {
		t.Fatalf("contract = %q, want %q", c.contract, defaultUSDCContract)
	}
	if usdcDecimals != 6 {
		t.Fatalf("USDC is 6 decimals everywhere it is deployed, got %d", usdcDecimals)
	}
}
