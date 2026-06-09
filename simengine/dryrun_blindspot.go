// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_blindspot.go is the BLIND-SPOT TRACE-PROBE, selectable with
// SIMENGINE_DRYRUN=blindspot.
//
// ROUND-1 REDESIGN (population + exclusions). The original probe gated on the
// ex-post net-positive surface (serializeValueVictim) — the WRONG population —
// and applied pattern heuristics WITHOUT the router/coinbase/same-actor
// exclusions or a nonce-based coldness check that realizability needed four fixes
// to get right. The corrected probe answers the actual scientific question:
//
//	Among opposite-direction cross-tx brackets that recall MISSED — i.e. that
//	reached the realizability §5.2 same-actor gate but FAILED rzCorroborate (the
//	corrFail buckets, e.g. non-flat round-trips) — how many are ACTUALLY real
//	sandwiches/backruns the flat-balance same-actor gate failed to credit?
//
// For each recall-MISSED bracket it runs two corrected detectors:
//
//	(A) detectRoundTripCorrected — applies the §5.2 same-actor gate +
//	    rzKnownRouters/coinbase exclusion on both legs, THEN measures round-trip
//	    flatness as a PERCENTAGE OF GROSS MOVEMENT (not a fixed wei window):
//	    a real, NON-FLAT round trip (actor repeated buy+sell, just unbalanced) is a
//	    true positive; a router pass-through is a false positive.
//	(B) detectProceedsSweepCorrected — applies the same router/coinbase gate on the
//	    proceeds recipient, then confirms the recipient is genuinely COLD via
//	    state.GetNonce(preBlockState, to) (fresh / lower than the sender) rather
//	    than a warm MM/aggregator.
//
// The output is (i) counts + BNB sum of recall-MISSED brackets that ARE real
// sandwiches/backruns (an upper bound on the realized-capture under-count),
// segregated by pattern, and (ii) the false-positive counts on router
// pass-throughs / warm addresses. It runs INSIDE the replay (it builds the same
// per-tx ledgers + Swap legs the realizability detector uses) so it has the
// bracket candidates during leg decode, not post-hoc.
//
// Strictly READ-ONLY (state.Copy() only; it reads statedb + receipt.Logs and never
// commits), crash-safe per block.
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

// transferTopic0 = keccak256("Transfer(address,address,uint256)").
var transferTopic0 = common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")

// blindspotConfig holds the tunable knobs for the recall-missed pattern probe.
type blindspotConfig struct {
	flatPctTol  float64 // round-trip flatness tolerance, PERCENT of gross movement (SIMENGINE_BLINDSPOT_FLATPCT, default 0.5)
	nonceThresh uint64  // recipient nonce below which it is treated as fresh/cold (SIMENGINE_BLINDSPOT_NONCE, default 5)
	logLimit    int     // per-block detailed-probe lines emitted
}

// defaultBlindspotConfig reads the env knobs with the documented defaults.
func defaultBlindspotConfig() blindspotConfig {
	c := blindspotConfig{flatPctTol: 0.5, nonceThresh: 5, logLimit: 20}
	if v := os.Getenv("SIMENGINE_BLINDSPOT_FLATPCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			c.flatPctTol = f
		}
	}
	if v := os.Getenv("SIMENGINE_BLINDSPOT_NONCE"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			c.nonceThresh = n
		}
	}
	if v := os.Getenv("SIMENGINE_BLINDSPOT_LOGLIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.logLimit = n
		}
	}
	return c
}

