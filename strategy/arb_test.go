// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Unit tests for the pure-math arbitrage core. They pin the exact integer
// outputs of GetAmountOut / OptimalArb against the worked numeric example from
// the Phase 2 research (validated to machine precision vs brute force) and cover
// the no-arb edge case.
package strategy

import (
	"math/big"
	"testing"
)

// scale18 returns v * 1e18 as a *big.Int (BSC tokens in the watch set are 18dp).
func scale18(v int64) *big.Int {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	return new(big.Int).Mul(big.NewInt(v), scale)
}

func mustBig(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad bigint literal %q", s)
	}
	return v
}

// TestGetAmountOut checks the single-hop pricing against PancakeRouter math.
func TestGetAmountOut(t *testing.T) {
	// reserveIn = reserveOut = 1_000_000e18, amountIn = 1_000e18.
	// amountInWithFee = 1000e18 * 9975
	// out = (amountInWithFee * 1_000_000e18) / (1_000_000e18*10000 + amountInWithFee)
	rIn := scale18(1_000_000)
	rOut := scale18(1_000_000)
	in := scale18(1_000)
	got := GetAmountOut(in, rIn, rOut, GammaPancakeV2)

	// Independently computed expected value (floor division).
	want := mustBig(t, "996505985279683515693")
	if got.Cmp(want) != 0 {
		t.Fatalf("GetAmountOut = %s, want %s", got.String(), want.String())
	}

	// Sanity: out must be strictly below the no-fee linear estimate (1000e18).
	if got.Cmp(scale18(1000)) >= 0 {
		t.Fatalf("GetAmountOut not reduced by fee/slippage: %s", got.String())
	}
}

func TestGetAmountOutZeroCases(t *testing.T) {
	r := scale18(1000)
	if v := GetAmountOut(big.NewInt(0), r, r, GammaPancakeV2); v.Sign() != 0 {
		t.Fatalf("zero amountIn must give 0, got %s", v)
	}
	if v := GetAmountOut(scale18(1), big.NewInt(0), r, GammaPancakeV2); v.Sign() != 0 {
		t.Fatalf("zero reserveIn must give 0, got %s", v)
	}
	if v := GetAmountOut(nil, r, r, GammaPancakeV2); v.Sign() != 0 {
		t.Fatalf("nil amountIn must give 0, got %s", v)
	}
}

// TestGetAmountInRoundTrip checks GetAmountIn is the (rounded-up) inverse of
// GetAmountOut: feeding the computed input must yield at least the target output.
func TestGetAmountInRoundTrip(t *testing.T) {
	rIn := scale18(1_000_000)
	rOut := scale18(1_000_000)
	target := scale18(500)
	in := GetAmountIn(target, rIn, rOut, GammaPancakeV2)
	if in.Sign() <= 0 {
		t.Fatalf("GetAmountIn returned non-positive: %s", in)
	}
	out := GetAmountOut(in, rIn, rOut, GammaPancakeV2)
	if out.Cmp(target) < 0 {
		t.Fatalf("GetAmountIn underfills: out=%s < target=%s", out, target)
	}
}

// TestOptimalArbWorkedExample pins the EXACT wei-scale outputs from the research
// worked example:
//
//	gamma=0.9975 (Pancake V2)
//	pool A (X->T): Ra_in=1_000_000e18, Ra_out=1_050_000e18
//	pool B (T->X): Rb_in=1_000_000e18, Rb_out=1_050_000e18
//	=> optIn  = 23197379247006336893580
//	   profit = 1098975841826925210332
func TestOptimalArbWorkedExample(t *testing.T) {
	raIn := scale18(1_000_000)
	raOut := scale18(1_050_000)
	rbIn := scale18(1_000_000)
	rbOut := scale18(1_050_000)

	optIn, gross := OptimalArb(raIn, raOut, rbIn, rbOut, GammaPancakeV2)

	wantIn := mustBig(t, "23197379247006336893580")
	wantProfit := mustBig(t, "1098975841826925210332")

	if optIn.Cmp(wantIn) != 0 {
		t.Fatalf("OptimalArb optIn = %s, want %s", optIn.String(), wantIn.String())
	}
	if gross.Cmp(wantProfit) != 0 {
		t.Fatalf("OptimalArb grossProfit = %s, want %s", gross.String(), wantProfit.String())
	}
}

