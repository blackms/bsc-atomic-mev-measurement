// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// backrun_any_test.go is the unit proof for the ROUND-1 backrun-any REDESIGN
// (simengine/dryrun_backrun_any.go). The original detector valued a backrun as a
// numeraire ROUND-TRIP on the SAME pool the victim touched
// (numeraire->other->numeraire). Under constant-product fee dynamics (gamma < 1)
// that is ALWAYS a structural loss — a round trip at the post-victim reserves eats
// two legs of fees with NO arbitrage capture — so the detector fired zero times.
//
// The redesign values the backrun as a CROSS-POOL negative cycle, reusing the
// graph detector the rest of strategy/ uses: NegativeCycles -> CycleOptimum ->
// BackrunNet, starting at the victim's INPUT token. This test pins both halves of
// the fix on ONE post-victim graph:
//
//	(a) a genuine 3-pool cross-pool gap yields a POSITIVE CycleOptimum gross and a
//	    POSITIVE BackrunNet (the redesign captures real value), AND
//	(b) the OLD single-pool numeraire round-trip on EACH of those same pools is
//	    <= 0 (proving the structural zero the round-1 design always produced).
package strategy

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestBackrunAnyCrossPoolCycleBeatsSinglePoolRoundTrip pins the redesign:
// cross-pool cycle positive, single-pool round-trip non-positive.
func TestBackrunAnyCrossPoolCycleBeatsSinglePoolRoundTrip(t *testing.T) {
	A := tokenAddr(0xA) // the victim's INPUT token (cycle start), NOT necessarily a numeraire
	B := tokenAddr(0xB)
	C := tokenAddr(0xC)

	g := NewGraph()
	gamma := Gamma{Num: big.NewInt(999), Den: big.NewInt(1000)} // 0.1% fee per hop

	// POST-VICTIM state with a genuine cross-pool gap: each leg's marginal rate ~1.02
	// so the loop product ~1.061 clears 3 hops of fee -> a real A->B->C->A backrun.
	rA1, rB1 := scale18(1_000_000), scale18(1_020_000) // pool1 A/B
	rB2, rC2 := scale18(1_000_000), scale18(1_020_000) // pool2 B/C
	rC3, rA3 := scale18(1_000_000), scale18(1_020_000) // pool3 C/A
	addV2(g, 1, A, B, rA1, rB1, gamma)
	addV2(g, 2, B, C, rB2, rC2, gamma)
	addV2(g, 3, C, A, rC3, rA3, gamma)

	// (a) The CROSS-POOL cycle (the redesign) starting at the victim's input token A.
	cycles := g.NegativeCycles(A, 4)
	if len(cycles) == 0 {
		t.Fatalf("redesign: expected a cross-pool negative cycle from victim input token A, got none")
	}
	var cyc *Cycle
	for i := range cycles {
		tk := cycles[i].Tokens()
		if len(tk) == 4 && tk[0] == A && tk[1] == B && tk[2] == C && tk[3] == A {
			cyc = &cycles[i]
			break
		}
	}
	if cyc == nil {
		t.Fatalf("expected the A->B->C->A cross-pool cycle among %d cycles", len(cycles))
	}
	optIn, gross := CycleOptimum(*cyc)
	if optIn.Sign() <= 0 || gross.Sign() <= 0 {
		t.Fatalf("cross-pool backrun must yield positive sizing: optIn=%s gross=%s", optIn, gross)
	}
	// The gas-only BackrunNet gate (no flash, no frontrun) on the cross-pool gross.
	gasPrice := big.NewInt(1_000_000_000) // 1 gwei
	eval := BackrunNet(gross, gasPrice, CycleGasUnits(*cyc), big.NewInt(0))
	if !eval.Profitable || eval.NetProfit.Sign() <= 0 {
		t.Fatalf("cross-pool BackrunNet must be net-positive: net=%s gross=%s", eval.NetProfit, eval.GrossProfit)
	}

	// (b) The OLD single-pool numeraire ROUND-TRIP on EACH touched pool: in->out->in
	// on the SAME pool's reserves. Under gamma<1 this is always a structural loss; the
	// sizer returns (0,0). If ANY of these were positive the round-1 design would have
	// captured something — it never did. This is the structural zero the fix removed.
	roundTrip := func(id byte, x, y common.Address, rIn, rOut *big.Int) Cycle {
		return Cycle{
			Start: x,
			Edges: []Edge{
				{Pool: poolAddr(id), Gamma: gamma, TokenIn: x, TokenOut: y, ReserveIn: rIn, ReserveOut: rOut},
				{Pool: poolAddr(id), Gamma: gamma, TokenIn: y, TokenOut: x, ReserveIn: rOut, ReserveOut: rIn},
			},
		}
	}
	for _, rc := range []struct {
		name string
		c    Cycle
	}{
		{"pool1 A->B->A", roundTrip(1, A, B, rA1, rB1)},
		{"pool2 B->C->B", roundTrip(2, B, C, rB2, rC2)},
		{"pool3 C->A->C", roundTrip(3, C, A, rC3, rA3)},
	} {
		rtIn, rtGross := CycleOptimum(rc.c)
		if rtGross.Sign() > 0 {
			t.Fatalf("OLD single-pool round-trip %s must be <= 0 (structural loss); got optIn=%s gross=%s",
				rc.name, rtIn, rtGross)
		}
	}
}

