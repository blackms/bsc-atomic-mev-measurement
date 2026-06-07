// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// sandwich.go is the pure-math core of the SANDWICH-attack valuator — the
// paper's new headline result (sandwiching is the dominant atomic MEV on BSC,
// ~51% of MEV volume). Like arb.go and quoter_oracle.go it has NO chain access
// and NO global state: every function is a deterministic big.Int computation,
// unit-testable in isolation (see sandwich_test.go). The chain-facing
// ground-truth re-execution (real PancakeSwap router code on a state.Copy) lives
// in simengine/sandwich.go; this file supplies the analytics it seeds and bounds
// the search with, plus the victim decoding and the net-profit gate.
//
// METHOD (matching the rest of the pipeline: closed form to seed/bound, EVM to
// decide). A sandwich on a victim swap that spends token X on a watched pool:
//
//	FRONTRUN: attacker swaps f of X -> Y on the pool (victim's direction).
//	VICTIM:   the REAL victim tx runs on the frontrun-mutated pool.
//	BACKRUN:  attacker sells the Y it bought back to X.
//	profit(f) = (X recovered) - f, in start-token X units.
//
// For a V2 CPMM the optimal frontrun has a closed form; arXiv:2601.19570 shows
// the small-trade maximiser is the classical "half-the-victim" rule f* = Vv/2.
// But the UNCONSTRAINED profit-maximising frontrun runs away above the victim
// whenever the victim is not small relative to depth — an ARTIFACT, because a
// frontrun that large makes the real victim tx breach its amountOutMin and
// REVERT, collapsing the sandwich. So the realistic optimum is
// f* = min(interior-FOC optimum, Vf_max(slippage)). The ground-truth EVM method
// enforces that revert for free (the victim carries its real amountOutMin), so
// the closed form here is used ONLY to seed/bound a golden-section search whose
// authority is the 3-step EVM re-execution. V3 has no global closed form (it is
// tick-piecewise) and is sized purely by the search.
package strategy

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ---------------------------------------------------------------------------
// V2 sandwich profit (closed form, contract-exact).
// ---------------------------------------------------------------------------

// V2SandwichGross returns the GROSS sandwich profit (in start-token X units, may
// be negative) of frontrunning a V2 victim swap, computed in the SAME floor
// integer arithmetic the on-chain pair uses (via GetAmountOut). All amounts are
// in X = the token the victim and attacker SPEND.
//
//	x = reserveIn  (pool reserve of X)
//	y = reserveOut (pool reserve of Y)
//	v = victim amountIn (X spent by the victim)
//	f = attacker frontrun amountIn (X)
//	g = pool fee factor (Pancake V2 = 9975/10000)
//
// Steps (each via the contract-exact GetAmountOut):
//
//	frontrun: yf = out(f, x, y)              new reserves (x+f, y-yf)
//	victim:   yv = out(v, x+f, y-yf)         new reserves (x+f+v, y-yf-yv)
//	backrun:  xb = out(yf, y-yf-yv, x+f+v)   attacker sells exactly yf of Y back
//	gross   = xb - f
//
// This models the attacker selling back EXACTLY the Y it acquired in the
// frontrun (the standard sandwich), so no residual inventory is left. A
// non-positive frontrun or reserve yields gross 0.
func V2SandwichGross(reserveIn, reserveOut, victimIn, frontrunIn *big.Int, g Gamma) *big.Int {
	if reserveIn == nil || reserveOut == nil || victimIn == nil || frontrunIn == nil {
		return big.NewInt(0)
	}
	if reserveIn.Sign() <= 0 || reserveOut.Sign() <= 0 || frontrunIn.Sign() <= 0 {
		return big.NewInt(0)
	}
	x := reserveIn
	y := reserveOut

	// FRONTRUN.
	yf := GetAmountOut(frontrunIn, x, y, g)
	if yf.Sign() <= 0 {
		return big.NewInt(0)
	}
	xAfterF := new(big.Int).Add(x, frontrunIn)
	yAfterF := new(big.Int).Sub(y, yf)
	if yAfterF.Sign() <= 0 {
		return big.NewInt(0)
	}

	// VICTIM (on the frontrun-mutated pool). A non-positive victim is allowed
	// (e.g. pure backrun edge case); it just leaves the pool unchanged.
	xAfterV := new(big.Int).Set(xAfterF)
	yAfterV := new(big.Int).Set(yAfterF)
	if victimIn.Sign() > 0 {
		yv := GetAmountOut(victimIn, xAfterF, yAfterF, g)
		xAfterV.Add(xAfterF, victimIn)
		yAfterV.Sub(yAfterF, yv)
		if yAfterV.Sign() <= 0 {
			return big.NewInt(0)
		}
	}

	// BACKRUN: sell the frontrun's yf of Y back into the (now propped-up) pool.
	xb := GetAmountOut(yf, yAfterV, xAfterV, g)
	return new(big.Int).Sub(xb, frontrunIn)
}

