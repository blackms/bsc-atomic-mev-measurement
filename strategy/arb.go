// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Package strategy implements the Phase 2 BSC backrun searcher: a pure, dry-run
// (no-submission) strategy that, given a DEX swap which moved a PancakeSwap V2
// pool's price, computes the profit-maximising 2-pool constant-product
// (x*y=k) arbitrage and quantifies the would-be profit.
//
// arb.go is the pure-math core. It has NO chain access and NO global state: every
// function is a deterministic big.Int computation, fully unit-testable in
// isolation (see arb_test.go). Chain wiring lives in pools.go and in the dry-run
// harness (simengine/dryrun.go).
//
// All math mirrors the UniswapV2 / PancakeSwap V2 pricing law. PancakeSwap V2
// charges a 0.25% fee, i.e. the multiplicative "kept" factor gamma = 9975/10000.
// (Uniswap/Sushi V2 use 997/1000 = 0.3%.) Each Gamma value carries its own
// numerator/denominator so the same code serves any V2-style fork.
package strategy

import "math/big"

// Gamma is a swap fee factor expressed as the exact rational Num/Den, where
// gamma = (1 - fee). For PancakeSwap V2 this is 9975/10000 (0.25% fee).
type Gamma struct {
	Num *big.Int // e.g. 9975
	Den *big.Int // e.g. 10000
}

// GammaPancakeV2 is the PancakeSwap V2 fee factor (0.25% fee => 9975/10000).
var GammaPancakeV2 = Gamma{Num: big.NewInt(9975), Den: big.NewInt(10000)}

// GammaUniswapV2 is the Uniswap/Sushi V2 fee factor (0.30% fee => 997/1000).
// Provided for forks; not used by the default BSC PancakeSwap watch set.
var GammaUniswapV2 = Gamma{Num: big.NewInt(997), Den: big.NewInt(1000)}

// GetAmountOut returns the output amount of a single UniswapV2/PancakeSwap V2
// swap given the input amount and the (reserveIn, reserveOut) of the pool, using
// floor integer division to match the on-chain contract exactly:
//
//	amountInWithFee = amountIn * gamma.Num
//	amountOut       = (amountInWithFee * reserveOut) / (reserveIn * gamma.Den + amountInWithFee)
//
// Returns 0 for any non-positive input or reserve (no panic, caller can clamp).
func GetAmountOut(amountIn, reserveIn, reserveOut *big.Int, g Gamma) *big.Int {
	if amountIn == nil || reserveIn == nil || reserveOut == nil {
		return big.NewInt(0)
	}
	if amountIn.Sign() <= 0 || reserveIn.Sign() <= 0 || reserveOut.Sign() <= 0 {
		return big.NewInt(0)
	}
	amountInWithFee := new(big.Int).Mul(amountIn, g.Num)
	numerator := new(big.Int).Mul(amountInWithFee, reserveOut)
	denominator := new(big.Int).Mul(reserveIn, g.Den)
	denominator.Add(denominator, amountInWithFee)
	return new(big.Int).Quo(numerator, denominator)
}

// GetAmountIn returns the input amount required to receive amountOut from a
// single swap (the inverse of GetAmountOut), matching UniswapV2Library getAmountIn:
//
//	amountIn = (reserveIn * amountOut * gamma.Den) / ((reserveOut - amountOut) * gamma.Num) + 1
//
// Returns 0 when amountOut is not strictly less than reserveOut (impossible to
// fill) or for non-positive inputs.
func GetAmountIn(amountOut, reserveIn, reserveOut *big.Int, g Gamma) *big.Int {
	if amountOut == nil || reserveIn == nil || reserveOut == nil {
		return big.NewInt(0)
	}
	if amountOut.Sign() <= 0 || reserveIn.Sign() <= 0 || reserveOut.Sign() <= 0 {
		return big.NewInt(0)
	}
	if amountOut.Cmp(reserveOut) >= 0 {
		return big.NewInt(0)
	}
	numerator := new(big.Int).Mul(reserveIn, amountOut)
	numerator.Mul(numerator, g.Den)
	denominator := new(big.Int).Sub(reserveOut, amountOut)
	denominator.Mul(denominator, g.Num)
	res := new(big.Int).Quo(numerator, denominator)
	return res.Add(res, big.NewInt(1))
}

