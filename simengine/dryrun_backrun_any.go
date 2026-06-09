// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_backrun_any.go is the ANY-POOL LONG-TAIL BACKRUN detector, selectable
// with SIMENGINE_DRYRUN=backrun-any. It is the single-transaction counterpart to
// sandwich-any (dryrun_sandwich_any.go): for EVERY decoded V2/V3 victim swap on
// ANY pool it values the optimal trailing BACKRUN that exploits the price the
// victim ALREADY moved.
//
// ROUND-1 REDESIGN (architectural): the original backrunProfitAny modelled a
// backrun as a numeraire round-trip (numeraire->other->numeraire) on the SAME
// pool. Under constant-product fee dynamics (gamma < 1) that is ALWAYS a
// structural loss — the victim's price move is already baked into the post-victim
// reserves, so a round trip at those reserves eats both legs of fees with NO
// arbitrage capture, and the detector fired zero times. A TRUE backrun is a
// CROSS-POOL cycle: the victim moves the price on pool P; the arb exploits the
// resulting gap via a DIFFERENT pool P' that shares a token. We now value the
// backrun by REUSING the existing cyclic-arbitrage detector (strategy.
// NegativeCycles / CycleOptimum / ValueCycle): on the POST-VICTIM state we build
// a graph seeded by the pools the victim TOUCHED plus the verified hub set, then
// enumerate negative cycles STARTING at the victim's INPUT token and size each
// (closed-form for all-V2 cycles; EVM oracle for V3-containing cycles), applying
// the gas-only BackrunNet gate.
//
// It walks the SAME any-pool victim universe as sandwich-any (it decodes every
// V2/V3 Swap log identically via decodeAnyVictim and shares the numeraire/fund/
// pool-meta machinery), so its victimsSeen matches sandwich-any's on the same
// blocks. Only the valuation engine changed; the eligibility funnel is identical.
//
// Strictly read-only (state.Copy() only), every block and every per-victim
// evaluation wrapped in defer/recover, and a complete no-op unless the mode is set.
package simengine

import (
	"math/big"
	"os"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/strategy"
)

// backrunAnyProbeLogCap bounds detailed per-victim probe lines (mirrors
// sandwich-any's cap of 15).
const backrunAnyProbeLogCap uint64 = 15

// backrunAnyMaxCycleLen bounds the negative-cycle search length for the any-pool
// backrun (triangular + quad cycles), matching the hub graph mode's K.
const backrunAnyMaxCycleLen = 4

// SANITY OUTLIER FILTER (catch-it-don't-bake-it; mirrors the prior sandwich
// units-bug catch). A decimal-mismatch or degenerate-pool math glitch in the
// closed-form V2 cycle optimiser can emit a single grossBNBWei of ~6e30 (~6e12
// BNB) and a grossUSD of ~3.6e15, polluting the aggregate by 12+ orders of
// magnitude. The cap is *forensic* — way above any realistic single-tx
// independent-searcher backrun ($100k default, 1000 BNB default) — so a real
// opp can never trip it; only math-glitch outliers do. Overridable by env:
//
//	SIMENGINE_BACKRUN_SANITY_USD_CAP     (default 100000  USD)
//	SIMENGINE_BACKRUN_SANITY_NET_BNB_CAP (default 1000    BNB; threshold = cap*1e18 wei)
var (
	sanityCapUSD    = parseSanityCapUSD()
	sanityCapNetWei = parseSanityCapNetWei()
)

