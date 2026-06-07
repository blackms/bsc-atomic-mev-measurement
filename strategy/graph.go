// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// graph.go is the Stage-A analytic detector of the IMPROVED arbitrage model
// (paper/40-models.md). It builds a directed token multigraph from a pool
// registry and enumerates profitable cyclic-arbitrage candidates of length
// 2..K via a negative-cycle search on edge weights w = -ln(rate * gamma).
//
// METHODOLOGICAL INVARIANT (paper/30-methodology.md §3): this file is a cheap
// DETECTOR only. The float64 log-weights are used purely to FIND candidate
// cycles; no profit figure is ever derived from them. The optimal cycle input
// and the gross profit are recomputed in EXACT big.Int integer arithmetic by
// CycleOptimum (the K-hop generalisation of arb.go's 2-pool OptimalArb), and in
// the full pipeline the candidate is then handed to the validated SimEngine for
// the ground-truth profit (see simengine/dryrun_graph.go). Detection in float,
// valuation in big.Int, verification in the EVM.
//
// graph.go is PURE: no chain access, no global state, fully deterministic and
// unit-testable (see graph_test.go). All cycle enumeration is order-stable so
// the harness output is reproducible.
package strategy

import (
	"math"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
)

// ---------------------------------------------------------------------------
// Graph data structures.
// ---------------------------------------------------------------------------

// Edge is one directed, priced hop through a single pool: swap TokenIn for
// TokenOut on pool Pool. ReserveIn/ReserveOut are the pool's reserves of the in
// and out tokens (V2). Gamma is the pool's fee factor. Weight is the detector
// weight w = -ln((ReserveOut/ReserveIn) * gamma); a negative-sum cycle of these
// weights is a candidate arbitrage. For V3 edges ReserveIn/ReserveOut are not
// the closed-form sizing inputs (L is tick-piecewise) — they carry the
// spot-equivalent virtual reserves used ONLY for detection; sizing/valuation of
// any cycle containing a V3 hop is deferred to the EVM oracle.
type Edge struct {
	Pool     common.Address
	DEX      string
	FeeTier  uint32 // V3 fee tier in hundredths of a bip (e.g. 2500 = 0.25%); 0 for V2
	Gamma    Gamma  // multiplicative kept factor (1 - fee)
	TokenIn  common.Address
	TokenOut common.Address
	IsV3     bool

	ReserveIn  *big.Int // reserve of TokenIn  (V2: real; V3: spot-equivalent virtual)
	ReserveOut *big.Int // reserve of TokenOut (V2: real; V3: spot-equivalent virtual)

	// SqrtPriceX96 carries the raw V3 price for record-keeping (0 for V2).
	SqrtPriceX96 *big.Int

	Weight float64 // -ln(rate * gamma); finite only when reserves are positive
}

// rate returns the marginal (zero-size) exchange rate ReserveOut/ReserveIn as a
// float64. Detection only.
func (e Edge) rate() float64 {
	if e.ReserveIn == nil || e.ReserveOut == nil || e.ReserveIn.Sign() <= 0 || e.ReserveOut.Sign() <= 0 {
		return 0
	}
	ri, _ := new(big.Float).SetInt(e.ReserveIn).Float64()
	ro, _ := new(big.Float).SetInt(e.ReserveOut).Float64()
	if ri == 0 {
		return 0
	}
	return ro / ri
}

// computeWeight fills Weight = -ln(rate * gamma). A non-positive rate yields
// +Inf (an edge that can never be part of a negative cycle).
func (e *Edge) computeWeight() {
	r := e.rate()
	g := 1.0
	if e.Gamma.Num != nil && e.Gamma.Den != nil && e.Gamma.Den.Sign() > 0 {
		gn, _ := new(big.Float).SetInt(e.Gamma.Num).Float64()
		gd, _ := new(big.Float).SetInt(e.Gamma.Den).Float64()
		if gd != 0 {
			g = gn / gd
		}
	}
	eff := r * g
	if eff <= 0 {
		e.Weight = math.Inf(1)
		return
	}
	e.Weight = -math.Log(eff)
}

// Graph is a directed token multigraph: nodes are token addresses, edges are
// priced directed hops. adjacency maps a source token to its outgoing edges.
type Graph struct {
	nodes map[common.Address]bool
	adj   map[common.Address][]Edge
}

