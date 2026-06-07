// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_realizability_test.go unit-proves the landed-MEV detection core of the
// in-block counterfactual (SIMENGINE_DRYRUN=realizability) WITHOUT a full block
// replay: it builds synthetic Swap legs + per-tx hub-asset ledgers (the exact
// intermediate structures the live replay populates) and asserts the conjunctive
// detection rule (§5):
//
//   - a known same-actor opposite-direction bracket with a flat round-trip in the
//     volatile token and a net-positive hub delta IS detected as a landed sandwich
//     and MATCHES an ex-post opp on the same pool/victim/direction (captured);
//   - a clean victim (an ordinary swap on the pool with no bracketing same-actor
//     opposite legs) is NOT detected and falls to left-on-the-table.
//
// These exercise detectLandedSandwiches + rzMatchAndAttribute directly, so they
// are independent of the EVM/state machinery (which the sandwich-any path already
// validates). The governing rule (do not over-count captured) is checked by the
// negative cases: a router-shared beneficiary, an opposite-direction-missing
// bracket, a non-flat round trip, and a below-dust hub delta all leave the opp on
// the table.
package simengine

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/strategy"
)

// rzFakeHeader is a minimal header carrying only a recognisable coinbase
// (validator) distinct from every synthetic actor, so coinbase-based same-actor
// exclusions are exercised without it accidentally matching a test actor.
var rzFakeHeader = types.Header{
	Coinbase: common.HexToAddress("0x000000000000000000000000000000000000FEED"),
	Number:   big.NewInt(1),
}

// newTestRealizabilityRunner builds a dryRunner with just the realizability state
// the detection/matching code touches (no chain, no engine). The two big.Int
// accumulators are seeded so the atomic loads never nil-panic.
func newTestRealizabilityRunner() *dryRunner {
	r := &dryRunner{
		cfg:            DefaultDryRunConfig(),
		rzCfg:          realizabilityConfig{dustUSD: 1.0, flatPct: 0.5, amtEpsPct: 2.0, repeatMin: 3},
		rzLeftDist:     strategy.NewGrossDist(nil),
		rzCapturedDist: strategy.NewGrossDist(nil),
		rzCaptureCount: make(map[common.Address]uint64),
		rzBuilderCount: make(map[common.Address]uint64),
	}
	r.rzCapturedNetWei.Store(big.NewInt(0))
	r.rzCapturedRealizedWei.Store(big.NewInt(0))
	r.rzLeftNetWei.Store(big.NewInt(0))
	return r
}

// rzTestPool / rzTestActor / rzTestVictim are recognisable synthetic addresses.
var (
	rzTestPool   = common.HexToAddress("0x00000000000000000000000000000000000000A1")
	rzTestActor  = common.HexToAddress("0x00000000000000000000000000000000000000B2")
	rzTestVictim = common.HexToAddress("0x00000000000000000000000000000000000000C3")
	rzTestOther  = common.HexToAddress("0x00000000000000000000000000000000000000D4")
)

// rzTestWbnbUSD is the synthetic live WBNB/USD price threaded to the detector. The
// synthetic rzTestPool is never seeded into globalPoolMetaCache, so the per-pool hub
// isolation falls back to the whole-tx hub ledger (perPoolOK==false) and this value
// is unused on these synthetic paths; it exists only to satisfy the signature.
const rzTestWbnbUSD = 600.0

// bnbWei returns n whole BNB in wei.
func bnbWei(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18))
}

// mWei returns n milli-BNB in wei (1e15).
func mWei(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e15))
}

// makeV2Leg builds a synthetic V2 Swap leg. inToken0 picks the direction; yOut/yIn
// set the volatile (token1) side amounts for the flatness check, and xIn/xOut the
// hub (token0) side. We model token1 as the volatile Y here.
func makeV2Leg(pool common.Address, txIdx int, sender, beneficiary, from common.Address, inToken0 bool, x, y *big.Int) rzSwapLeg {
	leg := rzSwapLeg{
		pool: pool, txIdx: txIdx, txHash: common.BigToHash(big.NewInt(int64(txIdx))),
		isV3: false, sender: sender, beneficiary: beneficiary, from: from,
		inToken0: inToken0,
		amt0In:   big.NewInt(0), amt1In: big.NewInt(0), amt0Out: big.NewInt(0), amt1Out: big.NewInt(0),
	}
	if inToken0 {
		// token0 (hub X) in, token1 (vol Y) out.
		leg.amt0In = new(big.Int).Set(x)
		leg.amt1Out = new(big.Int).Set(y)
	} else {
		// token1 (vol Y) in, token0 (hub X) out.
		leg.amt1In = new(big.Int).Set(y)
		leg.amt0Out = new(big.Int).Set(x)
	}
	return leg
}