// HasArb is the cheap, sqrt-free profitability pre-check for a 2-pool cycle
// X -> T (on pool A) -> X (on pool B):
//
//	arb exists iff gamma.Num^2 * Ra_out*Rb_out > gamma.Den^2 * Ra_in*Rb_in
//
// i.e. gamma^2 * (Ra_out/Ra_in) * (Rb_out/Rb_in) > 1. If this is false there is
// no profitable input in this direction and the optimal-input formula would
// return <= 0.
func HasArb(raIn, raOut, rbIn, rbOut *big.Int, g Gamma) bool {
	if raIn == nil || raOut == nil || rbIn == nil || rbOut == nil {
		return false
	}
	if raIn.Sign() <= 0 || raOut.Sign() <= 0 || rbIn.Sign() <= 0 || rbOut.Sign() <= 0 {
		return false
	}
	// lhs = gamma.Num^2 * raOut * rbOut
	lhs := new(big.Int).Mul(g.Num, g.Num)
	lhs.Mul(lhs, raOut)
	lhs.Mul(lhs, rbOut)
	// rhs = gamma.Den^2 * raIn * rbIn
	rhs := new(big.Int).Mul(g.Den, g.Den)
	rhs.Mul(rhs, raIn)
	rhs.Mul(rhs, rbIn)
	return lhs.Cmp(rhs) > 0
}

// OptimalArb computes the profit-maximising input amount and the resulting gross
// profit for the 2-pool cyclic arbitrage:
//
//	start with token X -> buy T on pool A -> sell T on pool B -> back to X.
//
// The FIRST hop's reserves are (raIn, raOut) = (X, T) on pool A, the SECOND
// hop's reserves are (rbIn, rbOut) = (T, X) on pool B. The closed-form optimum
// (UniswapV2 two-pool arb, ccyanxyz / Flashbots derivation) collapses the two
// hops into a synthetic constant-product pool and solves dP/dx = 0:
//
//	optimalAmountIn = ( gamma*sqrt(Ra_in*Ra_out*Rb_in*Rb_out) - Ra_in*Rb_in )
//	                  / ( gamma*(gamma*Ra_out + Rb_in) )
//
// realised as the exact integer form (gamma = Num/Den), using floor sqrt:
//
//	prod  = Ra_in*Ra_out*Rb_in*Rb_out * Num*Num
//	numer = Den * ( isqrt(prod) - Den*Ra_in*Rb_in )
//	denom = Num * ( Num*Ra_out + Den*Rb_in )
//	optIn = numer / denom
//
// grossProfit = GetAmountOut(GetAmountOut(optIn, A), B) - optIn, in units of X.
//
// When no profitable arb exists in this direction, both returned values are 0
// (never negative): the caller should then try the reverse direction by swapping
// the roles of pool A and pool B.
func OptimalArb(raIn, raOut, rbIn, rbOut *big.Int, g Gamma) (optimalAmountIn, grossProfit *big.Int) {
	zero := func() (*big.Int, *big.Int) { return big.NewInt(0), big.NewInt(0) }
	if raIn == nil || raOut == nil || rbIn == nil || rbOut == nil {
		return zero()
	}
	if raIn.Sign() <= 0 || raOut.Sign() <= 0 || rbIn.Sign() <= 0 || rbOut.Sign() <= 0 {
		return zero()
	}
	// Cheap gate before the (relatively) expensive sqrt.
	if !HasArb(raIn, raOut, rbIn, rbOut, g) {
		return zero()
	}

	// prod = raIn * raOut * rbIn * rbOut * Num^2
	prod := new(big.Int).Mul(raIn, raOut)
	prod.Mul(prod, rbIn)
	prod.Mul(prod, rbOut)
	prod.Mul(prod, g.Num)
	prod.Mul(prod, g.Num)
	root := new(big.Int).Sqrt(prod) // floor sqrt

	// numer = Den * ( root - Den*raIn*rbIn )
	inner := new(big.Int).Mul(raIn, rbIn)
	inner.Mul(inner, g.Den)
	numer := new(big.Int).Sub(root, inner)
	if numer.Sign() <= 0 {
		return zero()
	}
	numer.Mul(numer, g.Den)

	// denom = Num * ( Num*raOut + Den*rbIn )
	denom := new(big.Int).Mul(g.Num, raOut)
	tmp := new(big.Int).Mul(g.Den, rbIn)
	denom.Add(denom, tmp)
	denom.Mul(denom, g.Num)

	optIn := new(big.Int).Quo(numer, denom)
	if optIn.Sign() <= 0 {
		return zero()
	}

	// Realised gross profit at the (floored) optimal input.
	tBought := GetAmountOut(optIn, raIn, raOut, g)
	xBack := GetAmountOut(tBought, rbIn, rbOut, g)
	gross := new(big.Int).Sub(xBack, optIn)
	if gross.Sign() <= 0 {
		return zero()
	}
	return optIn, gross
}

