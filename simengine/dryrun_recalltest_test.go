// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_recalltest_test.go unit-proves the RECALL-VALIDATION injector
// (SIMENGINE_DRYRUN=recalltest) WITHOUT a full block replay: it builds the EXACT
// leg/ledger records rtBuildSandwich produces for each structure cell (reusing the
// real shaping helpers rtMakeLeg / rtApplyIdentity / rtMakeLedger) and asserts:
//
//   - a constructed sandwich of EACH dominant structure cell (1a same-EOA-swap-
//     sender, 1b same-EOA-contract-sender, 1c cross-EOA-shared-beneficiary, routed,
//     stable-numeraire, thin-pool) is WELL-FORMED and IS detected by the exact
//     detectLandedSandwiches path (recall == 1 for the cell);
//   - the two admitted BLIND-SPOT cells (marginally-flat, proceeds-swept) are NOT
//     detected (recall == 0), confirming the harness faithfully reproduces the known
//     false negatives;
//   - a constructed CLEAN one-way victim is NOT a sandwich (no false positive).
//
// The genuine-EVM swap amounts are stubbed here with realistic flat/imbalanced Y
// figures (the live mode supplies the real ones); what is under test is the SHAPING
// + IDENTITY OVERLAY that determines whether the detector recognises each structure
// — which is exactly the recall-sensitive code.
package simengine

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/strategy"
)

// rtTestCand is a synthetic WBNB-hub candidate pool (numeraire = token0 = WBNB) the
// injector shaping helpers operate on. The pool is NOT seeded into
// globalPoolMetaCache, so rzPerPoolBracketHubBNB falls back to the whole-tx hub
// (perPoolOK==false) and the cluster hub ledger drives corroboration — identical to
// the synthetic detector unit tests.
var rtTestCand = rtCandidate{
	pool: anyPool{
		pair:   common.HexToAddress("0x00000000000000000000000000000000000000F1"),
		token0: strategy.WBNB,
		token1: common.HexToAddress("0x00000000000000000000000000000000000000F2"), // volatile
		ok:     true,
	},
	numToken:    strategy.WBNB,
	numKind:     numWBNB,
	volToken:    common.HexToAddress("0x00000000000000000000000000000000000000F2"),
	numIsToken0: true,
	reserveNum:  bnbWei(1000),
	ok:          true,
}

// rtBuildTestInjection reconstructs, with the REAL shaping helpers, the legs/ledgers
// rtBuildSandwich would produce for structure `s` given genuine-style amounts: the
// frontrun buys yAmt of Y, the back sells yBack of Y, the round-trip hub gain is
// hubGain BNB (split as +0 front spend / +hubGain back for simplicity — the detector
// sums the cluster member across both legs). When swept, the back hub credit is 0.
func (r *dryRunner) rtBuildTestInjection(s rtStructure, yAmt, yBack, hubGain *big.Int, baseIdx int) rtInjection {
	cand := rtTestCand
	frontIdx, victimIdx, backIdx := baseIdx, baseIdx+1, baseIdx+2

	// Genuine-style hub-side amounts: front spends 1 BNB of numeraire, back recovers
	// 1 BNB + hubGain (so per-pool isolation, if ever engaged, is net-positive).
	xFront := bnbWei(1)
	xBack := new(big.Int).Add(bnbWei(1), hubGain)

	frontLeg := r.rtMakeLeg(cand, frontIdx, true, xFront, yAmt)
	victimLeg := r.rtMakeLeg(cand, victimIdx, true, bnbWei(2), new(big.Int).Mul(yAmt, big.NewInt(2)))
	backLeg := r.rtMakeLeg(cand, backIdx, false, xBack, yBack)

	r.rtApplyIdentity(s, &frontLeg, &backLeg)
	victimLeg.sender, victimLeg.beneficiary, victimLeg.from = rtVictimEOA, rtVictimEOA, rtVictimEOA

	gas := mWei(1)
	// Front-leg hub credit: the spend (negative) — small, so the COMBINED front+back
	// cluster delta is ~hubGain. Back-leg hub credit: the recovered profit.
	frontHub := big.NewInt(0)
	backHub := new(big.Int).Set(hubGain)
	if s == rtProceedsSwept {
		backHub = big.NewInt(0) // proceeds left the cluster mid-tx
	}
	frontLed := r.rtMakeLedger(frontLeg, frontHub, gas)
	backLed := r.rtMakeLedger(backLeg, backHub, gas)
	victimLed := makeRtLedger(victimIdx, rtVictimEOA, rtVictimEOA, big.NewInt(0), gas)

	return rtInjection{
		legs:          []rzSwapLeg{frontLeg, victimLeg, backLeg},
		ledgers:       []rzTxLedger{frontLed, victimLed, backLed},
		pool:          cand.pool.pair,
		frontTxIdx:    frontIdx,
		backTxIdx:     backIdx,
		inToken0Front: frontLeg.inToken0,
		yFrontOut:     rtLegY(frontLeg, frontLeg.inToken0, true),
		yBackIn:       rtLegY(backLeg, frontLeg.inToken0, false),
	}
}

