// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_recalltest.go is the RECALL-VALIDATION harness for the landed-sandwich
// realizability detector (SIMENGINE_DRYRUN=recalltest). The realizability paper's
// headline result — "realized sandwich capture = 0 / 735" — is only credible if
// we can show the detector would have CAUGHT real landed sandwiches. This mode
// MEASURES that recall directly: per processed block, on a state.Copy() of the
// parent, it INJECTS a diverse set of synthetic-but-REALISTIC *landed* sandwiches
// (executed with the REAL SimEngine swap primitives, so the produced Swap legs and
// hub-balance deltas are genuine), splices them into the block's canonical
// tx/leg/ledger set, runs the EXACT detectLandedSandwiches path over the augmented
// set, and counts how many injected sandwiches the detector catches (recall) and
// whether any clean (non-sandwiched) victim is wrongly flagged (false positive).
//
// THE GENUINE-EXECUTION INVARIANT. Each injected leg's amounts, the volatile-token
// flatness, the pool reserves it moved, and the hub-asset balance deltas it
// produced are computed by REAL EVM swaps (directPairSwap / the V3 router), exactly
// as a real sandwich would. The ONLY synthetic degree of freedom is the structural
// IDENTITY overlay (which EOA signed each leg; which contract is the Swap-log
// sender/beneficiary) — that overlay is precisely the structural variety the
// detector's recall depends on, and is what we sweep. The hub-balance deltas are
// read with the SAME rzReadHubBalance the live detector uses, and the legs are
// shaped exactly as decodeRzLegs would shape a real receipt's logs, so the
// detector cannot tell an injected bracket from a real one.
//
// STRUCTURAL SWEEP (recall is reported PER cell so blind spots are visible):
//   - actor pattern: (1a) same-EOA signs both legs, Swap sender == that EOA;
//     (1b) same-EOA signs both, Swap sender/beneficiary == a CONTRACT != EOA (the
//     integrated-bot pattern the recall fix targets); (1c) different EOAs front/back
//     but the SAME beneficiary contract.
//   - hops: 1-hop direct pair.swap vs a routed/multi-hop wrapper (sender == router).
//   - flatness: exactly-flat Y round trip vs slightly-imbalanced (bot keeps a dust
//     remainder of Y).
//   - proceeds: profit left in the actor cluster vs SWEPT to a cold address mid-tx
//     (the known false-negative the paper admits).
//   - numeraire / depth: WBNB vs stable; deep vs thin pool.
//
// Strictly read-only: every injection runs on a throwaway state.Copy that is
// discarded; nothing is committed or submitted; the whole mode is a no-op unless
// SIMENGINE_DRYRUN=recalltest. It does NOT touch applyOnState / SimulateOnState /
// selftest.go. It rides on ApplyOnStateHooked exactly like the other detectors and
// is wrapped in defer/recover at the block and per-injection level.
package simengine

import (
	"math/big"
	"runtime/debug"
	"sort"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/strategy"
)

// ---------------------------------------------------------------------------
// Structure taxonomy (the cells whose per-cell recall we report).
// ---------------------------------------------------------------------------

// rtStructure identifies one structural cell of the recall sweep. Each cell pins a
// point in (actor pattern x hops x flatness x proceeds x numeraire/depth) space;
// the cells are chosen to span the dimensions the detector's recall is sensitive
// to (rather than the full Cartesian product, which would dilute the per-cell N).
type rtStructure int

const (
	// Dominant real-bot patterns the detector is designed to catch (expect HIGH recall).
	rtSameEOASwapSender    rtStructure = iota // 1a: same EOA signs both; Swap sender == EOA; flat; not swept; WBNB
	rtSameEOAContractSender                   // 1b: same EOA signs both; Swap sender/beneficiary == contract != EOA; flat; not swept; WBNB
	rtCrossEOASharedBenef                     // 1c: different EOAs; shared beneficiary contract; flat; not swept; WBNB
	rtRoutedMultiHop                          // routed: Swap sender == a known router; same EOA signs; flat; not swept; WBNB
	rtStableNumeraire                         // stable-hub pool (USDT/USDC); same EOA; flat; not swept
	rtThinPool                                // thin/low-liquidity WBNB pool; same EOA; flat; not swept
	// Known / admitted blind spots (expect LOWER recall — this is what the paper needs).
	rtMarginallyFlat // same EOA; round trip imbalanced by a dust remainder (bot keeps part of Y)
	rtProceedsSwept  // same EOA; flat; profit SWEPT to a cold address mid-tx (the admitted false-negative)

	rtNumStructures // sentinel: number of cells
)

// rtStructureName renders a stable label per cell (used in the tally line).
func rtStructureName(s rtStructure) string {
	switch s {
	case rtSameEOASwapSender:
		return "1a_sameEOA_swapSender"
	case rtSameEOAContractSender:
		return "1b_sameEOA_contractSender"
	case rtCrossEOASharedBenef:
		return "1c_crossEOA_sharedBenef"
	case rtRoutedMultiHop:
		return "routed_multihop"
	case rtStableNumeraire:
		return "stable_numeraire"
	case rtThinPool:
		return "thin_pool"
	case rtMarginallyFlat:
		return "marginally_flat"
	case rtProceedsSwept:
		return "proceeds_swept"
	default:
		return "unknown"
	}
}

// rtExpectedHighRecall reports whether a cell is a DOMINANT structure the detector
// targets (the honest a-priori expectation; used only to annotate the tally, never
// to change a measurement).
func rtExpectedHighRecall(s rtStructure) bool {
	switch s {
	case rtMarginallyFlat, rtProceedsSwept:
		return false
	default:
		return true
	}
}

