// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// quoter_test.go unit-proves the v4 quoter-chaining valuator (quoter.go +
// quoter_oracle.go) WITHOUT a live node: the real PancakeSwap V3 QuoterV2 needs a
// deployed-contract state, so the live V3 path is exercised at runtime; here we
// pin the search/chaining math with mock quote functions and cross-check the
// all-V2 path against the exact closed form CycleOptimum.
//
// Coverage:
//   - (a) V2-only cycle: OptimalInput (golden-section) reproduces CycleOptimum
//     exactly (cross-check), and the optimum is a true local maximum.
//   - (b) the quoter chain composes correctly on a mocked V3 quote function (a
//     synthetic constant-product pool quoted via the encoded calldata round-trip).
//   - (c) golden-section finds the maximum of a synthetic concave profit curve.
//   - ABI encode/decode round-trips for quoteExactInputSingle.
package strategy

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// ---------------------------------------------------------------------------
// (a) V2-only cycle: search == closed form.
// ---------------------------------------------------------------------------

// buildV2Triangle builds a profitable triangular all-V2 cycle WBNB->USDT->USDC->
// WBNB across three pools with reserves chosen to leave an arb. Returns the cycle
// as NegativeCycles would (edges in start-token order from WBNB).
func buildV2TriangleCycle() Cycle {
	g := NewGraph()
	// Skew the prices so the loop product > 1 (a real arb). Reserves in wei-ish.
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	mul := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), e18) }
	// WBNB/USDT pool (token0=USDT, token1=WBNB): cheap WBNB.
	g.AddV2Pool(poolAddr(0x01), DEXPancakeV2, GammaPancakeV2, USDT, WBNB, mul(700_000), mul(1_000))
	// USDT/USDC pool (token0=USDT, token1=USDC): ~parity.
	g.AddV2Pool(poolAddr(0x02), DEXPancakeV2, GammaPancakeV2, USDT, USDC, mul(1_000_000), mul(1_000_000))
	// WBNB/USDC pool (token0=USDC, token1=WBNB): expensive WBNB (sell high).
	g.AddV2Pool(poolAddr(0x03), DEXPancakeV2, GammaPancakeV2, USDC, WBNB, mul(620_000), mul(1_000))

	cycles := g.NegativeCycles(WBNB, 4)
	for _, c := range cycles {
		if len(c.Edges) == 3 {
			return c
		}
	}
	if len(cycles) > 0 {
		return cycles[0]
	}
	return Cycle{}
}

func TestOptimalInputV2MatchesClosedForm(t *testing.T) {
	c := buildV2TriangleCycle()
	if len(c.Edges) < 2 {
		t.Fatalf("expected a profitable V2 cycle, got %d edges", len(c.Edges))
	}

	cfIn, cfGross := CycleOptimum(c)
	if cfGross.Sign() <= 0 {
		t.Fatalf("closed form found no profit; test fixture not profitable")
	}

	// nil quote: all-V2 cycle must not need the V3 callback.
	in, gross := OptimalInput(nil, c, nil)
	if gross.Sign() <= 0 {
		t.Fatalf("search found no profit on a profitable V2 cycle")
	}

	// The search must not regress below the exact closed-form gross.
	if gross.Cmp(cfGross) < 0 {
		t.Fatalf("search gross %s < closed-form gross %s", gross, cfGross)
	}

	// And the closed form must be a genuine optimum: the search's gross should not
	// EXCEED it by more than the flooring slop (a handful of wei from rounding at a
	// different x). We assert they are within a tiny relative band.
	diff := new(big.Int).Abs(new(big.Int).Sub(gross, cfGross))
	// allow up to 0.01% of gross slack for the integer-grid search vs closed form.
	slack := new(big.Int).Div(cfGross, big.NewInt(10_000))
	if slack.Sign() == 0 {
		slack = big.NewInt(100)
	}
	if diff.Cmp(slack) > 0 {
		t.Fatalf("search gross %s and closed-form gross %s differ by %s (> slack %s); in=%s cfIn=%s",
			gross, cfGross, diff, slack, in, cfIn)
	}

	// Cross-check that CycleGross at the closed-form input reproduces cfGross
	// exactly through the chaining code (V2 path of QuoteHop == GetAmountOut).
	gAtCf, ok := CycleGross(nil, c, cfIn)
	if !ok {
		t.Fatalf("CycleGross infeasible at closed-form input")
	}
	if gAtCf.Cmp(cfGross) != 0 {
		t.Fatalf("CycleGross(closed-form in)=%s != CycleOptimum gross=%s", gAtCf, cfGross)
	}
}

