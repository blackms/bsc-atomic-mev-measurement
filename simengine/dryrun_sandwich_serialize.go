// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_sandwich_serialize.go is the SERIALIZED CONTENDING SAME-POOL EXECUTION
// detector, selectable with SIMENGINE_DRYRUN=sandwich-serialize. It quantifies the
// CONCURRENCY OVER-COUNT that the per-victim-isolated sandwich-any evaluation
// introduces when MULTIPLE contending sandwich opportunities land on the SAME pool
// in one block.
//
// sandwich-any values every victim on a FRESH copy of the EXACT pre-victim state,
// so two victims on the same pool are each credited the FULL price impact as if
// they were alone — an upper bound (INDEPENDENT band). In reality the opps are
// mutually exclusive on the shared pool: executing one moves the price the next
// would have exploited. The SERIALIZED band applies the opps in block order on the
// CUMULATIVE state (each opp's valuation runs on the post-previous-opp state), so
// the sum can only SHRINK. The gap (independentUpper - serializedLower) is the
// concurrency over-count, reported in both opportunity count and BNB wei.
//
// Strictly READ-ONLY: it only ever Copy()s state and diffs balances; it never
// commits, never submits, and never mutates canonical state. Every block and
// per-opp evaluation is wrapped in defer/recover.
package simengine

import (
	"math/big"
	"os"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/strategy"
)

// isSerializeSamePoolEnabled reports the SIMENGINE_SERIALIZE_SAMEPOOL feature flag
// (informational only; the serialize mode always measures both bands — the flag is
// surfaced in the startup log so an operator can confirm the intended run).
func isSerializeSamePoolEnabled() bool {
	return os.Getenv("SIMENGINE_SERIALIZE_SAMEPOOL") == "1"
}

// ssVictim is one decoded same-pool victim captured during the block replay, with
// everything the independent/serialized re-valuation needs. preState is the EXACT
// pre-victim state (a Copy taken in the hook).
type ssVictim struct {
	txIndex    int
	victimTx   *types.Transaction
	preState   *state.StateDB
	pair       common.Address
	token0Side bool
	amountIn   *big.Int
	isV3       bool
}

// runSandwichSerializeBacktest subscribes to chain heads and runs the serialized
// same-pool contention measurement on every imported block. Read-only, crash-safe.
func (r *dryRunner) runSandwichSerializeBacktest() {
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	r.swCfg = defaultSandwichConfig()

	log.Info("SimEngine dry-run SANDWICH-SERIALIZE (same-pool contention) loop started",
		"flashBps", r.swCfg.flashBps, "minVictimUSD", r.swCfg.minVictimUSD,
		"samePoolFlag", isSerializeSamePoolEnabled())

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run sandwich-serialize loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run sandwich-serialize recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.sandwichSerializeBlock(head)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run sandwich-serialize loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// sandwichSerializeBlock replays one block tx-by-tx on its parent state, groups the
// decoded victims by pool, and for each pool measures the INDEPENDENT upper bound
// vs the SERIALIZED lower bound. Read-only.
func (r *dryRunner) sandwichSerializeBlock(head *types.Header) {
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

	// Collect victims grouped by pool, in block order.
	byPool := make(map[common.Address][]ssVictim)
	preState := parentState.Copy()

	onTx := func(i int, tx *types.Transaction, receipt *types.Receipt, statedb *state.StateDB) {
		victimPreState := preState
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
			byPool[pair] = append(byPool[pair], ssVictim{
				txIndex:    i,
				victimTx:   tx,
				preState:   victimPreState,
				pair:       pair,
				token0Side: token0Side,
				amountIn:   amountIn,
				isV3:       isV3,
			})
		}
	}

	if _, err := r.e.ApplyOnStateHooked(parentState.Copy(), r.bc, head, block.Transactions(), nil, onTx); err != nil {
		return
	}

	// Per-pool dual evaluation. The block's full ordered tx list is passed so the
	// serialized (lower) band can advance its cumulative substrate through ALL
	// intervening real txs (not just the contending victims), so both bands share
	// the identical canonical substrate.
	txs := block.Transactions()
	for pair, vs := range byPool {
		if len(vs) == 0 {
			continue
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Warn("SimEngine sandwich-serialize per-pool recovered from panic",
						"block", number, "pool", pair, "panic", rec)
				}
			}()
			r.serializeExecutePool(number, head, pair, vs, txs)
		}()
	}

	processed := r.blocks.Add(1)
	if r.cfg.TallyEvery > 0 && processed%r.cfg.TallyEvery == 0 {
		r.logSandwichSerializeTally(processed)
	}
}