// ---------------------------------------------------------------------------
// V2 optimal-frontrun closed form (seed for the search).
// ---------------------------------------------------------------------------

// HalfVictimSeed is the small-trade optimal frontrun from arXiv:2601.19570
// Prop. 1: f* = Vv/2 ("half the victim"). It is exact only in the small-trade
// regime; here it is a SEED for the golden-section search (the EVM re-execution
// is the authority). Returns 0 for a non-positive victim.
func HalfVictimSeed(victimIn *big.Int) *big.Int {
	if victimIn == nil || victimIn.Sign() <= 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Rsh(new(big.Int).Set(victimIn), 1) // victimIn / 2
}

// V2CombinedSeed is the tighter combined-trade frontrun seed used in the
// literature, realised in integer math:
//
//	f ≈ ( isqrt( x*Den * (x*Den + Num*v) ) - x*Den ) / Num
//
// where x = reserveIn, v = victimIn, (Num,Den) = gamma. It accounts for the
// frontrun's own price impact better than Vv/2 for larger victims. Like
// HalfVictimSeed it is a SEED only; returns 0 when no positive seed exists.
func V2CombinedSeed(reserveIn, victimIn *big.Int, g Gamma) *big.Int {
	if reserveIn == nil || victimIn == nil || g.Num == nil || g.Den == nil {
		return big.NewInt(0)
	}
	if reserveIn.Sign() <= 0 || victimIn.Sign() <= 0 || g.Num.Sign() <= 0 {
		return big.NewInt(0)
	}
	xDen := new(big.Int).Mul(reserveIn, g.Den)          // x*Den
	numV := new(big.Int).Mul(g.Num, victimIn)           // Num*v
	inner := new(big.Int).Add(xDen, numV)               // x*Den + Num*v
	prod := new(big.Int).Mul(xDen, inner)               // x*Den * (x*Den + Num*v)
	root := new(big.Int).Sqrt(prod)                     // isqrt
	numer := new(big.Int).Sub(root, xDen)               // root - x*Den
	if numer.Sign() <= 0 {
		return big.NewInt(0)
	}
	f := new(big.Int).Quo(numer, g.Num)
	if f.Sign() <= 0 {
		return big.NewInt(0)
	}
	return f
}

// SandwichProbe evaluates the gross sandwich profit at a given frontrun size. In
// the live pipeline it is bound to the ground-truth 3-step EVM re-execution
// (simengine.sandwichProfit on a fresh state.Copy); in tests it is the V2 closed
// form. ok=false means the frontrun size is infeasible (e.g. the victim tx would
// revert, or a V3 swap would exceed liquidity) and must be treated as the
// boundary of the feasible bracket. The returned gross may be negative (with
// ok=true) so the search can compare loss-making but feasible sizes.
type SandwichProbe func(frontrunIn *big.Int) (gross *big.Int, ok bool)

// sandwichSearchIters bounds the golden-section iterations for frontrun sizing.
// Mirrors optimalInputIters; each iteration is one (possibly EVM) probe.
const sandwichSearchIters = 60

