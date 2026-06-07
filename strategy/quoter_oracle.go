// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// quoter_oracle.go is the optimal-input search and net-profit assembly on top of
// the quoter-chaining valuator (quoter.go). It is the v4 replacement for the
// CycleOptimum closed form when a cycle contains a V3 hop:
//
//   - OptimalInput finds the gross-profit-maximising amountIn by golden-section
//     search (AMM cycle profit is unimodal in size: zero at x=0, rising to a
//     single interior peak, then falling as price impact dominates). Infeasible
//     sizes (V3 reverts when amountIn exceeds available liquidity) are treated as
//     the boundary of the feasible bracket.
//   - For an ALL-V2 cycle the exact closed form CycleOptimum is authoritative; the
//     search is still run and cross-checked but the closed-form result is returned
//     when it is at least as good (it is exact to the wei).
//   - ValueCycle assembles the full Evaluation: gross from the optimal input, gas
//     from per-hop gas units * gas price, minus bid and margin.
//
// PURE w.r.t. chain plumbing: all V3 pricing flows through the QuoteFn callback.
package strategy

import "math/big"

// Per-hop gas-unit estimates for the cost model. V3 single-hop swaps are heavier
// (tick crossing) than V2; these are conservative BSC-realistic budgets plus a
// fixed base for tx overhead and the executor's own bookkeeping. Used by
// CycleGasUnits when the caller wants a per-cycle gas figure instead of the flat
// DryRunConfig.GasUnits.
const (
	gasBaseUnits = 60_000  // tx base + executor entry/exit + token transfers
	gasPerV2Hop  = 90_000  // a V2 swap (incl. the pair transfer + k-check)
	gasPerV3Hop  = 130_000 // a V3 swap with at least one tick crossing
)

// CycleGasUnits returns an estimated gas budget for executing the whole cycle as
// a single backrun tx, summing a base cost plus a per-hop cost that depends on the
// hop's DEX version. This is the per-cycle cost the v4 detector charges against
// gross profit (replacing the flat 250k assumption for mixed/V3 cycles).
func CycleGasUnits(c Cycle) uint64 {
	g := uint64(gasBaseUnits)
	for _, e := range c.Edges {
		if e.IsV3 {
			g += gasPerV3Hop
		} else {
			g += gasPerV2Hop
		}
	}
	return g
}

// optimalInputIters bounds the golden-section iterations. ~60 iterations narrows a
// [1, hi] wei bracket to a handful of wei — far below the precision the wei-level
// gross needs — while keeping per-cycle quoter calls bounded (each iteration does
// one cycle evaluation = one quote per V3 hop).
const optimalInputIters = 60

// defaultMaxInputWei is the upper bracket bound for the golden-section search when
// the caller supplies none: 1000 WBNB (1e21 wei). Large arbs on the BSC hub set
// never exceed this; the search shrinks the bracket toward the true interior peak
// and infeasible (revert) sizes pull the high bound down automatically.
var defaultMaxInputWei = new(big.Int).Exp(big.NewInt(10), big.NewInt(21), nil)