// serializeExecutePool runs both bands for a single pool's contending victims on
// the SAME canonical substrate (the round-1 fix):
//
//   - INDEPENDENT (upper): value each victim on a FRESH Copy of its OWN exact
//     pre-victim state (= the real canonical post-tx[i-1] state, ALL intervening
//     block txs applied), exactly as sandwich-any does today. Accumulate to
//     ssIndependentUpper.
//   - SERIALIZED (lower): value the victims in block order on ONE evolving CUM copy
//     that is advanced through ALL INTERVENING REAL TXS between contending victims
//     (re-executing the real txs in order on cum), NOT just the victim txs. This is
//     the bug fix: round-1 advanced cum by ONLY the on-pool victim txs, dropping
//     every intervening non-victim tx, so the lower band ran on a stripped state and
//     the gap conflated mutual-exclusion with a stripped-state artifact (biasing the
//     over-count UPWARD). With the full canonical replay the lower band's substrate
//     matches the upper band's exactly; the ONLY difference between bands is that the
//     lower band reuses ONE evolving cum copy so contending same-pool opps see each
//     other's realized price impact, while the upper band isolates each on a fresh
//     copy.
//
// cum starts at the first victim's pre-state (the canonical state right before the
// first contending tx) and is advanced through txs[firstIdx .. lastVictimIdx] in
// order. A real tx that REVERTS on the mutated cum substrate aborts the whole pool
// group (both bands dropped + ssRevertedSteps incremented) — never silently
// continued on an inconsistent copy. txs is the block's full ordered tx list.
//
// Returns nothing; it accumulates into the ss* counters/dists directly. The
// invariant independentUpper >= serializedLower is enforced in serializeWithFallback
// (a violation EXCLUDES the group; it is NEVER clamped).
func (r *dryRunner) serializeExecutePool(number uint64, head *types.Header, pair common.Address, vs []ssVictim, txs types.Transactions) {
	r.ssPoolsProcessed.Add(1)
	if len(vs) > 1 {
		r.ssGroupsFormed.Add(1)
	}

	// INDEPENDENT (upper): each victim isolated on its own pre-state.
	var upperNet big.Int
	var upperCount uint64
	for _, v := range vs {
		net, ok := r.serializeValueVictim(head, v.preState, v)
		if !ok || net == nil || net.Sign() <= 0 {
			continue
		}
		upperNet.Add(&upperNet, net)
		upperCount++
		r.ssUpperDist.Add(net, strategy.SandwichGasUnits(v.isV3), weiToBNBFloat(net), ssDexLabel(v.isV3), 2)
	}

	// SERIALIZED (lower): value on ONE cum copy advanced through the FULL canonical
	// tx sequence. cum starts at the first contending victim's pre-state, which is
	// the canonical state right before tx[vs[0].txIndex].
	var lowerNet big.Int
	var lowerCount uint64
	cum := vs[0].preState.Copy()
	cumIdx := vs[0].txIndex // cum has applied real txs [0, cumIdx).

	for _, v := range vs {
		// Advance cum through ALL intervening real txs in [cumIdx, v.txIndex) so the
		// substrate matches the upper band's canonical preState for this victim. A
		// revert here means cum diverged from canonical — abort the whole group.
		if !r.serializeAdvanceTo(head, cum, txs, &cumIdx, v.txIndex) {
			r.ssRevertedSteps.Add(1)
			return // abort this pool group; both bands dropped (no partial accounting).
		}
		// Value this victim's sandwich on the cum substrate (now identical to the
		// canonical state apart from the prior contending opps already folded in).
		net, ok := r.serializeValueVictim(head, cum, v)
		if ok && net != nil && net.Sign() > 0 {
			lowerNet.Add(&lowerNet, net)
			lowerCount++
			r.ssLowerDist.Add(net, strategy.SandwichGasUnits(v.isV3), weiToBNBFloat(net), ssDexLabel(v.isV3), 2)
		}
		// Fold in this victim's own canonical tx (the realized price move) so the next
		// contending opp sees it. This is the serialization: cum now includes tx
		// vs[i].txIndex.
		if v.txIndex >= cumIdx {
			if !r.serializeApplyOne(head, cum, txs, v.txIndex) {
				r.ssRevertedSteps.Add(1)
				return
			}
			cumIdx = v.txIndex + 1
		}
	}

	// PROPERTY: serialization can only reduce or equal the independent sum. A
	// violation EXCLUDES the group from both aggregates (never clamps).
	r.serializeWithFallback(number, pair, &upperNet, &lowerNet, upperCount, lowerCount)
}

