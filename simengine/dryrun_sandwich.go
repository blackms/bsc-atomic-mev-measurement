// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_sandwich.go is the SANDWICH-attack detector mode, selectable with
// SIMENGINE_DRYRUN=sandwich. Sandwiching is the DOMINANT atomic MEV on BSC
// (~51% of MEV volume) and the paper's new headline result. For every VICTIM
// swap on a watched pool in an imported block, it measures the GROUND-TRUTH
// sandwich profit by re-executing FRONTRUN -> the REAL victim tx -> BACKRUN
// through the deployed PancakeSwap router on a state.Copy (simengine/sandwich.go),
// sizes the frontrun optimally, applies the flash-loan + gas net gate, and logs
// every net-positive opportunity.
//
// It re-uses the validated tx-by-tx replay machinery: the block is replayed on a
// parent COPY via ApplyOnStateHooked, and the PRE-VICTIM state (all txs up to but
// not including the victim) is the post-state of the previous applied tx. The
// victim's input token/amount is decoded from its Swap LOG (the robust source —
// most swaps route through aggregators).
//
// Strictly read-only (state.Copy() only, never commits), every block and every
// per-victim evaluation wrapped in defer/recover so a bug can never panic or
// stall the node, and a complete no-op unless SIMENGINE_DRYRUN=sandwich.
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

// sandwichProbeLogCap bounds how many above-threshold victims get a detailed
// per-step diagnostic line (sim13). Enough to characterise the failure without
// flooding the log.
const sandwichProbeLogCap uint64 = 15

// sandwichConfig holds the sandwich-specific cost/threshold knobs, read from env
// at startup with conservative BSC-realistic defaults.
type sandwichConfig struct {
	// flashBps is the flash-loan fee in basis points charged on the frontrun
	// notional (default 9 = Venus core-pool; Aave V3 = 5; Pancake V3 flash = 1).
	flashBps uint64
	// minVictimUSD is the dust floor: victims below this notional are skipped
	// (below it no frontrun clears 2 router-swap gas + the flash fee).
	minVictimUSD float64
}

// defaultSandwichConfig reads SIMENGINE_SANDWICH_FLASHBPS and
// SIMENGINE_SANDWICH_MINUSD, falling back to 9 bps and $100.
func defaultSandwichConfig() sandwichConfig {
	c := sandwichConfig{flashBps: 9, minVictimUSD: 100}
	if v := os.Getenv("SIMENGINE_SANDWICH_FLASHBPS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			c.flashBps = n
		}
	}
	if v := os.Getenv("SIMENGINE_SANDWICH_MINUSD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			c.minVictimUSD = f
		}
	}
	return c
}

// runSandwichBacktest subscribes to chain heads and runs the ground-truth
// sandwich valuator on every imported block. Read-only, crash-safe. Mirrors
// runIntrablockBacktest's wiring.
func (r *dryRunner) runSandwichBacktest() {
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	r.swCfg = defaultSandwichConfig()

	log.Info("SimEngine dry-run SANDWICH (ground-truth) loop started",
		"pools", len(strategy.ExtendedPools()),
		"flashBps", r.swCfg.flashBps, "minVictimUSD", r.swCfg.minVictimUSD,
		"v2router", pancakeV2Router.Hex(), "v3router", pancakeV3SwapRouter.Hex())
	log.Info("SimEngine sandwich watch-set audit\n" + strategy.RegistryAudit())

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run sandwich loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run sandwich recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.sandwichBlock(head)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run sandwich loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// runSandwichSelftest runs the one-shot funding+router-calldata self-test (sim13)
// on the first processed block. On a FRESH copy of parentState it funds a
// synthetic attacker with 1 WBNB and executes WBNB->USDT through BOTH the V2
// router (pool 0x16b9a8...) and the V3 fee-100 SwapRouter (pool 0x172fcd...),
// logging the USDT received and any revert. This isolates "does storage-funding +
// router calldata work in-process" (expect ~585 USDT, matching the on-node
// eth_call + stateDiff proof) from the sizing/sequence logic. Strictly read-only.
func (r *dryRunner) runSandwichSelftest(head *types.Header, parentState *state.StateDB) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Warn("SimEngine sandwich selftest recovered from panic", "block", head.Number, "panic", rec)
		}
	}()

	oneWBNB := new(big.Int).Mul(big.NewInt(1), big.NewInt(1e18))

	v2 := r.e.selftestRouterSwap(parentState, r.bc, head, "v2",
		pancakeV2Router, strategy.WBNB, strategy.USDT, false, 0, oneWBNB)
	log.Info("selftest v2 wbnb->usdt",
		"router", pancakeV2Router.Hex(),
		"pool", "0x16b9a82891338f9bA80E2D6970FddA79D1eb0daE",
		"amountInWBNB", "1e18",
		"out", v2.out.String(),
		"reason", v2.reason,
	)

	v3 := r.e.selftestRouterSwap(parentState, r.bc, head, "v3",
		pancakeV3SwapRouter, strategy.WBNB, strategy.USDT, true, 100, oneWBNB)
	log.Info("selftest v3 wbnb->usdt fee=100",
		"router", pancakeV3SwapRouter.Hex(),
		"pool", "0x172fcd41e0913e95784454622d1c3724f546f849",
		"amountInWBNB", "1e18",
		"out", v3.out.String(),
		"reason", v3.reason,
	)
}