// OptimalFrontrun finds the gross-profit-maximising frontrun size for a victim
// swap via golden-section search over `probe`, seeded by the supplied closed-form
// candidates and bounded above by maxFrontrun. It returns the best frontrun and
// its gross profit, or (0,0) when no positive-gross feasible size exists.
//
// Profit(f) is 0 at f=0, rises to a single interior peak, then falls (and is
// killed — probe returns !ok — at the victim's slippage cap). Within a V2 pool
// or a single V3 tick it is unimodal; across V3 tick boundaries it is only
// piecewise-unimodal, so the seeds (which bracket the analytic optimum) are
// always probed in addition to the search to avoid settling in a lower lobe.
//
// seeds are optional closed-form starting points (e.g. HalfVictimSeed,
// V2CombinedSeed); any non-positive or out-of-range seed is ignored. maxFrontrun
// bounds the bracket; a nil/non-positive bound falls back to defaultMaxInputWei.
func OptimalFrontrun(probe SandwichProbe, seeds []*big.Int, maxFrontrun *big.Int) (bestFrontrun, bestGross *big.Int) {
	zero := func() (*big.Int, *big.Int) { return big.NewInt(0), big.NewInt(0) }
	if probe == nil {
		return zero()
	}

	hi := maxFrontrun
	if hi == nil || hi.Sign() <= 0 {
		hi = new(big.Int).Set(defaultMaxInputWei)
	} else {
		hi = new(big.Int).Set(hi)
	}

	// Shrink hi to a feasible frontrun: an oversized frontrun makes the victim
	// revert (probe !ok), so probe down by halving until one succeeds. This keeps
	// the golden-section bracket inside the feasible region.
	infeasibleHi := (*big.Int)(nil) // smallest known-infeasible size above hi, if any.
	for hi.Sign() > 0 {
		if _, ok := probe(hi); ok {
			break
		}
		infeasibleHi = new(big.Int).Set(hi)
		hi.Rsh(hi, 1)
	}
	if hi.Sign() <= 0 {
		return zero()
	}

	// Recover the TRUE feasibility boundary (Vf_max) between the largest known
	// feasible size (hi) and the smallest known infeasible size (infeasibleHi) by
	// bisection. Halving alone can land hi far below the real cap (e.g. cap=12 but
	// halving 160->...->10 stops at 10), and for a sandwich the gross is still
	// RISING up to the slippage cap, so the constrained optimum sits AT the
	// boundary. Pushing hi up to the boundary lets the golden-section reach it.
	if infeasibleHi != nil {
		feasible := new(big.Int).Set(hi)
		bad := new(big.Int).Set(infeasibleHi)
		for new(big.Int).Sub(bad, feasible).Cmp(big.NewInt(1)) > 0 {
			midB := new(big.Int).Add(feasible, bad)
			midB.Rsh(midB, 1)
			if _, ok := probe(midB); ok {
				feasible.Set(midB)
			} else {
				bad.Set(midB)
			}
		}
		hi = feasible
	}

	bestFrontrun, bestGross = big.NewInt(0), big.NewInt(0)
	consider := func(x, gr *big.Int) {
		if gr != nil && gr.Sign() > 0 && gr.Cmp(bestGross) > 0 {
			bestGross = new(big.Int).Set(gr)
			bestFrontrun = new(big.Int).Set(x)
		}
	}

	// Probe every supplied seed (clamped into [1,hi]) up-front so the search never
	// regresses below the analytic optimum and the V3 piecewise lobes are sampled.
	for _, s := range seeds {
		if s == nil || s.Sign() <= 0 {
			continue
		}
		sx := s
		if sx.Cmp(hi) > 0 {
			sx = hi
		}
		if gr, ok := probe(sx); ok {
			consider(sx, gr)
		}
	}

	lo := big.NewInt(1)
	grossAt := func(x *big.Int) (*big.Int, bool) {
		if x.Sign() <= 0 {
			return nil, false
		}
		return probe(x)
	}

	three := big.NewInt(3)
	for i := 0; i < sandwichSearchIters && hi.Cmp(lo) > 0; i++ {
		span := new(big.Int).Sub(hi, lo)
		step := new(big.Int).Quo(span, three)
		if step.Sign() <= 0 {
			step = big.NewInt(1)
		}
		m1 := new(big.Int).Add(lo, step)
		m2 := new(big.Int).Sub(hi, step)
		if m1.Cmp(m2) > 0 {
			m1, m2 = m2, m1
		}

		g1, ok1 := grossAt(m1)
		g2, ok2 := grossAt(m2)
		consider(m1, g1)
		consider(m2, g2)

		switch {
		case !ok1 && !ok2:
			hi = new(big.Int).Sub(m1, big.NewInt(1))
		case !ok2:
			hi = m2
		case !ok1:
			lo = m1
		case g1.Cmp(g2) >= 0:
			hi = m2
		default:
			lo = m1
		}
	}

	// Final refinement on the surviving bracket.
	mid := new(big.Int).Add(lo, hi)
	mid.Rsh(mid, 1)
	for _, x := range []*big.Int{lo, mid, hi} {
		if gr, ok := grossAt(x); ok {
			consider(x, gr)
		}
	}

	if bestGross.Sign() <= 0 {
		return zero()
	}
	return bestFrontrun, bestGross
}