// serializeAdvanceTo re-executes the real block txs in [*cumIdx, target) onto the
// cumulative state in order, advancing *cumIdx. Returns false (and leaves the copy
// to be discarded by the caller) if any tx fails on the mutated substrate — that
// signals a state divergence and the caller aborts the pool group. Read-only on
// canonical state (cum is the caller's throwaway Copy).
func (r *dryRunner) serializeAdvanceTo(head *types.Header, cum *state.StateDB, txs types.Transactions, cumIdx *int, target int) (ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Warn("SimEngine sandwich-serialize advance recovered from panic",
				"block", head.Number, "target", target, "panic", rec)
			ok = false
		}
	}()
	for *cumIdx < target {
		if *cumIdx < 0 || *cumIdx >= len(txs) {
			return false
		}
		if !r.serializeApplyOne(head, cum, txs, *cumIdx) {
			return false
		}
		*cumIdx++
	}
	return true
}

// serializeApplyOne applies the single real tx at index idx onto cum via the
// canonical single-tx applier (system-tx skip + snapshot/revert-on-hard-error,
// tolerant of on-chain reverts). Returns false ONLY on a hard ApplyTransaction
// error (the copy has been rolled back but signals a state divergence; the caller
// aborts the pool group). Read-only on canonical state (cum is a throwaway Copy).
func (r *dryRunner) serializeApplyOne(head *types.Header, cum *state.StateDB, txs types.Transactions, idx int) bool {
	if idx < 0 || idx >= len(txs) || cum == nil {
		return false
	}
	return r.e.ApplyCanonicalTxOnto(cum, r.bc, head, txs[idx])
}

// serializeValueVictim values ONE victim's sandwich on the supplied pre-state and
// returns the net BNB profit (>0 when net-positive). It mirrors
// sandwichAnyEvaluateVictim's gates and numeraire/net math EXACTLY but is
// side-effect-free on the sa*/br* counters (the serialize bands have their own
// accounting). ok=false means the victim is uneligible/unfundable/unsupported.
func (r *dryRunner) serializeValueVictim(head *types.Header, preState *state.StateDB, v ssVictim) (net *big.Int, ok bool) {
	if preState == nil || head == nil || v.victimTx == nil || v.amountIn == nil || v.amountIn.Sign() <= 0 || (v.pair == common.Address{}) {
		return nil, false
	}
	pool, okm := r.e.resolvePoolMeta(preState.Copy(), r.bc, head, v.pair, v.isV3)
	if !okm || !pool.ok || (pool.token0 == common.Address{}) || (pool.token1 == common.Address{}) {
		return nil, false
	}
	victimTokenIn := pool.token0
	if !v.token0Side {
		victimTokenIn = pool.token1
	}
	if _, hasOther := poolOther(pool, victimTokenIn); !hasOther {
		return nil, false
	}
	if pool.isV3 && !pool.v3Supported {
		return nil, false
	}
	numToken, numKind, hasNum := poolNumeraire(pool)
	if !hasNum {
		return nil, false
	}
	attackerTokenIn := numToken
	attackerTokenOut, hasOther := poolOther(pool, attackerTokenIn)
	if !hasOther {
		return nil, false
	}
	probeCopy := preState.Copy()
	if _, fundable := r.e.resolveTokenSlots(probeCopy, r.bc, head, attackerTokenIn); !fundable {
		return nil, false
	}
	if _, fundable := r.e.resolveTokenSlots(probeCopy, r.bc, head, attackerTokenOut); !fundable {
		return nil, false
	}
	wbnbUSD := liveWbnbPriceUSD(preState)

	var victimAmountInNumeraire *big.Int
	victimSpentNumeraire := victimTokenIn == numToken
	if victimSpentNumeraire {
		victimAmountInNumeraire = v.amountIn
	}
	numTokenUSD := tokenUSDPrice(numToken, wbnbUSD)
	if victimSpentNumeraire && numTokenUSD > 0 {
		if !strategy.VictimAboveThreshold(v.amountIn, numTokenUSD, r.swCfg.minVictimUSD) {
			return nil, false
		}
	} else if v.amountIn.Cmp(minVictimInputHeuristicWei) < 0 {
		return nil, false
	}

	frontrun, grossNum, gasUnits := r.e.optimalFrontrunAny(preState, r.bc, head, v.victimTx, pool, attackerTokenIn, v.amountIn, victimAmountInNumeraire)
	if grossNum == nil || grossNum.Sign() <= 0 {
		return nil, true // eligible but no positive gross on this state.
	}
	grossBNB := numeraireToBNB(grossNum, numKind, wbnbUSD)
	frontrunBNB := numeraireToBNB(frontrun, numKind, wbnbUSD)
	if grossBNB.Sign() <= 0 {
		return nil, true
	}
	eval := strategy.SandwichNet(frontrunBNB, grossBNB, r.cfg.GasPriceWei, gasUnits, r.swCfg.flashBps, r.cfg.BuilderBidWei)
	if !eval.Profitable {
		return nil, true
	}
	return new(big.Int).Set(eval.NetProfit), true
}