// NewGraph returns an empty graph.
func NewGraph() *Graph {
	return &Graph{
		nodes: make(map[common.Address]bool),
		adj:   make(map[common.Address][]Edge),
	}
}

// AddEdge appends a directed edge, registering both endpoints as nodes and
// computing the detector weight. Edges with non-positive reserves are still
// stored (with +Inf weight) so the topology is complete; they simply never
// participate in a negative cycle.
func (g *Graph) AddEdge(e Edge) {
	if e.ReserveIn != nil {
		e.ReserveIn = new(big.Int).Set(e.ReserveIn)
	}
	if e.ReserveOut != nil {
		e.ReserveOut = new(big.Int).Set(e.ReserveOut)
	}
	e.computeWeight()
	g.nodes[e.TokenIn] = true
	g.nodes[e.TokenOut] = true
	g.adj[e.TokenIn] = append(g.adj[e.TokenIn], e)
}

// AddV2Pool adds BOTH directed edges of a V2 pool given its decoded reserves.
// tokenIn0/reserveIn0 are token0 and its reserve; token1/reserve1 the other.
// Two edges are produced: (token0->token1) and (token1->token0).
func (g *Graph) AddV2Pool(pool common.Address, dex string, gamma Gamma, token0, token1 common.Address, reserve0, reserve1 *big.Int) {
	g.AddEdge(Edge{
		Pool: pool, DEX: dex, Gamma: gamma,
		TokenIn: token0, TokenOut: token1,
		ReserveIn: reserve0, ReserveOut: reserve1,
	})
	g.AddEdge(Edge{
		Pool: pool, DEX: dex, Gamma: gamma,
		TokenIn: token1, TokenOut: token0,
		ReserveIn: reserve1, ReserveOut: reserve0,
	})
}

// Nodes returns the graph's tokens in a deterministic (sorted) order.
func (g *Graph) Nodes() []common.Address {
	out := make([]common.Address, 0, len(g.nodes))
	for n := range g.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hex() < out[j].Hex() })
	return out
}

// EdgeCount returns the number of directed edges.
func (g *Graph) EdgeCount() int {
	n := 0
	for _, es := range g.adj {
		n += len(es)
	}
	return n
}

// ---------------------------------------------------------------------------
// Negative-cycle enumeration.
// ---------------------------------------------------------------------------

// Cycle is a candidate arbitrage: an ordered list of edges forming a closed
// loop (Edges[0].TokenIn == Edges[len-1].TokenOut == Start). LogGain is the
// detector quantity -sum(weights) = ln(product(rate*gamma)); LogGain > 0 means
// the float spot product exceeds 1 (a candidate). It is NOT a profit.
type Cycle struct {
	Start   common.Address
	Edges   []Edge
	LogGain float64
}

// Tokens returns the ordered token path Start -> ... -> Start.
func (c Cycle) Tokens() []common.Address {
	out := make([]common.Address, 0, len(c.Edges)+1)
	if len(c.Edges) == 0 {
		return out
	}
	out = append(out, c.Edges[0].TokenIn)
	for _, e := range c.Edges {
		out = append(out, e.TokenOut)
	}
	return out
}

