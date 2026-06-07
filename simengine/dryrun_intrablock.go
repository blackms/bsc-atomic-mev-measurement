// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_intrablock.go is the v3 PER-SWAP INTRA-BLOCK backrun detector,
// selectable with SIMENGINE_DRYRUN=intrablock. It is the answer to the v2
// limitation of post-block evaluation: post-block evaluation
// (modes "backtest" and "graph") MISSES backrun MEV because competing arbers
// re-align pool prices WITHIN the block. The cross-DEX gap that a backrun would
// capture exists only in the TRANSIENT state right AFTER a victim swap and
// BEFORE the next transaction. By the end of the block it is gone.
//
// This mode re-executes each imported block transaction-by-transaction via the
// validated SimEngine.ApplyOnStateHooked path. After every successfully-applied
// (non-system) tx whose receipt logs include a watched-pool Swap/Sync, it builds
// the multi-DEX token graph from the LIVE INTERMEDIATE state — the exact pool
// reserves immediately after that swap — enumerates negative cycles from WBNB,
// sizes/values them in exact big.Int arithmetic, and reports every net-positive
// opportunity. The same candidate funnel as graph mode is maintained, but over
// transient states instead of the post-block residual, so the two modes'
// counts are directly comparable (intrablock >= graph by construction is the
// expected, paper-relevant finding).
//
// IT NEVER SUBMITS ANYTHING. Strictly read-only (state.Copy() only, never
// commits), every block wrapped in defer/recover so a bug can never panic or
// stall the node, and a complete no-op unless SIMENGINE_DRYRUN=intrablock.
package simengine

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/strategy"
)

// runIntrablockBacktest subscribes to chain heads and runs the v3 per-swap
// intra-block pipeline on every imported block. Read-only, crash-safe. Mirrors
// runGraphBacktest's wiring; the difference is entirely in how state is sampled
// (per-swap transient vs post-block residual).
func (r *dryRunner) runIntrablockBacktest() {
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	log.Info("SimEngine dry-run INTRA-BLOCK (v3 per-swap) loop started",
		"pools", len(strategy.ExtendedPools()), "maxCycleLen", intrablockMaxCycleLen)
	log.Info("SimEngine intrablock watch-set audit\n" + strategy.RegistryAudit())

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run intrablock loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run intrablock recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.intrablockBlock(head)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run intrablock loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// intrablockMaxCycleLen is the maximum cycle length K enumerated per transient
// state (triangular + quad cycles), matching graph mode's graphMaxCycleLen.
const intrablockMaxCycleLen = 4

// intrablockTopCandidates caps how many top Stage-A candidates per transient
// state are taken to Stage B, bounding per-swap work. Mirrors graph mode.
const intrablockTopCandidates = 64

// intrablockBlock re-executes one block transaction-by-transaction on its parent
// state via the hooked SimEngine path and, after each watched-pool swap,
// evaluates the transient-state backrun opportunity. Read-only.
func (r *dryRunner) intrablockBlock(head *types.Header) {
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

	params := r.cfg.evalParams()

	// onTx fires after each successfully-applied non-system tx with the LIVE
	// intermediate state. We only act on txs that touched a watched pool, and we
	// treat statedb strictly as read-only (BuildGraph only reads storage).
	onTx := func(i int, tx *types.Transaction, receipt *types.Receipt, statedb *state.StateDB) {
		if receipt == nil || len(receipt.Logs) == 0 {
			return
		}
		// Trigger on a swap touching ANY pool in the verified extended watch set
		// (Pancake V2 + Biswap V2 + PancakeSwap V3) — the same set the graph build
		// and evaluation use. V3 pools are matched via the V3 Swap topic; V2-style
		// pools via the V2 Swap/Sync topics (see strategy.IsExtendedWatchedSwapLog).
		touched := strategy.ExtendedPairsTouched(receipt.Logs)
		if len(touched) == 0 {
			return
		}
		r.ibWatchedSwaps.Add(1)
		// Evaluate the transient state RIGHT AFTER this victim swap. Crash-safe per
		// swap so one bad evaluation cannot abort the rest of the block replay.
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Warn("SimEngine intrablock per-swap recovered from panic",
						"block", number, "txIndex", i, "tx", tx.Hash(), "panic", rec)
				}
			}()
			r.intrablockEvaluateSwap(number, i, tx.Hash(), head, statedb, params)
		}()
	}

	// Re-execute the WHOLE block tx-by-tx on a COPY of the parent state through the
	// validated hooked loop. We do not need the aggregate SimResult here; the work
	// happens inside onTx on the transient states.
	if _, err := r.e.ApplyOnStateHooked(parentState.Copy(), r.bc, head, block.Transactions(), nil, onTx); err != nil {
		return
	}

	n := r.blocks.Add(1)
	if r.cfg.TallyEvery > 0 && n%r.cfg.TallyEvery == 0 {
		r.logIntrablockTally(n)
	}
}

