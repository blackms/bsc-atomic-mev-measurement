// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_graph.go is the IMPROVED-model backtest path (paper/40-models.md),
// selectable with SIMENGINE_DRYRUN=graph. It keeps the original "backtest" mode
// (the 2-pool x*y=k go/no-go in dryrun.go) fully intact and adds a richer
// pipeline:
//
//	STAGE A (cheap analytic detection): on each imported block, after re-executing
//	the block on its parent state, build the multi-DEX token graph from the
//	POST-BLOCK pool prices (V2 reserves at slot 8, V3 spot at slot0/sqrtPriceX96)
//	and enumerate negative cycles of length 2..K from WBNB via
//	strategy.NegativeCycles.
//
//	STAGE B (exact sizing / valuation): for every candidate cycle, size it and
//	value it in EXACT big.Int arithmetic via strategy.CycleOptimum (the K-hop
//	generalisation of the validated 2-pool closed form). The economic gate
//	subtracts measured gas, builder bid, and margin. A 'graph OPP' line is logged
//	per profitable cycle, and a candidate funnel (Stage-A count -> gross>0 ->
//	net>0) is tallied for the RQ3 analytic-over-count metric.
//
// METHODOLOGICAL INVARIANT / WHERE THE SIMENGINE-VERIFY STEP PLUGS IN: Stage A
// is float-weighted detection only; the profit number here comes from the EXACT
// integer sizer for ALL-V2 cycles. For cycles containing a V3 hop (or shared
// pool / split), CycleOptimum deliberately returns (0,0) because there is no
// closed-form optimum (V3 liquidity is tick-piecewise). Those — and, for the
// camera-ready, EVERY candidate — must be sized/valued by the validated
// SimEngine: construct the concrete backrun swap sequence and append it as a
// synthetic tx to the block on a state.Copy() (exactly as backtestBlock already
// does for the block itself), then read the executor's start-token balance delta
// as the ground-truth gross and the receipt GasUsed as the exact gas. That hook
// is marked TODO(simengine-verify) in verifyCycleEVM below; the analytic sizer
// stands in for it for the all-V2 case (where the two agree by construction —
// the SimEngine self-test already validates V2 execution to receipt-exact).
//
// IT NEVER SUBMITS ANYTHING. Read-only (state.Copy() only), every unit of work
// wrapped in defer/recover.
package simengine

import (
	"math/big"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/strategy"
)

// graphMaxCycleLen is the maximum cycle length K enumerated by Stage A. Default
// 4 (triangular + quad cycles, matching the design); overridable via env in a
// follow-up if needed. Kept conservative so per-block Stage-A cost is bounded.
const graphMaxCycleLen = 4

// graphTopCandidates caps how many top Stage-A candidates per block are taken to
// Stage B, to bound per-block work (the design's "cap top-N"). Cycles are sorted
// most-profitable-detector-signal first by NegativeCycles, so this keeps the
// best ones.
const graphTopCandidates = 64