// NegativeCycles enumerates simple (no repeated intermediate token) cyclic
// candidates of length 2..maxLen that start and end at src and have positive
// LogGain (negative weight sum). It is a depth-bounded DFS that prunes whenever
// the running weight sum cannot be beaten — a deterministic, exhaustive search
// over the small node-bounded watch graph (BSC hub set), which is the practical
// analogue of Bellman-Ford/SPFA predecessor-walk negative-cycle extraction for
// graphs of this size.
//
// Cycles are de-duplicated by their pool/direction signature and returned in a
// stable order (most negative weight first), so the harness output is
// reproducible. A token may appear at most once as an intermediate (the start
// token bookends the loop); this excludes degenerate A->B->A->B padding while
// still admitting every distinct triangular/quad cycle.
func (g *Graph) NegativeCycles(src common.Address, maxLen int) []Cycle {
	if maxLen < 2 {
		maxLen = 2
	}
	var out []Cycle
	seenSig := make(map[string]bool)

	visited := make(map[common.Address]bool)
	visited[src] = true
	path := make([]Edge, 0, maxLen)

	var dfs func(cur common.Address, depth int, weightSum float64)
	dfs = func(cur common.Address, depth int, weightSum float64) {
		for _, e := range g.adj[cur] {
			if math.IsInf(e.Weight, 1) {
				continue
			}
			next := e.TokenOut
			nextWeight := weightSum + e.Weight

			if next == src {
				// Closing the loop. Require length >= 2 and a strictly negative
				// weight sum (positive LogGain).
				if depth+1 >= 2 && nextWeight < -1e-12 {
					path = append(path, e)
					sig := cycleSig(path)
					if !seenSig[sig] {
						seenSig[sig] = true
						edges := make([]Edge, len(path))
						copy(edges, path)
						out = append(out, Cycle{Start: src, Edges: edges, LogGain: -nextWeight})
					}
					path = path[:len(path)-1]
				}
				continue
			}

			if visited[next] || depth+1 >= maxLen {
				// Cannot revisit an intermediate, and cannot extend past maxLen
				// hops without closing.
				continue
			}

			visited[next] = true
			path = append(path, e)
			dfs(next, depth+1, nextWeight)
			path = path[:len(path)-1]
			delete(visited, next)
		}
	}

	dfs(src, 0, 0)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LogGain != out[j].LogGain {
			return out[i].LogGain > out[j].LogGain // most profitable detector signal first
		}
		return len(out[i].Edges) < len(out[j].Edges)
	})
	return out
}