// dominant cells the detector is designed to catch (expect recall == 1).
var rtDominantCells = []rtStructure{
	rtSameEOASwapSender,
	rtSameEOAContractSender,
	rtCrossEOASharedBenef,
	rtRoutedMultiHop,
	rtStableNumeraire,
	rtThinPool,
}

// TestRecallInjectorDominantCellsDetected: each dominant structure cell, built with
// a flat round trip and an above-dust hub gain, IS detected by the exact
// detectLandedSandwiches path. This is the core recall guarantee on the patterns the
// detector targets.
func TestRecallInjectorDominantCellsDetected(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)
	yAmt := bnbWei(10)
	hubGain := mWei(500) // 0.5 BNB net, well above dust

	for _, s := range rtDominantCells {
		inj := r.rtBuildTestInjection(s, yAmt, yAmt /*flat*/, hubGain, 100)
		landed := r.detectLandedSandwiches(inj.legs, inj.ledgers, &rzFakeHeader, dust, rzTestWbnbUSD)
		if !rtBracketMatchesInjection(landed, inj) {
			t.Fatalf("dominant cell %s NOT detected (recall miss); landed=%d", rtStructureName(s), len(landed))
		}
	}
}

// TestRecallInjectorWellFormedLegs: every constructed dominant-cell bracket is
// structurally well-formed — opposite directions, a flat Y round trip, distinct
// front/back tx indices straddling the victim, and a non-empty actor identity.
func TestRecallInjectorWellFormedLegs(t *testing.T) {
	r := newTestRealizabilityRunner()
	yAmt := bnbWei(10)
	for _, s := range rtDominantCells {
		inj := r.rtBuildTestInjection(s, yAmt, yAmt, mWei(500), 200)
		if len(inj.legs) != 3 {
			t.Fatalf("%s: want 3 legs, got %d", rtStructureName(s), len(inj.legs))
		}
		front, back := inj.legs[0], inj.legs[2]
		if front.inToken0 == back.inToken0 {
			t.Fatalf("%s: front/back must be opposite directions", rtStructureName(s))
		}
		if !(front.txIdx < inj.legs[1].txIdx && inj.legs[1].txIdx < back.txIdx) {
			t.Fatalf("%s: bracket must straddle the victim: [%d,%d,%d]", rtStructureName(s), front.txIdx, inj.legs[1].txIdx, back.txIdx)
		}
		// Flat Y round trip: yFrontOut == yBackIn for the dominant cells.
		if inj.yFrontOut.Cmp(inj.yBackIn) != 0 {
			t.Fatalf("%s: dominant cells must be flat: yOut=%s yIn=%s", rtStructureName(s), inj.yFrontOut, inj.yBackIn)
		}
		if (front.from == common.Address{}) || (back.from == common.Address{}) {
			t.Fatalf("%s: legs must carry a non-empty signer", rtStructureName(s))
		}
	}
}

// TestRecallInjectorContractSenderAttribution: the integrated-bot cell (1b) — same
// EOA signs both, Swap sender/beneficiary == a contract != EOA — is detected AND
// attributed to the signing EOA with the contract as executor (the recall fix).
func TestRecallInjectorContractSenderAttribution(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)
	inj := r.rtBuildTestInjection(rtSameEOAContractSender, bnbWei(10), bnbWei(10), mWei(500), 300)
	landed := r.detectLandedSandwiches(inj.legs, inj.ledgers, &rzFakeHeader, dust, rzTestWbnbUSD)
	if len(landed) != 1 {
		t.Fatalf("1b contract-sender must be detected; got %d landed", len(landed))
	}
	if landed[0].actor != rtBotEOA {
		t.Fatalf("1b actor = %s, want signing EOA %s", landed[0].actor.Hex(), rtBotEOA.Hex())
	}
	if landed[0].executor != rtBotContract {
		t.Fatalf("1b executor = %s, want contract %s", landed[0].executor.Hex(), rtBotContract.Hex())
	}
}