// parseSanityCapUSD parses SIMENGINE_BACKRUN_SANITY_USD_CAP (default 1e5).
// Invalid / non-positive values fall back to the default.
func parseSanityCapUSD() float64 {
	const def = 100_000.0
	if v := os.Getenv("SIMENGINE_BACKRUN_SANITY_USD_CAP"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return def
}

// parseSanityCapNetWei parses SIMENGINE_BACKRUN_SANITY_NET_BNB_CAP (default
// 1000 BNB) and returns the wei threshold (cap*1e18). Invalid / non-positive
// values fall back to the default.
func parseSanityCapNetWei() *big.Int {
	cap := 1000.0
	if v := os.Getenv("SIMENGINE_BACKRUN_SANITY_NET_BNB_CAP"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			cap = f
		}
	}
	// cap (BNB) * 1e18 wei/BNB, computed via big.Float to preserve precision.
	wei := new(big.Float).Mul(big.NewFloat(cap), new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))
	out, _ := wei.Int(nil)
	if out == nil || out.Sign() <= 0 {
		out = new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	}
	return out
}

// sanityRejectBackrunOpp is the pure predicate used by the detector to decide
// whether a backrun opportunity is a math-glitch outlier (decimal mismatch /
// degenerate-pool math). It returns true iff the grossUSD exceeds capUSD OR
// the marginal net (wei) exceeds capNetWei. Nil inputs are treated as not-set
// (the cap on that side is skipped). Pulled out as a pure helper so the cap
// predicate is unit-testable in isolation from the full detector.
func sanityRejectBackrunOpp(grossUSD float64, marginalNet *big.Int, capUSD float64, capNetWei *big.Int) bool {
	if capUSD > 0 && grossUSD > capUSD {
		return true
	}
	if capNetWei != nil && marginalNet != nil && marginalNet.Cmp(capNetWei) > 0 {
		return true
	}
	return false
}

// runBackrunAnyBacktest subscribes to chain heads and runs the any-pool
// ground-truth backrun valuator on every imported block. Read-only, crash-safe.
func (r *dryRunner) runBackrunAnyBacktest() {
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	r.swCfg = defaultSandwichConfig()

	log.Info("SimEngine dry-run BACKRUN-ANY (cross-pool cycle backrun) loop started",
		"minVictimUSD", r.swCfg.minVictimUSD,
		"minInputWeiFallback", minVictimInputHeuristicWei.String(),
		"maxCycleLen", backrunAnyMaxCycleLen,
		"hubPools", len(strategy.ExtendedPools()))

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run backrun-any loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run backrun-any recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.backrunAnyBlock(head)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run backrun-any loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// backrunAnyBlock replays one block tx-by-tx on its parent state. For each victim
// Swap log on ANY pool it (a) records the pool the victim touched (so the
// cross-pool graph can be seeded by the union of all touched pools) and (b)
// evaluates the cross-pool backrun cycle on the EXACT post-victim state. Read-only.
func (r *dryRunner) backrunAnyBlock(head *types.Header) {
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
		return // parent state pruned during catch-up — skip silently.
	}

	// touchedPools accumulates the UNIQUE pools every victim in this block touched.
	// The graph for a victim's backrun is seeded by ALL touched pools plus the hub
	// (combining is cheaper than a per-swap graph and captures cross-touched-pool
	// cycles). Built once per victim from the running set (so earlier swaps in the
	// block contribute their pools to a later victim's cycle search).
	touchedPools := make(map[common.Address]anyPool)

	// preState tracks the EXACT pre-victim state (post-state of the previously
	// applied tx); it starts as the parent copy and advances after every tx. The
	// backrun happens AFTER the victim, so the post-victim state used for the graph
	// is the state AFTER the victim tx is applied (= the next preState snapshot).
	preState := parentState.Copy()

	onTx := func(i int, tx *types.Transaction, receipt *types.Receipt, statedb *state.StateDB) {
		victimPreState := preState
		// postVictimState is the state AFTER this tx applied: the price the backrun
		// races. statedb is the post-tx state; Copy it for read-only graph probing.
		postVictimState := statedb.Copy()
		preState = statedb.Copy()

		if receipt == nil || len(receipt.Logs) == 0 {
			return
		}
		seen := make(map[[20]byte]bool)
		for _, l := range receipt.Logs {
			pair, token0Side, amountIn, isV3, vok := decodeAnyVictim(l)
			if !vok {
				continue
			}
			if seen[pair] {
				continue
			}
			seen[pair] = true

			// Resolve and record the touched pool (best-effort) so later victims'
			// cycle searches can route through it.
			if pool, ok := r.e.resolvePoolMeta(victimPreState.Copy(), r.bc, head, pair, isV3); ok && pool.ok {
				touchedPools[pair] = pool
			}

			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine backrun-any per-victim recovered from panic",
							"block", number, "txIndex", i, "tx", tx.Hash(), "panic", rec)
					}
				}()
				r.backrunAnyEvaluateVictim(number, i, tx, head, victimPreState, postVictimState, touchedPools, pair, token0Side, amountIn, isV3)
			}()
		}
	}

	if _, err := r.e.ApplyOnStateHooked(parentState.Copy(), r.bc, head, block.Transactions(), nil, onTx); err != nil {
		return
	}

	n := r.blocks.Add(1)
	if r.cfg.TallyEvery > 0 && n%r.cfg.TallyEvery == 0 {
		r.logBackrunAnyTally(n)
	}
}