// ---------------------------------------------------------------------------
// Flash-loan model and net-profit gate.
// ---------------------------------------------------------------------------

// SandwichGasUnits is the assumed gas budget for the attacker's TWO router
// transactions (frontrun + backrun). Each is a full router swap (calldata
// decode + transferFrom + pair.swap + transfer), heavier than a raw pair.swap.
// V2 legs are lighter than V3 (which may cross ticks); both default to a
// conservative BSC-realistic budget per leg plus the per-leg base.
func SandwichGasUnits(isV3 bool) uint64 {
	per := uint64(gasBaseUnits + gasPerV2Hop) // V2 router swap ~120k
	if isV3 {
		per = uint64(gasBaseUnits + gasPerV3Hop) // V3 exactInputSingle ~150k
	}
	return 2 * per
}

// FlashFee returns the flash-loan fee charged on a borrowed notional, in the
// notional's own token units: fee = ceil(notional * flashBps / 10000). With a
// flash loan the attacker needs ZERO inventory, so funding the synthetic
// attacker by storage writes is realistic; the fee is the cost of that leverage.
// BSC menu: PancakeSwap V3 pool.flash() = pool fee tier (1/5/25/100 bps), Aave
// V3 = 5 bps, Venus core-pool = 9 bps. A non-positive notional or bps yields 0.
func FlashFee(notional *big.Int, flashBps uint64) *big.Int {
	if notional == nil || notional.Sign() <= 0 || flashBps == 0 {
		return big.NewInt(0)
	}
	num := new(big.Int).Mul(notional, new(big.Int).SetUint64(flashBps))
	// Ceiling division by 10000 so a sub-bp fee never rounds to free.
	num.Add(num, big.NewInt(9999))
	return num.Quo(num, big.NewInt(10000))
}

// SandwichEval is the full economic assessment of a sandwich opportunity, all in
// start-token (X) units.
type SandwichEval struct {
	FrontrunIn  *big.Int // optimal frontrun size, in X
	GrossProfit *big.Int // ground-truth gross (attacker X delta over the 3 steps)
	GasCost     *big.Int // gasUnits * gasPrice, in X (BNB:1:1 for non-BNB start tokens)
	FlashFee    *big.Int // flashBps * frontrunIn / 1e4, in X
	BuilderBid  *big.Int // bid paid to the builder, in X
	NetProfit   *big.Int // gross - gas - flashFee - bid (may be negative)
	Profitable  bool     // NetProfit > 0
}

// SandwichNet assembles the net-profit gate from a measured gross and the cost
// model: net = gross - gasUnits*gasPrice - flashFee(frontrunIn) - bid. The flash
// fee is charged on the FRONTRUN notional (the amount borrowed to fund the
// frontrun leg). gross/frontrunIn come from OptimalFrontrun (ground truth);
// gasUnits from SandwichGasUnits; the rest from the caller's cost knobs.
func SandwichNet(frontrunIn, gross, gasPriceWei *big.Int, gasUnits, flashBps uint64, builderBid *big.Int) SandwichEval {
	fr := orZero(frontrunIn)
	gr := orZero(gross)
	gp := orZero(gasPriceWei)
	bid := orZero(builderBid)

	gasCost := new(big.Int).Mul(new(big.Int).SetUint64(gasUnits), gp)
	flashFee := FlashFee(fr, flashBps)

	net := new(big.Int).Set(gr)
	net.Sub(net, gasCost)
	net.Sub(net, flashFee)
	net.Sub(net, bid)

	return SandwichEval{
		FrontrunIn:  new(big.Int).Set(fr),
		GrossProfit: new(big.Int).Set(gr),
		GasCost:     gasCost,
		FlashFee:    flashFee,
		BuilderBid:  new(big.Int).Set(bid),
		NetProfit:   net,
		Profitable:  net.Sign() > 0,
	}
}

// ---------------------------------------------------------------------------
// Victim detection / decoding from logs.
// ---------------------------------------------------------------------------

// SandwichVictim is a decoded victim swap: the pool it hit, the token the victim
// SPENT (the sandwich's start token X) and the amount spent, the token received
// (Y), and the originating ExtPool (for fee / V3 / fee-tier branching).
type SandwichVictim struct {
	Pool    ExtPool
	TokenIn common.Address // X: token the victim spent (== attacker frontrun input)
	AmountIn *big.Int      // victim amountIn in X (wei)
}