// runGraphBacktest subscribes to chain heads and runs the IMPROVED pipeline on
// every imported block. Read-only, crash-safe. Mirrors runBacktest's wiring.
func (r *dryRunner) runGraphBacktest() {
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	// Emit the exact watch set used, for auditability (design requirement).
	log.Info("SimEngine dry-run GRAPH (improved) loop started",
		"pools", len(strategy.ExtendedPools()), "maxCycleLen", graphMaxCycleLen)
	log.Info("SimEngine graph watch-set audit\n" + strategy.RegistryAudit())

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run graph loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run graph recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.graphBacktestBlock(head)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run graph loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// graphBacktestBlock re-executes one block on its parent state, builds the
// post-block graph, enumerates negative cycles, sizes/values each, and logs
// profitable opportunities. Read-only.
func (r *dryRunner) graphBacktestBlock(head *types.Header) {
	number := head.Number.Uint64()

	block := r.bc.GetBlockByHash(head.Hash())
	if block == nil {
		return
	}
	parent := r.bc.GetHeaderByHash(head.ParentHash)
	if parent == nil {
		return
	}
	parentState, err := r.bc.StateAt(parent.Root)
	if err != nil {
		// Expected during catch-up if the parent state is pruned — skip silently.
		return
	}

	// Re-execute the block onto a COPY of the parent state. The mutated copy IS
	// the post-block state we read pool prices from (slot 8 / slot0), so the
	// graph reflects reserves AFTER all real txs — the conservative residual
	// (threat T3: we evaluate the edge left over after the whole block).
	post := parentState.Copy()
	res, err := r.e.SimulateOnState(post, r.bc, head, block.Transactions(), nil)
	if err != nil {
		return
	}
	_ = res // logs not needed in graph mode; price comes from post-state storage.

	n := r.blocks.Add(1)

	r.graphEvaluateBlock(number, post, head, block)

	if r.cfg.TallyEvery > 0 && n%r.cfg.TallyEvery == 0 {
		r.logGraphTally(n)
	}
}

// graphEvaluateBlock builds the graph from the post-block state and runs the
// Stage-A -> Stage-B pipeline. The cycle start/end token is WBNB (the source),
// so gross/net are in WBNB-wei and directly comparable to wei-denominated gas.
func (r *dryRunner) graphEvaluateBlock(number uint64, post *state.StateDB, head *types.Header, block *types.Block) {
	g := strategy.BuildGraph(post)
	if g.EdgeCount() == 0 {
		return
	}

	cycles := g.NegativeCycles(strategy.WBNB, graphMaxCycleLen)
	if len(cycles) == 0 {
		return
	}
	if len(cycles) > graphTopCandidates {
		cycles = cycles[:graphTopCandidates]
	}
	r.gCandidates.Add(uint64(len(cycles)))

	params := r.cfg.evalParams()

	for _, c := range cycles {
		// STAGE B: exact valuation. For all-V2 cycles this is the closed-form
		// K-hop sizer; for V3-containing cycles it returns (0,0) and we route to
		// the EVM oracle hook (currently a documented stub — see verifyCycleEVM).
		eval := strategy.EvaluateCycle(c, params)

		// V3-containing (or otherwise non-closed-form) cycle: analytic sizing is
		// intentionally empty. Try the EVM oracle hook.
		if eval.GrossProfit.Sign() == 0 && cycleHasV3(c) {
			ev2, ok := r.verifyCycleEVM(post, head, block, c, params)
			if ok {
				eval = ev2
			}
		}

		if eval.GrossProfit.Sign() > 0 {
			r.gGrossPositive.Add(1)
		}
		if !eval.Profitable {
			continue
		}
		r.gNetPositive.Add(1)
		r.opps.Add(1)
		r.addProfit(eval.NetProfit)

		log.Info("graph OPP",
			"block", number,
			"hops", len(c.Edges),
			"path", cyclePathString(c),
			"dexmix", cycleDexMix(c),
			"amountInWei", eval.OptimalAmountIn.String(),
			"grossWei", eval.GrossProfit.String(),
			"gasWei", eval.GasCost.String(),
			"bidWei", eval.BuilderBid.String(),
			"netWei", eval.NetProfit.String(),
		)
	}
}

// verifyCycleEVM is the v4 SimEngine ground-truth valuation hook for a candidate
// cycle (the replacement for the former STUB). It values the cycle WITHOUT a
// custom on-chain executor by QUOTER-CHAINING on the supplied state:
//
//  1. Build a read-only QuoteFn bound to SimEngine.EthCall against a COPY of the
//     post-block state (strictly read-only; the copy is discarded).
//  2. strategy.ValueCycle sizes the cycle by golden-section search and prices each
//     hop on the exact state — V2 hops via closed-form GetAmountOut, V3 hops via an
//     in-process call to the deployed PancakeSwap V3 QuoterV2
//     (quoteExactInputSingle), which runs the real tick math and returns amountOut.
//  3. Gas is charged per-cycle (CycleGasUnits * gasPrice), V3-weighted, replacing
//     the flat 250k assumption; net = gross - gas - bid - margin.
//
// This handles V3 tick-crossing, fee tiers, reverts, and rounding exactly via the
// real quoter contract on the SAME state the detector trusts. block is unused
// (the quoter chain needs only the state + header) but kept in the signature for
// call-site symmetry. ok=false when no profitable input exists.
func (r *dryRunner) verifyCycleEVM(post *state.StateDB, head *types.Header, block *types.Block, c strategy.Cycle, params strategy.EvalParams) (strategy.Evaluation, bool) {
	_ = block
	stateCopy := post.Copy()
	quote := func(to common.Address, input []byte) ([]byte, error) {
		return r.e.EthCall(stateCopy, r.bc, head, to, input, 0)
	}
	eval := strategy.ValueCycle(quote, c, r.cfg.GasPriceWei, params)
	if eval.GrossProfit.Sign() <= 0 {
		return strategy.Evaluation{}, false
	}
	return eval, true
}

// cycleHasV3 reports whether any hop of the cycle is a V3 (non-closed-form) hop.
func cycleHasV3(c strategy.Cycle) bool {
	for _, e := range c.Edges {
		if e.IsV3 {
			return true
		}
	}
	return false
}

// cyclePathString renders the token path as short addresses joined by '->'.
func cyclePathString(c strategy.Cycle) string {
	toks := c.Tokens()
	s := ""
	for i, t := range toks {
		if i > 0 {
			s += "->"
		}
		s += shortAddr(t)
	}
	return s
}

// cycleDexMix renders the sorted, comma-joined set of DEX identifiers a cycle
// traverses (for the cross-DEX / cross-version breakdown metric).
func cycleDexMix(c strategy.Cycle) string {
	seen := make(map[string]bool)
	for _, e := range c.Edges {
		if e.DEX != "" {
			seen[e.DEX] = true
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	s := ""
	for i, d := range out {
		if i > 0 {
			s += "+"
		}
		s += d
	}
	return s
}

// logGraphTally emits the IMPROVED-model candidate funnel + profit summary.
func (r *dryRunner) logGraphTally(processed uint64) {
	cand := r.gCandidates.Load()
	gross := r.gGrossPositive.Load()
	net := r.gNetPositive.Load()

	// RQ3 over-count ratio: Stage-A candidates per Stage-B net-positive (avoid /0).
	overcount := "n/a"
	if net > 0 {
		overcount = bigRatio(cand, net)
	}

	log.Info("SimEngine dry-run graph tally",
		"processedBlocks", processed,
		"stageA_candidates", cand,
		"stageB_grossPositive", gross,
		"netPositive", net,
		"overcountA_per_net", overcount,
		"opportunities", r.opps.Load(),
		"totalWouldBeProfitWei", r.totalWei.Load().String(),
		"ts", time.Now().Format(time.RFC3339),
	)
}

// bigRatio returns a/b as a decimal string with 2 places, for the over-count
// metric. b is assumed > 0.
func bigRatio(a, b uint64) string {
	num := new(big.Int).Mul(new(big.Int).SetUint64(a), big.NewInt(100))
	q := new(big.Int).Quo(num, new(big.Int).SetUint64(b))
	whole := new(big.Int).Quo(q, big.NewInt(100))
	frac := new(big.Int).Mod(q, big.NewInt(100))
	fs := frac.String()
	if len(fs) < 2 {
		fs = "0" + fs
	}
	return whole.String() + "." + fs
}