// logSandwichProbe emits a single INFO line tracing one representative 3-step
// sandwich probe for an above-threshold victim (sim13). It runs an instrumented
// probe at frontrun = half the victim size (a feasible, representative size) so we
// can see which leg (frontrun / victim / backrun) fails and the EVM revert reason.
// bestFrontrun/searchGross are the search's own result for cross-reference.
// Strictly read-only (the diagnostic probe Copy()s preState internally).
func (r *dryRunner) logSandwichProbe(number uint64, txIndex int, victimTx *types.Transaction, head *types.Header, preState *state.StateDB, victim strategy.SandwichVictim, tokenOut common.Address, tokenUSD float64, bestFrontrun, searchGross *big.Int) {
	r.swProbeLogged.Add(1)

	size := strategy.HalfVictimSeed(victim.AmountIn)
	if size == nil || size.Sign() <= 0 {
		size = victim.AmountIn
	}
	d := r.e.probeSandwichDiag(preState, r.bc, head, victimTx, victim, size)

	victimAmtUSD := 0.0
	if tokenUSD > 0 {
		victimAmtUSD = weiToUSD(victim.AmountIn, tokenUSD)
	}

	dexLabel := victim.Pool.DEX
	if victim.Pool.IsV3 {
		dexLabel += "(v3 fee=" + strconv.FormatUint(uint64(victim.Pool.FeeTier), 10) + ")"
	}

	log.Info("sandwich probe",
		"block", number,
		"txIndex", txIndex,
		"victim", victimTx.Hash().Hex(),
		"pool", dexLabel+":"+poolLabel(victim.Pool.Pair),
		"dir", shortAddr(victim.TokenIn)+"->"+shortAddr(tokenOut),
		"victimAmtUSD", strconv.FormatFloat(victimAmtUSD, 'f', 2, 64),
		"probeFrontrunWei", size.String(),
		"fundOk", d.fundOk,
		"frontrunOk", d.frontrunOk,
		"yBought", bigOrNil(d.yBought),
		"victimOk", d.victimOk,
		"backrunOk", d.backrunOk,
		"probeGross", d.gross.String(),
		"searchBestFrontrun", bigOrNil(bestFrontrun),
		"searchGross", bigOrNil(searchGross),
		"reason", d.reason,
	)
}

// bigOrNil renders a *big.Int as its decimal string, or "nil" when unset.
func bigOrNil(x *big.Int) string {
	if x == nil {
		return "nil"
	}
	return x.String()
}