// makeLedger builds a per-tx ledger crediting `actor` a hub delta and charging gas.
func makeLedger(txIdx int, from, actor common.Address, hubDelta, gas *big.Int) rzTxLedger {
	return rzTxLedger{
		txIdx:       txIdx,
		txHash:      common.BigToHash(big.NewInt(int64(txIdx))),
		from:        from,
		gasBNBWei:   gas,
		coinbaseBNB: big.NewInt(0),
		deltaHub:    map[common.Address]*big.Int{actor: hubDelta},
		deltaTok:    map[common.Address]map[common.Address]*big.Int{},
	}
}

// TestLandedSandwichDetectedAndCaptured: a classic same-actor bracket
// (front buys Y, victim buys Y, back sells Y) with a flat Y round trip and a
// net-positive hub profit IS detected and matches the ex-post opp -> captured.
func TestLandedSandwichDetectedAndCaptured(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1) // 0.001 BNB floor

	// Bracket on rzTestPool: front (tx0, actor buys Y), victim (tx1, buys Y),
	// back (tx2, actor sells Y). Front and back share sender=rzTestActor.
	yAmt := bnbWei(10) // 10 units of Y, flat round trip
	front := makeV2Leg(rzTestPool, 0, rzTestActor, rzTestActor, rzTestActor, true /*X in, Y out*/, bnbWei(1), yAmt)
	victim := makeV2Leg(rzTestPool, 1, rzTestVictim, rzTestVictim, rzTestVictim, true /*same exploited dir*/, bnbWei(2), bnbWei(18))
	back := makeV2Leg(rzTestPool, 2, rzTestActor, rzTestActor, rzTestActor, false /*Y in, X out*/, bnbWei(1), yAmt)
	legs := []rzSwapLeg{front, victim, back}

	// Actor nets +0.5 BNB hub across the bracket (above dust), small gas.
	ledgers := []rzTxLedger{
		makeLedger(0, rzTestActor, rzTestActor, mWei(250), mWei(1)),
		makeLedger(1, rzTestVictim, rzTestVictim, big.NewInt(0), mWei(1)),
		makeLedger(2, rzTestActor, rzTestActor, mWei(300), mWei(1)),
	}

	landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD)
	if len(landed) != 1 {
		t.Fatalf("expected 1 landed sandwich, got %d", len(landed))
	}
	ls := landed[0]
	if ls.actor != rzTestActor {
		t.Fatalf("landed actor = %s, want %s", ls.actor.Hex(), rzTestActor.Hex())
	}
	if !(ls.frontTxIdx == 0 && ls.backTxIdx == 2) {
		t.Fatalf("bracket = [%d,%d], want [0,2]", ls.frontTxIdx, ls.backTxIdx)
	}
	// realizedNet = hub(0.25+0.30) - gas(0.001+0.001) - bribe(0) = 0.548 BNB.
	wantNet := new(big.Int).Sub(mWei(550), mWei(2))
	if ls.realizedNet.Cmp(wantNet) != 0 {
		t.Fatalf("realizedNet = %s, want %s", ls.realizedNet, wantNet)
	}

	// Now match an ex-post opp on the same pool/victim/direction -> captured.
	opp := &exPostOpp{
		pool: rzTestPool, isV3: false, token0Side: true, victimTxIdx: 1,
		victimTx: victim.txHash, netBNBWei: mWei(40), grossBNBWei: mWei(60),
	}
	r.rzMatchAndAttribute(123, &rzFakeHeader, []*exPostOpp{opp}, legs, ledgers, dust, rzTestWbnbUSD)

	if got := r.rzCaptured.Load(); got != 1 {
		t.Fatalf("rzCaptured = %d, want 1", got)
	}
	if got := r.rzLeftOnTable.Load(); got != 0 {
		t.Fatalf("rzLeftOnTable = %d, want 0", got)
	}
	if got := r.rzCapturedNetWei.Load(); got.Cmp(mWei(40)) != 0 {
		t.Fatalf("capturedOurNet = %s, want %s", got, mWei(40))
	}
}

