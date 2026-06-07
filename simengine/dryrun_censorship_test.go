// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_censorship_test.go unit-proves the conjunctive gate of the censorship-
// differential (D) detector (SIMENGINE_DRYRUN=censorship) WITHOUT a full block
// replay or a live EVM: it builds the exact intermediate structures the live
// replay populates (the public-tx ledger, the per-block inclusion / replacement /
// orthogonality indices, and the landed-sandwich scan) and asserts the structural
// gate verdict (csStructuralVerdict) for the four spec scenarios:
//
//   - a DROPPED profitable public opp (available at seal, orthogonal, not
//     captured, not in the sealed block)            -> counted in D (csVerdictDropped);
//   - an INCLUDED one (same (sender,nonce) in the block under the SAME hash)
//                                                    -> control, D=0 (csVerdictIncludedControl);
//   - a PRIVATE-flow-touching one (opp pool written by a private sealed tx)
//                                                    -> excluded (csVerdictSkipNonOrthogonal);
//   - an INVALID/REPLACED one (nonce moved past / slot filled under a different
//     hash / failed validation)                     -> excluded (Skip* verdicts).
//
// These exercise csStructuralVerdict + the pubLedger + csIsPoolWriteTopic +
// csAlreadyCaptured directly, so they are independent of the EVM/state machinery
// (which the sandwich-any path already validates). The governing rule (do not
// over-state D) is checked by the negative cases: every ambiguity must EXCLUDE.
package simengine

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/strategy"
	"github.com/holiman/uint256"
)

// csTestPool / csTestSender are recognisable synthetic addresses for the gate
// tests; csTestOtherPool is a second pool used to prove pool-scoped orthogonality.
var (
	csTestPool      = common.HexToAddress("0x00000000000000000000000000000000000000F1")
	csTestOtherPool = common.HexToAddress("0x00000000000000000000000000000000000000F2")
	csTestSender    = common.HexToAddress("0x00000000000000000000000000000000000000A0")
)

// newCsCandidate builds a pubTx with a recognisable hash; it is the drop
// candidate fed to the structural gate. The full *types.Transaction is not
// needed by csStructuralVerdict (the EVM valuation, GATE 3, is the only consumer
// of c.tx, and these tests exercise the structural gates with validAtSeal passed
// explicitly), so we leave it nil.
func newCsCandidate(hash common.Hash, from common.Address, nonce uint64) *pubTx {
	return &pubTx{
		hash:      hash,
		from:      from,
		nonce:     nonce,
		firstSeen: time.Unix(1_000, 0),
	}
}

// emptyReplay builds a csReplayResult with empty (non-nil) indices.
func emptyReplay() *csReplayResult {
	return &csReplayResult{
		includedHashes:   make(map[common.Hash]bool),
		includedSlots:    make(map[csSenderNonce]common.Hash),
		privatePoolTouch: make(map[common.Address]int),
		anyPoolTouch:     make(map[common.Address]int),
	}
}

// csPath is a convenience: the candidate's path-pool set for the gate tests. The
// orthogonality gate is applied to every pool in this set, so a single-pool opp is
// expressed as a one-element slice.
func csPath(pools ...common.Address) []common.Address { return pools }

// TestCensorshipDroppedProfitableCounted: a candidate that is available at seal
// (nonce == parent nonce, valid), NOT in the sealed block, on a pool NOT touched
// by private flow, and NOT captured by a landed competitor IS counted toward D.
func TestCensorshipDroppedProfitableCounted(t *testing.T) {
	c := newCsCandidate(common.HexToHash("0xd00"), csTestSender, 7)
	rep := emptyReplay() // not included, not replaced, no private touch.

	v := csStructuralVerdict(c, 7 /*parentNonce==c.nonce*/, true /*validAtSeal*/, rep, csTestPool, true, csPath(csTestPool), nil /*no landed*/)
	if v != csVerdictDropped {
		t.Fatalf("verdict = %v, want csVerdictDropped (a dropped profitable public opp must count toward D)", v)
	}
}

// TestCensorshipIncludedNotCounted: the SAME candidate, but its hash IS in the
// sealed block (builder included it) -> control, D=0, never a drop.
func TestCensorshipIncludedNotCounted(t *testing.T) {
	c := newCsCandidate(common.HexToHash("0x111"), csTestSender, 7)
	rep := emptyReplay()
	// Builder included exactly THIS hash (and registered its (sender,nonce) slot).
	rep.includedHashes[c.hash] = true
	rep.includedSlots[csSenderNonce{from: csTestSender, nonce: 7}] = c.hash

	v := csStructuralVerdict(c, 7, true, rep, csTestPool, true, csPath(csTestPool), nil)
	if v != csVerdictIncludedControl {
		t.Fatalf("verdict = %v, want csVerdictIncludedControl (an included public opp is a control, not a drop)", v)
	}
}

// TestCensorshipPrivateFlowTouchingExcluded: a candidate on a pool that a PRIVATE
// sealed tx wrote is non-orthogonal (SUTVA fails) and must be EXCLUDED, never
// counted toward D — even though it is dropped, available, and not captured.
func TestCensorshipPrivateFlowTouchingExcluded(t *testing.T) {
	c := newCsCandidate(common.HexToHash("0xb01"), csTestSender, 7)
	rep := emptyReplay()
	// A private (not-public-seen) sealed tx wrote csTestPool at index 2.
	rep.privatePoolTouch[csTestPool] = 2

	v := csStructuralVerdict(c, 7, true, rep, csTestPool, true, csPath(csTestPool), nil)
	if v != csVerdictSkipNonOrthogonal {
		t.Fatalf("verdict = %v, want csVerdictSkipNonOrthogonal (private-flow-touching opp must be excluded)", v)
	}

	// Control: a private touch on a DIFFERENT pool does NOT exclude this opp.
	rep2 := emptyReplay()
	rep2.privatePoolTouch[csTestOtherPool] = 2
	if v := csStructuralVerdict(c, 7, true, rep2, csTestPool, true, csPath(csTestPool), nil); v != csVerdictDropped {
		t.Fatalf("verdict = %v, want csVerdictDropped (a private touch on a DIFFERENT pool must not exclude)", v)
	}
}