// sandwichBlock re-executes one block transaction-by-transaction on its parent
// state via the hooked SimEngine path and, for each watched-pool victim swap,
// evaluates the ground-truth sandwich on the EXACT pre-victim state. Read-only.
func (r *dryRunner) sandwichBlock(head *types.Header) {
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

	// One-shot startup self-test (sim13): isolate funding+router calldata from the
	// sizing/sequence logic. Run once, on the first processed block's parent state,
	// using `head` as the block environment. Read-only (each leg on a fresh copy).
	if r.swSelftestDone.CompareAndSwap(false, true) {
		r.runSandwichSelftest(head, parentState)
	}

	// preState tracks the EXACT pre-victim state: the post-state of the previously
	// applied tx. It starts as the parent copy (pre-state of the FIRST tx) and is
	// updated to a fresh Copy after every applied tx. When the CURRENT tx is a
	// victim, preState is its pre-victim state.
	preState := parentState.Copy()

	onTx := func(i int, tx *types.Transaction, receipt *types.Receipt, statedb *state.StateDB) {
		// Capture the pre-state BEFORE updating it: it is the state right before
		// this tx (= post-state of the previous tx). statedb here is post-this-tx.
		victimPreState := preState
		// Advance preState to post-this-tx for the NEXT tx.
		preState = statedb.Copy()

		if receipt == nil || len(receipt.Logs) == 0 {
			return
		}
		// Is this tx a victim swap on a watched pool? Decode each Swap log; one tx
		// may carry several (multi-hop). Evaluate each distinct watched-pool victim.
		seen := make(map[[20]byte]bool)
		for _, l := range receipt.Logs {
			victim, vok := strategy.DecodeVictim(l)
			if !vok {
				continue
			}
			if seen[victim.Pool.Pair] {
				continue
			}
			seen[victim.Pool.Pair] = true

			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine sandwich per-victim recovered from panic",
							"block", number, "txIndex", i, "tx", tx.Hash(), "panic", rec)
					}
				}()
				r.sandwichEvaluateVictim(number, i, tx, head, victimPreState, victim)
			}()
		}
	}

	if _, err := r.e.ApplyOnStateHooked(parentState.Copy(), r.bc, head, block.Transactions(), nil, onTx); err != nil {
		return
	}

	n := r.blocks.Add(1)
	if r.cfg.TallyEvery > 0 && n%r.cfg.TallyEvery == 0 {
		r.logSandwichTally(n)
	}
}

// sandwichEvaluateVictim values a single victim swap on its EXACT pre-victim
// state: size the frontrun optimally via the ground-truth 3-step re-execution,
// apply the flash-loan + gas net gate, record the funnel/distribution, and log
// every net-positive opportunity. preState is read-only here (sandwichProfit
// Copy()s it internally). Crash-safe via the caller's recover.
func (r *dryRunner) sandwichEvaluateVictim(number uint64, txIndex int, victimTx *types.Transaction, head *types.Header, preState *state.StateDB, victim strategy.SandwichVictim) {
	// Eligibility: both pool tokens must be fundable (known storage slots), else
	// we cannot fund the synthetic attacker -> skip (counted separately).
	tokenOut, hasOther := victim.Pool.Other(victim.TokenIn)
	if !hasOther || !TokenFundable(victim.TokenIn) || !TokenFundable(tokenOut) {
		r.swSkippedUnfundable.Add(1)
		return
	}

	// Dust filter: skip victims below the USD floor. Price X in USD: WBNB via the
	// live spot, stables ~1.0.
	wbnbUSD := liveWbnbPriceUSD(preState)
	tokenUSD := tokenUSDPrice(victim.TokenIn, wbnbUSD)
	if !strategy.VictimAboveThreshold(victim.AmountIn, tokenUSD, r.swCfg.minVictimUSD) {
		r.swBelowThreshold.Add(1)
		return
	}

	r.swVictimsConsidered.Add(1)

	// Ground-truth optimal frontrun (golden-section over the 3-step EVM re-exec).
	frontrun, gross, gasUnits := r.e.optimalFrontrun(preState, r.bc, head, victimTx, victim)

	// PER-VICTIM DIAGNOSTICS (sim13): for the first sandwichProbeLogCap
	// above-threshold victims, emit a single INFO line tracing each leg of a
	// representative probe (frontrun = half the victim) so we can see WHICH step
	// fails and why. This is logged whether or not the search found gross>0, so a
	// silent grossPositive=0 funnel is immediately explained.
	if r.swProbeLogged.Load() < sandwichProbeLogCap {
		r.logSandwichProbe(number, txIndex, victimTx, head, preState, victim, tokenOut, tokenUSD, frontrun, gross)
	}

	if gross.Sign() <= 0 {
		return
	}
	r.swGrossPositive.Add(1)

	// Characterise the gross-positive population (grossUSD percentiles, breakeven
	// gas price, gas-price sweep) — reuse the same O(1) distribution accumulator
	// as the intra-block detector. dexMix is the single victim pool's DEX.
	grossUSD := weiToUSD(gross, tokenUSD)
	r.swDist.Add(gross, gasUnits, grossUSD, victim.Pool.DEX, 2 /*2 attacker legs*/)

	// Net gate: gross - gas - flashFee(frontrun notional) - bid.
	eval := strategy.SandwichNet(frontrun, gross, r.cfg.GasPriceWei, gasUnits, r.swCfg.flashBps, r.cfg.BuilderBidWei)
	if !eval.Profitable {
		return
	}
	r.swNetPositive.Add(1)
	r.opps.Add(1)
	r.addProfit(eval.NetProfit)
	r.addSandwichNet(eval.NetProfit)

	log.Info("sandwich OPP @tx",
		"block", number,
		"txIndex", txIndex,
		"victimTx", victimTx.Hash().Hex(),
		"pool", victim.Pool.DEX+":"+poolLabel(victim.Pool.Pair),
		"dir", shortAddr(victim.TokenIn)+"->"+shortAddr(tokenOut),
		"victimAmountWei", victim.AmountIn.String(),
		"frontrunWei", frontrun.String(),
		"grossWei", eval.GrossProfit.String(),
		"netWei", eval.NetProfit.String(),
		"gasWei", eval.GasCost.String(),
		"flashFeeWei", eval.FlashFee.String(),
	)
}