// OptimalInput finds the gross-profit-maximising amountIn for the cycle and the
// gross profit at that input, valuing every hop on the intermediate state via the
// quoter chain. quote is the V3 callback (bound to SimEngine.EthCall); it may be
// nil for an all-V2 cycle. maxInput bounds the search bracket (nil => 1000 WBNB).
//
// For an all-V2 cycle the exact closed form (CycleOptimum) is used and the
// golden-section result is cross-checked: whichever yields the larger gross (at
// equal feasibility) is returned, so the search can never regress below the exact
// optimum. Returns (0,0) when no profitable input exists.
func OptimalInput(quote QuoteFn, c Cycle, maxInput *big.Int) (bestAmountIn, bestGross *big.Int) {
	zero := func() (*big.Int, *big.Int) { return big.NewInt(0), big.NewInt(0) }
	if len(c.Edges) < 2 {
		return zero()
	}

	// All-V2 fast path: the closed form is exact. Seed best with it and still run
	// the search as a cross-check / safety net (they must agree to within flooring).
	allV2 := true
	for _, e := range c.Edges {
		if e.IsV3 {
			allV2 = false
			break
		}
	}
	bestAmountIn, bestGross = big.NewInt(0), big.NewInt(0)
	if allV2 {
		cfIn, cfGross := CycleOptimum(c)
		if cfGross.Sign() > 0 {
			bestAmountIn, bestGross = cfIn, cfGross
		}
	}

	hi := maxInput
	if hi == nil || hi.Sign() <= 0 {
		hi = new(big.Int).Set(defaultMaxInputWei)
	} else {
		hi = new(big.Int).Set(hi)
	}

	// Shrink hi to the largest feasible (non-reverting) size: a V3 cycle reverts
	// for amountIn beyond available liquidity, so probe down by halving until a
	// quote succeeds. This keeps the golden-section bracket inside the feasible
	// region where profit is unimodal.
	for hi.Sign() > 0 {
		if _, ok := CycleGross(quote, c, hi); ok {
			break
		}
		hi.Rsh(hi, 1) // hi /= 2
	}
	if hi.Sign() <= 0 {
		return bestAmountIn, bestGross
	}

	lo := big.NewInt(1)

	// Golden-section search for the interior maximum of gross(x). We compare on
	// gross (which may be negative); infeasible probes are treated as -inf so the
	// bracket migrates toward the feasible peak.
	grossAt := func(x *big.Int) (*big.Int, bool) {
		if x.Sign() <= 0 {
			return nil, false
		}
		return CycleGross(quote, c, x)
	}
	consider := func(x, g *big.Int) {
		if g != nil && g.Sign() > 0 && g.Cmp(bestGross) > 0 {
			bestGross = new(big.Int).Set(g)
			bestAmountIn = new(big.Int).Set(x)
		}
	}

	// Two interior probes via the 1/3 split (ternary-style golden section on
	// integers); robust without floating point.
	three := big.NewInt(3)
	for i := 0; i < optimalInputIters && hi.Cmp(lo) > 0; i++ {
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
			// Both infeasible: pull the high bound in.
			hi = new(big.Int).Sub(m1, big.NewInt(1))
		case !ok2:
			// Right probe infeasible (too large): peak is left of m2.
			hi = m2
		case !ok1:
			// Left probe infeasible (shouldn't happen for x>=1, but be safe).
			lo = m1
		case g1.Cmp(g2) >= 0:
			hi = m2
		default:
			lo = m1
		}
	}

	// Final refinement: evaluate the surviving bracket endpoints and midpoint.
	mid := new(big.Int).Add(lo, hi)
	mid.Rsh(mid, 1)
	for _, x := range []*big.Int{lo, mid, hi} {
		if g, ok := grossAt(x); ok {
			consider(x, g)
		}
	}

	if bestGross.Sign() <= 0 {
		return zero()
	}
	return bestAmountIn, bestGross
}

// ValueCycle is the v4 ground-truth Stage-B valuation: it finds the optimal input
// via the quoter chain, computes gross at that input, and subtracts the per-cycle
// gas cost (CycleGasUnits * gasPriceWei) plus bid and margin to yield the net
// Evaluation. quote is the V3 callback (nil for all-V2). gasPriceWei is the
// assumed effective gas price in start-token wei; bid/margin come from EvalParams.
//
// The returned Evaluation uses the SAME shape as EvaluateCycle so the harness
// reporting is uniform; GasCost is the computed per-cycle gas (not the flat
// EvalParams.GasCost), so V3 cycles carry a realistic V3-weighted gas charge.
func ValueCycle(quote QuoteFn, c Cycle, gasPriceWei *big.Int, p EvalParams) Evaluation {
	optIn, gross := OptimalInput(quote, c, nil)

	gp := orZero(gasPriceWei)
	gasUnits := new(big.Int).SetUint64(CycleGasUnits(c))
	gasCost := new(big.Int).Mul(gasUnits, gp)

	bid := orZero(p.BuilderBid)
	margin := orZero(p.Margin)

	net := new(big.Int).Set(gross)
	net.Sub(net, gasCost)
	net.Sub(net, bid)
	net.Sub(net, margin)

	return Evaluation{
		OptimalAmountIn: optIn,
		GrossProfit:     gross,
		GasCost:         gasCost,
		BuilderBid:      new(big.Int).Set(bid),
		Margin:          new(big.Int).Set(margin),
		NetProfit:       net,
		Profitable:      net.Sign() > 0,
	}
}