// TestCleanVictimNotCaptured: an ordinary victim swap on the pool with NO
// bracketing same-actor opposite legs is NOT detected -> the ex-post opp falls to
// left-on-the-table.
func TestCleanVictimNotCaptured(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	// Only the victim swap on the pool (plus an unrelated same-direction swap by a
	// different actor before it — NOT an opposite-direction bracket).
	other := makeV2Leg(rzTestPool, 0, rzTestOther, rzTestOther, rzTestOther, true, bnbWei(1), bnbWei(9))
	victim := makeV2Leg(rzTestPool, 1, rzTestVictim, rzTestVictim, rzTestVictim, true, bnbWei(2), bnbWei(18))
	legs := []rzSwapLeg{other, victim}
	ledgers := []rzTxLedger{
		makeLedger(0, rzTestOther, rzTestOther, mWei(100), mWei(1)),
		makeLedger(1, rzTestVictim, rzTestVictim, big.NewInt(0), mWei(1)),
	}

	if landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); len(landed) != 0 {
		t.Fatalf("expected 0 landed sandwiches for a clean victim, got %d", len(landed))
	}

	opp := &exPostOpp{
		pool: rzTestPool, isV3: false, token0Side: true, victimTxIdx: 1,
		victimTx: victim.txHash, netBNBWei: mWei(40), grossBNBWei: mWei(60),
	}
	r.rzMatchAndAttribute(124, &rzFakeHeader, []*exPostOpp{opp}, legs, ledgers, dust, rzTestWbnbUSD)

	if got := r.rzCaptured.Load(); got != 0 {
		t.Fatalf("rzCaptured = %d, want 0", got)
	}
	if got := r.rzLeftOnTable.Load(); got != 1 {
		t.Fatalf("rzLeftOnTable = %d, want 1", got)
	}
	if got := r.rzLeftNetWei.Load(); got.Cmp(mWei(40)) != 0 {
		t.Fatalf("leftNet = %s, want %s", got, mWei(40))
	}
}

// TestSandwichRejectedRouterBeneficiary: a bracket whose two legs share ONLY the
// beneficiary, and that beneficiary is a known router, is NON-discriminating ->
// no same-actor confirmation -> NOT detected (governing rule). Different senders.
func TestSandwichRejectedRouterBeneficiary(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	yAmt := bnbWei(10)
	front := makeV2Leg(rzTestPool, 0, rzTestActor, pancakeV2Router, rzTestActor, true, bnbWei(1), yAmt)
	back := makeV2Leg(rzTestPool, 2, rzTestOther, pancakeV2Router, rzTestOther, false, bnbWei(1), yAmt)
	legs := []rzSwapLeg{front, back}
	ledgers := []rzTxLedger{
		makeLedger(0, rzTestActor, rzTestActor, mWei(300), mWei(1)),
		makeLedger(2, rzTestOther, rzTestOther, mWei(300), mWei(1)),
	}

	if landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); len(landed) != 0 {
		t.Fatalf("router-shared beneficiary must NOT confirm same-actor; got %d landed", len(landed))
	}
}

// TestSandwichRejectedNonFlat: a same-actor opposite-direction bracket whose Y
// round trip is NOT flat (front buys 10 Y, back sells only 5 Y) fails the
// volatile-flat corroboration -> NOT detected.
func TestSandwichRejectedNonFlat(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	front := makeV2Leg(rzTestPool, 0, rzTestActor, rzTestActor, rzTestActor, true, bnbWei(1), bnbWei(10))
	back := makeV2Leg(rzTestPool, 2, rzTestActor, rzTestActor, rzTestActor, false, bnbWei(1), bnbWei(5)) // 50% off
	legs := []rzSwapLeg{front, back}
	ledgers := []rzTxLedger{
		makeLedger(0, rzTestActor, rzTestActor, mWei(300), mWei(1)),
		makeLedger(2, rzTestActor, rzTestActor, mWei(300), mWei(1)),
	}

	if landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); len(landed) != 0 {
		t.Fatalf("non-flat round trip must NOT be a landed sandwich; got %d landed", len(landed))
	}
}