// backrunAnyEvaluateVictim values a single any-pool victim's trailing BACKRUN as
// its MARGINAL cross-pool cycle contribution, denominated in BNB. It mirrors
// sandwichAnyEvaluateVictim's eligibility funnel verbatim (so victimsSeen and the
// skip counts match sandwich-any on the same blocks) but the valuation is the
// cross-pool cycle detector, NOT a single-pool round trip, and it is MARGINAL, not
// absolute:
//
//	(1) build a graph seeded by the union of touched pools + hub (BuildAnyPoolGraph)
//	    on BOTH the victim's PRE-state and its POST-state,
//	(2) enumerate negative cycles STARTING at the victim's INPUT token (the token
//	    the victim started with — NOT WBNB) on each,
//	(3) size each cycle (closed-form CycleOptimum for all-V2; verifyCycleEVM for
//	    V3-containing), the gross being in the cycle's START-token wei,
//	(4) convert the gross to BNB and apply the gas-only BackrunNet gate (gas
//	    per-cycle, V3-weighted) to get the best NET on EACH state, and
//	(5) report marginal = max(0, bestNet(post) - bestNet(pre)): only the arbitrage
//	    the victim CREATED counts; a STANDING cycle present in both states cancels,
//	    which also removes the cross-victim double counting.
//
// Both preState (pre-victim) and postVictimState are read-only Copies in the caller;
// BuildAnyPoolGraph reads them and verifyCycleEVM copies internally.
func (r *dryRunner) backrunAnyEvaluateVictim(number uint64, txIndex int, victimTx *types.Transaction, head *types.Header, preState, postVictimState *state.StateDB, touchedPools map[common.Address]anyPool, pair common.Address, token0Side bool, amountIn *big.Int, isV3 bool) {
	r.brVictimsSeen.Add(1)

	if preState == nil || postVictimState == nil || head == nil || victimTx == nil || amountIn == nil || amountIn.Sign() <= 0 || (pair == common.Address{}) {
		r.brSkippedUnsupported.Add(1)
		return
	}

	pool, ok := r.e.resolvePoolMeta(preState.Copy(), r.bc, head, pair, isV3)
	if !ok || !pool.ok || (pool.token0 == common.Address{}) || (pool.token1 == common.Address{}) {
		r.brSkippedUnsupported.Add(1)
		return
	}

	// victimTokenIn is the token the victim SPENT — this is the cycle's start/end
	// token (the backrun arb must start and end in the same token the victim's
	// input opened a gap for). It is NOT necessarily a numeraire.
	victimTokenIn := pool.token0
	if !token0Side {
		victimTokenIn = pool.token1
	}
	if _, hasOther := poolOther(pool, victimTokenIn); !hasOther {
		r.brSkippedUnsupported.Add(1)
		return
	}

	// V3 fork support: only Pancake V3 is exploitable (the existing router). A
	// non-Pancake V3 victim pool cannot itself be a sized leg, but it may still
	// reveal a cross-pool gap; we keep the eligibility gate identical to
	// sandwich-any (skip) so the funnels match.
	if pool.isV3 && !pool.v3Supported {
		r.brSkippedUnsupported.Add(1)
		return
	}

	// NUMERAIRE: identify the WBNB/stable side for the dust gate + BNB conversion.
	numToken, _, hasNum := poolNumeraire(pool)
	if !hasNum {
		r.brSkippedNoNumeraire.Add(1)
		return
	}

	// Fundability: both tokens must have resolvable storage slots (same gate as
	// sandwich-any so the funnel matches).
	probeCopy := preState.Copy()
	if _, fundable := r.e.resolveTokenSlots(probeCopy, r.bc, head, pool.token0); !fundable {
		r.brSkippedUnfundable.Add(1)
		return
	}
	if _, fundable := r.e.resolveTokenSlots(probeCopy, r.bc, head, pool.token1); !fundable {
		r.brSkippedUnfundable.Add(1)
		return
	}

	wbnbUSD := liveWbnbPriceUSD(preState)

	// Dust gate, in BNB, mirroring sandwich-any: price the victim notional in USD
	// when it is the numeraire; otherwise fall back to the raw-input heuristic.
	victimSpentNumeraire := victimTokenIn == numToken
	numTokenUSD := tokenUSDPrice(numToken, wbnbUSD)
	if victimSpentNumeraire && numTokenUSD > 0 {
		if !strategy.VictimAboveThreshold(amountIn, numTokenUSD, r.swCfg.minVictimUSD) {
			r.brBelowThreshold.Add(1)
			return
		}
	} else if amountIn.Cmp(minVictimInputHeuristicWei) < 0 {
		r.brBelowThreshold.Add(1)
		return
	}

	// The cycle's gross is denominated in the START token (the victim's input). It
	// must be a numeraire (WBNB/stable) to be valued in BNB; numeraireToBNB would
	// otherwise silently zero it. Gate explicitly and COUNT the skip so the narrowed
	// population (numeraire-start victims only) is auditable, not silent.
	startNumKind := numeraireOf(victimTokenIn)
	if startNumKind == numNone {
		r.brSkippedNonNumeraire.Add(1)
		return
	}

	// ---- MARGINAL CROSS-POOL CYCLE VALUATION (the round-2 fix) ----
	// Value the best cross-pool backrun NET on BOTH the victim's PRE-state and its
	// POST-state with the SAME graph/cycle machinery and the SAME seed token, then
	// report only the MARGINAL (post-minus-pre) contribution. This (a) strips any
	// STANDING arbitrage that pre-existed the victim (it appears in both states and
	// cancels) and (b) eliminates cross-victim double counting (a standing cycle is
	// no longer credited to every victim that re-runs the graph). See
	// strategy.MarginalBackrunGrossV2 for the pure analogue of this rule.
	touched := make([]anyPool, 0, len(touchedPools))
	for _, tp := range touchedPools {
		touched = append(touched, tp)
	}

	postEval, bestCycle, nPost, postFound := r.backrunAnyBestNetOnState(postVictimState, head, touched, victimTokenIn, startNumKind, wbnbUSD)
	preEval, _, _, preFound := r.backrunAnyBestNetOnState(preState, head, touched, victimTokenIn, startNumKind, wbnbUSD)

	// Per-victim probe diagnostics (first few above-threshold victims): report the
	// POST candidate (the gap being raced).
	if r.brProbeLogged.Load() < backrunAnyProbeLogCap {
		r.logBackrunAnyProbe(number, txIndex, victimTx, pool, victimTokenIn, amountIn, nPost, postFound, postEval)
	}

	// Best realizable NET on each state, floored at 0 (you would not execute a
	// net-negative backrun). marginal = max(0, postNet - preNet) via the SHARED,
	// tested strategy.MarginalNet rule (the detector and the pure helper agree).
	postNet := backrunNetFloor(postEval, postFound)
	preNet := backrunNetFloor(preEval, preFound)
	marginalNet := strategy.MarginalNet(preNet, postNet)
	if marginalNet.Sign() <= 0 {
		return // standing/pre-existing cycle (marginal 0) or no victim-created gap.
	}

	grossUSD := weiToUSD(postEval.GrossProfit, wbnbUSD)
	dexLabel := cycleDexMix(bestCycle)

	// Sanity outlier filter (catch-it-don't-bake-it; mirrors the prior units-bug catch).
	// Decimal-mismatch or degenerate-pool math can produce a single opp with billions+
	// of USD gross. We refuse to bake those into the aggregate; we log forensic detail
	// and skip. NOTE: this MUST stay before brGrossPositive / brNetPositive / opps /
	// addProfit / addBackrunAnyNet / brDist.Add / "backrun OPP @tx" so the rejection
	// neither pollutes the aggregate nor double-logs as an opportunity.
	if sanityRejectBackrunOpp(grossUSD, marginalNet, sanityCapUSD, sanityCapNetWei) {
		r.brSkippedSanityOutlier.Add(1)
		log.Warn("backrun OPP REJECTED sanity cap",
			"block", number, "txIndex", txIndex, "victimTx", victimTx.Hash().Hex(),
			"victimPool", poolLabel(pair), "cycleStart", shortAddr(victimTokenIn),
			"hops", len(bestCycle.Edges), "path", cyclePathString(bestCycle),
			"dexmix", dexLabel,
			"grossBNBWei", postEval.GrossProfit.String(),
			"netBNBWei", marginalNet.String(),
			"grossUSD", strconv.FormatFloat(grossUSD, 'f', 4, 64),
			"capUSD", sanityCapUSD,
			"capNetBNBWei", sanityCapNetWei.String(),
		)
		return
	}

	r.brGrossPositive.Add(1)
	r.brNetPositive.Add(1)
	r.opps.Add(1)
	r.addProfit(marginalNet)
	r.addBackrunAnyNet(marginalNet)

	r.brDist.Add(postEval.GrossProfit, strategy.CycleGasUnits(bestCycle), grossUSD, dexLabel, len(bestCycle.Edges))

	log.Info("backrun OPP @tx",
		"block", number,
		"txIndex", txIndex,
		"victimTx", victimTx.Hash().Hex(),
		"victimPool", poolLabel(pair),
		"cycleStart", shortAddr(victimTokenIn),
		"hops", len(bestCycle.Edges),
		"path", cyclePathString(bestCycle),
		"dexmix", dexLabel,
		"amountInWei", postEval.FrontrunIn.String(),
		"grossBNBWei", postEval.GrossProfit.String(),
		"preNetBNBWei", preNet.String(),
		"postNetBNBWei", postNet.String(),
		"netBNBWei", marginalNet.String(),
		"grossUSD", strconv.FormatFloat(grossUSD, 'f', 4, 64),
		"gasBNBWei", postEval.GasCost.String(),
	)
}

