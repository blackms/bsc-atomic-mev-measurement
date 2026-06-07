// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// intrablock_test.go is the unit proof for the v3 PER-SWAP intra-block backrun
// detector (simengine/dryrun_intrablock.go). It demonstrates the core claim of
// the intra-block detector: a cross-DEX price gap that exists in the
// TRANSIENT state right after a victim swap — and is therefore detectable
// intra-block — would be MISSED by post-block evaluation because competing
// arbers re-align the two pools back to parity before the block ends.
//
// The detector's per-state core is exactly BuildGraph -> NegativeCycles ->
// EvaluateCycle (graph reads pool reserves out of state; here we feed the same
// reserves directly into the graph so the test is pure and needs no StateDB).
// We model the SAME token pair (WBNB/USDT) on TWO venues (PancakeSwap V2 vs
// Biswap V2), which is precisely the cross-DEX cycle the live detector watches.
//
//   - TRANSIENT state (just after a big WBNB->USDT victim swap on Pancake):
//     Pancake's WBNB price is depressed relative to Biswap -> a WBNB->...->WBNB
//     negative cycle exists and EvaluateCycle returns a POSITIVE net profit.
//   - RE-ALIGNED post-block state (a competitor already backran, pulling the two
//     pools back to parity): NO negative cycle -> the post-block detector finds
//     NOTHING.
//
// This is the unit-level evidence that intra-block detection catches what
// post-block misses.
package strategy

import (
	"math/big"
	"testing"
)

// buildCrossDexGraph builds a graph with the same WBNB/USDT pair on two V2
// venues from explicit reserves, mirroring exactly what BuildGraph produces from
// chain state (two directed edges per pool). Returns the graph so the test can
// run NegativeCycles + EvaluateCycle on it.
//
// Reserves are passed as (wbnb, usdt) per pool for readability; the pool's
// canonical ordering is token0=USDT < token1=WBNB (the verified on-chain order),
// so we map accordingly.
func buildCrossDexGraph(pancakeWBNB, pancakeUSDT, biswapWBNB, biswapUSDT *big.Int) *Graph {
	g := NewGraph()
	// Pancake V2 WBNB/USDT: token0=USDT, token1=WBNB.
	g.AddV2Pool(poolAddr(0x11), DEXPancakeV2, GammaPancakeV2, USDT, WBNB, pancakeUSDT, pancakeWBNB)
	// Biswap V2 WBNB/USDT: token0=USDT, token1=WBNB.
	g.AddV2Pool(poolAddr(0x22), DEXBiswapV2, GammaBiswapV2, USDT, WBNB, biswapUSDT, biswapWBNB)
	return g
}

// evalBestFromWBNB runs the per-swap detection+valuation that the intra-block
// detector applies to each transient state: enumerate WBNB cycles, value each
// exactly, and return the most profitable Evaluation (and whether any cycle was
// detected at Stage A at all). params carries the gas/bid/margin cost model.
func evalBestFromWBNB(g *Graph, params EvalParams) (best Evaluation, detected bool) {
	cycles := g.NegativeCycles(WBNB, 4)
	if len(cycles) == 0 {
		return Evaluation{NetProfit: big.NewInt(0), GrossProfit: big.NewInt(0)}, false
	}
	best = Evaluation{NetProfit: big.NewInt(0), GrossProfit: big.NewInt(0)}
	for _, c := range cycles {
		e := EvaluateCycle(c, params)
		if best.NetProfit == nil || e.NetProfit.Cmp(best.NetProfit) > 0 {
			best = e
		}
	}
	return best, true
}