// cycleSig is a stable signature of a cycle by its (pool, tokenIn) hops, so the
// same loop discovered along different DFS branches is emitted once.
func cycleSig(edges []Edge) string {
	b := make([]byte, 0, len(edges)*(20+20))
	for _, e := range edges {
		b = append(b, e.Pool.Bytes()...)
		b = append(b, e.TokenIn.Bytes()...)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Exact K-hop cycle sizing (big.Int) — generalises OptimalArb to a path.
// ---------------------------------------------------------------------------
//
// Each V2 hop maps an input x of TokenIn to an output of TokenOut by the
// linear-fractional (Mobius) map
//
//	f(x) = (gamma.Num * Rout * x) / (gamma.Den * Rin + gamma.Num * x)
//	     = (a * x) / (b + c * x)
//
// with a = gamma.Num * Rout, b = gamma.Den * Rin, c = gamma.Num. Composing k
// such maps is itself linear-fractional; composing g(y)=(A*y)/(B+C*y) after
// f(x)=(a*x)/(b+c*x) gives
//
//	(g∘f)(x) = (A*a*x) / (B*b + (B*c + C*a)*x).
//
// So the whole cycle reduces to a single (a,b,c). The output back in the start
// token is out(x) = a*x/(b+c*x); profit p(x) = out(x) - x is maximised at
//
//	x* = (sqrt(a*b) - b) / c        (real, integer floor-sqrt)
//
// and an arb exists iff a > b (marginal output rate a/b > 1). This is the exact
// generalisation of arb.go's 2-pool OptimalArb (which is the k=2 case). Because
// of integer flooring the closed-form x* is refined by a tiny local search to
// land on the true integer-arithmetic maximum.

// mobius is a linear-fractional map y = (A*x) / (B + C*x) in big.Int.
type mobius struct {
	A, B, C *big.Int
}

// v2Mobius builds the per-hop Mobius map for a V2 swap of TokenIn->TokenOut with
// reserves (rin, rout) and fee gamma.
func v2Mobius(rin, rout *big.Int, g Gamma) mobius {
	return mobius{
		A: new(big.Int).Mul(g.Num, rout), // a = Num * Rout
		B: new(big.Int).Mul(g.Den, rin),  // b = Den * Rin
		C: new(big.Int).Set(g.Num),       // c = Num
	}
}

// compose returns g∘f (apply f first, then g): the map taking f's input to g's
// output. (g∘f)(x) = (A_g*A_f * x) / (B_g*B_f + (B_g*C_f + C_g*A_f) * x).
func (g mobius) compose(f mobius) mobius {
	A := new(big.Int).Mul(g.A, f.A)
	B := new(big.Int).Mul(g.B, f.B)
	c1 := new(big.Int).Mul(g.B, f.C)
	c2 := new(big.Int).Mul(g.C, f.A)
	C := new(big.Int).Add(c1, c2)
	return mobius{A: A, B: B, C: C}
}

// apply evaluates y = floor((A*x)/(B + C*x)).
func (m mobius) apply(x *big.Int) *big.Int {
	if x == nil || x.Sign() <= 0 {
		return big.NewInt(0)
	}
	num := new(big.Int).Mul(m.A, x)
	den := new(big.Int).Add(m.B, new(big.Int).Mul(m.C, x))
	if den.Sign() <= 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Quo(num, den)
}

// CycleOptimum computes, in EXACT big.Int arithmetic, the profit-maximising
// input and the realised gross profit for an ALL-V2 cycle (the closed-form
// path). For any cycle containing a V3/non-V2 hop it returns (0,0): such cycles
// have no closed-form optimum (L is tick-piecewise) and MUST be sized by the
// EVM oracle. The result is in units of the cycle's start token.
//
// grossProfit is computed by actually walking the hops at the chosen input with
// the per-hop floor division, so it equals the on-chain V2 result exactly (same
// rounding as GetAmountOut). Returns (0,0) when no profitable input exists.
func CycleOptimum(c Cycle) (optimalAmountIn, grossProfit *big.Int) {
	zero := func() (*big.Int, *big.Int) { return big.NewInt(0), big.NewInt(0) }
	if len(c.Edges) < 2 {
		return zero()
	}
	for _, e := range c.Edges {
		if e.IsV3 {
			return zero() // defer to EVM oracle
		}
		if e.ReserveIn == nil || e.ReserveOut == nil || e.ReserveIn.Sign() <= 0 || e.ReserveOut.Sign() <= 0 {
			return zero()
		}
	}

	// Compose the per-hop Mobius maps along the cycle: total = f_k ∘ ... ∘ f_1.
	total := v2Mobius(c.Edges[0].ReserveIn, c.Edges[0].ReserveOut, c.Edges[0].Gamma)
	for i := 1; i < len(c.Edges); i++ {
		hop := v2Mobius(c.Edges[i].ReserveIn, c.Edges[i].ReserveOut, c.Edges[i].Gamma)
		total = hop.compose(total)
	}

	// No-arb gate: marginal output rate a/b <= 1 means no profit at any size.
	if total.A.Cmp(total.B) <= 0 {
		return zero()
	}

	// Closed form: x* = (sqrt(a*b) - b) / c.
	ab := new(big.Int).Mul(total.A, total.B)
	root := new(big.Int).Sqrt(ab) // floor sqrt
	numer := new(big.Int).Sub(root, total.B)
	if numer.Sign() <= 0 {
		return zero()
	}
	if total.C.Sign() <= 0 {
		return zero()
	}
	x0 := new(big.Int).Quo(numer, total.C)
	if x0.Sign() <= 0 {
		x0 = big.NewInt(1)
	}

	// Realised gross profit walks the actual hops (exact V2 rounding), and a
	// tiny local search around the floored closed form pins the true integer
	// maximum (flooring can shift the optimum by a few wei).
	profitAt := func(x *big.Int) *big.Int {
		if x == nil || x.Sign() <= 0 {
			return big.NewInt(-1) // worse than any non-negative profit
		}
		cur := new(big.Int).Set(x)
		for _, e := range c.Edges {
			cur = GetAmountOut(cur, e.ReserveIn, e.ReserveOut, e.Gamma)
			if cur.Sign() <= 0 {
				return big.NewInt(-1)
			}
		}
		return new(big.Int).Sub(cur, x)
	}

	bestX := new(big.Int).Set(x0)
	bestP := profitAt(bestX)
	// Hill-climb a bounded neighbourhood (closed form is exact up to flooring).
	for _, d := range []int64{-2, -1, 1, 2} {
		cand := new(big.Int).Add(x0, big.NewInt(d))
		p := profitAt(cand)
		if p.Cmp(bestP) > 0 {
			bestP = p
			bestX = cand
		}
	}

	if bestP.Sign() <= 0 {
		return zero()
	}
	return bestX, bestP
}

// EvaluateCycle sizes an all-V2 cycle and subtracts the supplied economic costs
// to produce a net would-be Evaluation, reusing the exact same EvalParams /
// Evaluation types as the 2-pool path so the harness reporting is uniform. For
// V3-containing cycles the gross is (0,0) here and the caller must use the EVM
// oracle (simengine) instead.
func EvaluateCycle(c Cycle, p EvalParams) Evaluation {
	optIn, gross := CycleOptimum(c)
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