// ---------------------------------------------------------------------------
// (b) quoter chain composes correctly on a mocked V3 quote function.
// ---------------------------------------------------------------------------

// mockV3Pool models a single constant-product pool (rIn,rOut) per directed token
// pair, and answers quoteExactInputSingle calldata exactly like the real QuoterV2
// would for that math: it decodes (tokenIn,tokenOut,amountIn,fee) from the inline
// tuple, applies GetAmountOut, and ABI-encodes the 4-word return. This proves the
// encode/decode + chaining wiring without a node.
type mockV3 struct {
	// reserves keyed by (tokenIn||tokenOut) -> (rIn,rOut)
	res   map[string][2]*big.Int
	gamma Gamma
	calls int
}

func (m *mockV3) key(in, out common.Address) string { return string(in.Bytes()) + string(out.Bytes()) }

func (m *mockV3) quote(to common.Address, input []byte) ([]byte, error) {
	m.calls++
	if to != QuoterV2Address {
		return nil, errMockBadTarget
	}
	// Decode the inline tuple: selector(4) + tokenIn(32)+tokenOut(32)+amountIn(32)+fee(32)+limit(32).
	if len(input) != 4+32*5 {
		return nil, errMockBadCalldata
	}
	tokenIn := common.BytesToAddress(input[4+12 : 4+32])
	tokenOut := common.BytesToAddress(input[4+32+12 : 4+64])
	amountIn := new(big.Int).SetBytes(input[4+64 : 4+96])
	rr, ok := m.res[m.key(tokenIn, tokenOut)]
	if !ok {
		return nil, errMockNoPool // empty-data revert equivalent
	}
	out := GetAmountOut(amountIn, rr[0], rr[1], m.gamma)
	if out.Sign() <= 0 {
		return nil, errMockUnfillable
	}
	// Encode 4 words: amountOut | sqrtAfter(0) | ticks(0) | gasEstimate(92161).
	ret := make([]byte, 128)
	copy(ret[0:32], leftPad32(out.Bytes()))
	copy(ret[96:128], leftPad32(big.NewInt(92161).Bytes()))
	return ret, nil
}

var (
	errMockBadTarget   = mockErr("bad target")
	errMockBadCalldata = mockErr("bad calldata")
	errMockNoPool      = mockErr("no pool")
	errMockUnfillable  = mockErr("unfillable")
)

type mockErr string

func (e mockErr) Error() string { return string(e) }