// TestCensorshipPathPoolOrthogonality: the orthogonality gate is applied to EVERY
// pool in the candidate's path, not just the chosen opp pool. A multi-hop candidate
// whose opp pool is clean but whose OTHER traversed pool was written by private flow
// must be EXCLUDED (its value is still entangled with private flow). This pins the
// fix for the "single-pool orthogonality blind spot" issue (overstates-D direction).
func TestCensorshipPathPoolOrthogonality(t *testing.T) {
	c := newCsCandidate(common.HexToHash("0xpa1"), csTestSender, 7)
	rep := emptyReplay()
	// The opp pool (csTestPool) is CLEAN, but a second hop (csTestOtherPool) was
	// written by private flow. The candidate traverses BOTH.
	rep.privatePoolTouch[csTestOtherPool] = 3

	if v := csStructuralVerdict(c, 7, true, rep, csTestPool, true, csPath(csTestPool, csTestOtherPool), nil); v != csVerdictSkipNonOrthogonal {
		t.Fatalf("verdict = %v, want csVerdictSkipNonOrthogonal (a private write on ANY path pool must exclude)", v)
	}
	// Sanity: if the candidate only traversed the clean opp pool, it is a drop.
	if v := csStructuralVerdict(c, 7, true, rep, csTestPool, true, csPath(csTestPool), nil); v != csVerdictDropped {
		t.Fatalf("verdict = %v, want csVerdictDropped (clean single-pool path must count)", v)
	}
}

// TestCensorshipAnySealedWriteExcluded: even a PUBLIC sealed write on a path pool
// excludes the candidate — the block-top parent valuation is stale once ANY sealed
// tx moved the pool, so the opp may not have survived the realized in-block
// trajectory. This is the upward-bias guard (value on stale block-top reserves).
func TestCensorshipAnySealedWriteExcluded(t *testing.T) {
	c := newCsCandidate(common.HexToHash("0xa5e"), csTestSender, 7)
	rep := emptyReplay()
	// No PRIVATE write on the opp pool, but SOME sealed tx (public or private) wrote
	// it (recorded in anyPoolTouch). The block-top valuation is stale -> EXCLUDE.
	rep.anyPoolTouch[csTestPool] = 4

	if v := csStructuralVerdict(c, 7, true, rep, csTestPool, true, csPath(csTestPool), nil); v != csVerdictSkipNonOrthogonal {
		t.Fatalf("verdict = %v, want csVerdictSkipNonOrthogonal (any sealed write on a path pool must exclude)", v)
	}
}

// TestCensorshipReplacedExcluded: a candidate whose (sender,nonce) slot was filled
// in the sealed block under a DIFFERENT hash (a repricing/replacement) must be
// EXCLUDED — the slot was not dropped, it was replaced.
func TestCensorshipReplacedExcluded(t *testing.T) {
	c := newCsCandidate(common.HexToHash("0x01d"), csTestSender, 7)
	rep := emptyReplay()
	// The slot is filled by a DIFFERENT hash (the repriced replacement).
	rep.includedSlots[csSenderNonce{from: csTestSender, nonce: 7}] = common.HexToHash("0x0ce")

	v := csStructuralVerdict(c, 7, true, rep, csTestPool, true, csPath(csTestPool), nil)
	if v != csVerdictSkipReplaced {
		t.Fatalf("verdict = %v, want csVerdictSkipReplaced (a replaced (sender,nonce) slot must be excluded)", v)
	}
}

// TestCensorshipInvalidExcluded: the availability gate. A nonce moved past the
// candidate (superseded/mined), a nonce gap, a non-attributable sender, and a
// failed static+stateful validation each EXCLUDE with the matching Skip verdict.
func TestCensorshipInvalidExcluded(t *testing.T) {
	rep := emptyReplay()

	// (a) account nonce moved PAST the candidate -> superseded/mined.
	c := newCsCandidate(common.HexToHash("0x111e"), csTestSender, 7)
	if v := csStructuralVerdict(c, 9 /*parent nonce 9 > 7*/, true, rep, csTestPool, true, csPath(csTestPool), nil); v != csVerdictSkipNonceMoved {
		t.Fatalf("verdict = %v, want csVerdictSkipNonceMoved", v)
	}

	// (b) nonce GAP -> not executable at seal.
	if v := csStructuralVerdict(c, 5 /*parent nonce 5 < 7*/, true, rep, csTestPool, true, csPath(csTestPool), nil); v != csVerdictSkipNonceGap {
		t.Fatalf("verdict = %v, want csVerdictSkipNonceGap", v)
	}

	// (c) non-attributable sender (zero from) -> can't anchor a nonce check.
	cZero := newCsCandidate(common.HexToHash("0xe0e"), common.Address{}, 7)
	if v := csStructuralVerdict(cZero, 7, true, rep, csTestPool, true, csPath(csTestPool), nil); v != csVerdictSkipInvalidAtSeal {
		t.Fatalf("verdict = %v, want csVerdictSkipInvalidAtSeal (zero sender)", v)
	}

	// (d) failed static+stateful validation (validAtSeal=false).
	if v := csStructuralVerdict(c, 7, false /*invalid*/, rep, csTestPool, true, csPath(csTestPool), nil); v != csVerdictSkipInvalidAtSeal {
		t.Fatalf("verdict = %v, want csVerdictSkipInvalidAtSeal (failed validation)", v)
	}
}