// TestSandwichRejectedBelowDust: a same-actor flat bracket whose net hub delta is
// below the dust floor (after gas) is NOT detected (ordinary tiny flow, not MEV).
func TestSandwichRejectedBelowDust(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(10) // 0.01 BNB floor

	yAmt := bnbWei(10)
	front := makeV2Leg(rzTestPool, 0, rzTestActor, rzTestActor, rzTestActor, true, bnbWei(1), yAmt)
	back := makeV2Leg(rzTestPool, 2, rzTestActor, rzTestActor, rzTestActor, false, bnbWei(1), yAmt)
	legs := []rzSwapLeg{front, back}
	// hub net = 0.002+0.002 - gas 0.002 = 0.002 BNB < 0.01 dust.
	ledgers := []rzTxLedger{
		makeLedger(0, rzTestActor, rzTestActor, mWei(2), mWei(1)),
		makeLedger(2, rzTestActor, rzTestActor, mWei(2), mWei(1)),
	}

	if landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); len(landed) != 0 {
		t.Fatalf("below-dust net must NOT be a landed sandwich; got %d landed", len(landed))
	}
}

// TestSandwichRejectedSameDirection: two same-actor legs in the SAME direction
// (no opposite-direction bracket) are NOT a sandwich.
func TestSandwichRejectedSameDirection(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	front := makeV2Leg(rzTestPool, 0, rzTestActor, rzTestActor, rzTestActor, true, bnbWei(1), bnbWei(10))
	second := makeV2Leg(rzTestPool, 2, rzTestActor, rzTestActor, rzTestActor, true, bnbWei(1), bnbWei(10))
	legs := []rzSwapLeg{front, second}
	ledgers := []rzTxLedger{
		makeLedger(0, rzTestActor, rzTestActor, mWei(300), mWei(1)),
		makeLedger(2, rzTestActor, rzTestActor, mWei(300), mWei(1)),
	}

	if landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); len(landed) != 0 {
		t.Fatalf("same-direction legs must NOT be a sandwich; got %d landed", len(landed))
	}
}

// TestBuilderAttribution: a captured bracket whose actor is a registered builder
// EOA is attributed to "builder".
func TestBuilderAttribution(t *testing.T) {
	r := newTestRealizabilityRunner()
	builder := common.HexToAddress("0x4848489f0b2BEdd788c696e2D79b6b69D7484848") // 48club, in registry
	ls := &landedSandwich{pool: rzTestPool, actor: builder, executor: builder}
	class, _ := r.rzClassifyCaptor(&rzFakeHeader, ls)
	if class != rzClassBuilder {
		t.Fatalf("classifier = %s, want builder", class)
	}
}

// TestRepeatedAddrAttribution: an actor that has already captured in >= repeatMin-1
// prior blocks is classified repeatedAddr on its next capture.
func TestRepeatedAddrAttribution(t *testing.T) {
	r := newTestRealizabilityRunner()
	r.rzCfg.repeatMin = 3
	r.rzCaptureCount[rzTestActor] = 2 // +1 this block reaches the threshold
	ls := &landedSandwich{pool: rzTestPool, actor: rzTestActor, executor: rzTestActor}
	class, _ := r.rzClassifyCaptor(&rzFakeHeader, ls)
	if class != rzClassRepeated {
		t.Fatalf("classifier = %s, want repeatedAddr", class)
	}
}

// TestSandwichRejectedRouterSender (FIX #1/#3, sender-path gap): two UNRELATED
// opposite-direction swaps that merely route through the SAME PancakeV2 router
// (front.sender == back.sender == pancakeV2Router) with DISTINCT beneficiary/`from`
// EOAs must NOT be confirmed as a same-actor sandwich. Before the fix the Topics[1]
// sender path confirmed any shared sender (including a router) and attributed a
// fake landed capture; the router exclusion was only on the beneficiary path. This
// is the dangerous over-counts-captured direction.
func TestSandwichRejectedRouterSender(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	yAmt := bnbWei(10)
	// Shared sender = router; distinct beneficiaries AND distinct `from` EOAs.
	front := makeV2Leg(rzTestPool, 0, pancakeV2Router, rzTestActor, rzTestActor, true, bnbWei(1), yAmt)
	back := makeV2Leg(rzTestPool, 2, pancakeV2Router, rzTestOther, rzTestOther, false, bnbWei(1), yAmt)
	legs := []rzSwapLeg{front, back}
	// Even if the router shows a positive whole-tx hub delta (e.g. fee-accruing /
	// transiently holding WBNB), the same-actor gate must still reject it.
	ledgers := []rzTxLedger{
		makeLedger(0, rzTestActor, pancakeV2Router, mWei(300), mWei(1)),
		makeLedger(2, rzTestOther, pancakeV2Router, mWei(300), mWei(1)),
	}

	if landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); len(landed) != 0 {
		t.Fatalf("router-shared SENDER must NOT confirm same-actor; got %d landed", len(landed))
	}

	// And it must fall to left-on-the-table, not captured.
	opp := &exPostOpp{
		pool: rzTestPool, isV3: false, token0Side: true, victimTxIdx: 1,
		victimTx: common.BigToHash(big.NewInt(1)), netBNBWei: mWei(40), grossBNBWei: mWei(60),
	}
	r.rzMatchAndAttribute(125, &rzFakeHeader, []*exPostOpp{opp}, legs, ledgers, dust, rzTestWbnbUSD)
	if got := r.rzCaptured.Load(); got != 0 {
		t.Fatalf("rzCaptured = %d, want 0 (router-sender must not capture)", got)
	}
	if got := r.rzLeftOnTable.Load(); got != 1 {
		t.Fatalf("rzLeftOnTable = %d, want 1", got)
	}
}