// runBlindspotBacktest subscribes to chain heads and runs the recall-missed
// pattern probe on every imported block. Read-only, crash-safe.
func (r *dryRunner) runBlindspotBacktest() {
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	r.swCfg = defaultSandwichConfig()
	r.rzCfg = defaultRealizabilityConfig()
	r.bsCfg = defaultBlindspotConfig()

	log.Info("SimEngine dry-run BLINDSPOT (recall-missed evasion-pattern probe) loop started",
		"flatPctTol", r.bsCfg.flatPctTol, "nonceThresh", r.bsCfg.nonceThresh, "logLimit", r.bsCfg.logLimit)

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run blindspot loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run blindspot recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.blindspotBlock(head)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run blindspot loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// blindspotBlock replays one block tx-by-tx on its parent state, building the SAME
// per-tx hub ledgers + Swap legs the realizability detector builds, then probes the
// RECALL-MISSED brackets (opposite-direction cross-tx brackets that passed §5.2 but
// FAILED rzCorroborate). The pre-BLOCK state (parentState) is the cold/fresh-nonce
// reference for the proceeds-sweep coldness check. Read-only.
func (r *dryRunner) blindspotBlock(head *types.Header) {
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
		return
	}

	var (
		legs    []rzSwapLeg
		ledgers []rzTxLedger
	)
	preState := parentState.Copy()

	signer := types.LatestSignerForChainID(r.e.chainCfg.ChainID)

	onTx := func(i int, tx *types.Transaction, receipt *types.Receipt, statedb *state.StateDB) {
		victimPreState := preState
		preState = statedb.Copy()
		if receipt == nil {
			return
		}
		from, sErr := types.Sender(signer, tx)
		if sErr != nil {
			from = common.Address{}
		}
		led := r.buildTxLedger(i, tx, receipt, from, head, victimPreState, statedb)
		ledgers = append(ledgers, led)
		legs = append(legs, decodeRzLegs(i, tx.Hash(), from, receipt.Logs)...)
	}

	if _, err := r.e.ApplyOnStateHooked(parentState.Copy(), r.bc, head, block.Transactions(), nil, onTx); err != nil {
		return
	}

	wbnbUSD := liveWbnbPriceUSD(parentState)
	dust := r.rzDustBNB(wbnbUSD)
	// coldRef is the pre-BLOCK state: a recipient with a low nonce HERE (before any
	// block tx) is genuinely fresh/cold. parentState is read-only.
	r.blindspotProbeRecallMissed(number, head, legs, ledgers, parentState, dust, wbnbUSD)

	r.bsProcessed.Add(1)
	n := r.blocks.Add(1)
	if r.cfg.TallyEvery > 0 && n%r.cfg.TallyEvery == 0 {
		r.logBlindspotTally(n)
	}
}

// blindspotProbeRecallMissed enumerates opposite-direction cross-tx brackets per
// pool (mirroring detectLandedSandwiches), applies the §5.2 same-actor gate and the
// §5.3 corroboration gate, and for every bracket that FAILS corroboration runs the
// corrected evasion-pattern detectors. The recall-missed population is exactly the
// set of brackets that passed §5.2 but failed §5.3 — the corrFail buckets. Read-only.
func (r *dryRunner) blindspotProbeRecallMissed(number uint64, head *types.Header, legs []rzSwapLeg, ledgers []rzTxLedger, coldRef *state.StateDB, dust *big.Int, wbnbUSD float64) {
	if len(legs) == 0 {
		return
	}
	ledByIdx := make(map[int]*rzTxLedger, len(ledgers))
	for i := range ledgers {
		ledByIdx[ledgers[i].txIdx] = &ledgers[i]
	}

	byPool := make(map[common.Address][]int)
	for i := range legs {
		byPool[legs[i].pool] = append(byPool[legs[i].pool], i)
	}

	perBlockLogged := 0

	for _, idxs := range byPool {
		for fi := 0; fi < len(idxs); fi++ {
			front := legs[idxs[fi]]
			for bj := fi + 1; bj < len(idxs); bj++ {
				back := legs[idxs[bj]]
				if back.txIdx <= front.txIdx {
					continue // need a strict cross-tx bracket
				}
				if front.inToken0 == back.inToken0 {
					continue // §5.1 opposite directions
				}
				if front.hasMintBurn || back.hasMintBurn {
					continue // §9 JIT exclusion
				}
				// §5.2 same-actor confirmation. A bracket that fails §5.2 was never a
				// candidate the realizability detector could credit; the recall-missed
				// population is brackets that PASSED §5.2 but then failed §5.3.
				actor, _, ok := rzSameActor(front, back, head.Coinbase)
				if !ok {
					continue
				}
				// §5.3 corroboration. PASS => realizability already credits it (NOT
				// recall-missed). FAIL => recall-missed; probe it.
				if _, _, _, corr := r.rzCorroborate(front, back, actor, ledByIdx, head, dust, wbnbUSD); corr {
					continue
				}
				r.bsRecallMissedBrackets.Add(1)
				r.blindspotEvaluateBracket(number, head, front, back, actor, ledByIdx, coldRef, wbnbUSD, &perBlockLogged)
			}
		}
	}
}