// TestCensorshipAlreadyCapturedExcluded: a dropped, available, orthogonal opp on a
// pool/side that a landed competitor ALREADY captured must be EXCLUDED — the value
// was taken, not left on the table by dropping. The opposite SIDE is not captured.
func TestCensorshipAlreadyCapturedExcluded(t *testing.T) {
	c := newCsCandidate(common.HexToHash("0xcab"), csTestSender, 7)
	rep := emptyReplay()
	// A landed competitor captured csTestPool on the token0Side==true direction.
	landed := []landedSandwich{{pool: csTestPool, inToken0Front: true}}

	if v := csStructuralVerdict(c, 7, true, rep, csTestPool, true /*captured side*/, csPath(csTestPool), landed); v != csVerdictSkipAlreadyCaptured {
		t.Fatalf("verdict = %v, want csVerdictSkipAlreadyCaptured (a captured pool/side must be excluded)", v)
	}
	// The OPPOSITE side was not captured -> still a drop.
	if v := csStructuralVerdict(c, 7, true, rep, csTestPool, false /*other side*/, csPath(csTestPool), landed); v != csVerdictDropped {
		t.Fatalf("verdict = %v, want csVerdictDropped (the uncaptured side must still count)", v)
	}
}

// TestCensorshipGateOrdering: the conjunction is strict — a candidate that is BOTH
// invalid AND would-be a drop is excluded on the FIRST failing gate (availability),
// never counted. This pins the lower-bound construction (any failure excludes).
func TestCensorshipGateOrdering(t *testing.T) {
	c := newCsCandidate(common.HexToHash("0x0bd"), csTestSender, 7)
	rep := emptyReplay()
	rep.privatePoolTouch[csTestPool] = 0 // also non-orthogonal
	// Nonce moved past (availability fails) takes precedence over orthogonality.
	if v := csStructuralVerdict(c, 99, true, rep, csTestPool, true, csPath(csTestPool), nil); v != csVerdictSkipNonceMoved {
		t.Fatalf("verdict = %v, want csVerdictSkipNonceMoved (availability is checked first)", v)
	}
}

// ---------------------------------------------------------------------------
// Public-mempool ledger tests.
// ---------------------------------------------------------------------------

// TestPubLedgerInsertAndSnapshot: inserts are indexed by hash and (sender,nonce),
// and snapshotBefore returns only txs with firstSeen strictly before the cutoff
// (the load-bearing public+availability margin).
func TestPubLedgerInsertAndSnapshot(t *testing.T) {
	l := newPubLedger(100, 0 /*no TTL*/)
	t0 := time.Unix(1_000, 0)
	early := &pubTx{hash: common.HexToHash("0x1"), from: csTestSender, nonce: 1, firstSeen: t0.Add(-2 * time.Second)}
	late := &pubTx{hash: common.HexToHash("0x2"), from: csTestSender, nonce: 2, firstSeen: t0.Add(2 * time.Second)}
	l.insert(early)
	l.insert(late)

	if !l.has(early.hash) || !l.has(late.hash) {
		t.Fatalf("both txs must be indexed by hash")
	}
	if fs, ok := l.firstSeenOf(early.hash); !ok || !fs.Equal(early.firstSeen) {
		t.Fatalf("firstSeenOf(early) = (%v,%v), want (%v,true)", fs, ok, early.firstSeen)
	}
	// snapshotBefore(t0) must include ONLY `early` (firstSeen < t0); `late` arrived
	// after t0 and is conservatively excluded (under-stating D, the safe direction).
	snap := l.snapshotBefore(t0)
	if len(snap) != 1 || snap[0].hash != early.hash {
		t.Fatalf("snapshotBefore(t0) = %d entries, want exactly the early tx", len(snap))
	}
}

// TestPubLedgerCapEviction: inserting beyond the cap evicts the OLDEST (FIFO).
func TestPubLedgerCapEviction(t *testing.T) {
	l := newPubLedger(2, 0)
	t0 := time.Unix(1_000, 0)
	a := &pubTx{hash: common.HexToHash("0xA"), from: csTestSender, nonce: 1, firstSeen: t0}
	b := &pubTx{hash: common.HexToHash("0xB"), from: csTestSender, nonce: 2, firstSeen: t0.Add(time.Second)}
	cc := &pubTx{hash: common.HexToHash("0xC"), from: csTestSender, nonce: 3, firstSeen: t0.Add(2 * time.Second)}
	l.insert(a)
	l.insert(b)
	l.insert(cc) // evicts a (oldest).

	if l.has(a.hash) {
		t.Fatalf("oldest tx must be evicted past the cap")
	}
	if !l.has(b.hash) || !l.has(cc.hash) {
		t.Fatalf("the two newest txs must remain")
	}
	if l.size() != 2 {
		t.Fatalf("ledger size = %d, want 2 (the cap)", l.size())
	}
}

// TestPubLedgerZeroSenderNotIndexedByNonce: a non-attributable (zero-from) tx is
// tracked by hash but NEVER indexed by (sender,nonce) — it can never anchor a
// nonce check (it would be a false-positive risk), per the governing rule.
func TestPubLedgerZeroSenderNotIndexedByNonce(t *testing.T) {
	l := newPubLedger(10, 0)
	z := &pubTx{hash: common.HexToHash("0xdead"), from: common.Address{}, nonce: 5, firstSeen: time.Unix(1, 0)}
	l.insert(z)
	if !l.has(z.hash) {
		t.Fatalf("zero-sender tx must still be tracked by hash (so it can match a sealed inclusion)")
	}
	l.mu.RLock()
	_, indexed := l.bySenderNonce[common.Address{}]
	l.mu.RUnlock()
	if indexed {
		t.Fatalf("zero-sender tx must NOT be indexed by (sender,nonce)")
	}
}

// TestCsIsPoolWriteTopic: the orthogonality test recognises Swap/Sync/Mint/Burn as
// pool writes and nothing else (a non-pool topic must not entangle a pool).
func TestCsIsPoolWriteTopic(t *testing.T) {
	writes := []common.Hash{strategy.SwapTopic0, strategy.V3SwapTopic0, strategy.SyncTopic0, rzMintTopic0, rzBurnTopic0}
	for _, w := range writes {
		if !csIsPoolWriteTopic(w) {
			t.Fatalf("topic %s must be recognised as a pool write", w.Hex())
		}
	}
	// An unrelated topic (e.g. ERC20 Transfer) is NOT a pool write.
	transfer := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	if csIsPoolWriteTopic(transfer) {
		t.Fatalf("a non-pool topic must not be treated as a pool write")
	}
}