// TestSandwichRejectedSharedMMNonRoundTrip (CORROBORATION FP guard): a shared
// NON-router contract (e.g. a market-maker / aggregator bot) is the Topics[1] sender
// of two opposite-direction legs signed by two distinct, unrelated EOAs. Under the
// fixed same-actor rule the shared non-router sender PASSES §5.2 (corroboration is
// the proper FP guard, not the from-EOA check). But these two swaps are UNRELATED
// (the MM merely routed two different users' flow): the Y round trip is NOT flat
// (front buys 10 Y, back sells only 4 Y). Corroboration must therefore REJECT it.
// This is the realistic coincidental-MM false positive the corroboration gate
// exists to catch.
func TestSandwichRejectedSharedMMNonRoundTrip(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	mm := common.HexToAddress("0x000000000000000000000000000000000000E55E") // shared MM contract, NOT a router
	// Shared sender = mm; txs signed by two distinct, unrelated EOAs; NON-flat Y.
	front := makeV2Leg(rzTestPool, 0, mm, mm, rzTestActor, true, bnbWei(1), bnbWei(10))
	back := makeV2Leg(rzTestPool, 2, mm, mm, rzTestOther, false, bnbWei(1), bnbWei(4)) // 60% off — not a round trip
	legs := []rzSwapLeg{front, back}
	ledgers := []rzTxLedger{
		makeLedger(0, rzTestActor, mm, mWei(300), mWei(1)),
		makeLedger(2, rzTestOther, mm, mWei(300), mWei(1)),
	}

	if landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); len(landed) != 0 {
		t.Fatalf("coincidental shared-MM non-round-trip must be rejected by corroboration; got %d landed", len(landed))
	}
}

// TestSandwichSenderActorIsFromAccepted (FIX #1 positive control): a genuine
// searcher CONTRACT that is the shared Topics[1] sender AND is the `from` EOA of at
// least one leg (the searcher signs its own bracket tx) IS still detected. This
// proves the `from`-EOA guard does not over-reject real direct-pair searchers.
func TestSandwichSenderActorIsFromAccepted(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	searcher := common.HexToAddress("0x000000000000000000000000000000000000AbCd")
	yAmt := bnbWei(10)
	// Shared sender = searcher; searcher is the `from` of BOTH legs (signs its own).
	front := makeV2Leg(rzTestPool, 0, searcher, searcher, searcher, true, bnbWei(1), yAmt)
	back := makeV2Leg(rzTestPool, 2, searcher, searcher, searcher, false, bnbWei(1), yAmt)
	legs := []rzSwapLeg{front, back}
	ledgers := []rzTxLedger{
		makeLedger(0, searcher, searcher, mWei(300), mWei(1)),
		makeLedger(2, searcher, searcher, mWei(300), mWei(1)),
	}

	landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD)
	if len(landed) != 1 {
		t.Fatalf("genuine searcher (sender==from) must be detected; got %d landed", len(landed))
	}
	if landed[0].actor != searcher {
		t.Fatalf("landed actor = %s, want %s", landed[0].actor.Hex(), searcher.Hex())
	}
}

