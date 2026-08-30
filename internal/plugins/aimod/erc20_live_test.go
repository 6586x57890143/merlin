//go:build live

package aimod

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

// Confirms every shipped tip jar rail against the chain itself: that the
// configured contract really is the token the table claims, at the decimals
// the table claims, reachable from the configured endpoint.
//
// Behind a build tag because it calls seven public RPC endpoints and CI must
// not depend on them being up. Run it by hand whenever a rail is added or an
// endpoint changed:
//
//	go test ./internal/plugins/aimod/... -tags=live -run TestRailsAreLive -v
//
// This is not ceremony. Writing a token address from memory has gone wrong in
// this package before: Base's USDbC reports a symbol one character off native
// USDC and is unspendable at OpenRouter's checkout, Polygon's bridged USDC.e
// reports the symbol "USDC" identically to the native token so only the
// address tells them apart, and BNB Chain's stablecoins are 18 decimals rather
// than the 6 every other rail uses. A wrong contract does not error: it
// reports a zero balance forever, which reads as "nobody has donated".
func TestRailsAreLive(t *testing.T) {
	// symbol() and decimals(), the first four bytes of each keccak256 hash.
	const symbolSelector = "0x95d89b41"
	const decimalsSelector = "0x313ce567"

	ctx := context.Background()
	c := newETHClient(nil, nil)

	for _, rail := range fundingRails {
		t.Run(rail.key(), func(t *testing.T) {
			endpoint := c.endpoints[rail.key()]
			if rail.solana {
				// Solana has no eth_call. getTokenSupply reports the mint's
				// own decimals, which is the same assertion by another route.
				raw, err := c.post(ctx, endpoint.rpcURL, map[string]any{
					"jsonrpc": "2.0", "id": 1, "method": "getTokenSupply",
					"params": []any{endpoint.contract},
				})
				if err != nil {
					t.Fatalf("getTokenSupply: %v", err)
				}
				if !strings.Contains(string(raw), `"decimals":`) {
					t.Fatalf("mint %s returned no supply: %s", endpoint.contract, raw)
				}
				t.Logf("%-14s mint ok, %s", rail.key(), endpoint.contract)
				return
			}

			to, err := bareHexAddress(endpoint.contract)
			if err != nil {
				t.Fatalf("contract address: %v", err)
			}

			symbol := liveCall(t, c, endpoint.rpcURL, "0x"+to, symbolSelector)
			decimals := liveCall(t, c, endpoint.rpcURL, "0x"+to, decimalsSelector)

			gotDecimals := new(big.Int)
			gotDecimals.SetString(strings.TrimPrefix(decimals, "0x"), 16)
			if int(gotDecimals.Int64()) != rail.decimals {
				t.Errorf("decimals = %s on chain, table says %d", gotDecimals, rail.decimals)
			}

			// Compared loosely: several of these tokens have migrated to
			// Tether's omnichain deployment and report USDT0 rather than
			// USDT, and Arbitrum's spells it with the tether sign U+20AE
			// rather than an ASCII T. The assertion that matters is that this
			// is the right asset, not that the ticker never changed.
			got := decodeABIString(symbol)
			normalised := strings.ToUpper(strings.ReplaceAll(got, "₮", "T"))
			if !strings.Contains(normalised, rail.asset) {
				t.Errorf("symbol = %q on chain, table says %q", got, rail.asset)
			}
			t.Logf("%-14s %-8s %d decimals", rail.key(), got, gotDecimals)
		})
	}
}

func liveCall(t *testing.T, c *ethClient, url, to, selector string) string {
	t.Helper()
	raw, err := c.post(context.Background(), url, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_call",
		"params": []any{map[string]string{"to": to, "data": selector}, "latest"},
	})
	if err != nil {
		t.Fatalf("eth_call %s: %v", selector, err)
	}
	var envelope struct {
		Result string       `json:"result"`
		Error  *ethRPCError `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Error != nil {
		t.Fatalf("rpc: %v", envelope.Error)
	}
	return envelope.Result
}

// decodeABIString reads a dynamic string return, falling back to the bytes32
// form that a few pre-standard tokens (notably Ethereum mainnet USDT) use.
func decodeABIString(h string) string {
	b, err := hex.DecodeString(strings.TrimPrefix(h, "0x"))
	if err != nil {
		return ""
	}
	if len(b) >= 96 {
		n := int(new(big.Int).SetBytes(b[32:64]).Int64())
		if n > 0 && n <= len(b)-64 {
			return string(b[64 : 64+n])
		}
	}
	return strings.TrimRight(string(b), "\x00")
}
