// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// quoter.go is the v4 GROUND-TRUTH cycle valuator: it computes the exact gross
// profit of ANY candidate cycle — including cycles that contain PancakeSwap V3
// hops, which CycleOptimum cannot size in closed form — by QUOTER-CHAINING on the
// intermediate state.
//
// METHOD (paper/40-models.md Stage B, v4):
//   - V2 hop: exact closed-form GetAmountOut from the pool reserves (the same
//     receipt-exact arithmetic the SimEngine self-test validates for V2).
//   - V3 hop: ABI-encode PancakeSwap V3 QuoterV2.quoteExactInputSingle and call it
//     in-process against the SAME intermediate state via a caller-supplied QuoteFn
//     (wired to SimEngine.EthCall). The quoter runs the real tick math and returns
//     the exact amountOut. Because each pool appears at most once in a simple
//     cycle, chaining independent single-hop quotes equals atomic execution.
//   - The hops are chained around the cycle to a final amount; gross = final -
//     amountIn, in the cycle's start token.
//   - The profit-maximising amountIn is found by a golden-section search (profit is
//     unimodal in size for an AMM cycle). For an all-V2 cycle the closed form
//     CycleOptimum is used directly and the search is cross-checked against it.
//
// This file is PURE with respect to chain plumbing: it never imports core/vm. The
// only chain interaction is funnelled through the QuoteFn callback, so strategy
// stays decoupled from simengine and the search/chaining logic is unit-testable
// with a mock quote function (see quoter_test.go).
package strategy

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// QuoterV2Address is the deployed PancakeSwap V3 QuoterV2 on BSC. VERIFIED on the
// live node: factory()=0x0BFbCF9f...091865 (canonical Pancake V3 Factory),
// WETH9()=WBNB, deployer()=0x41ff9AA7...071c9, and
// factory.getPool(USDT,WBNB,100)=0x172fcd41... matches the verified fee-100 pool
// in pools_ext.go. It RETURNS NORMALLY (unlike the legacy V1 quoter): a plain
// eth_call returns the decodable 4-tuple, so we decode ret directly and never
// parse revert data.
var QuoterV2Address = common.HexToAddress("0xB048Bbc1Ee6b733FFfCFb9e9CeF7375518e25997")

// quoteExactInputSingleSelector = keccak256("quoteExactInputSingle((address,address,uint256,uint24,uint160))")[:4].
var quoteExactInputSingleSelector = []byte{0xc6, 0xa5, 0x02, 0x6a}

// QuoteFn is the read-only EVM-call callback the valuator uses for V3 hops. It
// must execute `input` against contract `to` on the intermediate state and return
// the raw ABI-encoded return data (or a non-nil error on revert/failure). In the
// live pipeline this is bound to SimEngine.EthCall; in tests it is mocked. The
// valuator never inspects the state itself for V3 — all V3 pricing flows through
// this callback so strategy stays decoupled from core/vm.
type QuoteFn func(to common.Address, input []byte) ([]byte, error)

// EncodeQuoteExactInputSingle builds the calldata for
// QuoterV2.quoteExactInputSingle((tokenIn,tokenOut,amountIn,fee,sqrtPriceLimitX96)).
//
// The single argument is a tuple of FIVE STATIC fields, so it is ABI-encoded
// INLINE right after the selector with NO head/tail offset and NO dynamic section:
//
//	selector(4) ++ tokenIn(32) ++ tokenOut(32) ++ amountIn(32) ++ fee(32) ++ sqrtPriceLimitX96(32)
//
// sqrtPriceLimitX96 is passed as 0, the "no price limit" sentinel (the quoter
// substitutes MIN/MAX_SQRT_RATIO+/-1 for the direction internally).
func EncodeQuoteExactInputSingle(tokenIn, tokenOut common.Address, amountIn *big.Int, feeTier uint32) []byte {
	data := make([]byte, 0, 4+32*5)
	data = append(data, quoteExactInputSingleSelector...)
	data = append(data, leftPad32(tokenIn.Bytes())...)
	data = append(data, leftPad32(tokenOut.Bytes())...)
	if amountIn == nil {
		amountIn = big.NewInt(0)
	}
	data = append(data, leftPad32(amountIn.Bytes())...)
	data = append(data, leftPad32(new(big.Int).SetUint64(uint64(feeTier)).Bytes())...)
	data = append(data, make([]byte, 32)...) // sqrtPriceLimitX96 = 0 (no limit)
	return data
}