// Evaluation is the result of a full economic assessment of an opportunity.
type Evaluation struct {
	OptimalAmountIn *big.Int // input size of the cycle, in start-token units
	GrossProfit     *big.Int // GetAmountOut chain - input, in start-token units
	GasCost         *big.Int // gasUsed * effectiveGasPrice, converted to start-token units
	BuilderBid      *big.Int // bid paid to the block builder, in start-token units
	Margin          *big.Int // safety margin subtracted, in start-token units
	NetProfit       *big.Int // gross - gas - bid - margin (may be negative)
	Profitable      bool     // NetProfit > 0
}

// EvalParams configures the economic costs subtracted from gross profit. All
// amounts are expressed in the SAME token units as the gross profit (the cycle's
// start/end token X). GasCost is supplied directly because converting BNB gas to
// the start token requires a price the caller already knows; for cycles starting
// in WBNB it is simply gasUsed*gasPrice in wei.
type EvalParams struct {
	GasCost    *big.Int // total gas cost in start-token units (e.g. gasUsed*gasPrice for WBNB cycles)
	BuilderBid *big.Int // bid paid to builder in start-token units (0 in dry-run)
	Margin     *big.Int // extra safety margin in start-token units
}

// Evaluate runs OptimalArb and then subtracts the economic costs to yield the
// net would-be profit. It performs NO chain access: the reserves and the cost
// parameters are all supplied by the caller. net = gross - gasCost - bid - margin.
func Evaluate(raIn, raOut, rbIn, rbOut *big.Int, g Gamma, p EvalParams) Evaluation {
	optIn, gross := OptimalArb(raIn, raOut, rbIn, rbOut, g)

	gas := orZero(p.GasCost)
	bid := orZero(p.BuilderBid)
	margin := orZero(p.Margin)

	net := new(big.Int).Set(gross)
	net.Sub(net, gas)
	net.Sub(net, bid)
	net.Sub(net, margin)

	return Evaluation{
		OptimalAmountIn: optIn,
		GrossProfit:     gross,
		GasCost:         new(big.Int).Set(gas),
		BuilderBid:      new(big.Int).Set(bid),
		Margin:          new(big.Int).Set(margin),
		NetProfit:       net,
		Profitable:      net.Sign() > 0,
	}
}

// BestArb tries BOTH cycle directions for two pools that share a common token
// and returns the more profitable Evaluation together with a flag indicating
// which direction (forward = A then B). Reserves are given per-direction:
// forward uses (aIn,aOut,bIn,bOut); reverse swaps the roles of A and B.
func BestArb(aIn, aOut, bIn, bOut *big.Int, g Gamma, p EvalParams) (eval Evaluation, forward bool) {
	fwd := Evaluate(aIn, aOut, bIn, bOut, g, p)
	rev := Evaluate(bIn, bOut, aIn, aOut, g, p)
	if rev.NetProfit.Cmp(fwd.NetProfit) > 0 {
		return rev, false
	}
	return fwd, true
}

func orZero(v *big.Int) *big.Int {
	if v == nil {
		return big.NewInt(0)
	}
	return v
}
