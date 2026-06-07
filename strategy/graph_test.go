// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Unit tests for the Stage-A negative-cycle detector and the exact K-hop cycle
// sizer. They pin: (1) a hand-built triangular graph with a known profitable
// cycle is detected with the correct token path and a POSITIVE optimal input /
// gross profit verified to be a local maximum; (2) a balanced no-arb graph
// yields nothing; (3) the K-hop sizer reproduces the 2-pool closed form exactly
// (CycleOptimum == OptimalArb on the k=2 case); (4) fee handling and V3 deferral.
package strategy

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// tokenAddr builds a deterministic distinct address for a single-byte id.
func tokenAddr(id byte) common.Address {
	var a common.Address
	a[19] = id
	return a
}

// poolAddr builds a deterministic distinct pool address for a single-byte id.
func poolAddr(id byte) common.Address {
	var a common.Address
	a[0] = id
	a[19] = id
	return a
}

// addV2 is a test helper to add both directions of a V2 pool to a graph.
func addV2(g *Graph, id byte, t0, t1 common.Address, r0, r1 *big.Int, gamma Gamma) {
	g.AddV2Pool(poolAddr(id), "test", gamma, t0, t1, r0, r1)
}

// TestNegativeCycleTriangularDetected builds a 3-token graph (A,B,C) with a
// deliberately profitable triangular loop A->B->C->A and asserts the detector
// finds it with the right path and a positive exact sizing.
//
// We construct the loop so the product of zero-fee marginal rates around it
// exceeds 1 even after fees. Using a tiny fee (gamma=999/1000) and a strong ~6%
// price loop guarantees a candidate.
func TestNegativeCycleTriangularDetected(t *testing.T) {
	A := tokenAddr(0xA)
	B := tokenAddr(0xB)
	C := tokenAddr(0xC)

	g := NewGraph()
	gamma := Gamma{Num: big.NewInt(999), Den: big.NewInt(1000)} // 0.1% fee

	// Pool 1: A/B. Marginal A->B rate = rB/rA. Make A->B rate = 1.02.
	//   rA = 1_000_000e18, rB = 1_020_000e18.
	addV2(g, 1, A, B, scale18(1_000_000), scale18(1_020_000), gamma)
	// Pool 2: B/C. Make B->C rate = 1.02: rB = 1_000_000e18, rC = 1_020_000e18.
	addV2(g, 2, B, C, scale18(1_000_000), scale18(1_020_000), gamma)
	// Pool 3: C/A. Make C->A rate = 1.02: rC = 1_000_000e18, rA = 1_020_000e18.
	addV2(g, 3, C, A, scale18(1_000_000), scale18(1_020_000), gamma)
	// Loop product of rates ~ 1.02^3 = 1.061, fee^3 ~ 0.997 => clearly > 1.

	cycles := g.NegativeCycles(A, 4)
	if len(cycles) == 0 {
		t.Fatalf("expected at least one negative cycle from A, got none")
	}

	// Find the A->B->C->A cycle.
	var found *Cycle
	for i := range cycles {
		toks := cycles[i].Tokens()
		if len(toks) == 4 && toks[0] == A && toks[1] == B && toks[2] == C && toks[3] == A {
			found = &cycles[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("did not find the A->B->C->A cycle among %d cycles", len(cycles))
	}
	if found.LogGain <= 0 {
		t.Fatalf("cycle LogGain must be positive, got %v", found.LogGain)
	}

	// Exact big.Int sizing must be positive and a local maximum.
	optIn, gross := CycleOptimum(*found)
	if optIn.Sign() <= 0 {
		t.Fatalf("expected positive optimal input, got %s", optIn)
	}
	if gross.Sign() <= 0 {
		t.Fatalf("expected positive gross profit, got %s", gross)
	}

	// Verify it is a maximum: profit at optIn beats optIn +/- a step.
	profitAt := func(x *big.Int) *big.Int {
		if x.Sign() <= 0 {
			return big.NewInt(-1)
		}
		cur := new(big.Int).Set(x)
		for _, e := range found.Edges {
			cur = GetAmountOut(cur, e.ReserveIn, e.ReserveOut, e.Gamma)
		}
		return new(big.Int).Sub(cur, x)
	}
	step := scale18(100)
	lo := profitAt(new(big.Int).Sub(optIn, step))
	hi := profitAt(new(big.Int).Add(optIn, step))
	if gross.Cmp(lo) < 0 || gross.Cmp(hi) < 0 {
		t.Fatalf("optimum not maximal: gross=%s lo=%s hi=%s", gross, lo, hi)
	}
}

// TestNoArbGraphYieldsNothing builds a balanced graph where every loop's gain is
// below the cumulative fee, so no negative cycle exists.
func TestNoArbGraphYieldsNothing(t *testing.T) {
	A := tokenAddr(0xA)
	B := tokenAddr(0xB)
	C := tokenAddr(0xC)

	g := NewGraph()
	gamma := GammaPancakeV2 // 0.25% fee

	// All pools perfectly balanced (rate 1.0 each direction): loop product = 1,
	// times fee^3 < 1 => no arb.
	addV2(g, 1, A, B, scale18(1_000_000), scale18(1_000_000), gamma)
	addV2(g, 2, B, C, scale18(1_000_000), scale18(1_000_000), gamma)
	addV2(g, 3, C, A, scale18(1_000_000), scale18(1_000_000), gamma)

	cycles := g.NegativeCycles(A, 4)
	if len(cycles) != 0 {
		t.Fatalf("expected no negative cycles in a balanced graph, got %d: %+v", len(cycles), cycles)
	}
}

// TestSubFeeLoopRejected checks a loop whose price gain is positive but smaller
// than the cumulative fee is correctly rejected (fee handling).
func TestSubFeeLoopRejected(t *testing.T) {
	A := tokenAddr(0xA)
	B := tokenAddr(0xB)
	C := tokenAddr(0xC)

	g := NewGraph()
	gamma := GammaPancakeV2 // 0.25% per hop => ~0.75% over 3 hops

	// Each hop +0.1% rate: loop product ~1.003, but fee^3 ~ 0.9925 => net < 1.
	addV2(g, 1, A, B, scale18(1_000_000), scale18(1_001_000), gamma)
	addV2(g, 2, B, C, scale18(1_000_000), scale18(1_001_000), gamma)
	addV2(g, 3, C, A, scale18(1_000_000), scale18(1_001_000), gamma)

	cycles := g.NegativeCycles(A, 4)
	if len(cycles) != 0 {
		t.Fatalf("expected sub-fee loop to be rejected, got %d cycles", len(cycles))
	}
}

// TestCycleOptimumMatches2PoolClosedForm asserts the K-hop sizer reproduces the
// existing 2-pool OptimalArb exactly on the k=2 case (the generalisation is
// consistent with the validated base case). Cycle: X -> T (pool A) -> X (pool B).
func TestCycleOptimumMatches2PoolClosedForm(t *testing.T) {
	X := tokenAddr(0x1)
	T := tokenAddr(0x2)

	// Pool A (X->T): reserves (X, T) = (1_000_000e18, 1_050_000e18).
	// Pool B (T->X): reserves (T, X) = (1_000_000e18, 1_050_000e18).
	raIn, raOut := scale18(1_000_000), scale18(1_050_000)
	rbIn, rbOut := scale18(1_000_000), scale18(1_050_000)

	wantIn, wantGross := OptimalArb(raIn, raOut, rbIn, rbOut, GammaPancakeV2)

	// Build the equivalent 2-hop cycle. Hop1: X->T on pool A with (raIn,raOut).
	// Hop2: T->X on pool B with (rbIn,rbOut).
	c := Cycle{
		Start: X,
		Edges: []Edge{
			{Pool: poolAddr(1), Gamma: GammaPancakeV2, TokenIn: X, TokenOut: T, ReserveIn: raIn, ReserveOut: raOut},
			{Pool: poolAddr(2), Gamma: GammaPancakeV2, TokenIn: T, TokenOut: X, ReserveIn: rbIn, ReserveOut: rbOut},
		},
	}
	gotIn, gotGross := CycleOptimum(c)

	// The optimal input may differ by a couple of wei due to the local-search
	// refinement vs the single-floor closed form; the realised gross must be at
	// least as good as the 2-pool closed form (never worse).
	if gotGross.Cmp(wantGross) < 0 {
		t.Fatalf("CycleOptimum gross %s < OptimalArb gross %s", gotGross, wantGross)
	}
	// Inputs should be within a small neighbourhood.
	diff := new(big.Int).Abs(new(big.Int).Sub(gotIn, wantIn))
	if diff.Cmp(big.NewInt(8)) > 0 {
		t.Fatalf("CycleOptimum optIn %s differs from OptimalArb %s by more than 8 wei (%s)", gotIn, wantIn, diff)
	}
}

// TestCycleOptimumNoArb checks a balanced 2-hop cycle yields (0,0).
func TestCycleOptimumNoArb(t *testing.T) {
	X := tokenAddr(0x1)
	T := tokenAddr(0x2)
	r := scale18(1_000_000)
	c := Cycle{
		Start: X,
		Edges: []Edge{
			{Pool: poolAddr(1), Gamma: GammaPancakeV2, TokenIn: X, TokenOut: T, ReserveIn: r, ReserveOut: r},
			{Pool: poolAddr(2), Gamma: GammaPancakeV2, TokenIn: T, TokenOut: X, ReserveIn: r, ReserveOut: r},
		},
	}
	optIn, gross := CycleOptimum(c)
	if optIn.Sign() != 0 || gross.Sign() != 0 {
		t.Fatalf("balanced cycle must yield (0,0), got (%s,%s)", optIn, gross)
	}
}

// TestCycleOptimumDefersV3 asserts a cycle containing a V3 hop is NOT sized
// analytically (returns (0,0)) — the methodological invariant.
func TestCycleOptimumDefersV3(t *testing.T) {
	X := tokenAddr(0x1)
	T := tokenAddr(0x2)
	r := scale18(1_000_000)
	rBig := scale18(1_100_000)
	c := Cycle{
		Start: X,
		Edges: []Edge{
			{Pool: poolAddr(1), Gamma: GammaPancakeV2, TokenIn: X, TokenOut: T, ReserveIn: r, ReserveOut: rBig},
			{Pool: poolAddr(2), Gamma: GammaPancakeV2, TokenIn: T, TokenOut: X, ReserveIn: r, ReserveOut: rBig, IsV3: true},
		},
	}
	optIn, gross := CycleOptimum(c)
	if optIn.Sign() != 0 || gross.Sign() != 0 {
		t.Fatalf("V3-containing cycle must defer to EVM oracle (return 0,0), got (%s,%s)", optIn, gross)
	}
}

// TestEvaluateCycleNet checks the net = gross - costs path mirrors Evaluate.
func TestEvaluateCycleNet(t *testing.T) {
	A := tokenAddr(0xA)
	B := tokenAddr(0xB)
	C := tokenAddr(0xC)
	g := NewGraph()
	gamma := Gamma{Num: big.NewInt(999), Den: big.NewInt(1000)}
	addV2(g, 1, A, B, scale18(1_000_000), scale18(1_020_000), gamma)
	addV2(g, 2, B, C, scale18(1_000_000), scale18(1_020_000), gamma)
	addV2(g, 3, C, A, scale18(1_000_000), scale18(1_020_000), gamma)

	cycles := g.NegativeCycles(A, 4)
	if len(cycles) == 0 {
		t.Fatalf("expected a profitable cycle")
	}
	// Pick the best (sorted first).
	c := cycles[0]

	// With zero cost, net == gross and profitable.
	e0 := EvaluateCycle(c, EvalParams{})
	if !e0.Profitable || e0.NetProfit.Cmp(e0.GrossProfit) != 0 {
		t.Fatalf("zero-cost net must equal gross and be profitable, got net=%s gross=%s", e0.NetProfit, e0.GrossProfit)
	}

	// With cost exceeding gross, not profitable and net negative.
	huge := new(big.Int).Add(e0.GrossProfit, scale18(1))
	e1 := EvaluateCycle(c, EvalParams{GasCost: huge})
	if e1.Profitable || e1.NetProfit.Sign() >= 0 {
		t.Fatalf("cost above gross must be unprofitable with negative net, got %s", e1.NetProfit)
	}
}

// TestV3SpotPriceAndSlot0Decode checks the V3 slot0 decode + spot price helpers
// on a known sqrtPriceX96 (price exactly 4 => sqrtPriceX96 = 2 * 2^96).
func TestV3SpotPriceAndSlot0Decode(t *testing.T) {
	// sqrtPriceX96 = 2 * 2^96 => P = (2)^2 = 4.
	sqrtP := new(big.Int).Mul(big.NewInt(2), two96)
	p := V3SpotPrice(sqrtP)
	if p.Cmp(big.NewRat(4, 1)) != 0 {
		t.Fatalf("V3SpotPrice = %s, want 4", p.RatString())
	}

	// toInt24 sign extension: 0x800000 => -8388608; 0x000001 => 1.
	if got := toInt24(big.NewInt(0x800000)); got != -8388608 {
		t.Fatalf("toInt24(0x800000) = %d, want -8388608", got)
	}
	if got := toInt24(big.NewInt(1)); got != 1 {
		t.Fatalf("toInt24(1) = %d, want 1", got)
	}
}

// TestGammaForFeeTier checks V3 fee-tier -> gamma conversion.
func TestGammaForFeeTier(t *testing.T) {
	// 0.25% fee tier = 2500 hundredths-of-bip => gamma = 997500/1000000.
	g := gammaForFeeTier(2500)
	if g.Num.Cmp(big.NewInt(997_500)) != 0 || g.Den.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("gammaForFeeTier(2500) = %s/%s, want 997500/1000000", g.Num, g.Den)
	}
}
