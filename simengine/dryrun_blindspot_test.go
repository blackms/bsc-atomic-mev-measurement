// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_blindspot_test.go unit-proves the ROUND-1 blindspot corrected detectors
// (simengine/dryrun_blindspot.go) WITHOUT a block replay, reusing the synthetic
// leg/ledger helpers (makeV2Leg, makeLedger, bnbWei) and recognisable addresses
// (rzTestPool, rzTestActor, pancakeV2Router) from dryrun_realizability_test.go.
//
// The round-1 probe applied pattern heuristics WITHOUT the router/coinbase
// exclusion and WITHOUT a nonce-based coldness check. The two corrected detectors
// add exactly those gates:
//
//	detectRoundTripCorrected  — TRUE for a real (possibly non-flat) round trip by a
//	  non-router same actor; FALSE (routerFP) when the actor / a leg is a known
//	  router or the coinbase.
//	detectProceedsSweepCorrected — TRUE when proceeds are swept to a genuinely COLD
//	  (fresh / low-nonce) recipient distinct from the executor; FALSE (coldFP) for a
//	  warm (high-nonce) recipient or a router/coinbase pass-through.
//
// These exercise the precise round-1 failure modes (no exclusions, no coldness
// check), so the tests would have caught the original bug.
package simengine

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
)

// newTestBlindspotRunner builds a dryRunner carrying only the blindspot config the
// corrected detectors read (flatPctTol=0.5, nonceThresh=5). No chain/engine.
func newTestBlindspotRunner() *dryRunner {
	return &dryRunner{bsCfg: defaultBlindspotConfig()}
}

// TestBlindspotDetectRoundTripCorrected pins the router/coinbase exclusion the
// round-1 round-trip heuristic lacked, and the "real two-sided Y movement" accept.
func TestBlindspotDetectRoundTripCorrected(t *testing.T) {
	r := newTestBlindspotRunner()
	cb := rzFakeHeader.Coinbase

	// REAL, NON-FLAT round trip by a non-router same actor: front buys Y (X in, Y out),
	// back sells Y (Y in, X out) with a DIFFERENT Y amount (10 vs 8). This is the
	// dominant recall-miss the flat-balance same-actor gate failed to credit; the
	// corrected detector accepts it as a real round trip.
	front := makeV2Leg(rzTestPool, 0, rzTestActor, rzTestActor, rzTestActor, true /*X in, Y out*/, bnbWei(1), bnbWei(10))
	back := makeV2Leg(rzTestPool, 2, rzTestActor, rzTestActor, rzTestActor, false /*Y in, X out*/, bnbWei(1), bnbWei(8))
	if isReal, isFP := r.detectRoundTripCorrected(front, back, rzTestActor, cb); !isReal || isFP {
		t.Fatalf("real non-flat round trip by non-router actor must be (true,false), got (%v,%v)", isReal, isFP)
	}

	// ROUTER actor pass-through -> routerFP (false,true). Round-1 had no such exclusion.
	if isReal, isFP := r.detectRoundTripCorrected(front, back, pancakeV2Router, cb); isReal || !isFP {
		t.Fatalf("router actor must be (false,true) routerFP, got (%v,%v)", isReal, isFP)
	}

	// COINBASE actor pass-through -> routerFP.
	if isReal, isFP := r.detectRoundTripCorrected(front, back, cb, cb); isReal || !isFP {
		t.Fatalf("coinbase actor must be (false,true) routerFP, got (%v,%v)", isReal, isFP)
	}

	// ROUTER on a LEG identity (front.sender) excludes via bsLegIsRouter even when the
	// discriminating actor itself is not a router.
	frontRouterLeg := makeV2Leg(rzTestPool, 0, pancakeV2Router, rzTestActor, rzTestActor, true, bnbWei(1), bnbWei(10))
	if isReal, isFP := r.detectRoundTripCorrected(frontRouterLeg, back, rzTestActor, cb); isReal || !isFP {
		t.Fatalf("router on a leg must be (false,true) routerFP, got (%v,%v)", isReal, isFP)
	}

	// One-sided movement (back never sold Y) is ambiguous, NOT a round trip: (false,false).
	oneSided := makeV2Leg(rzTestPool, 2, rzTestActor, rzTestActor, rzTestActor, true /*same direction*/, bnbWei(1), bnbWei(8))
	if isReal, isFP := r.detectRoundTripCorrected(front, oneSided, rzTestActor, cb); isReal || isFP {
		t.Fatalf("one-sided (non-round-trip) movement must be (false,false), got (%v,%v)", isReal, isFP)
	}
}