// DecodeVictim inspects a single execution log and, if it is a Swap on a watched
// pool, decodes the victim's input token + amount (the sandwich's X). It handles
// both layouts:
//
//   - V2 Swap(sender, amount0In, amount1In, amount0Out, amount1Out, to): the
//     nonzero amountIn side is the spent token; data = 4 uint256 words.
//   - V3 Swap(sender, recipient, amount0(int256), amount1(int256), sqrtPriceX96,
//     liquidity, tick): the POSITIVE signed amount is tokens INTO the pool (the
//     victim's input); data = 5 words, amount0/amount1 are signed.
//
// ok=false for a non-watched log, a malformed payload, or a degenerate (zero
// amountIn) swap. The robust source is the LOG (most swaps route through
// aggregators), not the router calldata.
func DecodeVictim(l *types.Log) (SandwichVictim, bool) {
	p, ok := IsExtendedWatchedSwapLog(l)
	if !ok {
		return SandwichVictim{}, false
	}
	if p.IsV3 {
		return decodeV3Victim(l, p)
	}
	return decodeV2Victim(l, p)
}

// decodeV2Victim decodes a V2 Swap log. Sync logs (which also pass the watch
// filter) carry no directional amounts, so they are rejected here.
func decodeV2Victim(l *types.Log, p ExtPool) (SandwichVictim, bool) {
	if len(l.Topics) == 0 || l.Topics[0] != SwapTopic0 {
		return SandwichVictim{}, false
	}
	if len(l.Data) < 128 {
		return SandwichVictim{}, false
	}
	amount0In := new(big.Int).SetBytes(l.Data[0:32])
	amount1In := new(big.Int).SetBytes(l.Data[32:64])

	switch {
	case amount0In.Sign() > 0:
		// Victim spent token0.
		return SandwichVictim{Pool: p, TokenIn: p.Token0, AmountIn: amount0In}, true
	case amount1In.Sign() > 0:
		// Victim spent token1.
		return SandwichVictim{Pool: p, TokenIn: p.Token1, AmountIn: amount1In}, true
	default:
		return SandwichVictim{}, false
	}
}

// decodeV3Victim decodes a V3 Swap log. amount0/amount1 are int256 deltas from
// the POOL's perspective: positive = token flowed INTO the pool (victim input),
// negative = token flowed OUT (victim output). The positive side is X.
func decodeV3Victim(l *types.Log, p ExtPool) (SandwichVictim, bool) {
	if len(l.Topics) == 0 || l.Topics[0] != V3SwapTopic0 {
		return SandwichVictim{}, false
	}
	if len(l.Data) < 64 {
		return SandwichVictim{}, false
	}
	amount0 := signed256(l.Data[0:32])
	amount1 := signed256(l.Data[32:64])

	switch {
	case amount0.Sign() > 0:
		return SandwichVictim{Pool: p, TokenIn: p.Token0, AmountIn: amount0}, true
	case amount1.Sign() > 0:
		return SandwichVictim{Pool: p, TokenIn: p.Token1, AmountIn: amount1}, true
	default:
		return SandwichVictim{}, false
	}
}

// two256 = 2^256, used to interpret a 32-byte word as a signed two's-complement
// int256.
var two256 = new(big.Int).Lsh(big.NewInt(1), 256)

// signed256 interprets a 32-byte big-endian word as a two's-complement int256.
func signed256(b []byte) *big.Int {
	v := new(big.Int).SetBytes(b)
	// If the top bit is set the value is negative: v - 2^256.
	if len(b) == 32 && b[0]&0x80 != 0 {
		v.Sub(v, two256)
	}
	return v
}

// VictimAboveThreshold reports whether a victim's input notional clears a minimum
// USD floor — the dust filter. Below the floor no frontrun can clear 2 router-swap
// gas + the flash fee, so the sandwich is skipped. amountIn is the victim's X
// input (wei, 18dp for the watch-set tokens); tokenUSD is the USD price of one
// whole X token (e.g. liveWbnbPriceUSD for WBNB, ~1.0 for the stables); minUSD is
// the floor. A non-positive price or floor disables the gate (always true).
func VictimAboveThreshold(amountIn *big.Int, tokenUSD, minUSD float64) bool {
	if minUSD <= 0 || tokenUSD <= 0 {
		return true
	}
	if amountIn == nil || amountIn.Sign() <= 0 {
		return false
	}
	whole := new(big.Float).Quo(new(big.Float).SetInt(amountIn), big.NewFloat(1e18))
	w, _ := whole.Float64()
	return w*tokenUSD >= minUSD
}