// TestSandwichContractSenderSameEOA (THE RECALL-BUG REGRESSION): the previously
// BROKEN real-bot case. The front/back Swap logs carry sender = a CONTRACT (the
// bot's router/executor contract, NOT the EOA) so the Swap-log actor never equals a
// leg's `from`; but the SAME EOA signed BOTH bracket txs (front.from == back.from).
// This is the dominant integrated-bot pattern on BSC. The old code rejected it
// (rzActorIsLegFrom required the Swap-log actor == a leg's `from`), zeroing recall.
// It MUST now be detected, attributed to the EOA so repeated-address clustering keys
// by the bot's signing key.
func TestSandwichContractSenderSameEOA(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	botEOA := common.HexToAddress("0x000000000000000000000000000000000000B07E")    // the signing EOA
	botContract := common.HexToAddress("0x000000000000000000000000000000000000C047") // the bot's executor contract (Swap-log sender), NOT a router
	yAmt := bnbWei(10)
	// Swap-log sender/beneficiary = the CONTRACT (≠ EOA); but `from` = the same EOA on BOTH legs.
	front := makeV2Leg(rzTestPool, 0, botContract, botContract, botEOA, true, bnbWei(1), yAmt)
	back := makeV2Leg(rzTestPool, 2, botContract, botContract, botEOA, false, bnbWei(1), yAmt)
	legs := []rzSwapLeg{front, back}
	// REALISTIC integrated-bot ledger: the EOA only SIGNS and PAYS GAS (zero hub
	// delta); the hub PROFIT accrues to the bot's executor CONTRACT. Corroboration
	// must read the hub delta over the actor CLUSTER (EOA + contract) — not the EOA
	// alone — or this real bracket is wrongly zeroed (the second half of the recall
	// bug). from==EOA on both legs (gas charged to the EOA).
	ledgers := []rzTxLedger{
		makeLedger(0, botEOA, botContract, mWei(300), mWei(1)),
		makeLedger(2, botEOA, botContract, mWei(300), mWei(1)),
	}

	landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD)
	if len(landed) != 1 {
		t.Fatalf("contract-sender + same signing EOA must be detected (the recall bug); got %d landed", len(landed))
	}
	if landed[0].actor != botEOA {
		t.Fatalf("landed actor = %s, want the EOA %s (attribute by signing key)", landed[0].actor.Hex(), botEOA.Hex())
	}
	if landed[0].executor != botContract {
		t.Fatalf("landed executor = %s, want the contract %s", landed[0].executor.Hex(), botContract.Hex())
	}
}

// TestSandwichSameEOARejectedNonRoundTrip (CORROBORATION FP guard, signal-1 path):
// same pool, opposite directions, the SAME signing EOA on both legs (signal 1
// passes §5.2), but the activity is NOT a flat-in-Y round trip (front buys 10 Y,
// back sells only 3 Y). Even with a matching EOA the corroboration gate MUST reject
// it — a same-EOA actor doing two unrelated opposite swaps is not a sandwich. Guards
// against over-counting captured on the new (accepting) signal-1 path.
func TestSandwichSameEOARejectedNonRoundTrip(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	eoa := common.HexToAddress("0x000000000000000000000000000000000000Ee0A")
	front := makeV2Leg(rzTestPool, 0, eoa, eoa, eoa, true, bnbWei(1), bnbWei(10))
	back := makeV2Leg(rzTestPool, 2, eoa, eoa, eoa, false, bnbWei(1), bnbWei(3)) // 70% off — not a round trip
	legs := []rzSwapLeg{front, back}
	ledgers := []rzTxLedger{
		makeLedger(0, eoa, eoa, mWei(300), mWei(1)),
		makeLedger(2, eoa, eoa, mWei(300), mWei(1)),
	}

	if landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); len(landed) != 0 {
		t.Fatalf("same-EOA but non-round-trip must be rejected by corroboration; got %d landed", len(landed))
	}
}

// TestSameEOABelowDustRejected (CORROBORATION FP guard, signal-1 path, net side):
// same EOA, flat round trip, but the net hub delta is below the dust floor after
// gas. Corroboration's net-positive-above-dust gate must reject it (ordinary tiny
// same-EOA flow, not landed MEV).
func TestSameEOABelowDustRejected(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(10) // 0.01 BNB floor

	eoa := common.HexToAddress("0x000000000000000000000000000000000000Ee0A")
	yAmt := bnbWei(10)
	front := makeV2Leg(rzTestPool, 0, eoa, eoa, eoa, true, bnbWei(1), yAmt)
	back := makeV2Leg(rzTestPool, 2, eoa, eoa, eoa, false, bnbWei(1), yAmt)
	legs := []rzSwapLeg{front, back}
	// hub net = 0.002 + 0.002 - gas 0.002 = 0.002 BNB < 0.01 dust.
	ledgers := []rzTxLedger{
		makeLedger(0, eoa, eoa, mWei(2), mWei(1)),
		makeLedger(2, eoa, eoa, mWei(2), mWei(1)),
	}

	if landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); len(landed) != 0 {
		t.Fatalf("same-EOA below-dust must be rejected by corroboration; got %d landed", len(landed))
	}
}