func TestQuoterChainComposesOnMock(t *testing.T) {
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	mul := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), e18) }

	// Two V3 pools forming a WBNB->USDT->WBNB loop with an arb (buy low, sell high).
	m := &mockV3{res: map[string][2]*big.Int{}, gamma: gammaForFeeTier(100)}
	// Pool A: WBNB->USDT cheap (lots of USDT out per WBNB).
	m.res[m.key(WBNB, USDT)] = [2]*big.Int{mul(1_000), mul(700_000)}
	// Pool B: USDT->WBNB (other venue) returns more WBNB than A took in.
	m.res[m.key(USDT, WBNB)] = [2]*big.Int{mul(620_000), mul(1_000)}

	// Build a 2-hop V3 cycle by hand.
	c := Cycle{
		Start: WBNB,
		Edges: []Edge{
			{Pool: poolAddr(0xA1), DEX: DEXPancakeV3, IsV3: true, FeeTier: 100, Gamma: m.gamma, TokenIn: WBNB, TokenOut: USDT},
			{Pool: poolAddr(0xB2), DEX: DEXPancakeV3, IsV3: true, FeeTier: 100, Gamma: m.gamma, TokenIn: USDT, TokenOut: WBNB},
		},
	}

	// Single-hop check: QuoteHop(V3) == GetAmountOut on the mock's reserves.
	in := mul(1)
	got, ok := QuoteHop(m.quote, c.Edges[0], in)
	if !ok {
		t.Fatalf("V3 QuoteHop failed")
	}
	want := GetAmountOut(in, mul(1_000), mul(700_000), m.gamma)
	if got.Cmp(want) != 0 {
		t.Fatalf("V3 QuoteHop %s != expected %s", got, want)
	}

	// Full-chain check: CycleFinal == manual two-hop composition.
	final, ok := CycleFinal(m.quote, c, in)
	if !ok {
		t.Fatalf("CycleFinal failed")
	}
	mid := GetAmountOut(in, mul(1_000), mul(700_000), m.gamma)
	wantFinal := GetAmountOut(mid, mul(620_000), mul(1_000), m.gamma)
	if final.Cmp(wantFinal) != 0 {
		t.Fatalf("CycleFinal %s != manual %s", final, wantFinal)
	}

	// OptimalInput must find a profitable size on this V3 cycle.
	optIn, optGross := OptimalInput(m.quote, c, nil)
	if optGross.Sign() <= 0 {
		t.Fatalf("OptimalInput found no profit on a profitable V3 mock cycle")
	}
	if optIn.Sign() <= 0 {
		t.Fatalf("OptimalInput returned non-positive input %s", optIn)
	}
	// Sanity: gross at the found input matches CycleGross.
	g2, ok := CycleGross(m.quote, c, optIn)
	if !ok || g2.Cmp(optGross) != 0 {
		t.Fatalf("CycleGross(optIn)=%s ok=%v != reported optGross=%s", g2, ok, optGross)
	}
	if m.calls == 0 {
		t.Fatalf("mock quoter was never called — V3 path not exercised")
	}
}

// ---------------------------------------------------------------------------
// (c) golden-section finds the max of a synthetic concave profit curve.
// ---------------------------------------------------------------------------

// TestGoldenSectionConcave proves OptimalInput's search locates the peak of a
// known-unimodal profit curve. We synthesise a cycle whose gross profit as a
// function of x has a single interior maximum by using a single constant-product
// "round-trip" pool (a degenerate cycle priced through the mock), where the peak
// input is analytically known to be near sqrt-based optimum. We assert the search
// gets within a small band of an exhaustive coarse scan's best.
func TestGoldenSectionConcave(t *testing.T) {
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	mul := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), e18) }

	m := &mockV3{res: map[string][2]*big.Int{}, gamma: gammaForFeeTier(100)}
	m.res[m.key(WBNB, USDT)] = [2]*big.Int{mul(1_000), mul(700_000)}
	m.res[m.key(USDT, WBNB)] = [2]*big.Int{mul(640_000), mul(1_000)}
	c := Cycle{
		Start: WBNB,
		Edges: []Edge{
			{Pool: poolAddr(0xA1), DEX: DEXPancakeV3, IsV3: true, FeeTier: 100, Gamma: m.gamma, TokenIn: WBNB, TokenOut: USDT},
			{Pool: poolAddr(0xB2), DEX: DEXPancakeV3, IsV3: true, FeeTier: 100, Gamma: m.gamma, TokenIn: USDT, TokenOut: WBNB},
		},
	}

	// Exhaustive coarse scan for the true peak over [1, 200] WBNB in 0.5-WBNB steps.
	half := new(big.Int).Div(e18, big.NewInt(2))
	bestScanGross := big.NewInt(-1)
	bestScanX := big.NewInt(0)
	for k := int64(1); k <= 400; k++ {
		x := new(big.Int).Mul(big.NewInt(k), half)
		if g, ok := CycleGross(m.quote, c, x); ok && g.Cmp(bestScanGross) > 0 {
			bestScanGross = new(big.Int).Set(g)
			bestScanX = x
		}
	}
	if bestScanGross.Sign() <= 0 {
		t.Fatalf("coarse scan found no profit; fixture not concave-positive")
	}

	_, searchGross := OptimalInput(m.quote, c, mul(1_000))
	if searchGross.Sign() <= 0 {
		t.Fatalf("golden-section found no profit")
	}
	// The continuous golden-section peak should be >= the coarse-grid scan's best
	// (finer resolution can only do as well or better), within a small slack for
	// the scan possibly straddling the peak.
	if searchGross.Cmp(bestScanGross) < 0 {
		diff := new(big.Int).Sub(bestScanGross, searchGross)
		slack := new(big.Int).Div(bestScanGross, big.NewInt(1_000)) // 0.1%
		if diff.Cmp(slack) > 0 {
			t.Fatalf("search gross %s < coarse-scan best %s (at x=%s) by %s > slack %s",
				searchGross, bestScanGross, bestScanX, diff, slack)
		}
	}
}