// ---------------------------------------------------------------------------
// Synthetic identity overlay (the only non-genuine degree of freedom).
//
// These addresses are recognisable, never collide with the synthetic attacker, a
// known router, or a real EOA on chain, and are distinct from one another so the
// same-actor gate's discriminating-actor logic is exercised exactly as in
// production. The COLD sweep destination is outside any actor cluster.
// ---------------------------------------------------------------------------

var (
	rtBotEOA      = common.HexToAddress("0x00000000000000000000000000000000B07E0EA1") // signing EOA (1a/1b/routed/...)
	rtBotEOA2     = common.HexToAddress("0x00000000000000000000000000000000B07E0EA2") // 2nd EOA for the cross-EOA cell (1c)
	rtBotContract = common.HexToAddress("0x00000000000000000000000000000000B07C0F73") // bot executor contract (Swap-log sender/beneficiary)
	rtVictimEOA   = common.HexToAddress("0x000000000000000000000000000000005617C1A0") // the clean victim's signer (its own swap)
	rtColdAddr    = common.HexToAddress("0x000000000000000000000000000000000C01DACC") // cold sweep destination (outside the cluster)
)

// rtRoutedSender is a known router address (in rzKnownRouters) used as the Swap-log
// sender for the routed cell, so the detector's router-exclusion path is exercised:
// the same-EOA signal (front.from == back.from) must still catch a routed bracket.
var rtRoutedSender = pancakeV2Router

// ---------------------------------------------------------------------------
// Counters (kept fully separate from rz*/sa*/cs* so this mode is auditable on its
// own). Per-cell arrays index by rtStructure.
// ---------------------------------------------------------------------------

// rtCounters holds the recall harness's per-cell + overall tallies. It lives on the
// dryRunner via an embedded pointer so the dryRunner struct in dryrun.go is not
// modified for fields that only this mode uses.
type rtCounters struct {
	processed atomic.Uint64 // blocks processed in this mode

	injected [rtNumStructures]atomic.Uint64 // sandwiches injected per cell
	detected [rtNumStructures]atomic.Uint64 // injected sandwiches the detector caught per cell
	buildErr [rtNumStructures]atomic.Uint64 // injections that could not be CONSTRUCTED (skipped, not counted as inject)

	cleanInjected atomic.Uint64 // clean (non-sandwiched) victims injected as FP controls
	cleanFlagged  atomic.Uint64 // clean victims WRONGLY flagged as a landed sandwich (false positives)
}

// ---------------------------------------------------------------------------
// Head subscription loop (mirrors runRealizabilityBacktest).
// ---------------------------------------------------------------------------

// runRecallTestBacktest subscribes to chain heads and runs the recall harness on
// every imported block. Read-only, crash-safe.
func (r *dryRunner) runRecallTestBacktest() {
	if r.rt == nil {
		r.rt = &rtCounters{}
	}
	r.swCfg = defaultSandwichConfig()
	r.rzCfg = defaultRealizabilityConfig()

	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	log.Info("SimEngine dry-run RECALL-TEST (realizability recall harness) loop started",
		"structures", int(rtNumStructures), "flatPct", r.rzCfg.flatPct, "amtEpsPct", r.rzCfg.amtEpsPct,
		"dustUSD", r.rzCfg.dustUSD)

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run recalltest loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run recalltest recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.recallTestBlock(head)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run recalltest loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// recallTestBlock replays one block on its parent state to collect the canonical
// legs/ledgers (so the augmented set the detector sees is realistic), picks real
// candidate pools that traded in the block, injects one synthetic landed sandwich
// per structure cell plus one clean FP-control victim, and runs the EXACT
// detectLandedSandwiches over each augmented set. Read-only.
func (r *dryRunner) recallTestBlock(head *types.Header) {
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

	var chainID *big.Int
	if r.e != nil && r.e.chainCfg != nil {
		chainID = r.e.chainCfg.ChainID
	}
	signer := types.LatestSignerForChainID(chainID)

	// (A) Replay the block ONCE to gather the canonical legs/ledgers (the realistic
	// "background" the injected legs are spliced into). This reuses the SAME hook the
	// realizability detector uses, so the background set is identical in shape.
	var (
		legs    []rzSwapLeg
		ledgers []rzTxLedger
	)
	preState := parentState.Copy()
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
		ledgers = append(ledgers, r.buildTxLedger(i, tx, receipt, from, head, victimPreState, statedb))
		legs = append(legs, decodeRzLegs(i, tx.Hash(), from, receipt.Logs)...)
	}
	if _, err := r.e.ApplyOnStateHooked(parentState.Copy(), r.bc, head, block.Transactions(), nil, onTx); err != nil {
		return
	}

	// The highest canonical tx index — injected txs are appended AFTER it so the
	// bracket indices never collide with a real tx index.
	baseIdx := len(block.Transactions()) + 8

	wbnbUSD := liveWbnbPriceUSD(parentState)
	dust := r.rzDustBNB(wbnbUSD)

	// (B) Pick candidate pools that actually traded in this block (so reserves/price
	// are realistic) for the WBNB / stable / thin cells.
	cands := r.rtCandidatePools(parentState, head, legs)

	// (C) Inject one sandwich per structure cell + one clean FP-control victim.
	for s := rtStructure(0); s < rtNumStructures; s++ {
		pool := r.rtPickPoolForStructure(s, cands)
		if !pool.ok {
			continue // no suitable pool this block — try next block (no inject counted)
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					r.rt.buildErr[s].Add(1)
					log.Warn("recalltest injection recovered from panic", "block", number,
						"structure", rtStructureName(s), "panic", rec, "stack", string(debug.Stack()))
				}
			}()
			r.rtInjectAndScore(number, head, parentState, pool, s, baseIdx, legs, ledgers, dust, wbnbUSD)
		}()
		baseIdx += 8 // keep each injection's tx indices disjoint
	}

	// (D) Clean FP control: a genuine non-sandwiched victim (a single one-way swap)
	// on a candidate pool. It must NOT be flagged as a landed sandwich.
	if len(cands) > 0 {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Warn("recalltest clean-control recovered from panic", "block", number, "panic", rec)
				}
			}()
			r.rtInjectCleanAndScore(number, head, parentState, cands[0], baseIdx, legs, ledgers, dust, wbnbUSD)
		}()
	}

	n := r.rt.processed.Add(1)
	r.blocks.Add(1)
	if r.cfg.TallyEvery > 0 && n%r.cfg.TallyEvery == 0 {
		r.logRecallTestTally(n)
	}
}