// TestMultiVictimBracketRealizedCountedOnce (FIX #4): a single wide same-actor
// bracket [front=tx0, back=tx3] straddles TWO distinct victims (tx1, tx2) on the
// same pool/direction. The captured COUNT is legitimately 2 (the bracket captures
// both victims), but the COMPETITOR's realizedNet must be credited to
// rzCapturedRealizedWei AT MOST ONCE (the bracket realized its profit once, not
// twice). The per-opp ourNetBNBWei accumulator stays per-opp.
func TestMultiVictimBracketRealizedCountedOnce(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	yAmt := bnbWei(10)
	front := makeV2Leg(rzTestPool, 0, rzTestActor, rzTestActor, rzTestActor, true, bnbWei(1), yAmt)
	v1 := makeV2Leg(rzTestPool, 1, rzTestVictim, rzTestVictim, rzTestVictim, true, bnbWei(2), bnbWei(18))
	v2 := makeV2Leg(rzTestPool, 2, rzTestOther, rzTestOther, rzTestOther, true, bnbWei(2), bnbWei(18))
	back := makeV2Leg(rzTestPool, 3, rzTestActor, rzTestActor, rzTestActor, false, bnbWei(1), yAmt)
	legs := []rzSwapLeg{front, v1, v2, back}
	ledgers := []rzTxLedger{
		makeLedger(0, rzTestActor, rzTestActor, mWei(250), mWei(1)),
		makeLedger(1, rzTestVictim, rzTestVictim, big.NewInt(0), mWei(1)),
		makeLedger(2, rzTestOther, rzTestOther, big.NewInt(0), mWei(1)),
		makeLedger(3, rzTestActor, rzTestActor, mWei(300), mWei(1)),
	}

	landed := r.detectLandedSandwiches(legs, ledgers, &rzFakeHeader, dust, rzTestWbnbUSD)
	if len(landed) != 1 {
		t.Fatalf("expected 1 wide landed bracket, got %d", len(landed))
	}
	// realizedNet = hub(0.25+0.30) - gas(0.001+0.001) - bribe(0) = 0.548 BNB.
	wantRealized := new(big.Int).Sub(mWei(550), mWei(2))
	if landed[0].realizedNet.Cmp(wantRealized) != 0 {
		t.Fatalf("realizedNet = %s, want %s", landed[0].realizedNet, wantRealized)
	}

	// Two distinct ex-post opps, both straddled by the SAME bracket.
	opp1 := &exPostOpp{
		pool: rzTestPool, isV3: false, token0Side: true, victimTxIdx: 1,
		victimTx: v1.txHash, netBNBWei: mWei(40), grossBNBWei: mWei(60),
	}
	opp2 := &exPostOpp{
		pool: rzTestPool, isV3: false, token0Side: true, victimTxIdx: 2,
		victimTx: v2.txHash, netBNBWei: mWei(30), grossBNBWei: mWei(50),
	}
	r.rzMatchAndAttribute(126, &rzFakeHeader, []*exPostOpp{opp1, opp2}, legs, ledgers, dust, rzTestWbnbUSD)

	// Both victims captured (count = 2 is defensible).
	if got := r.rzCaptured.Load(); got != 2 {
		t.Fatalf("rzCaptured = %d, want 2 (both victims captured by the bracket)", got)
	}
	// OUR per-opp sizing accumulates per opp: 0.040 + 0.030 = 0.070 BNB.
	if got := r.rzCapturedNetWei.Load(); got.Cmp(mWei(70)) != 0 {
		t.Fatalf("rzCapturedNetWei = %s, want %s (per-opp sum)", got, mWei(70))
	}
	// COMPETITOR's realizedNet credited ONCE for the bracket (not 2x).
	if got := r.rzCapturedRealizedWei.Load(); got.Cmp(wantRealized) != 0 {
		t.Fatalf("rzCapturedRealizedWei = %s, want %s (bracket realized counted ONCE, not 2x)", got, wantRealized)
	}
}