// DecodeQuoteExactInputSingle decodes the QuoterV2 return data. The return is 128
// bytes / 4 words: amountOut | sqrtPriceX96After | initializedTicksCrossed |
// gasEstimate. amountOut is word[0], gasEstimate is word[3]. Returns ok=false if
// the data is too short (e.g. an empty-data revert that slipped through).
func DecodeQuoteExactInputSingle(ret []byte) (amountOut, gasEstimate *big.Int, ok bool) {
	if len(ret) < 32 {
		return nil, nil, false
	}
	amountOut = new(big.Int).SetBytes(ret[0:32])
	gasEstimate = big.NewInt(0)
	if len(ret) >= 128 {
		gasEstimate = new(big.Int).SetBytes(ret[96:128])
	}
	return amountOut, gasEstimate, true
}

// leftPad32 left-pads b to a 32-byte big-endian word.
func leftPad32(b []byte) []byte {
	if len(b) >= 32 {
		return b[len(b)-32:]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// QuoteHop returns the output amount of a single cycle hop (TokenIn -> TokenOut on
// edge.Pool) given amountIn, valuing it on the EXACT intermediate state:
//
//   - V2 edge: closed-form GetAmountOut from the edge's reserves (read into the
//     graph from the same intermediate state by BuildGraph).
//   - V3 edge: ABI-encode QuoterV2.quoteExactInputSingle for the edge's direction
//     and fee tier and invoke quote(QuoterV2Address, calldata). The quoter's
//     amountOut is decoded directly (it returns normally).
//
// ok=false signals the hop is not quotable at this size — a V3 revert (bad pool,
// or amountIn too large to fill against available liquidity, which the quoter
// reports as a revert rather than a clamp), a zero/negative output, or missing
// reserves. The optimal-input search treats !ok as out-of-feasible-range. quote
// may be nil; then V3 hops are unquotable (used by the all-V2 path).
func QuoteHop(quote QuoteFn, e Edge, amountIn *big.Int) (amountOut *big.Int, ok bool) {
	if amountIn == nil || amountIn.Sign() <= 0 {
		return big.NewInt(0), false
	}
	if e.IsV3 {
		if quote == nil {
			return big.NewInt(0), false
		}
		input := EncodeQuoteExactInputSingle(e.TokenIn, e.TokenOut, amountIn, e.FeeTier)
		ret, err := quote(QuoterV2Address, input)
		if err != nil {
			// Revert: bad pool/fee tier (empty data) or amountIn too large
			// ("Unexpected error"); either way this size is infeasible for this edge.
			return big.NewInt(0), false
		}
		out, _, dok := DecodeQuoteExactInputSingle(ret)
		if !dok || out == nil || out.Sign() <= 0 {
			return big.NewInt(0), false
		}
		return out, true
	}
	// V2 hop: exact closed form.
	if e.ReserveIn == nil || e.ReserveOut == nil || e.ReserveIn.Sign() <= 0 || e.ReserveOut.Sign() <= 0 {
		return big.NewInt(0), false
	}
	out := GetAmountOut(amountIn, e.ReserveIn, e.ReserveOut, e.Gamma)
	if out.Sign() <= 0 {
		return big.NewInt(0), false
	}
	return out, true
}

// CycleFinal chains the per-hop quotes around the cycle, starting with amountIn in
// the cycle's start token, and returns the final amount back in the start token.
// ok=false if any hop is unquotable (revert / zero output) at this size.
func CycleFinal(quote QuoteFn, c Cycle, amountIn *big.Int) (final *big.Int, ok bool) {
	if len(c.Edges) < 2 || amountIn == nil || amountIn.Sign() <= 0 {
		return big.NewInt(0), false
	}
	amt := new(big.Int).Set(amountIn)
	for _, e := range c.Edges {
		out, hopOK := QuoteHop(quote, e, amt)
		if !hopOK {
			return big.NewInt(0), false
		}
		amt = out
	}
	return amt, true
}

// CycleGross returns the gross profit (final - amountIn, in start-token units) of
// running the cycle at amountIn. A non-positive or infeasible result yields
// (0,false); a profitable one yields (profit,true). Loss-making but feasible sizes
// return the (negative) profit with ok=true so the search can compare them.
func CycleGross(quote QuoteFn, c Cycle, amountIn *big.Int) (gross *big.Int, ok bool) {
	final, fok := CycleFinal(quote, c, amountIn)
	if !fok {
		return big.NewInt(0), false
	}
	return new(big.Int).Sub(final, amountIn), true
}