// TestIntrablockCatchesWhatPostblockMisses is the headline v3 unit proof.
func TestIntrablockCatchesWhatPostblockMisses(t *testing.T) {
	// Modest cost model so a real cross-DEX gap clears it but noise does not:
	// 250k gas at 1 gwei ~= 2.5e14 wei. Gross from the gap below is far larger.
	params := EvalParams{
		GasCost:    new(big.Int).Mul(big.NewInt(250_000), big.NewInt(1_000_000_000)),
		BuilderBid: big.NewInt(0),
		Margin:     big.NewInt(0),
	}

	// --- TRANSIENT state: right after a WBNB->USDT victim swap on Pancake. ---
	// Baseline both pools at ~600 USDT per WBNB (3000 WBNB : 1_800_000 USDT). The
	// victim sells WBNB into Pancake, so Pancake now holds MORE WBNB and LESS USDT
	// (WBNB cheaper on Pancake) while Biswap is untouched: a ~2% cross-DEX gap.
	transient := buildCrossDexGraph(
		// Pancake: WBNB up, USDT down (victim sold WBNB here).
		scale18(3_150), scale18(1_715_000),
		// Biswap: untouched baseline.
		scale18(3_000), scale18(1_800_000),
	)

	transientEval, transientDetected := evalBestFromWBNB(transient, params)
	if !transientDetected {
		t.Fatalf("transient state: expected a negative cycle (cross-DEX gap) but Stage A found none")
	}
	if !transientEval.Profitable {
		t.Fatalf("transient state: expected a NET-POSITIVE backrun, got net=%s gross=%s",
			transientEval.NetProfit, transientEval.GrossProfit)
	}
	if transientEval.OptimalAmountIn.Sign() <= 0 {
		t.Fatalf("transient state: expected positive optimal input, got %s", transientEval.OptimalAmountIn)
	}
	t.Logf("intra-block (transient) backrun: optInWei=%s grossWei=%s netWei=%s",
		transientEval.OptimalAmountIn, transientEval.GrossProfit, transientEval.NetProfit)

	// --- RE-ALIGNED post-block state: a competitor already arbed the gap away. ---
	// Both venues priced identically (parity) -> NO arbitrage remains. This is the
	// state the POST-BLOCK detector (graph mode) would see and find nothing in.
	postblock := buildCrossDexGraph(
		scale18(3_000), scale18(1_800_000),
		scale18(3_000), scale18(1_800_000),
	)
	postEval, postDetected := evalBestFromWBNB(postblock, params)
	if postDetected && postEval.Profitable {
		t.Fatalf("post-block (re-aligned) state: expected NO opportunity, but got net=%s gross=%s",
			postEval.NetProfit, postEval.GrossProfit)
	}
	t.Logf("post-block (re-aligned): detected=%v profitable=%v (correctly nothing to capture)",
		postDetected, postEval.Profitable)
}

// TestIntrablockAlignedReservesYieldNothing pins the negative control: when the
// two venues already agree (no >fee gap), the per-swap detector returns nothing,
// regardless of cost model. (Distinct, focused assertion alongside the headline
// test so a regression points straight at the no-arb path.)
func TestIntrablockAlignedReservesYieldNothing(t *testing.T) {
	params := EvalParams{GasCost: big.NewInt(0), BuilderBid: big.NewInt(0), Margin: big.NewInt(0)}

	g := buildCrossDexGraph(
		scale18(3_000), scale18(1_800_000),
		scale18(3_000), scale18(1_800_000),
	)
	_, detected := evalBestFromWBNB(g, params)
	if detected {
		t.Fatalf("aligned reserves must produce no negative cycle even at zero cost")
	}
}

// TestIntrablockGapBelowFeeRejected checks that a cross-DEX price difference
// SMALLER than the two pools' combined fee is correctly rejected — the gap must
// exceed fees to be a real backrun, the v2->v3 distinction is about WHEN the gap
// exists, not about lowering the profitability bar.
func TestIntrablockGapBelowFeeRejected(t *testing.T) {
	params := EvalParams{GasCost: big.NewInt(0), BuilderBid: big.NewInt(0), Margin: big.NewInt(0)}

	// Pancake fee 0.25% + Biswap fee 0.20% = ~0.45% round-trip. A ~0.1% gap is
	// well below that, so no profitable cycle exists even with zero gas.
	g := buildCrossDexGraph(
		scale18(3_003), scale18(1_798_200), // ~+0.1% WBNB on Pancake
		scale18(3_000), scale18(1_800_000),
	)
	best, detected := evalBestFromWBNB(g, params)
	if detected && best.Profitable {
		t.Fatalf("sub-fee gap must not be profitable, got net=%s", best.NetProfit)
	}
}