// backrunNetFloor returns the best realizable backrun NET profit for a state,
// floored at 0: a not-found or net-non-positive best cycle contributes 0 to the
// marginal (an unprofitable backrun would not be executed). This is the baseline
// used on BOTH the pre- and post-victim states so a STANDING cycle cancels out.
func backrunNetFloor(e strategy.SandwichEval, found bool) *big.Int {
	if !found || e.NetProfit == nil || e.NetProfit.Sign() <= 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Set(e.NetProfit)
}

// backrunAnyBestNetOnState builds the cross-pool graph on the supplied state (seeded
// by the union of touched pools + hub), enumerates negative cycles starting at the
// victim's input token, and returns the most net-profitable sized cycle (via
// backrunAnyBestCycle). It is the SAME machinery for the pre- and post-victim states
// so the marginal is an apples-to-apples post-minus-pre. Returns the best eval, its
// cycle, the candidate-cycle count (for probe diagnostics) and found. Read-only
// (BuildAnyPoolGraph reads the state; verifyCycleEVM copies internally).
func (r *dryRunner) backrunAnyBestNetOnState(s *state.StateDB, head *types.Header, touched []anyPool, startToken common.Address, startNumKind numeraireKind, wbnbUSD float64) (strategy.SandwichEval, strategy.Cycle, int, bool) {
	if s == nil {
		return strategy.SandwichEval{}, strategy.Cycle{}, 0, false
	}
	g, _ := BuildAnyPoolGraph(s, touched)
	if g.EdgeCount() == 0 {
		return strategy.SandwichEval{}, strategy.Cycle{}, 0, false
	}
	cycles := g.NegativeCycles(startToken, backrunAnyMaxCycleLen)
	if len(cycles) == 0 {
		return strategy.SandwichEval{}, strategy.Cycle{}, 0, false
	}
	eval, cycle, found := r.backrunAnyBestCycle(s, head, cycles, startToken, startNumKind, wbnbUSD)
	return eval, cycle, len(cycles), found
}