// TestLog10Big: the pool-depth covariate is monotone and zero-safe.
func TestLog10Big(t *testing.T) {
	if got := log10Big(nil); got != 0 {
		t.Fatalf("log10Big(nil) = %v, want 0", got)
	}
	if got := log10Big(big.NewInt(0)); got != 0 {
		t.Fatalf("log10Big(0) = %v, want 0", got)
	}
	if got := log10Big(big.NewInt(1000)); got < 2.99 || got > 3.01 {
		t.Fatalf("log10Big(1000) = %v, want ~3", got)
	}
}

// ---------------------------------------------------------------------------
// Seal-time cutoff + inclusion-cost floor (the new conservative-direction knobs).
// ---------------------------------------------------------------------------

// TestCsSealCutoffUsesLocalClock: the availability cutoff is the LOCALLY-observed
// seal time minus the safety margin (the same clock as firstSeen), NOT the
// proposer-set head.Time. A non-zero margin only PULLS the cutoff earlier (fewer
// admitted candidates => under-states D, the safe direction).
func TestCsSealCutoffUsesLocalClock(t *testing.T) {
	r := &dryRunner{csCfg: censorshipConfig{sealMargin: 500 * time.Millisecond}}
	// head.Time is a wildly different (proposer) clock; it must be ignored when a
	// local seal time is present.
	head := &types.Header{Time: 9_999_999_999}
	localSeal := time.Unix(1_000, 0)

	got := r.csSealCutoff(head, localSeal)
	want := localSeal.Add(-500 * time.Millisecond)
	if !got.Equal(want) {
		t.Fatalf("csSealCutoff = %v, want %v (localSeal - margin, ignoring head.Time)", got, want)
	}
	// A larger margin must move the cutoff EARLIER (never later), so it can only
	// exclude more candidates.
	r2 := &dryRunner{csCfg: censorshipConfig{sealMargin: 2 * time.Second}}
	if got2 := r2.csSealCutoff(head, localSeal); !got2.Before(got) {
		t.Fatalf("a larger margin must move the cutoff earlier: got2=%v got=%v", got2, got)
	}
}

// TestCsInclusionCostFloorIsConservative: the inclusion-cost floor charges the
// candidate's gas at the conservative-HIGH inclusion price PLUS a non-zero builder-
// bid floor, so it is strictly positive and strictly above the gas-only cost. This
// pins the "use a high gas price + positive bid" fix (under-states D, the safe dir).
func TestCsInclusionCostFloorIsConservative(t *testing.T) {
	r := &dryRunner{}
	const gasUsed = 150_000
	floor := r.csInclusionCostFloorWei(gasUsed)
	if floor.Sign() <= 0 {
		t.Fatalf("inclusion floor must be strictly positive, got %v", floor)
	}
	gasOnly := new(big.Int).Mul(big.NewInt(gasUsed), csInclusionGasPriceWei)
	if floor.Cmp(gasOnly) <= 0 {
		t.Fatalf("inclusion floor %v must exceed gas-only cost %v (builder-bid floor must be additive)", floor, gasOnly)
	}
	// The conservative inclusion gas price must be >= BSC's generous 3 gwei searcher
	// default (the governing rule: a HIGHER price under-states D).
	if csInclusionGasPriceWei.Cmp(big.NewInt(3_000_000_000)) < 0 {
		t.Fatalf("inclusion gas price %v must be >= 3 gwei (conservative direction)", csInclusionGasPriceWei)
	}
	if csBuilderBidFloorWei.Sign() <= 0 {
		t.Fatalf("builder-bid floor must be strictly positive (a zero bid over-states D)")
	}
}

// ---------------------------------------------------------------------------
// GATE 3 post-block-state valuation (the over-statement fix).
//
// The fix values each dropped candidate ALONE on a COPY of the POST-SEALED-BLOCK
// state, not the stale pre-seal parent. The decision that determines counted-vs-
// excluded is: measure the executor's OWN hub-asset delta (rzActorHubDeltaBNB)
// between the BEFORE state and BEFORE+candidate, then net the candidate's own gas
// + the conservative builder inclusion floor; count toward D iff that net is > 0.
// These tests pin the invariant at that decision boundary with real StateDBs: an
// opp profitable on the PARENT state but CLOSED on the POST-BLOCK state is now
// EXCLUDED (the over-statement the isolation valuation produced), while an opp
// profitable on BOTH is still counted.
// ---------------------------------------------------------------------------