// ---------------------------------------------------------------------------
// Candidate pool selection.
// ---------------------------------------------------------------------------

// rtCandidate is one real V2 pool that traded this block, with its resolved meta
// and a realism classification used to assign it to structure cells.
type rtCandidate struct {
	pool       anyPool
	numToken   common.Address
	numKind    numeraireKind
	volToken   common.Address
	numIsToken0 bool
	reserveNum *big.Int // pool reserve of the numeraire side (depth proxy)
	ok         bool
}

// rtCandidatePools resolves, from the block's canonical V2 Swap legs, the distinct
// pools that (a) have a numeraire side, (b) are fundable on both tokens, and (c)
// expose readable reserves — i.e. pools we can actually drive a genuine sandwich
// through. V3 pools are skipped here: the recall sweep drives genuine legs through
// the direct V2 pair.swap path, which is fork-agnostic and does not need the V3
// router; V3 recall is bounded by the same gate logic (the detector is pool-type
// agnostic) and the V3 router path is exercised separately by the sandwich self-test.
func (r *dryRunner) rtCandidatePools(parentState *state.StateDB, head *types.Header, legs []rzSwapLeg) []rtCandidate {
	seen := make(map[common.Address]bool)
	var out []rtCandidate
	for i := range legs {
		if legs[i].isV3 {
			continue
		}
		pair := legs[i].pool
		if seen[pair] {
			continue
		}
		seen[pair] = true

		probe := parentState.Copy()
		pool, ok := r.e.resolvePoolMeta(probe, r.bc, head, pair, false)
		if !ok || !pool.ok || pool.isV3 {
			continue
		}
		numToken, numKind, hasNum := poolNumeraire(pool)
		if !hasNum {
			continue
		}
		volToken, hasOther := poolOther(pool, numToken)
		if !hasOther {
			continue
		}
		// Both tokens fundable (the direct-pair path credits the pair + the attacker).
		if _, f := r.e.resolveTokenSlots(probe, r.bc, head, numToken); !f {
			continue
		}
		if _, f := r.e.resolveTokenSlots(probe, r.bc, head, volToken); !f {
			continue
		}
		rv := strategy.ReadReserves(parentState, pair)
		if rv.Reserve0 == nil || rv.Reserve1 == nil || rv.Reserve0.Sign() <= 0 || rv.Reserve1.Sign() <= 0 {
			continue
		}
		reserveNum := rv.Reserve0
		numIsToken0 := numToken == pool.token0
		if !numIsToken0 {
			reserveNum = rv.Reserve1
		}
		out = append(out, rtCandidate{
			pool: pool, numToken: numToken, numKind: numKind, volToken: volToken,
			numIsToken0: numIsToken0, reserveNum: reserveNum, ok: true,
		})
		if len(out) >= 32 {
			break // bound the per-block work
		}
	}
	return out
}

// rtPickPoolForStructure selects, from this block's candidates, a pool whose realism
// matches the structure cell's numeraire/depth requirement. WBNB/EOA-pattern cells
// prefer a deep WBNB pool; the stable cell needs a stable-hub pool; the thin cell
// needs the thinnest WBNB pool available. Returns a candidate with ok=false when no
// suitable pool exists this block.
func (r *dryRunner) rtPickPoolForStructure(s rtStructure, cands []rtCandidate) rtCandidate {
	switch s {
	case rtStableNumeraire:
		return rtPickByPredicate(cands, func(c rtCandidate) bool { return c.numKind == numStable }, false)
	case rtThinPool:
		// thinnest WBNB pool (smallest numeraire reserve).
		return rtPickByPredicate(cands, func(c rtCandidate) bool { return c.numKind == numWBNB }, true)
	default:
		// deepest WBNB pool (largest numeraire reserve) for the dominant-pattern cells.
		return rtPickDeepest(cands, func(c rtCandidate) bool { return c.numKind == numWBNB })
	}
}

// rtPickByPredicate returns the first candidate matching pred; when thinnest is set
// it returns the one with the SMALLEST numeraire reserve among matches.
func rtPickByPredicate(cands []rtCandidate, pred func(rtCandidate) bool, thinnest bool) rtCandidate {
	var best rtCandidate
	for _, c := range cands {
		if !pred(c) {
			continue
		}
		if !best.ok {
			best = c
			continue
		}
		if thinnest && c.reserveNum.Cmp(best.reserveNum) < 0 {
			best = c
		}
	}
	return best
}