// ---------------------------------------------------------------------------
// ABI encode/decode round-trip.
// ---------------------------------------------------------------------------

func TestEncodeQuoteExactInputSingle(t *testing.T) {
	amountIn := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) // 1e18
	data := EncodeQuoteExactInputSingle(WBNB, USDT, amountIn, 100)
	if len(data) != 4+32*5 {
		t.Fatalf("calldata len %d != %d", len(data), 4+32*5)
	}
	// selector
	if data[0] != 0xc6 || data[1] != 0xa5 || data[2] != 0x02 || data[3] != 0x6a {
		t.Fatalf("wrong selector %x", data[0:4])
	}
	// tokenIn / tokenOut right-aligned
	if common.BytesToAddress(data[4+12:4+32]) != WBNB {
		t.Fatalf("tokenIn mismatch")
	}
	if common.BytesToAddress(data[4+44:4+64]) != USDT {
		t.Fatalf("tokenOut mismatch")
	}
	// amountIn
	if new(big.Int).SetBytes(data[4+64:4+96]).Cmp(amountIn) != 0 {
		t.Fatalf("amountIn mismatch")
	}
	// fee (uint24 right-aligned)
	if new(big.Int).SetBytes(data[4+96:4+128]).Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("fee mismatch")
	}
	// sqrtPriceLimitX96 = 0
	if new(big.Int).SetBytes(data[4+128:4+160]).Sign() != 0 {
		t.Fatalf("sqrtPriceLimit not zero")
	}

	// Verify against the verified-on-node calldata from the research.
	wantHex := "c6a5026a000000000000000000000000bb4cdb9cbd36b01bd1cbaebf2de08d9173bc095c00000000000000000000000055d398326f99059ff775485246999027b31979550000000000000000000000000000000000000000000000000de0b6b3a764000000000000000000000000000000000000000000000000000000000000000000640000000000000000000000000000000000000000000000000000000000000000"
	if common.Bytes2Hex(data) != wantHex {
		t.Fatalf("calldata\n got %s\nwant %s", common.Bytes2Hex(data), wantHex)
	}

	// Decode round-trip of a synthetic return.
	ret := make([]byte, 128)
	copy(ret[0:32], leftPad32(big.NewInt(590847647987115936).Bytes()))
	copy(ret[96:128], leftPad32(big.NewInt(92161).Bytes()))
	out, gasEst, ok := DecodeQuoteExactInputSingle(ret)
	if !ok || out.Int64() != 590847647987115936 || gasEst.Int64() != 92161 {
		t.Fatalf("decode mismatch out=%v gas=%v ok=%v", out, gasEst, ok)
	}
}

func TestCycleGasUnits(t *testing.T) {
	c := Cycle{Edges: []Edge{
		{IsV3: false}, // V2
		{IsV3: true},  // V3
		{IsV3: false}, // V2
	}}
	want := uint64(gasBaseUnits + gasPerV2Hop + gasPerV3Hop + gasPerV2Hop)
	if got := CycleGasUnits(c); got != want {
		t.Fatalf("CycleGasUnits = %d, want %d", got, want)
	}
}