// TestBlindspotDetectProceedsSweepCorrected pins the nonce-based coldness check the
// round-1 sweep heuristic lacked (fresh/cold recipient accepted; warm/router
// rejected). Uses a real in-memory StateDB as the cold/nonce reference.
func TestBlindspotDetectProceedsSweepCorrected(t *testing.T) {
	r := newTestBlindspotRunner()
	cb := rzFakeHeader.Coinbase

	coldRef, err := state.New(common.Hash{}, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	// The executing sender (front.from) is an established/warm EOA (nonce 10).
	senderEOA := rzTestActor
	coldRef.SetNonce(senderEOA, 10, tracing.NonceChangeUnspecified)

	coldAddr := common.HexToAddress("0x00000000000000000000000000000000C01DC01D") // nonce 0 (< thresh 5): cold
	warmAddr := common.HexToAddress("0x0000000000000000000000000000000000A11A11") // nonce 100: warm
	coldRef.SetNonce(warmAddr, 100, tracing.NonceChangeUnspecified)

	mkFront := func() rzSwapLeg {
		return makeV2Leg(rzTestPool, 0, senderEOA, senderEOA, senderEOA, true, bnbWei(1), bnbWei(10))
	}
	// back leg sells Y; its beneficiary (4th arg) is the proceeds recipient under test.
	mkBack := func(to common.Address) rzSwapLeg {
		return makeV2Leg(rzTestPool, 2, senderEOA, to, senderEOA, false, bnbWei(1), bnbWei(10))
	}

	// REAL: proceeds swept to a fresh/cold address distinct from the executor.
	if isReal, sweepTo, isFP := r.detectProceedsSweepCorrected(mkFront(), mkBack(coldAddr), nil, coldRef, cb); !isReal || isFP || sweepTo != coldAddr {
		t.Fatalf("cold sweep must be (true, coldAddr, false), got (%v,%s,%v)", isReal, sweepTo.Hex(), isFP)
	}

	// FALSE POSITIVE: warm (high-nonce) recipient -> coldFP. Round-1 had no nonce check.
	if isReal, sweepTo, isFP := r.detectProceedsSweepCorrected(mkFront(), mkBack(warmAddr), nil, coldRef, cb); isReal || !isFP || sweepTo != warmAddr {
		t.Fatalf("warm recipient must be (false, warm, true), got (%v,%s,%v)", isReal, sweepTo.Hex(), isFP)
	}

	// FALSE POSITIVE: router recipient -> pass-through (coldFP), regardless of nonce.
	if isReal, _, isFP := r.detectProceedsSweepCorrected(mkFront(), mkBack(pancakeV2Router), nil, coldRef, cb); isReal || !isFP {
		t.Fatalf("router recipient must be (false, _, true), got (%v,%v)", isReal, isFP)
	}

	// NOT A SWEEP: proceeds returned to the executing identity itself -> (false,_,false).
	if isReal, _, isFP := r.detectProceedsSweepCorrected(mkFront(), mkBack(senderEOA), nil, coldRef, cb); isReal || isFP {
		t.Fatalf("proceeds to the executor must be (false,_,false), got (%v,%v)", isReal, isFP)
	}
}