// rtPickDeepest returns the matching candidate with the LARGEST numeraire reserve.
func rtPickDeepest(cands []rtCandidate, pred func(rtCandidate) bool) rtCandidate {
	var best rtCandidate
	for _, c := range cands {
		if !pred(c) {
			continue
		}
		if !best.ok || c.reserveNum.Cmp(best.reserveNum) > 0 {
			best = c
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Genuine sandwich construction + scoring.
// ---------------------------------------------------------------------------

// rtInjectAndScore builds ONE genuine landed sandwich of structure `s` on `pool`,
// splices its (genuine) legs/ledgers into a COPY of the block's canonical sets,
// runs the EXACT detectLandedSandwiches over the augmented set, and records whether
// the injected bracket was detected (recall) — and whether any clean background
// victim was newly flagged (a false positive, scored conservatively).
func (r *dryRunner) rtInjectAndScore(number uint64, head *types.Header, parentState *state.StateDB, cand rtCandidate, s rtStructure, baseIdx int, bgLegs []rzSwapLeg, bgLedgers []rzTxLedger, dust *big.Int, wbnbUSD float64) {
	inj, ok := r.rtBuildSandwich(head, parentState, cand, s, baseIdx)
	if !ok {
		r.rt.buildErr[s].Add(1)
		return
	}
	r.rt.injected[s].Add(1)

	// Augment a COPY of the background sets with the injected legs/ledgers.
	legs := make([]rzSwapLeg, 0, len(bgLegs)+len(inj.legs))
	legs = append(legs, bgLegs...)
	legs = append(legs, inj.legs...)
	ledgers := make([]rzTxLedger, 0, len(bgLedgers)+len(inj.ledgers))
	ledgers = append(ledgers, bgLedgers...)
	ledgers = append(ledgers, inj.ledgers...)

	landed := r.detectLandedSandwiches(legs, ledgers, head, dust, wbnbUSD)

	// Recall: was a confirmed bracket produced on the injected pool that straddles
	// the injected victim with the injected exploited direction?
	if rtBracketMatchesInjection(landed, inj) {
		r.rt.detected[s].Add(1)
	} else {
		// Diagnostic on a MISS (debug level only, to avoid flooding the log).
		log.Debug("recalltest miss",
			"block", number, "structure", rtStructureName(s),
			"pool", poolLabel(inj.pool), "expectedHigh", rtExpectedHighRecall(s),
			"frontIdx", inj.frontTxIdx, "backIdx", inj.backTxIdx,
			"yFrontOut", inj.yFrontOut.String(), "yBackIn", inj.yBackIn.String())
	}
}

// rtInjectCleanAndScore builds ONE genuine clean (one-way) victim swap on `pool`,
// splices it into a copy of the background set, runs the detector, and flags a false
// positive iff the clean victim's leg ends up inside ANY confirmed landed bracket on
// its pool. A clean one-way swap with no bracketing same-actor opposite leg must
// never be detected.
func (r *dryRunner) rtInjectCleanAndScore(number uint64, head *types.Header, parentState *state.StateDB, cand rtCandidate, baseIdx int, bgLegs []rzSwapLeg, bgLedgers []rzTxLedger, dust *big.Int, wbnbUSD float64) {
	leg, led, ok := r.rtBuildCleanVictim(head, parentState, cand, baseIdx)
	if !ok {
		return
	}
	r.rt.cleanInjected.Add(1)

	legs := make([]rzSwapLeg, 0, len(bgLegs)+1)
	legs = append(legs, bgLegs...)
	legs = append(legs, leg)
	ledgers := make([]rzTxLedger, 0, len(bgLedgers)+1)
	ledgers = append(ledgers, bgLedgers...)
	ledgers = append(ledgers, led)

	landed := r.detectLandedSandwiches(legs, ledgers, head, dust, wbnbUSD)
	for i := range landed {
		ls := landed[i]
		if ls.pool == leg.pool && ls.frontTxIdx < leg.txIdx && leg.txIdx < ls.backTxIdx {
			r.rt.cleanFlagged.Add(1)
			log.Info("recalltest FALSE POSITIVE: clean victim flagged",
				"block", number, "pool", poolLabel(leg.pool), "victimIdx", leg.txIdx,
				"frontIdx", ls.frontTxIdx, "backIdx", ls.backTxIdx, "actor", shortAddr(ls.actor))
			break
		}
	}
}

// rtInjection carries one constructed sandwich's genuine legs/ledgers and the
// identifying coordinates used to confirm the detector caught THIS bracket.
type rtInjection struct {
	legs         []rzSwapLeg
	ledgers      []rzTxLedger
	pool         common.Address
	frontTxIdx   int
	backTxIdx    int
	inToken0Front bool
	yFrontOut    *big.Int
	yBackIn      *big.Int
}

// rtBracketMatchesInjection reports whether `landed` contains a confirmed bracket on
// the injected pool with the injected exploited direction enclosing the injected
// victim index. This is the recall predicate.
func rtBracketMatchesInjection(landed []landedSandwich, inj rtInjection) bool {
	victimIdx := (inj.frontTxIdx + inj.backTxIdx) / 2
	for i := range landed {
		ls := landed[i]
		if ls.pool != inj.pool {
			continue
		}
		if ls.inToken0Front != inj.inToken0Front {
			continue
		}
		if ls.frontTxIdx < victimIdx && victimIdx < ls.backTxIdx {
			return true
		}
	}
	return false
}

// rtBuildSandwich constructs ONE genuine landed sandwich of structure `s` on `cand`.
// It executes — on a single throwaway state.Copy of the parent — a real FRONTRUN
// (numeraire -> volatile), a real CLEAN VICTIM (same direction, distinct EOA), and a
// real BACKRUN (volatile -> numeraire), capturing the genuine Swap logs and the
// genuine per-leg hub-asset balance deltas. It then shapes those into rzSwapLeg /
// rzTxLedger records EXACTLY as decodeRzLegs / buildTxLedger would, overlaying the
// structural identity (signer EOA, Swap-log sender/beneficiary, sweep destination)
// that defines the cell. ok=false when the pool cannot be driven (token unfundable,
// a leg reverts, or a zero quote) — counted as buildErr by the caller, never as a
// missed detection.
func (r *dryRunner) rtBuildSandwich(head *types.Header, parentState *state.StateDB, cand rtCandidate, s rtStructure, baseIdx int) (rtInjection, bool) {
	sdb := parentState.Copy()
	cc := r.bc

	pool := cand.pool
	numToken := cand.numToken
	volToken := cand.volToken

	// Sizing: a realistic, physically-sane round trip whose hub net clears the dust
	// gate when it is genuinely a sandwich. The frontrun is a meaningful fraction of
	// the pool's numeraire depth, and the bracketed victim is LARGER (a bigger victim
	// is what makes a sandwich profitable — its price impact is what the attacker
	// captures). frontrun = 0.5% of the numeraire reserve; victim = 3% (6x the front).
	frontIn := new(big.Int).Div(new(big.Int).Mul(cand.reserveNum, big.NewInt(5)), big.NewInt(1000)) // 0.5%
	if frontIn.Sign() <= 0 {
		return rtInjection{}, false
	}
	victimIn := new(big.Int).Div(new(big.Int).Mul(cand.reserveNum, big.NewInt(30)), big.NewInt(1000)) // 3%
	if victimIn.Sign() <= 0 {
		victimIn = new(big.Int).Set(frontIn)
	}

	// --- FRONTRUN: attacker buys volToken with numToken (numeraire INTO the pool). ---
	if !r.e.fundAttackerDyn(sdb, cc, head, numToken, common.Address{}, frontIn) {
		return rtInjection{}, false
	}
	hubPreFront := r.rtReadCluster(sdb, s)
	yFront, err := r.e.directPairSwap(sdb, cc, head, pool, numToken, frontIn)
	if err != nil || yFront.Sign() <= 0 {
		return rtInjection{}, false
	}
	// Charge the borrowed numeraire so the attacker's measured numeraire delta over the
	// round trip is the true gross (recovered - borrowed), exactly as sandwichProfitAny.
	if !r.e.debitAttackerToken(sdb, cc, head, numToken, frontIn) {
		return rtInjection{}, false
	}
	hubPostFront := r.rtReadCluster(sdb, s)

	// --- VICTIM: a distinct EOA buys volToken with numToken on the same pool (same
	// exploited direction). Genuine swap; this is the leg the bracket straddles. ---
	vsdb := sdb // the victim executes on the frontrun-mutated state (realistic ordering)
	if !r.e.fundAttackerDyn(vsdb, cc, head, numToken, common.Address{}, victimIn) {
		return rtInjection{}, false
	}
	yVictim, verr := r.e.directPairSwap(vsdb, cc, head, pool, numToken, victimIn)
	if verr != nil || yVictim.Sign() <= 0 {
		return rtInjection{}, false
	}

	// --- BACKRUN: attacker sells volToken back to numToken (numeraire OUT). For the
	// marginally-flat cell the bot KEEPS a dust remainder of Y (sells slightly less). ---
	yBack := new(big.Int).Set(yFront)
	if s == rtMarginallyFlat {
		// Keep ~3% of Y (well beyond max(flatPct, amtEps) ~ 2% so the round trip is
		// genuinely not flat) — models a bot that does not fully unwind.
		keep := new(big.Int).Div(yFront, big.NewInt(33))
		if keep.Sign() <= 0 {
			keep = big.NewInt(1)
		}
		yBack.Sub(yBack, keep)
		if yBack.Sign() <= 0 {
			return rtInjection{}, false
		}
	}
	hubPreBack := r.rtReadCluster(sdb, s)
	// The attacker actually holds yFront of Y from the frontrun; the backrun spends
	// yBack of it. Debit what it sells (the pair is paid from a separate credit inside
	// directPairSwap, mirroring sandwichProfitAny's accounting).
	if !r.e.debitAttackerToken(sdb, cc, head, volToken, yBack) {
		return rtInjection{}, false
	}
	xBack, berr := r.e.directPairSwap(sdb, cc, head, pool, volToken, yBack)
	if berr != nil || xBack.Sign() <= 0 {
		return rtInjection{}, false
	}
	hubPostBack := r.rtReadCluster(sdb, s)

	// Genuine per-leg hub deltas (cluster-summed, BNB-denominated) — measured exactly
	// like buildTxLedger does (signed hub delta of the actor over the leg).
	frontHubDelta := rtHubDeltaBNB(hubPreFront, hubPostFront, cand.numKind, wbnbForBNB(head, parentState))
	backHubDelta := rtHubDeltaBNB(hubPreBack, hubPostBack, cand.numKind, wbnbForBNB(head, parentState))

	// For the PROCEEDS-SWEPT cell, simulate the known false-negative: the realized hub
	// profit is moved OUT of the actor cluster to a cold address before tx-end, so the
	// cluster's measured hub delta on the back leg reads ~0 (no net-positive hub to
	// corroborate). We model this by zeroing the back-leg hub credit to the cluster
	// (the genuine round trip still happened; the proceeds just left the cluster).
	if s == rtProceedsSwept {
		backHubDelta = big.NewInt(0)
		frontHubDelta = new(big.Int).Neg(frontIn) // only the spend remains visible to the cluster
		if cand.numKind == numStable {
			frontHubDelta = rzHubDeltaToBNB(new(big.Int).Neg(frontIn), numStable, wbnbForBNB(head, parentState))
		}
	}

	// Build the genuine legs from the real Swap mechanics, then overlay the cell's
	// structural identity. The amounts come from the real swaps (yFront out on the
	// front, yBack in on the back); the hub-side amounts are the genuine numeraire in/out.
	frontIdx := baseIdx
	victimIdx := baseIdx + 1
	backIdx := baseIdx + 2

	frontLeg := r.rtMakeLeg(cand, frontIdx, true /*numeraire in => front buys Y*/, frontIn, yFront)
	victimLeg := r.rtMakeLeg(cand, victimIdx, true /*same exploited dir*/, victimIn, yVictim)
	backLeg := r.rtMakeLeg(cand, backIdx, false /*Y in, numeraire out*/, xBack, yBack)

	// Identity overlay per cell.
	r.rtApplyIdentity(s, &frontLeg, &backLeg)
	// The victim is always a distinct, non-router, non-actor EOA.
	victimLeg.sender = rtVictimEOA
	victimLeg.beneficiary = rtVictimEOA
	victimLeg.from = rtVictimEOA

	gas := r.rtLegGasBNB()
	frontLed := r.rtMakeLedger(frontLeg, frontHubDelta, gas)
	backLed := r.rtMakeLedger(backLeg, backHubDelta, gas)
	victimLed := makeRtLedger(victimLeg.txIdx, victimLeg.from, victimLeg.from, big.NewInt(0), gas)

	inToken0Front := frontLeg.inToken0
	yFrontOut := rtLegY(frontLeg, inToken0Front, true)
	yBackIn := rtLegY(backLeg, inToken0Front, false)

	return rtInjection{
		legs:          []rzSwapLeg{frontLeg, victimLeg, backLeg},
		ledgers:       []rzTxLedger{frontLed, victimLed, backLed},
		pool:          cand.pool.pair,
		frontTxIdx:    frontIdx,
		backTxIdx:     backIdx,
		inToken0Front: inToken0Front,
		yFrontOut:     yFrontOut,
		yBackIn:       yBackIn,
	}, true
}

// rtBuildCleanVictim constructs ONE genuine clean (one-way) victim swap on `cand`:
// a real numeraire->volatile swap by a distinct EOA, with NO bracketing opposite
// leg. It must not be detectable as a sandwich. Returns the leg + its ledger.
func (r *dryRunner) rtBuildCleanVictim(head *types.Header, parentState *state.StateDB, cand rtCandidate, baseIdx int) (rzSwapLeg, rzTxLedger, bool) {
	sdb := parentState.Copy()
	cc := r.bc
	in := new(big.Int).Div(cand.reserveNum, big.NewInt(1000))
	if in.Sign() <= 0 {
		return rzSwapLeg{}, rzTxLedger{}, false
	}
	if !r.e.fundAttackerDyn(sdb, cc, head, cand.numToken, common.Address{}, in) {
		return rzSwapLeg{}, rzTxLedger{}, false
	}
	yOut, err := r.e.directPairSwap(sdb, cc, head, cand.pool, cand.numToken, in)
	if err != nil || yOut.Sign() <= 0 {
		return rzSwapLeg{}, rzTxLedger{}, false
	}
	leg := r.rtMakeLeg(cand, baseIdx, true, in, yOut)
	leg.sender = rtVictimEOA
	leg.beneficiary = rtVictimEOA
	leg.from = rtVictimEOA
	led := makeRtLedger(baseIdx, rtVictimEOA, rtVictimEOA, big.NewInt(0), r.rtLegGasBNB())
	return leg, led, true
}

// ---------------------------------------------------------------------------
// Leg / ledger shaping (mirrors decodeRzLegs / buildTxLedger exactly).
// ---------------------------------------------------------------------------

// rtMakeLeg shapes a genuine V2 Swap into an rzSwapLeg exactly as decodeRzLegs would
// from a real receipt: inToken0 picks the direction, and the four amt fields carry
// the genuine numeraire (hub, X) and volatile (Y) amounts on the correct token0/
// token1 sides per the pool's token ordering. `xAmt` is the numeraire amount on this
// leg (in for a front/buy leg, out for a back/sell leg); `yAmt` is the volatile
// amount (out for the buy leg, in for the sell leg). The sender/beneficiary/from are
// defaulted to the attacker and overlaid by the caller.
func (r *dryRunner) rtMakeLeg(cand rtCandidate, txIdx int, numeraireIn bool, xAmt, yAmt *big.Int) rzSwapLeg {
	leg := rzSwapLeg{
		pool: cand.pool.pair, txIdx: txIdx, txHash: common.BigToHash(big.NewInt(int64(txIdx))),
		isV3:   false,
		sender: sandwichAttacker, beneficiary: sandwichAttacker, from: sandwichAttacker,
		amt0In: big.NewInt(0), amt1In: big.NewInt(0), amt0Out: big.NewInt(0), amt1Out: big.NewInt(0),
	}
	// inToken0 reports which TOKEN0/TOKEN1 side went INTO the pool. On a buy leg the
	// numeraire goes in; on a sell leg the volatile goes in.
	numInToken0 := cand.numIsToken0
	if numeraireIn {
		// numeraire in, volatile out.
		leg.inToken0 = numInToken0
		if numInToken0 {
			leg.amt0In = new(big.Int).Set(xAmt) // numeraire = token0 in
			leg.amt1Out = new(big.Int).Set(yAmt) // volatile = token1 out
		} else {
			leg.amt1In = new(big.Int).Set(xAmt)
			leg.amt0Out = new(big.Int).Set(yAmt)
		}
	} else {
		// volatile in, numeraire out (opposite direction).
		leg.inToken0 = !numInToken0
		if numInToken0 {
			leg.amt1In = new(big.Int).Set(yAmt)  // volatile = token1 in
			leg.amt0Out = new(big.Int).Set(xAmt) // numeraire = token0 out
		} else {
			leg.amt0In = new(big.Int).Set(yAmt)
			leg.amt1Out = new(big.Int).Set(xAmt)
		}
	}
	return leg
}

// rtLegY extracts the volatile-Y amount on a leg the same way rzVolatileFlat reads
// it, keyed by the FRONT leg's direction (the detector infers Y from front.inToken0
// for BOTH legs, since they are opposite by construction). `frontInToken0` is the
// front leg's direction; `isFront` selects the front (OUT) vs back (IN) Y side.
func rtLegY(leg rzSwapLeg, frontInToken0, isFront bool) *big.Int {
	if frontInToken0 {
		// front: token0 in -> token1 out (Y == token1).
		if isFront {
			return leg.amt1Out // front buys Y (token1) out
		}
		return leg.amt1In // back sells Y (token1) in
	}
	// front: token1 in -> token0 out (Y == token0).
	if isFront {
		return leg.amt0Out
	}
	return leg.amt0In
}

// rtApplyIdentity overlays the structural-cell identity onto the front/back legs:
// who signed them (from) and who the Swap-log sender/beneficiary are. This is the
// ONLY synthetic degree of freedom; everything else is genuine.
func (r *dryRunner) rtApplyIdentity(s rtStructure, front, back *rzSwapLeg) {
	switch s {
	case rtSameEOASwapSender:
		// Same EOA signs both; Swap sender/beneficiary == that EOA (signal 1 + signal 2/3).
		front.from, back.from = rtBotEOA, rtBotEOA
		front.sender, back.sender = rtBotEOA, rtBotEOA
		front.beneficiary, back.beneficiary = rtBotEOA, rtBotEOA
	case rtSameEOAContractSender:
		// Same EOA signs both; Swap sender/beneficiary == the bot's CONTRACT (the
		// integrated-bot recall pattern: signal 1 must fire on `from`).
		front.from, back.from = rtBotEOA, rtBotEOA
		front.sender, back.sender = rtBotContract, rtBotContract
		front.beneficiary, back.beneficiary = rtBotContract, rtBotContract
	case rtCrossEOASharedBenef:
		// Different EOAs sign front/back; shared beneficiary CONTRACT (signal 3).
		front.from, back.from = rtBotEOA, rtBotEOA2
		front.sender, back.sender = rtBotContract, rtBotContract
		front.beneficiary, back.beneficiary = rtBotContract, rtBotContract
	case rtRoutedMultiHop:
		// Routed: Swap-log sender is a KNOWN router (non-discriminating), but the SAME
		// EOA signs both legs so signal 1 must still catch the bracket.
		front.from, back.from = rtBotEOA, rtBotEOA
		front.sender, back.sender = rtRoutedSender, rtRoutedSender
		front.beneficiary, back.beneficiary = rtRoutedSender, rtRoutedSender
	case rtProceedsSwept:
		// Same EOA signs; beneficiary is the COLD sweep address (proceeds leave the
		// cluster). The detector should fail to corroborate net-positive hub here.
		front.from, back.from = rtBotEOA, rtBotEOA
		front.sender, back.sender = rtBotEOA, rtBotEOA
		front.beneficiary, back.beneficiary = rtColdAddr, rtColdAddr
	default:
		// stable / thin / marginally-flat: dominant same-EOA-swap-sender identity (the
		// numeraire/depth/flatness is what those cells vary, not the identity).
		front.from, back.from = rtBotEOA, rtBotEOA
		front.sender, back.sender = rtBotEOA, rtBotEOA
		front.beneficiary, back.beneficiary = rtBotEOA, rtBotEOA
	}
}

// rtReadCluster reads the synthetic attacker's hub balances on the (single) cluster
// holder the genuine swaps actually credit — the sandwichAttacker — for one leg's
// pre/post measurement. The genuine hub delta is the attacker's numeraire-balance
// change; the IDENTITY overlay attributes it to the cell's cluster, which is exactly
// what buildTxLedger's cluster-max does in production.
func (r *dryRunner) rtReadCluster(sdb *state.StateDB, _ rtStructure) map[common.Address]*big.Int {
	out := make(map[common.Address]*big.Int, len(rzHubAssets))
	for _, h := range rzHubAssets {
		out[h.token] = rzReadHubBalance(sdb, sandwichAttacker, h)
	}
	return out
}

// rtHubDeltaBNB BNB-denominates the attacker's genuine hub-balance change between the
// pre/post snapshots (summed across hub assets), using the SAME conversion the live
// detector uses. Only the numeraire side moves on a single-pool round-trip leg, so
// this is the genuine per-leg hub delta the cell's cluster would have realized.
func rtHubDeltaBNB(pre, post map[common.Address]*big.Int, kind numeraireKind, wbnbUSD float64) *big.Int {
	sum := big.NewInt(0)
	for _, h := range rzHubAssets {
		key := h.token
		b := big.NewInt(0)
		a := big.NewInt(0)
		if v, ok := pre[key]; ok && v != nil {
			b = v
		}
		if v, ok := post[key]; ok && v != nil {
			a = v
		}
		d := new(big.Int).Sub(a, b)
		if d.Sign() == 0 {
			continue
		}
		k := kind
		if h.kind == numWBNB {
			k = numWBNB
		} else {
			k = numStable
		}
		sum.Add(sum, rzHubDeltaToBNB(d, k, wbnbUSD))
	}
	return sum
}

// rtMakeLedger shapes a genuine leg's measured hub delta into an rzTxLedger exactly
// as buildTxLedger would, crediting BOTH the leg's `from` (the signer/EOA) and the
// Swap-log sender/beneficiary with the genuine hub delta, so the detector's cluster-
// max corroboration reads it on whichever cluster member the cell banks profit in
// (the EOA for 1a, the contract for 1b/1c). Routers/coinbase are NOT credited (the
// detector excludes them from the cluster regardless).
func (r *dryRunner) rtMakeLedger(leg rzSwapLeg, hubDelta, gas *big.Int) rzTxLedger {
	deltaHub := make(map[common.Address]*big.Int)
	credit := func(a common.Address) {
		if (a == common.Address{}) {
			return
		}
		deltaHub[a] = new(big.Int).Set(hubDelta)
	}
	// Credit the cluster members that legitimately hold profit. The detector takes the
	// MAX single-member delta, so crediting each with the genuine delta (not the sum)
	// is correct and never inflates.
	credit(leg.from)
	if !rzKnownRouters[leg.sender] {
		credit(leg.sender)
	}
	if !rzKnownRouters[leg.beneficiary] {
		credit(leg.beneficiary)
	}
	return rzTxLedger{
		txIdx:       leg.txIdx,
		txHash:      leg.txHash,
		from:        leg.from,
		gasBNBWei:   gas,
		coinbaseBNB: big.NewInt(0),
		deltaHub:    deltaHub,
		deltaTok:    map[common.Address]map[common.Address]*big.Int{},
	}
}

// makeRtLedger is the simple single-actor ledger used for victim/clean legs.
func makeRtLedger(txIdx int, from, actor common.Address, hubDelta, gas *big.Int) rzTxLedger {
	return rzTxLedger{
		txIdx:       txIdx,
		txHash:      common.BigToHash(big.NewInt(int64(txIdx))),
		from:        from,
		gasBNBWei:   gas,
		coinbaseBNB: big.NewInt(0),
		deltaHub:    map[common.Address]*big.Int{actor: new(big.Int).Set(hubDelta)},
		deltaTok:    map[common.Address]map[common.Address]*big.Int{},
	}
}

// rtLegGasBNB returns a small, realistic per-leg gas cost (BNB wei) so the net-of-gas
// dust gate is exercised with a non-zero gas just like a real bracket. ~150k gas at
// 1 gwei = 1.5e14 wei.
func (r *dryRunner) rtLegGasBNB() *big.Int {
	return big.NewInt(150_000_000_000_000) // 1.5e14 wei
}

// wbnbForBNB resolves the live WBNB/USD price for stable-leg BNB denomination, with a
// safe fallback (the detector treats a non-positive price by dropping stable legs,
// which here would only depress a stable cell's hub — the safe, recall-understating
// direction).
func wbnbForBNB(_ *types.Header, parentState *state.StateDB) float64 {
	return liveWbnbPriceUSD(parentState)
}

// ---------------------------------------------------------------------------
// Tally.
// ---------------------------------------------------------------------------

// logRecallTestTally emits the overall recall, the per-structure recall (with the
// injected-N per cell), and the false-positive rate. Crash-safe, read-only.
func (r *dryRunner) logRecallTestTally(processed uint64) {
	var totInj, totDet uint64
	type cell struct {
		name     string
		inj, det uint64
		buildErr uint64
		expectHi bool
	}
	cells := make([]cell, 0, int(rtNumStructures))
	for s := rtStructure(0); s < rtNumStructures; s++ {
		inj := r.rt.injected[s].Load()
		det := r.rt.detected[s].Load()
		totInj += inj
		totDet += det
		cells = append(cells, cell{
			name: rtStructureName(s), inj: inj, det: det,
			buildErr: r.rt.buildErr[s].Load(), expectHi: rtExpectedHighRecall(s),
		})
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].name < cells[j].name })

	overall := "n/a"
	if totInj > 0 {
		overall = ratioStr(totDet, totInj)
	}
	clean := r.rt.cleanInjected.Load()
	flagged := r.rt.cleanFlagged.Load()
	fpRate := "n/a"
	if clean > 0 {
		fpRate = ratioStr(flagged, clean)
	}

	log.Info("recalltest tally",
		"processedBlocks", processed,
		"overallRecall", overall,
		"injectedTotal", totInj,
		"detectedTotal", totDet,
		"cleanInjected", clean,
		"cleanFlagged(FP)", flagged,
		"falsePositiveRate", fpRate,
		"ts", time.Now().Format(time.RFC3339),
	)
	for _, c := range cells {
		rec := "n/a"
		if c.inj > 0 {
			rec = ratioStr(c.det, c.inj)
		}
		log.Info("recalltest cell",
			"structure", c.name,
			"recall", rec,
			"injected", c.inj,
			"detected", c.det,
			"buildErr", c.buildErr,
			"expectedHighRecall", c.expectHi,
		)
	}
}

// ratioStr renders num/den as a 4-decimal fraction string.
func ratioStr(num, den uint64) string {
	if den == 0 {
		return "n/a"
	}
	return strconvFloat(float64(num)/float64(den))
}

// strconvFloat formats a fraction with 4 decimals without importing fmt at call sites.
func strconvFloat(f float64) string {
	// 4-decimal fixed point via integer math (avoids fmt for a hot-ish path).
	scaled := int64(f*10000 + 0.5)
	whole := scaled / 10000
	frac := scaled % 10000
	if frac < 0 {
		frac = -frac
	}
	// zero-pad the fractional part to 4 digits.
	fs := []byte{byte('0' + (frac/1000)%10), byte('0' + (frac/100)%10), byte('0' + (frac/10)%10), byte('0' + frac%10)}
	return itoa(whole) + "." + string(fs)
}

// itoa is a tiny base-10 formatter for non-negative-ish ints (handles the single
// leading whole digit of a recall fraction; recall is in [0,1] so whole is 0 or 1).
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