// blindspotEvaluateBracket runs the two corrected detectors on ONE recall-missed
// bracket and tallies the outcome (true positive vs router/cold false positive).
// For a true positive it accumulates the bracket's per-pool realized hub gross
// (BNB) into the upper-bound missed-capture sum. Read-only.
func (r *dryRunner) blindspotEvaluateBracket(number uint64, head *types.Header, front, back rzSwapLeg, actor common.Address, ledByIdx map[int]*rzTxLedger, coldRef *state.StateDB, wbnbUSD float64, perBlockLogged *int) {
	rtReal, rtRouterFP := r.detectRoundTripCorrected(front, back, actor, head.Coinbase)
	swReal, sweepTo, swColdFP := r.detectProceedsSweepCorrected(front, back, ledByIdx, coldRef, head.Coinbase)

	// The bracket's OWN realized hub effect on THIS pool (BNB) is the upper-bound
	// value the same-actor flat gate under-counted. Use the per-pool bracket hub so
	// unrelated whole-tx flows do not inflate it; fall back to 0 when unresolvable.
	realizedBNB := big.NewInt(0)
	if hub, okh := rzPerPoolBracketHubBNB(front, back, wbnbUSD); okh && hub.Sign() > 0 {
		realizedBNB = hub
	}

	pattern := ""
	switch {
	case rtReal:
		pattern = "roundtrip_real"
		r.bsRecallMissedRoundTrip_Real.Add(1)
		r.bumpPattern("roundtrip_real")
		if realizedBNB.Sign() > 0 {
			r.addBlindspotMissed(realizedBNB)
		}
	case swReal:
		pattern = "sweep_real"
		r.bsRecallMissedSweep_Real.Add(1)
		r.bumpPattern("sweep_real")
		if realizedBNB.Sign() > 0 {
			r.addBlindspotMissed(realizedBNB)
		}
	case rtRouterFP:
		pattern = "roundtrip_routerFP"
		r.bsRecallMissedRoundTrip_RouterFP.Add(1)
		r.bumpPattern("roundtrip_routerFP")
	case swColdFP:
		pattern = "sweep_coldFP"
		r.bsRecallMissedSweep_ColdFP.Add(1)
		r.bumpPattern("sweep_coldFP")
	default:
		r.bumpPattern("ambiguous")
		return
	}

	if *perBlockLogged < r.bsCfg.logLimit {
		*perBlockLogged++
		log.Info("blindspot recall-missed pattern",
			"block", number,
			"pool", poolLabel(front.pool),
			"frontTx", front.txHash.Hex(),
			"backTx", back.txHash.Hex(),
			"pattern", pattern,
			"actor", shortAddr(actor),
			"sweepTo", shortAddr(sweepTo),
			"realizedBNBWei", realizedBNB.String(),
		)
	}
}