// tokenUSDPrice returns the USD price of one whole unit of `token`: WBNB via the
// live spot, the stables (USDT/USDC) ~1.0, anything else 0 (then the dust gate is
// disabled for that victim — but such tokens are already excluded by TokenFundable
// since the watch set only funds WBNB/USDT/USDC).
func tokenUSDPrice(token common.Address, wbnbUSD float64) float64 {
	switch token {
	case strategy.WBNB:
		return wbnbUSD
	case strategy.USDT, strategy.USDC:
		return 1.0
	default:
		return 0
	}
}

// logSandwichTally emits the sandwich candidate funnel + profit summary and the
// gross-positive characterisation distribution. Crash-safe and read-only.
func (r *dryRunner) logSandwichTally(processed uint64) {
	considered := r.swVictimsConsidered.Load()
	gross := r.swGrossPositive.Load()
	net := r.swNetPositive.Load()

	overcount := "n/a"
	if net > 0 {
		overcount = bigRatio(considered, net)
	}

	log.Info("sandwich tally",
		"processedBlocks", processed,
		"victimsConsidered", considered,
		"skippedUnfundable", r.swSkippedUnfundable.Load(),
		"belowThreshold", r.swBelowThreshold.Load(),
		"grossPositive", gross,
		"netPositive", net,
		"consideredPerNet", overcount,
		"opportunities", r.opps.Load(),
		"totalNetWei", r.swTotalNetWei.Load().String(),
		"ts", time.Now().Format(time.RFC3339),
	)

	r.logSandwichDist(processed)
}

// logSandwichDist emits the gross-positive sandwich characterisation: grossUSD
// percentiles, breakeven gas price, the gas-price sweep, and the per-DEX / per-leg
// breakdowns. Reuses the same GrossDist accumulator and Snapshot as intra-block.
func (r *dryRunner) logSandwichDist(processed uint64) {
	s := r.swDist.Snapshot()
	log.Info("sandwich dist",
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
		"ts", time.Now().Format(time.RFC3339),
	)
}

// addSandwichNet accumulates the sandwich-specific net would-be profit.
func (r *dryRunner) addSandwichNet(delta *big.Int) {
	for {
		cur := r.swTotalNetWei.Load()
		next := new(big.Int).Add(cur, delta)
		if r.swTotalNetWei.CompareAndSwap(cur, next) {
			return
		}
	}
}