// csNewState builds a fresh in-memory StateDB for the valuation-boundary tests.
func csNewState(t *testing.T) *state.StateDB {
	t.Helper()
	sdb, err := state.New(common.Hash{}, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	return sdb
}

// csValueNetWei mirrors the GATE-3 arithmetic in censorshipValueOpp: the executor's
// OWN BNB-equivalent hub delta between `before` and `after`, net of the candidate's
// own gas and the conservative builder inclusion floor. > 0 means it counts toward
// D; <= 0 means it is excluded (csSkipClosedByBlock when measured on the post-block
// state). gasUsed/gasPriceWei are the candidate's own receipt-exact gas cost.
func csValueNetWei(r *dryRunner, before, after *state.StateDB, actor common.Address, gasUsed uint64, gasPriceWei *big.Int) *big.Int {
	const wbnbUSD = 600.0
	ownProfit := rzActorHubDeltaBNB(before, after, actor, wbnbUSD)
	ownGas := new(big.Int).Mul(new(big.Int).SetUint64(gasUsed), gasPriceWei)
	net := new(big.Int).Sub(ownProfit, ownGas)
	net.Sub(net, r.csInclusionCostFloorWei(gasUsed))
	return net
}

// TestCensorshipClosedByBlockExcluded: an arb that nets a large positive own-BNB
// delta on the PRE-SEAL PARENT state (the old isolation valuation would COUNT it)
// but whose value is CLOSED once the whole sealed block executes (zero/negative own
// delta on the post-block state) is now EXCLUDED. A second candidate profitable on
// BOTH states is still counted. This is the over-statement fix's core invariant.
func TestCensorshipClosedByBlockExcluded(t *testing.T) {
	r := &dryRunner{}
	actor := csTestSender
	const gasUsed = 150_000
	gasPrice := big.NewInt(1_000_000_000) // 1 gwei, the candidate's own effective price

	// A genuine arb: ~0.3 BNB of own native-BNB profit (well clears gas + inclusion
	// floor) — enough that the OLD parent-isolation valuation would have counted it.
	arbProfit := big.NewInt(300_000_000_000_000_000) // 0.3 BNB

	// --- PARENT-state valuation (the OLD, over-stating path): the candidate realises
	// the full arb profit, so it is (wrongly, in near-empty low-tip blocks) counted. ---
	parentBefore := csNewState(t)
	parentBefore.SetBalance(actor, uint256.NewInt(0), tracing.BalanceChangeUnspecified)
	parentAfter := csNewState(t)
	parentAfter.SetBalance(actor, uint256.MustFromBig(arbProfit), tracing.BalanceChangeUnspecified)

	netOnParent := csValueNetWei(r, parentBefore, parentAfter, actor, gasUsed, gasPrice)
	if netOnParent.Sign() <= 0 {
		t.Fatalf("sanity: arb must be profitable on the parent state (the old isolation path), got net=%v", netOnParent)
	}

	// --- POST-BLOCK-state valuation (the FIXED path): the sealed block already closed
	// the opp (e.g. the builder's own internalized arb), so the candidate realises NO
	// positive own delta on top of the realized block. net <= 0 -> EXCLUDED. ---
	postBlockBefore := csNewState(t)
	postBlockBefore.SetBalance(actor, uint256.MustFromBig(arbProfit), tracing.BalanceChangeUnspecified)
	postBlockAfter := csNewState(t)
	// Re-running the candidate after the block yields no further gain (the price moved
	// to no-arb); the actor's balance is unchanged by the candidate (only loses gas).
	postBlockAfter.SetBalance(actor, uint256.MustFromBig(arbProfit), tracing.BalanceChangeUnspecified)

	netOnPostBlock := csValueNetWei(r, postBlockBefore, postBlockAfter, actor, gasUsed, gasPrice)
	if netOnPostBlock.Sign() > 0 {
		t.Fatalf("an opp CLOSED by the sealed block must net <= 0 on the post-block state (EXCLUDED), got net=%v", netOnPostBlock)
	}

	// --- A candidate profitable on BOTH states (the opp genuinely survives the whole
	// block uncaptured) is STILL counted: own delta on top of the post-block state is
	// large enough to clear gas + the inclusion floor. ---
	survivorBefore := csNewState(t)
	survivorBefore.SetBalance(actor, uint256.MustFromBig(arbProfit), tracing.BalanceChangeUnspecified)
	survivorAfter := csNewState(t)
	survivorAfter.SetBalance(actor, uint256.MustFromBig(new(big.Int).Add(arbProfit, arbProfit)), tracing.BalanceChangeUnspecified)

	netSurvivor := csValueNetWei(r, survivorBefore, survivorAfter, actor, gasUsed, gasPrice)
	if netSurvivor.Sign() <= 0 {
		t.Fatalf("an opp that survives the whole sealed block still-profitable must still count toward D, got net=%v", netSurvivor)
	}
}

// TestCensorshipSkipClosedByBlockCounter: the new csSkipClosedByBlock counter is
// the EXCLUSION counter for opps the sealed block closed (revert / non-positive net
// on the post-block state). It must be addressable and start at zero, so each
// closed-by-block exclusion is auditable in the funnel (lower-bound direction).
func TestCensorshipSkipClosedByBlockCounter(t *testing.T) {
	r := &dryRunner{}
	if r.csSkipClosedByBlock.Load() != 0 {
		t.Fatalf("csSkipClosedByBlock must start at 0, got %d", r.csSkipClosedByBlock.Load())
	}
	r.csSkipClosedByBlock.Add(1)
	if r.csSkipClosedByBlock.Load() != 1 {
		t.Fatalf("csSkipClosedByBlock increment failed, got %d", r.csSkipClosedByBlock.Load())
	}
}

// TestCensorshipRoundTripGate: the gross-proceeds over-statement guard. A self-
// contained arb/backrun (the only own-positive-hub-delta that is genuine forwent
// MEV) must emit >= 2 directional Swap legs. A SINGLE-leg one-way swap (whose hub
// delta is gross sale proceeds, not profit) and a zero-leg candidate are EXCLUDED.
// This pins the fix for the single-hop $100+ "drops" from near-empty low-tip blocks.
func TestCensorshipRoundTripGate(t *testing.T) {
	if csIsRoundTrip(0) {
		t.Fatalf("0 swap legs is not a round trip (must be excluded)")
	}
	if csIsRoundTrip(1) {
		t.Fatalf("a SINGLE-leg one-way swap must NOT count as a round trip (its hub delta is gross sale proceeds, not arb profit — excluding it under-states D, the safe direction)")
	}
	if !csIsRoundTrip(2) {
		t.Fatalf("2 directional swap legs (acquire + dispose) IS the minimal self-contained arb signature and must count")
	}
	if !csIsRoundTrip(3) {
		t.Fatalf("a multi-hop (>=2 leg) arb must count as a round trip")
	}
}

// TestCensorshipSkipNotRoundTripCounter: the new csSkipNotRoundTrip counter is the
// EXCLUSION counter for single-leg one-way swaps; it must be addressable and start
// at zero so each gross-proceeds exclusion is auditable (lower-bound direction).
func TestCensorshipSkipNotRoundTripCounter(t *testing.T) {
	r := &dryRunner{}
	if r.csSkipNotRoundTrip.Load() != 0 {
		t.Fatalf("csSkipNotRoundTrip must start at 0, got %d", r.csSkipNotRoundTrip.Load())
	}
	r.csSkipNotRoundTrip.Add(1)
	if r.csSkipNotRoundTrip.Load() != 1 {
		t.Fatalf("csSkipNotRoundTrip increment failed, got %d", r.csSkipNotRoundTrip.Load())
	}
}

// TestCensorshipCrossBlockDedup: a public tx that stays pending across multiple
// heads must be credited to D-hat AT MOST ONCE. csMarkCreditedDrop returns false
// (credit it) the FIRST time a hash is seen and true (skip — already credited) on
// every repeat, so the same lingering opportunity cannot inflate D-hat block after
// block. Counting once instead of N times strictly under-states D (the safe dir).
func TestCensorshipCrossBlockDedup(t *testing.T) {
	r := &dryRunner{}
	h := common.HexToHash("0xabc1")

	if r.csMarkCreditedDrop(h) {
		t.Fatalf("first sighting of a drop hash must NOT be already-credited (it should be counted)")
	}
	// The SAME candidate lingers and is re-flagged on subsequent heads: each repeat
	// must report already-credited (skip), never re-adding V_i to D-hat.
	for i := 0; i < 5; i++ {
		if !r.csMarkCreditedDrop(h) {
			t.Fatalf("repeat sighting #%d of the same drop hash must be already-credited (skipped)", i+1)
		}
	}
	// A DIFFERENT candidate is a distinct opportunity and is credited on first sight.
	if r.csMarkCreditedDrop(common.HexToHash("0xdef2")) {
		t.Fatalf("a distinct drop hash must be credited on its first sighting")
	}
}

// ---------------------------------------------------------------------------
// SETTLE WINDOW (deferred-drop finalization) — the FINAL correctness fix.
//
// A candidate that passes every gate at block N is NOT credited to D immediately;
// it is enqueued and finalized at height N+K. It counts toward D ONLY IF it was
// STILL un-mined K blocks later. A candidate mined within the window was merely
// PENDING (delayed-inclusion), NOT censored, and is discarded (csSkipMinedLater)
// — the over-statement this fix removes. These tests pin: the mined index, the
// finalize-height boundary, the mined-later vs never-mined classification, the
// superseded discard, the config knob, and that D is credited ONLY at finalize.
// ---------------------------------------------------------------------------

// csTestOpp builds a minimal valid csOpp for the finalize tests (the value carrier
// + the fields logCensorshipOpp / the dist read). vBNBWei is V_i in BNB wei.
func csTestOpp(vBNBWei *big.Int) *csOpp {
	return &csOpp{
		pool:        csTestPool,
		isV3:        false,
		token0Side:  true,
		netBNBWei:   vBNBWei,
		grossBNBWei: new(big.Int).Add(vBNBWei, vBNBWei),
		gasUnits:    150_000,
		dexLabel:    "v2_any",
		hops:        2,
		numKind:     numWBNB,
		pathPools:   []common.Address{csTestPool},
	}
}

// newSettleRunner builds a dryRunner wired for the settle-window finalize tests: a
// fresh mined index, an empty pending queue, a dist, and a settleBlocks config.
// bc is left nil (the superseded check is nil-guarded), so these are pure unit
// tests of the finalize/enqueue logic with no EVM or live chain.
func newSettleRunner(settleBlocks uint64) *dryRunner {
	r := &dryRunner{}
	r.csDhatWei.Store(big.NewInt(0))
	r.csMined = newMinedIndex(2*settleBlocks + 256)
	r.csCfg = censorshipConfig{settleBlocks: settleBlocks}
	r.csDist = strategy.NewGrossDist(strategy.ParseGasPriceSweepGwei(""))
	return r
}

// TestMinedIndexRecordAndLookup: recordBlock indexes every tx hash at its height,
// minedHeight reads it back, and the EARLIEST height wins (a re-org resurrection
// must not post-date a tx — bias toward "mined", the safe direction).
func TestMinedIndexRecordAndLookup(t *testing.T) {
	m := newMinedIndex(1000)
	h1 := types.NewTx(&types.LegacyTx{Nonce: 1, Gas: 21000, GasPrice: big.NewInt(1)})
	h2 := types.NewTx(&types.LegacyTx{Nonce: 2, Gas: 21000, GasPrice: big.NewInt(1)})
	m.recordBlock(100, types.Transactions{h1})
	m.recordBlock(105, types.Transactions{h2})

	if ht, ok := m.minedHeight(h1.Hash()); !ok || ht != 100 {
		t.Fatalf("minedHeight(h1) = (%d,%v), want (100,true)", ht, ok)
	}
	if ht, ok := m.minedHeight(h2.Hash()); !ok || ht != 105 {
		t.Fatalf("minedHeight(h2) = (%d,%v), want (105,true)", ht, ok)
	}
	if _, ok := m.minedHeight(common.HexToHash("0xnotmined")); ok {
		t.Fatalf("an un-recorded hash must report not-mined")
	}
	// Re-recording the same hash at a LATER height must NOT post-date it (earliest wins).
	m.recordBlock(200, types.Transactions{h1})
	if ht, _ := m.minedHeight(h1.Hash()); ht != 100 {
		t.Fatalf("minedHeight(h1) after re-record = %d, want 100 (earliest wins)", ht)
	}
}

// TestMinedIndexEviction: entries older than the retention window are evicted so
// the index stays O(K blocks) — bounded memory.
func TestMinedIndexEviction(t *testing.T) {
	m := newMinedIndex(10) // retain 10 blocks behind the max seen
	old := types.NewTx(&types.LegacyTx{Nonce: 1, Gas: 21000, GasPrice: big.NewInt(1)})
	m.recordBlock(100, types.Transactions{old})
	if _, ok := m.minedHeight(old.Hash()); !ok {
		t.Fatalf("just-recorded hash must be present")
	}
	// Advance the head far past the retention window; the old entry must be evicted.
	recent := types.NewTx(&types.LegacyTx{Nonce: 2, Gas: 21000, GasPrice: big.NewInt(1)})
	m.recordBlock(200, types.Transactions{recent})
	if _, ok := m.minedHeight(old.Hash()); ok {
		t.Fatalf("an entry older than the retention window must be evicted")
	}
	if _, ok := m.minedHeight(recent.Hash()); !ok {
		t.Fatalf("the recent entry (within the window) must remain")
	}
}

// TestSettleNeverMinedCountedAfterFinalize: a candidate that is NEVER mined within
// its settle window is GENUINELY CENSORED — it is credited to D-hat, but ONLY after
// finalize fires at finalizeHeight (= flagBlock + K), never before.
func TestSettleNeverMinedCountedAfterFinalize(t *testing.T) {
	const K = 256
	r := newSettleRunner(K)
	const flagBlock = 1000
	V := big.NewInt(500_000_000_000_000_000) // 0.5 BNB

	r.csEnqueuePending(&pendingDrop{
		hash:           common.HexToHash("0xnevermined"),
		from:           csTestSender,
		nonce:          7,
		flagBlock:      flagBlock,
		finalizeHeight: flagBlock + K,
		netBNBWei:      new(big.Int).Set(V),
		grossBNBWei:    new(big.Int).Set(V),
		gasUnits:       150_000,
		opp:            csTestOpp(new(big.Int).Set(V)),
		headSnapshot:   &types.Header{Number: big.NewInt(flagBlock)},
	})

	// BEFORE the window elapses: finalize must NOT credit anything.
	r.csFinalizePending(flagBlock + K - 1)
	if r.csDhatCount.Load() != 0 {
		t.Fatalf("D must NOT be credited before the settle window elapses, got count=%d", r.csDhatCount.Load())
	}
	if r.csDhatWei.Load().Sign() != 0 {
		t.Fatalf("D-hat wei must be 0 before finalize, got %v", r.csDhatWei.Load())
	}

	// AT finalizeHeight: the candidate was never mined -> credit it to D-hat.
	r.csFinalizePending(flagBlock + K)
	if r.csDhatCount.Load() != 1 {
		t.Fatalf("a never-mined candidate must be credited to D after finalize, got count=%d", r.csDhatCount.Load())
	}
	if r.csDhatWei.Load().Cmp(V) != 0 {
		t.Fatalf("D-hat wei = %v, want %v (the frozen block-N value)", r.csDhatWei.Load(), V)
	}
	if r.csSkipMinedLater.Load() != 0 {
		t.Fatalf("a never-mined candidate must NOT increment csSkipMinedLater, got %d", r.csSkipMinedLater.Load())
	}
}

// TestSettleMinedLaterNotCounted: a candidate MINED within (flagBlock, finalizeHeight]
// was merely PENDING (delayed-inclusion), NOT censored -> it is DISCARDED from D
// (csSkipMinedLater) and counted as an included-comparable control (T=1). This is
// the core of the fix: delayed-inclusion never reaches D-hat.
func TestSettleMinedLaterNotCounted(t *testing.T) {
	const K = 256
	r := newSettleRunner(K)
	const flagBlock = 2000
	V := big.NewInt(700_000_000_000_000_000) // 0.7 BNB

	// The candidate's real tx hash; it gets mined 30 blocks after the flag block
	// (delayed-inclusion, well inside the K-block window).
	tx := types.NewTx(&types.LegacyTx{Nonce: 7, Gas: 150000, GasPrice: big.NewInt(1)})
	r.csMined.recordBlock(flagBlock+30, types.Transactions{tx})

	r.csEnqueuePending(&pendingDrop{
		hash:           tx.Hash(),
		from:           csTestSender,
		nonce:          7,
		flagBlock:      flagBlock,
		finalizeHeight: flagBlock + K,
		netBNBWei:      new(big.Int).Set(V),
		grossBNBWei:    new(big.Int).Set(V),
		gasUnits:       150_000,
		opp:            csTestOpp(new(big.Int).Set(V)),
		headSnapshot:   &types.Header{Number: big.NewInt(flagBlock)},
	})

	r.csFinalizePending(flagBlock + K)
	if r.csDhatCount.Load() != 0 {
		t.Fatalf("a delayed-inclusion (mined-later) candidate must NOT be credited to D, got count=%d", r.csDhatCount.Load())
	}
	if r.csDhatWei.Load().Sign() != 0 {
		t.Fatalf("D-hat wei must stay 0 for a mined-later candidate, got %v", r.csDhatWei.Load())
	}
	if r.csSkipMinedLater.Load() != 1 {
		t.Fatalf("a mined-later candidate must increment csSkipMinedLater, got %d", r.csSkipMinedLater.Load())
	}
	if r.csIncludedComparable.Load() != 1 {
		t.Fatalf("a mined-later candidate is a legitimate included-comparable control (T=1), want includedComparable=1, got %d", r.csIncludedComparable.Load())
	}
}

// TestSettleMinedAtOrBeforeFlagBlockNotDelayed: a hash whose EARLIEST mined height
// is <= flagBlock cannot be this still-pending drop (the drop was, by construction,
// dropped-FROM block N). The finalizer requires minedHeight > flagBlock, so such a
// stale index entry must NOT classify the candidate as delayed-inclusion; a never-
// mined-AFTER-flag candidate is credited.
func TestSettleMinedAtOrBeforeFlagBlockNotDelayed(t *testing.T) {
	const K = 64
	r := newSettleRunner(K)
	const flagBlock = 3000
	V := big.NewInt(100_000_000_000_000_000) // 0.1 BNB

	tx := types.NewTx(&types.LegacyTx{Nonce: 1, Gas: 150000, GasPrice: big.NewInt(1)})
	// Index says it was mined AT the flag block (not strictly after) — must not count
	// as delayed-inclusion.
	r.csMined.recordBlock(flagBlock, types.Transactions{tx})

	r.csEnqueuePending(&pendingDrop{
		hash:           tx.Hash(),
		flagBlock:      flagBlock,
		finalizeHeight: flagBlock + K,
		netBNBWei:      new(big.Int).Set(V),
		grossBNBWei:    new(big.Int).Set(V),
		opp:            csTestOpp(new(big.Int).Set(V)),
		headSnapshot:   &types.Header{Number: big.NewInt(flagBlock)},
	})
	r.csFinalizePending(flagBlock + K)
	if r.csSkipMinedLater.Load() != 0 {
		t.Fatalf("a hash mined AT/BEFORE the flag block must not be classified as delayed-inclusion, got skipMinedLater=%d", r.csSkipMinedLater.Load())
	}
	if r.csDhatCount.Load() != 1 {
		t.Fatalf("a candidate not mined strictly after the flag block must be credited, got count=%d", r.csDhatCount.Load())
	}
}

// TestSettleFinalizeFiresOnlyForElapsed: with multiple pending drops at different
// flag blocks, a single finalize call settles ONLY those whose finalizeHeight has
// elapsed; the rest stay queued (and pendingDrops depth reflects that).
func TestSettleFinalizeFiresOnlyForElapsed(t *testing.T) {
	const K = 100
	r := newSettleRunner(K)
	V := big.NewInt(10_000_000_000_000_000)

	// Drop A flagged at 1000 (finalizes at 1100); Drop B flagged at 1090 (finalizes at 1190).
	for _, fb := range []uint64{1000, 1090} {
		r.csEnqueuePending(&pendingDrop{
			hash:           common.BigToHash(big.NewInt(int64(fb))),
			flagBlock:      fb,
			finalizeHeight: fb + K,
			netBNBWei:      new(big.Int).Set(V),
			grossBNBWei:    new(big.Int).Set(V),
			opp:            csTestOpp(new(big.Int).Set(V)),
			headSnapshot:   &types.Header{Number: big.NewInt(int64(fb))},
		})
	}

	// At height 1100: only Drop A's window elapsed -> exactly one finalized.
	r.csFinalizePending(1100)
	if r.csDhatCount.Load() != 1 {
		t.Fatalf("only the elapsed drop must finalize at h=1100, got count=%d", r.csDhatCount.Load())
	}
	r.csPendingMu.Lock()
	remaining := len(r.csPending)
	r.csPendingMu.Unlock()
	if remaining != 1 {
		t.Fatalf("one drop must remain pending (window not yet elapsed), got %d", remaining)
	}

	// At height 1190: Drop B's window elapses too.
	r.csFinalizePending(1190)
	if r.csDhatCount.Load() != 2 {
		t.Fatalf("both drops must be finalized by h=1190, got count=%d", r.csDhatCount.Load())
	}
	r.csPendingMu.Lock()
	remaining = len(r.csPending)
	r.csPendingMu.Unlock()
	if remaining != 0 {
		t.Fatalf("queue must be drained after both windows elapse, got %d", remaining)
	}
}

// TestSettleBlocksEnvKnob: SIMENGINE_CS_SETTLE_BLOCKS overrides the default K, and
// the default is 256 when unset/invalid.
func TestSettleBlocksEnvKnob(t *testing.T) {
	t.Setenv("SIMENGINE_CS_SETTLE_BLOCKS", "")
	if c := defaultCensorshipConfig(); c.settleBlocks != 256 {
		t.Fatalf("default settleBlocks = %d, want 256", c.settleBlocks)
	}
	t.Setenv("SIMENGINE_CS_SETTLE_BLOCKS", "512")
	if c := defaultCensorshipConfig(); c.settleBlocks != 512 {
		t.Fatalf("settleBlocks from env = %d, want 512", c.settleBlocks)
	}
	// An invalid (non-positive / non-numeric) value falls back to the default.
	t.Setenv("SIMENGINE_CS_SETTLE_BLOCKS", "0")
	if c := defaultCensorshipConfig(); c.settleBlocks != 256 {
		t.Fatalf("invalid settleBlocks must fall back to 256, got %d", c.settleBlocks)
	}
}

// TestSettleNilMinedIndexDoesNotCredit: defensively, with no mined index the
// finalizer cannot prove "never mined", so it must DISCARD (not credit) — the safe
// lower-bound direction.
func TestSettleNilMinedIndexDoesNotCredit(t *testing.T) {
	r := &dryRunner{}
	r.csDhatWei.Store(big.NewInt(0))
	r.csDist = strategy.NewGrossDist(strategy.ParseGasPriceSweepGwei(""))
	V := big.NewInt(1_000_000_000_000_000_000)
	r.csEnqueuePending(&pendingDrop{
		hash:           common.HexToHash("0xnoindex"),
		flagBlock:      10,
		finalizeHeight: 20,
		netBNBWei:      V,
		grossBNBWei:    V,
		opp:            csTestOpp(V),
		headSnapshot:   &types.Header{Number: big.NewInt(10)},
	})
	r.csFinalizePending(20)
	if r.csDhatCount.Load() != 0 || r.csDhatWei.Load().Sign() != 0 {
		t.Fatalf("with no mined index the finalizer must NOT credit D (lower-bound dir), got count=%d wei=%v", r.csDhatCount.Load(), r.csDhatWei.Load())
	}
}

// TestSettlePendingMaxHighWater: the pending-queue depth high-water-mark tracks the
// max queue depth (a memory covariate).
func TestSettlePendingMaxHighWater(t *testing.T) {
	r := newSettleRunner(100)
	for i := 0; i < 5; i++ {
		r.csEnqueuePending(&pendingDrop{
			hash:           common.BigToHash(big.NewInt(int64(i))),
			flagBlock:      uint64(i),
			finalizeHeight: uint64(i) + 100,
			netBNBWei:      big.NewInt(1),
			grossBNBWei:    big.NewInt(1),
			opp:            csTestOpp(big.NewInt(1)),
			headSnapshot:   &types.Header{Number: big.NewInt(int64(i))},
		})
	}
	if r.csPendingMax.Load() != 5 {
		t.Fatalf("pendingDropsMax = %d, want 5", r.csPendingMax.Load())
	}
}