// intrablockEvaluateSwap builds the graph from the supplied transient state,
// enumerates negative cycles from WBNB, sizes/values each in exact big.Int
// arithmetic, and logs every net-positive opportunity. statedb is the live
// intermediate state right after the victim swap (victimTx). It is read-only.
func (r *dryRunner) intrablockEvaluateSwap(number uint64, txIndex int, victimTx common.Hash, head *types.Header, statedb *state.StateDB, params strategy.EvalParams) {
	cycles := intrablockFindCycles(statedb)
	if len(cycles) == 0 {
		return
	}
	if len(cycles) > intrablockTopCandidates {
		cycles = cycles[:intrablockTopCandidates]
	}
	r.ibCandidates.Add(uint64(len(cycles)))

	// V4 EVM ORACLE: value EVERY candidate cycle on the EXACT intermediate state by
	// quoter-chaining. V2 hops use the closed-form GetAmountOut from reserves; V3
	// hops are priced by calling the deployed PancakeSwap V3 QuoterV2 in-process via
	// EthCall against a single COPY of the intermediate state (strictly read-only:
	// the copy is discarded, the live state is never touched). The optimal input is
	// found by golden-section search; the V3 hops' tick math is exact.
	stateCopy := statedb.Copy()
	quote := func(to common.Address, input []byte) ([]byte, error) {
		return r.e.EthCall(stateCopy, r.bc, head, to, input, 0)
	}
	gasPriceWei := r.cfg.GasPriceWei

	// Live WBNB/USD price from the EXACT transient state, used to express each
	// gross-positive cycle's profit in USD for the distribution. Derived from the
	// Pancake V2 WBNB/USDT reserves already present in the graph build inputs:
	// price = USDT_reserve/WBNB_reserve (both 18dp), ~586. Zero if unavailable
	// (then grossUSD is reported as 0 and only the wei/breakeven stats are affected).
	wbnbUSD := liveWbnbPriceUSD(statedb)

	for _, c := range cycles {
		// STAGE B (v4): ground-truth valuation via the quoter chain. ValueCycle
		// sizes the cycle (golden-section over amountIn) and charges a per-cycle,
		// V3-weighted gas cost (CycleGasUnits * gasPrice) instead of the flat 250k.
		// For all-V2 cycles it cross-checks the exact closed form, so this never
		// regresses below the receipt-exact V2 result the self-test validates; for
		// V3-containing cycles it now produces a real gross/net (the v3 stub did not).
		eval := strategy.ValueCycle(quote, c, gasPriceWei, params)

		if eval.GrossProfit.Sign() > 0 {
			r.ibGrossPositive.Add(1)
			// Characterise EVERY gross-positive cycle (even when net<=0): grossUSD,
			// per-cycle gas units, breakeven gas price, dexMix and hop count. This is
			// the "how far below gas" distribution. O(1) memory, crash-safe.
			gasUnits := strategy.CycleGasUnits(c)
			grossUSD := weiToUSD(eval.GrossProfit, wbnbUSD)
			r.ibDist.Add(eval.GrossProfit, gasUnits, grossUSD, cycleDexMix(c), len(c.Edges))
		}
		if !eval.Profitable {
			continue
		}
		r.ibNetPositive.Add(1)
		r.opps.Add(1)
		r.addProfit(eval.NetProfit)

		log.Info("backrun OPP @tx",
			"block", number,
			"txIndex", txIndex,
			"victimTx", victimTx.Hex(),
			"hops", len(c.Edges),
			"path", intrablockCyclePath(c),
			"dexmix", cycleDexMix(c),
			"optimalInputWei", eval.OptimalAmountIn.String(),
			"grossProfitWei", eval.GrossProfit.String(),
			"netProfitWei", eval.NetProfit.String(),
			"gasCostWei", eval.GasCost.String(),
		)
	}
}

// intrablockFindCycles builds the multi-DEX graph from the given state and
// returns the candidate negative cycles from WBNB (Stage A). Factored out of
// intrablockEvaluateSwap so the per-state detection is unit-testable without a
// full block replay.
func intrablockFindCycles(statedb *state.StateDB) []strategy.Cycle {
	g := strategy.BuildGraph(statedb)
	if g.EdgeCount() == 0 {
		return nil
	}
	return g.NegativeCycles(strategy.WBNB, intrablockMaxCycleLen)
}

// intrablockCyclePath renders the cycle as a readable "DEX:name" hop sequence so
// opportunity logs identify which pools/DEXes the backrun would route through
// (the design's "cycle path (pool names/DEXes)" requirement). Falls back to a
// short pool address when a pool is not in the registry by name.
func intrablockCyclePath(c strategy.Cycle) string {
	s := ""
	for i, e := range c.Edges {
		if i > 0 {
			s += " -> "
		}
		s += e.DEX + ":" + poolLabel(e.Pool)
	}
	return s
}