// buildArbTriangle returns a 3-pool graph (A/B, B/C, C/A) whose marginal rates are
// `ratePerHopNum/ratePerHopDen` per hop, so the A->B->C->A loop clears 3 hops of fee
// when the rate is comfortably above 1. gamma is a tiny 0.1% fee.
func buildArbTriangle(ratePerMille int64) *Graph {
	A, B, C := tokenAddr(0xA), tokenAddr(0xB), tokenAddr(0xC)
	g := NewGraph()
	gamma := Gamma{Num: big.NewInt(999), Den: big.NewInt(1000)}
	base := int64(1_000_000)
	out := base * ratePerMille / 1000
	addV2(g, 1, A, B, scale18(base), scale18(out), gamma)
	addV2(g, 2, B, C, scale18(base), scale18(out), gamma)
	addV2(g, 3, C, A, scale18(base), scale18(out), gamma)
	return g
}

// buildBalancedTriangle returns the same 3-pool topology with rate 1.0 each hop, so
// the loop product is 1 and fees make every cycle sub-break-even (no negative cycle).
func buildBalancedTriangle() *Graph {
	A, B, C := tokenAddr(0xA), tokenAddr(0xB), tokenAddr(0xC)
	g := NewGraph()
	gamma := Gamma{Num: big.NewInt(999), Den: big.NewInt(1000)}
	addV2(g, 1, A, B, scale18(1_000_000), scale18(1_000_000), gamma)
	addV2(g, 2, B, C, scale18(1_000_000), scale18(1_000_000), gamma)
	addV2(g, 3, C, A, scale18(1_000_000), scale18(1_000_000), gamma)
	return g
}

// TestMarginalBackrunStandingCycleIsZero pins the ROUND-2 attribution bug: a
// PROFITABLE cross-pool cycle that exists IDENTICALLY in the victim's PRE- and
// POST-state is a STANDING arbitrage NOT created by the victim, so its marginal
// contribution must be 0 — even though its absolute (post) value is large. The
// round-2 detector reported the full absolute post value (over-counting the
// standing gap and crediting it to every victim that re-ran the graph); the
// MARGINAL rule (post - pre, floored at 0) reports 0.
func TestMarginalBackrunStandingCycleIsZero(t *testing.T) {
	A := tokenAddr(0xA)

	// IDENTICAL pre and post: the same +2%/hop standing triangle in both states.
	pre := buildArbTriangle(1020)
	post := buildArbTriangle(1020)

	// Sanity: the cycle is genuinely profitable on each state in isolation (so a
	// marginal of 0 is meaningful, not trivially zero because nothing was there).
	absPost := BestCycleGrossV2(post, A, 4)
	if absPost.Sign() <= 0 {
		t.Fatalf("precondition: standing cycle must be profitable in the post state, got gross=%s", absPost)
	}
	absPre := BestCycleGrossV2(pre, A, 4)
	if absPre.Cmp(absPost) != 0 {
		t.Fatalf("precondition: identical pre/post must have identical best gross, pre=%s post=%s", absPre, absPost)
	}

	marginal := MarginalBackrunGrossV2(pre, post, A, 4)
	if marginal.Sign() != 0 {
		t.Fatalf("STANDING cycle marginal must be 0 (round-2 over-count would report %s); got %s", absPost, marginal)
	}
}

// TestMarginalBackrunVictimCreatedGapIsPositive pins the complementary half: when
// the victim CREATES the gap (no profitable cycle in pre, profitable in post) the
// marginal equals the full post value (> 0). A second sub-case WIDENS an existing
// gap (smaller-but-profitable pre, larger post) and asserts 0 < marginal < absPost.
func TestMarginalBackrunVictimCreatedGapIsPositive(t *testing.T) {
	A := tokenAddr(0xA)

	// Created-from-nothing: balanced (no-arb) pre, +2%/hop profitable post.
	preFlat := buildBalancedTriangle()
	post := buildArbTriangle(1020)

	if BestCycleGrossV2(preFlat, A, 4).Sign() != 0 {
		t.Fatalf("precondition: balanced pre must have no profitable cycle")
	}
	absPost := BestCycleGrossV2(post, A, 4)
	if absPost.Sign() <= 0 {
		t.Fatalf("precondition: post must be profitable, got %s", absPost)
	}
	created := MarginalBackrunGrossV2(preFlat, post, A, 4)
	if created.Sign() <= 0 {
		t.Fatalf("victim-CREATED gap marginal must be > 0, got %s", created)
	}
	if created.Cmp(absPost) != 0 {
		t.Fatalf("created-from-nothing marginal must equal full post value; marginal=%s absPost=%s", created, absPost)
	}

	// Widened: a small +0.5%/hop standing gap pre, a larger +2%/hop gap post. The
	// marginal must be strictly between 0 and the absolute post value.
	preSmall := buildArbTriangle(1005)
	absPre := BestCycleGrossV2(preSmall, A, 4)
	widened := MarginalBackrunGrossV2(preSmall, post, A, 4)
	if widened.Sign() <= 0 {
		t.Fatalf("widened gap marginal must be > 0, got %s (pre=%s post=%s)", widened, absPre, absPost)
	}
	if widened.Cmp(absPost) >= 0 {
		t.Fatalf("widened marginal must be < absolute post value (pre-existing baseline stripped); marginal=%s absPost=%s", widened, absPost)
	}
}