// TestOptimalArbIsMaximum checks the closed-form input beats nearby inputs (the
// brute-force optimality sanity check).
func TestOptimalArbIsMaximum(t *testing.T) {
	raIn := scale18(1_000_000)
	raOut := scale18(1_050_000)
	rbIn := scale18(1_000_000)
	rbOut := scale18(1_050_000)

	optIn, gross := OptimalArb(raIn, raOut, rbIn, rbOut, GammaPancakeV2)

	profitAt := func(x *big.Int) *big.Int {
		tBought := GetAmountOut(x, raIn, raOut, GammaPancakeV2)
		xBack := GetAmountOut(tBought, rbIn, rbOut, GammaPancakeV2)
		return new(big.Int).Sub(xBack, x)
	}

	delta := scale18(100)
	lower := profitAt(new(big.Int).Sub(optIn, delta))
	higher := profitAt(new(big.Int).Add(optIn, delta))

	if gross.Cmp(lower) < 0 {
		t.Fatalf("profit at optIn (%s) < profit at optIn-delta (%s)", gross, lower)
	}
	if gross.Cmp(higher) < 0 {
		t.Fatalf("profit at optIn (%s) < profit at optIn+delta (%s)", gross, higher)
	}
}

// TestNoArb verifies that balanced/unfavourable pools yield no opportunity:
// OptimalArb returns (0,0) and HasArb is false.
func TestNoArb(t *testing.T) {
	// Identical, perfectly-balanced pools: gamma^2 < 1 so no arb.
	r := scale18(1_000_000)
	if HasArb(r, r, r, r, GammaPancakeV2) {
		t.Fatalf("HasArb true for balanced pools (should be false)")
	}
	optIn, gross := OptimalArb(r, r, r, r, GammaPancakeV2)
	if optIn.Sign() != 0 || gross.Sign() != 0 {
		t.Fatalf("balanced pools must yield no arb, got optIn=%s gross=%s", optIn, gross)
	}

	// A tiny price difference that does NOT overcome the double fee: still no arb.
	raIn := scale18(1_000_000)
	raOut := scale18(1_001_000) // ~0.1% better, < 2*0.25% fee
	rbIn := scale18(1_000_000)
	rbOut := scale18(1_000_000)
	optIn2, gross2 := OptimalArb(raIn, raOut, rbIn, rbOut, GammaPancakeV2)
	if optIn2.Sign() != 0 || gross2.Sign() != 0 {
		t.Fatalf("sub-fee imbalance must yield no arb, got optIn=%s gross=%s", optIn2, gross2)
	}
}

// TestEvaluateNetProfit checks the cost subtraction and profitability flag.
func TestEvaluateNetProfit(t *testing.T) {
	raIn := scale18(1_000_000)
	raOut := scale18(1_050_000)
	rbIn := scale18(1_000_000)
	rbOut := scale18(1_050_000)

	// Gross ~ 1098.97e18. Subtract small gas + zero bid + small margin: still net positive.
	p := EvalParams{
		GasCost:    scale18(1),  // 1 token-unit
		BuilderBid: big.NewInt(0),
		Margin:     scale18(10), // 10 token-units
	}
	eval := Evaluate(raIn, raOut, rbIn, rbOut, GammaPancakeV2, p)
	if !eval.Profitable {
		t.Fatalf("expected profitable, net=%s", eval.NetProfit)
	}
	wantNet := new(big.Int).Sub(eval.GrossProfit, scale18(11))
	if eval.NetProfit.Cmp(wantNet) != 0 {
		t.Fatalf("net = %s, want %s", eval.NetProfit, wantNet)
	}

	// Now make costs exceed gross: not profitable.
	p2 := EvalParams{GasCost: scale18(2_000), BuilderBid: big.NewInt(0), Margin: big.NewInt(0)}
	eval2 := Evaluate(raIn, raOut, rbIn, rbOut, GammaPancakeV2, p2)
	if eval2.Profitable {
		t.Fatalf("expected NOT profitable, net=%s", eval2.NetProfit)
	}
	if eval2.NetProfit.Sign() >= 0 {
		t.Fatalf("expected negative net, got %s", eval2.NetProfit)
	}
}

// TestBestArbPicksDirection verifies BestArb selects the more profitable side
// for an asymmetric pair-of-pools.
func TestBestArbPicksDirection(t *testing.T) {
	// Pool A favourable for X->T->X, pool B neutral.
	aIn := scale18(1_000_000)
	aOut := scale18(1_100_000)
	bIn := scale18(1_000_000)
	bOut := scale18(1_000_000)

	eval, _ := BestArb(aIn, aOut, bIn, bOut, GammaPancakeV2, EvalParams{})
	if !eval.Profitable {
		t.Fatalf("expected a profitable direction, net=%s", eval.NetProfit)
	}
	if eval.OptimalAmountIn.Sign() <= 0 {
		t.Fatalf("expected positive optimal input, got %s", eval.OptimalAmountIn)
	}
}