// poolLabelByAddr indexes the extended watch set by pool address -> human name,
// built once. Used only for log rendering.
var poolLabelByAddr = func() map[common.Address]string {
	m := make(map[common.Address]string)
	for _, p := range strategy.AllExtendedPools() {
		if (p.Pair != common.Address{}) {
			m[p.Pair] = p.Name
		}
	}
	return m
}()

// poolLabel returns the registry name for a pool address, or a short address.
func poolLabel(pool common.Address) string {
	if name, ok := poolLabelByAddr[pool]; ok {
		return name
	}
	return shortAddr(pool)
}

// pancakeV2WbnbUsdt is the VERIFIED PancakeSwap V2 WBNB/USDT pool (token0=USDT,
// token1=WBNB, both 18dp). Its reserves give the live WBNB/USD spot used to
// express gross profit (WBNB wei) in USD for the distribution log.
var pancakeV2WbnbUsdt = common.HexToAddress("0x16b9a82891338f9bA80E2D6970FddA79D1eb0daE")

// liveWbnbPriceUSD reads the Pancake V2 WBNB/USDT reserves from the EXACT given
// (transient) state and returns the spot WBNB price in USD: USDT_reserve /
// WBNB_reserve (Reserve0=USDT, Reserve1=WBNB; both 18dp so the decimals cancel).
// Returns 0 when reserves are unavailable. This is the same pool BuildGraph reads
// for the graph, so no extra storage layout assumptions are introduced. ~586.
func liveWbnbPriceUSD(statedb *state.StateDB) float64 {
	rv := strategy.ReadReserves(statedb, pancakeV2WbnbUsdt)
	if rv.Reserve0 == nil || rv.Reserve1 == nil || rv.Reserve1.Sign() <= 0 {
		return 0
	}
	// USDT/WBNB; both 18dp so the ratio is already the USD price per WBNB.
	price, _ := new(big.Rat).SetFrac(rv.Reserve0, rv.Reserve1).Float64()
	return price
}

// weiToUSD converts a WBNB-wei amount to USD given the live WBNB/USD price:
// USD = (wei / 1e18) * priceUSD. Returns 0 for a nil/non-positive amount or a
// non-positive price.
func weiToUSD(wei *big.Int, priceUSD float64) float64 {
	if wei == nil || wei.Sign() <= 0 || priceUSD <= 0 {
		return 0
	}
	bnb, _ := new(big.Float).Quo(new(big.Float).SetInt(wei), big.NewFloat(1e18)).Float64()
	return bnb * priceUSD
}

// logIntrablockTally emits the v3 per-swap candidate funnel + profit summary AND,
// as a sibling line, the gross-positive characterisation distribution.
func (r *dryRunner) logIntrablockTally(processed uint64) {
	swaps := r.ibWatchedSwaps.Load()
	cand := r.ibCandidates.Load()
	gross := r.ibGrossPositive.Load()
	net := r.ibNetPositive.Load()

	// Stage-A candidates per net-positive (RQ3-style over-count; avoid /0).
	overcount := "n/a"
	if net > 0 {
		overcount = bigRatio(cand, net)
	}

	log.Info("intrablock tally",
		"processedBlocks", processed,
		"watchedSwaps", swaps,
		"stageA_candidates", cand,
		"stageB_grossPositive", gross,
		"netPositive", net,
		"overcountA_per_net", overcount,
		"opportunities", r.opps.Load(),
		"totalWouldBeProfitWei", r.totalWei.Load().String(),
		"ts", time.Now().Format(time.RFC3339),
	)

	r.logIntrablockDist(processed)
}

// logIntrablockDist emits the NEW 'intrablock dist' line characterising the
// gross-positive cycle population: grossUSD percentiles (p50/p90/p99/max), the
// breakeven gas price (gwei) percentiles (p50/p90/max), the gas-price sensitivity
// sweep (count + % that would be net-positive at each sweep gas price, bid=margin=0),
// and the per-dexMix / per-cycle-length breakdowns. Crash-safe and read-only.
func (r *dryRunner) logIntrablockDist(processed uint64) {
	s := r.ibDist.Snapshot()
	log.Info("intrablock dist",
		"processedBlocks", processed,
		"grossPosSamples", s.Count,
		"grossUSD_p50", s.GrossUSDp50,
		"grossUSD_p90", s.GrossUSDp90,
		"grossUSD_p99", s.GrossUSDp99,
		"grossUSD_max", s.GrossUSDMax,
		"breakevenGwei_p50", s.BreakevenGweiP50,
		"breakevenGwei_p90", s.BreakevenGweiP90,
		"breakevenGwei_max", s.BreakevenGweiMax,
		"gasSweep_netPos", s.SweepString(),
		"byDexMix", s.DexMixString(),
		"byCycleLen", s.LenString(),
		"ts", time.Now().Format(time.RFC3339),
	)
}