// backrunAnyBestCycle sizes every candidate cycle and returns the most profitable
// one (by net BNB) after the gas-only gate. Sizing routes all-V2 cycles to the
// exact closed form (CycleOptimum via EvaluateCycle) and V3-containing cycles to
// the EVM oracle (verifyCycleEVM), exactly as the hub graph mode does. The gross
// from sizing is in the cycle's START-token wei; it is converted to BNB via the
// start token's numeraire kind before the net gate so the BNB net is comparable to
// the wei gas cost. Returns (eval, cycle, found); found=false when no cycle has a
// positive gross. Read-only (verifyCycleEVM copies the state internally).
func (r *dryRunner) backrunAnyBestCycle(postVictimState *state.StateDB, head *types.Header, cycles []strategy.Cycle, startToken common.Address, startNumKind numeraireKind, wbnbUSD float64) (strategy.SandwichEval, strategy.Cycle, bool) {
	var (
		bestEval  strategy.SandwichEval
		bestCycle strategy.Cycle
		found     bool
	)
	// Cap the candidate count taken to Stage B (cycles are sorted best-signal-first).
	if len(cycles) > graphTopCandidates {
		cycles = cycles[:graphTopCandidates]
	}

	for _, c := range cycles {
		// Stage-B sizing. EvaluateCycle uses zero economic costs here; we apply the
		// BackrunNet gate ourselves (in BNB) below so gas is per-cycle and V3-weighted.
		var grossStart *big.Int
		var optIn *big.Int
		if cycleHasV3(c) {
			ev, ok := r.verifyCycleEVM(postVictimState, head, nil, c, strategy.EvalParams{
				GasCost: big.NewInt(0), BuilderBid: big.NewInt(0), Margin: big.NewInt(0),
			})
			if !ok || ev.GrossProfit == nil || ev.GrossProfit.Sign() <= 0 {
				continue
			}
			grossStart = ev.GrossProfit
			optIn = ev.OptimalAmountIn
		} else {
			optIn, grossStart = strategy.CycleOptimum(c)
			if grossStart == nil || grossStart.Sign() <= 0 {
				continue
			}
		}

		// Convert the start-token gross to BNB (pitfall: cycles start at the victim's
		// input token, not always WBNB).
		grossBNB := numeraireToBNB(grossStart, startNumKind, wbnbUSD)
		if grossBNB.Sign() <= 0 {
			continue // start token is not a numeraire / no live price -> can't value.
		}

		// Gas: per-cycle, V3-hop-weighted (NOT a fixed backrun budget).
		gasUnits := strategy.CycleGasUnits(c)
		eval := strategy.BackrunNet(grossBNB, r.cfg.GasPriceWei, gasUnits, r.cfg.BuilderBidWei)
		// Record the sizing input in the eval for logging (BackrunNet leaves it 0).
		eval.FrontrunIn = orZeroBig(optIn)

		if eval.NetProfit.Cmp(bestNetOf(bestEval, found)) > 0 {
			bestEval = eval
			bestCycle = c
			found = true
		}
	}
	return bestEval, bestCycle, found
}