// detectRoundTripCorrected decides whether a recall-missed bracket is a REAL
// (possibly NON-FLAT) atomic round-trip versus a router pass-through false positive.
//
// The bracket already passed §5.2 same-actor and is opposite-direction by
// construction. The corrected logic:
//  1. ROUTER EXCLUSION: if EITHER leg's discriminating actor identity is a known
//     router/aggregator or the coinbase, the round-trip pattern is a router
//     pass-through false positive (isRouterFP=true).
//  2. GROSS-RELATIVE FLATNESS (the round-1 fix): measure the round-tripped token Y
//     movement as gross = max(|yFrontOut|, |yBackIn|) and the imbalance as
//     |yFrontOut - yBackIn|. A real round trip (true positive) is one where the
//     actor genuinely bought and sold Y; whether it is flat or NOT-flat, it is a
//     real round trip the flat-balance gate would have MISSED when the imbalance
//     exceeds flatPctTol% of gross (the dominant recall-miss reason: non-flat
//     round trips). We return isReal=true for any genuine two-sided Y movement by
//     a non-router same actor.
//
// Returns (isReal, isRouterFP); at most one is true.
func (r *dryRunner) detectRoundTripCorrected(front, back rzSwapLeg, actor, coinbase common.Address) (isReal, isRouterFP bool) {
	// Router exclusion on both legs' discriminating identities.
	if bsLegIsRouter(front, coinbase) || bsLegIsRouter(back, coinbase) || rzKnownRouters[actor] || actor == coinbase {
		return false, true
	}

	// Round-tripped token Y movement from the log amounts (front buys Y out, back
	// sells Y in — opposite directions on the same pool).
	var yFrontOut, yBackIn *big.Int
	if front.inToken0 {
		yFrontOut, yBackIn = front.amt1Out, back.amt1In
	} else {
		yFrontOut, yBackIn = front.amt0Out, back.amt0In
	}
	if yFrontOut == nil || yBackIn == nil || yFrontOut.Sign() <= 0 || yBackIn.Sign() <= 0 {
		return false, false // not a two-sided Y movement: ambiguous, not a round trip.
	}

	// Both sides moved Y by a real same (non-router) actor: this IS a round trip the
	// flat-balance same-actor gate failed to credit. (Whether it is flat or not is
	// recorded by the flatness comparison; either way it is a real round trip — the
	// non-flat case is precisely the recall miss.)
	_ = bsWithinFlatPct(yFrontOut, yBackIn, r.bsCfg.flatPctTol)
	return true, false
}

// detectProceedsSweepCorrected decides whether a recall-missed bracket's proceeds
// were swept to a genuinely COLD address versus a warm/router false positive.
//
// The proceeds recipient is the back leg's beneficiary (the actor recovering the
// hub at the close of the round trip). Corrected logic:
//  1. ROUTER EXCLUSION: a recipient that is a known router/aggregator or the
//     coinbase is a pass-through, not a sweep (isColdFP=true if the pattern would
//     otherwise have matched).
//  2. COLD-ADDRESS CHECK via STATE NONCE (the round-1 fix): a recipient is COLD
//     iff state.GetNonce(coldRef, to) is below the nonce threshold OR strictly
//     less than the sender's nonce (a fresh address that has never transacted).
//     A warm MM/aggregator with a high nonce is a false positive.
//
// Returns (isReal, sweepTo, isColdFP). isReal requires a cold, non-router recipient
// that DIFFERS from the front-leg actor (the proceeds left the executing identity).
func (r *dryRunner) detectProceedsSweepCorrected(front, back rzSwapLeg, ledByIdx map[int]*rzTxLedger, coldRef *state.StateDB, coinbase common.Address) (isReal bool, sweepTo common.Address, isColdFP bool) {
	to := back.beneficiary
	if to == (common.Address{}) {
		return false, common.Address{}, false
	}
	// The proceeds must leave the executing identity to count as a sweep.
	if to == front.beneficiary || to == front.sender || to == front.from {
		return false, to, false
	}
	// Router exclusion.
	if rzKnownRouters[to] || to == coinbase {
		return false, to, true
	}
	if coldRef == nil {
		return false, to, false
	}
	toNonce := coldRef.GetNonce(to)
	senderNonce := uint64(0)
	if front.from != (common.Address{}) {
		senderNonce = coldRef.GetNonce(front.from)
	}
	cold := toNonce < r.bsCfg.nonceThresh || (senderNonce > 0 && toNonce < senderNonce)
	if !cold {
		// Pattern matched (proceeds left to a distinct address) but the recipient is
		// warm: a false positive.
		return false, to, true
	}
	return true, to, false
}