// TestRecallInjectorBlindSpotsNotDetected: the two admitted blind-spot cells are NOT
// detected, confirming the harness faithfully reproduces the known false negatives
// (a non-flat round trip and proceeds swept out of the cluster).
func TestRecallInjectorBlindSpotsNotDetected(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	// marginally-flat: back sells ~3% less Y than the front bought (beyond tolerance).
	yAmt := bnbWei(100)
	yBack := new(big.Int).Sub(yAmt, bnbWei(3)) // 3% imbalance
	injFlat := r.rtBuildTestInjection(rtMarginallyFlat, yAmt, yBack, mWei(500), 400)
	if landed := r.detectLandedSandwiches(injFlat.legs, injFlat.ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); rtBracketMatchesInjection(landed, injFlat) {
		t.Fatalf("marginally-flat must NOT be detected (admitted blind spot); got detected")
	}

	// proceeds-swept: flat round trip but the back-leg hub credit to the cluster is 0
	// (profit moved to a cold address), so corroboration finds no net-positive hub.
	injSwept := r.rtBuildTestInjection(rtProceedsSwept, yAmt, yAmt, mWei(500), 500)
	if landed := r.detectLandedSandwiches(injSwept.legs, injSwept.ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); rtBracketMatchesInjection(landed, injSwept) {
		t.Fatalf("proceeds-swept must NOT be detected (admitted blind spot); got detected")
	}
}

// TestRecallInjectorCleanVictimNoFalsePositive: a clean one-way victim swap (a single
// numeraire->volatile leg, no bracketing opposite leg) is NOT detected as a landed
// sandwich — the harness's false-positive control.
func TestRecallInjectorCleanVictimNoFalsePositive(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(1)

	leg := r.rtMakeLeg(rtTestCand, 600, true, bnbWei(1), bnbWei(10))
	leg.sender, leg.beneficiary, leg.from = rtVictimEOA, rtVictimEOA, rtVictimEOA
	led := makeRtLedger(600, rtVictimEOA, rtVictimEOA, big.NewInt(0), mWei(1))

	landed := r.detectLandedSandwiches([]rzSwapLeg{leg}, []rzTxLedger{led}, &rzFakeHeader, dust, rzTestWbnbUSD)
	if len(landed) != 0 {
		t.Fatalf("clean one-way victim must NOT be a landed sandwich; got %d", len(landed))
	}
}

// TestRecallInjectorBelowDustNotDetected: a well-formed flat bracket whose net hub
// gain is below the dust floor is NOT detected — the injector's hub credit flows
// through the same net-of-gas dust gate as production.
func TestRecallInjectorBelowDustNotDetected(t *testing.T) {
	r := newTestRealizabilityRunner()
	dust := mWei(10) // 0.01 BNB floor
	inj := r.rtBuildTestInjection(rtSameEOASwapSender, bnbWei(10), bnbWei(10), mWei(2) /*0.002 BNB < dust*/, 700)
	if landed := r.detectLandedSandwiches(inj.legs, inj.ledgers, &rzFakeHeader, dust, rzTestWbnbUSD); rtBracketMatchesInjection(landed, inj) {
		t.Fatalf("below-dust bracket must NOT be detected; got detected")
	}
}

// TestRatioStr sanity-checks the recall fraction formatter used in the tally line.
func TestRatioStr(t *testing.T) {
	cases := []struct {
		num, den uint64
		want     string
	}{
		{0, 0, "n/a"},
		{0, 100, "0.0000"},
		{100, 100, "1.0000"},
		{1, 2, "0.5000"},
		{995, 1000, "0.9950"},
		{1, 3, "0.3333"},
	}
	for _, c := range cases {
		if got := ratioStr(c.num, c.den); got != c.want {
			t.Fatalf("ratioStr(%d,%d) = %q, want %q", c.num, c.den, got, c.want)
		}
	}
}