// bestNetOf returns the current best net profit for the max-selection in
// backrunAnyBestCycle (a very negative sentinel when nothing has been chosen yet,
// so the first positive-gross cycle always wins).
func bestNetOf(e strategy.SandwichEval, found bool) *big.Int {
	if !found || e.NetProfit == nil {
		return new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 255))
	}
	return e.NetProfit
}

// logBackrunAnyProbe emits a single diagnostic line tracing a representative
// any-pool backrun cycle probe for the first few above-threshold victims.
func (r *dryRunner) logBackrunAnyProbe(number uint64, txIndex int, victimTx *types.Transaction, pool anyPool, startToken common.Address, amountIn *big.Int, nCycles int, found bool, bestEval strategy.SandwichEval) {
	r.brProbeLogged.Add(1)

	dexLabel := "v2_any"
	if pool.isV3 {
		dexLabel = "pancake_v3(fee=" + strconv.FormatUint(uint64(pool.feeTier), 10) + ")"
	}
	bestGross := "0"
	bestNet := "n/a"
	if found {
		bestGross = bigOrNil(bestEval.GrossProfit)
		bestNet = bigOrNil(bestEval.NetProfit)
	}

	log.Info("backrun-any probe",
		"block", number,
		"txIndex", txIndex,
		"victim", victimTx.Hash().Hex(),
		"victimPool", dexLabel+":"+poolLabel(pool.pair),
		"cycleStart", shortAddr(startToken),
		"victimAmtWei", amountIn.String(),
		"negCyclesFound", nCycles,
		"bestGrossBNBWei", bestGross,
		"bestNetBNBWei", bestNet,
	)
}