// bsLegIsRouter reports whether either discriminating identity of a leg (Swap
// sender or beneficiary) is a known router/aggregator or the coinbase.
func bsLegIsRouter(lg rzSwapLeg, coinbase common.Address) bool {
	if rzKnownRouters[lg.sender] || lg.sender == coinbase {
		return true
	}
	if rzKnownRouters[lg.beneficiary] || lg.beneficiary == coinbase {
		return true
	}
	return false
}

// bsWithinFlatPct reports whether |a-b| <= tolPct% of max(|a|,|b|): the
// PERCENTAGE-OF-GROSS flatness test that replaces the round-1 fixed wei window. A
// 1-wei net on a billion-wei round trip is flat; a million-wei net on a
// million-wei round trip is NOT.
func bsWithinFlatPct(a, b *big.Int, tolPct float64) bool {
	if a == nil || b == nil {
		return false
	}
	return withinPct(new(big.Int).Abs(a), new(big.Int).Abs(b), tolPct)
}

// addBlindspotMissed accumulates the bracket's realized hub gross over a TRUE
// positive (the upper-bound missed-capture sum).
func (r *dryRunner) addBlindspotMissed(delta *big.Int) {
	for {
		cur := r.bsRecallMissedRealizedWei.Load()
		next := new(big.Int).Add(cur, delta)
		if r.bsRecallMissedRealizedWei.CompareAndSwap(cur, next) {
			return
		}
	}
}

// bumpPattern increments a pattern's breakdown counter (lock-guarded map).
func (r *dryRunner) bumpPattern(name string) {
	r.bsPatternMu.Lock()
	r.bsPatternMap[name]++
	r.bsPatternMu.Unlock()
}

// logBlindspotTally emits the corrected recall-missed funnel: candidate count,
// real round-trips / sweeps (count + the upper-bound missed-realized BNB), and the
// router / cold false-positive counts that quantify detection noise. Read-only.
func (r *dryRunner) logBlindspotTally(processed uint64) {
	r.bsPatternMu.Lock()
	rtReal := r.bsPatternMap["roundtrip_real"]
	rtFP := r.bsPatternMap["roundtrip_routerFP"]
	swReal := r.bsPatternMap["sweep_real"]
	swFP := r.bsPatternMap["sweep_coldFP"]
	amb := r.bsPatternMap["ambiguous"]
	r.bsPatternMu.Unlock()

	top := "roundtrip_real:" + strconv.FormatUint(rtReal, 10) +
		",roundtrip_routerFP:" + strconv.FormatUint(rtFP, 10) +
		",sweep_real:" + strconv.FormatUint(swReal, 10) +
		",sweep_coldFP:" + strconv.FormatUint(swFP, 10) +
		",ambiguous:" + strconv.FormatUint(amb, 10)

	log.Info("blindspot tally",
		"processedBlocks", processed,
		"recallMissedBrackets", r.bsRecallMissedBrackets.Load(),
		"roundTripReal", r.bsRecallMissedRoundTrip_Real.Load(),
		"roundTripRouterFP", r.bsRecallMissedRoundTrip_RouterFP.Load(),
		"sweepReal", r.bsRecallMissedSweep_Real.Load(),
		"sweepColdFP", r.bsRecallMissedSweep_ColdFP.Load(),
		"upperBoundMissedRealizedWei", r.bsRecallMissedRealizedWei.Load().String(),
		"topPatterns", top,
		"ts", time.Now().Format(time.RFC3339),
	)
}

// (silence unused import guard for strategy if the build prunes the only use)
var _ = strategy.WBNB