// serializeWithFallback finalizes a pool group's dual measurement. The round-1 bug
// was a ONE-SIDED FLOOR: when lower>upper it clamped the lower band DOWN to the
// upper band, which further inflated the over-count gap. A lower>upper violation is
// not a benign rounding artifact — it signals a STATE-DIVERGENCE / methodology
// break for that group (the lower band's evolving substrate diverged from the
// per-victim canonical substrate). The fix: COUNT such a group (ssDivergedGroups)
// and EXCLUDE it from BOTH aggregates rather than clamping. Never silently clamp.
func (r *dryRunner) serializeWithFallback(number uint64, pair common.Address, upperNet, lowerNet *big.Int, upperCount, lowerCount uint64) {
	if !serializeBandsValid(upperNet, lowerNet) {
		r.ssDivergedGroups.Add(1)
		log.Warn("sandwich-serialize INVARIANT violated (lower>upper) — EXCLUDING group from both aggregates",
			"block", number, "pool", pair,
			"upperNetWei", upperNet.String(), "lowerNetWei", lowerNet.String(),
			"upperCount", upperCount, "lowerCount", lowerCount)
		return
	}

	r.ssIndependentUpper.Add(upperCount)
	r.ssSerializedLower.Add(lowerCount)
	r.addSerializeBand(&r.ssUpperTotalNetWei, upperNet)
	r.addSerializeBand(&r.ssLowerTotalNetWei, lowerNet)
}

// serializeBandsValid reports whether the band pair respects the invariant
// upper >= lower (serialization can only reduce or equal the independent sum). A
// nil band is treated as zero. Extracted as a pure function so the divergence
// exclusion is unit-testable without an EVM.
func serializeBandsValid(upperNet, lowerNet *big.Int) bool {
	u := upperNet
	if u == nil {
		u = big.NewInt(0)
	}
	l := lowerNet
	if l == nil {
		l = big.NewInt(0)
	}
	return l.Cmp(u) <= 0
}

// addSerializeBand atomically adds delta to a band's running total (BNB wei).
func (r *dryRunner) addSerializeBand(p *atomic.Pointer[big.Int], delta *big.Int) {
	for {
		cur := p.Load()
		next := new(big.Int).Add(cur, delta)
		if p.CompareAndSwap(cur, next) {
			return
		}
	}
}

// ssDexLabel renders the per-band dex label.
func ssDexLabel(isV3 bool) string {
	if isV3 {
		return "pancake_v3"
	}
	return "v2_any"
}

// logSandwichSerializeTally emits the contention funnel + both bands and the
// over-count factor, plus the upper/lower distribution snapshots. Read-only.
func (r *dryRunner) logSandwichSerializeTally(processed uint64) {
	iu := r.ssIndependentUpper.Load()
	sl := r.ssSerializedLower.Load()
	upper := r.ssUpperTotalNetWei.Load()
	lower := r.ssLowerTotalNetWei.Load()

	// overcountFactor_pct = (upper-lower)*100/lower (BNB-wei terms), guarded.
	overcountPct := "n/a"
	if lower.Sign() > 0 {
		diff := new(big.Int).Sub(upper, lower)
		diff.Mul(diff, big.NewInt(100))
		diff.Quo(diff, lower)
		overcountPct = diff.String()
	}
	oppOvercount := int64(iu) - int64(sl)

	log.Info("sandwich-serialize tally",
		"processedBlocks", processed,
		"poolsProcessed", r.ssPoolsProcessed.Load(),
		"groupsFormed", r.ssGroupsFormed.Load(),
		"divergedGroupsExcluded", r.ssDivergedGroups.Load(),
		"revertedStepsAborted", r.ssRevertedSteps.Load(),
		"independentUpper", iu,
		"serializedLower", sl,
		"concurrencyOvercount", oppOvercount,
		"upperBNBWei", upper.String(),
		"lowerBNBWei", lower.String(),
		"overcountBNBWei", new(big.Int).Sub(upper, lower).String(),
		"overcountFactorXpct", overcountPct,
		"ts", time.Now().Format(time.RFC3339),
	)

	U := r.ssUpperDist.Snapshot()
	L := r.ssLowerDist.Snapshot()
	log.Info("sandwich-serialize dist",
		"processedBlocks", processed,
		"upper_samples", U.Count,
		"upper_BNB_p50", U.GrossUSDp50,
		"upper_BNB_p90", U.GrossUSDp90,
		"upper_BNB_p99", U.GrossUSDp99,
		"upper_BNB_max", U.GrossUSDMax,
		"lower_samples", L.Count,
		"lower_BNB_p50", L.GrossUSDp50,
		"lower_BNB_p90", L.GrossUSDp90,
		"lower_BNB_p99", L.GrossUSDp99,
		"lower_BNB_max", L.GrossUSDMax,
		"ts", time.Now().Format(time.RFC3339),
	)
}