// addBackrunAnyNet accumulates the any-pool backrun net would-be profit.
func (r *dryRunner) addBackrunAnyNet(delta *big.Int) {
	for {
		cur := r.brTotalNetWei.Load()
		next := new(big.Int).Add(cur, delta)
		if r.brTotalNetWei.CompareAndSwap(cur, next) {
			return
		}
	}
}

// logBackrunAnyTally emits the any-pool backrun candidate funnel + profit summary
// and the gross-positive distribution. Crash-safe and read-only. The output shape
// matches sandwich-any's tally/dist lines for a direct comparison.
func (r *dryRunner) logBackrunAnyTally(processed uint64) {
	seen := r.brVictimsSeen.Load()
	gross := r.brGrossPositive.Load()
	net := r.brNetPositive.Load()

	overcount := "n/a"
	if net > 0 {
		overcount = bigRatio(seen, net)
	}

	log.Info("backrun-any tally",
		"processedBlocks", processed,
		"victimsSeen", seen,
		"skippedUnfundable", r.brSkippedUnfundable.Load(),
		"skippedUnsupported", r.brSkippedUnsupported.Load(),
		"skippedNoNumeraire", r.brSkippedNoNumeraire.Load(),
		"skippedNonNumeraire", r.brSkippedNonNumeraire.Load(),
		"belowThreshold", r.brBelowThreshold.Load(),
		"grossPositive", gross,
		"netPositive", net,
		"skippedSanityOutlier", r.brSkippedSanityOutlier.Load(),
		"seenPerNet", overcount,
		"opportunities", r.opps.Load(),
		"totalNetWei", r.brTotalNetWei.Load().String(),
		"ts", time.Now().Format(time.RFC3339),
	)

	s := r.brDist.Snapshot()
	log.Info("backrun-any dist",
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
		"byDex", s.DexMixString(),
		"byCycleLen", s.LenString(),
		"ts", time.Now().Format(time.RFC3339),
	)
}
