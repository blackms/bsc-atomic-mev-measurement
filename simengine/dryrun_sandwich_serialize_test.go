// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_sandwich_serialize_test.go unit-proves the ROUND-1 sandwich-serialize
// fixes (simengine/dryrun_sandwich_serialize.go) on the pieces that are decidable
// without an EVM replay:
//
//	(1) the CONCURRENCY OVER-COUNT is a REAL economic effect of the shared
//	    substrate, not a stripped-state artifact: two contending same-pool
//	    opportunities valued INDEPENDENTLY (each on the same fresh reserves) sum to
//	    MORE than when valued SERIALLY on one evolving substrate (the second sees the
//	    first's realized price impact). Modeled with the AMM math the live mode
//	    drives (strategy.OptimalArb / GetAmountOut), so independentUpper >
//	    serializedLower for an honest, deterministic reason.
//	(2) the round-1 ONE-SIDED FLOOR bug: when lower>upper the old code clamped lower
//	    DOWN to upper and ADDED both bands (inflating the gap). The fix EXCLUDES the
//	    group (ssDivergedGroups++) and adds NOTHING. Pinned via serializeBandsValid
//	    + serializeWithFallback.
//
// The EVM-driven serializeExecutePool replay (which wires countDiverged-style
// reverts and the per-victim valuation onto a real chain) needs a full node and is
// exercised under SIMENGINE_DRYRUN=sandwich-serialize at runtime; here we pin the
// invariant logic and the economic principle hermetically.
package simengine

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/strategy"
)

// newTestSerializeRunner builds a dryRunner with the ss* big.Int band pointers
// seeded so the atomic loads never nil-panic. No chain/engine.
func newTestSerializeRunner() *dryRunner {
	r := &dryRunner{}
	r.ssUpperTotalNetWei.Store(big.NewInt(0))
	r.ssLowerTotalNetWei.Store(big.NewInt(0))
	return r
}

// TestSerializeBandsValid pins the pure invariant: serialization can only reduce or
// equal the independent sum, so lower<=upper is valid and lower>upper is a
// divergence (nil treated as zero).
func TestSerializeBandsValid(t *testing.T) {
	if !serializeBandsValid(bnbWei(20), bnbWei(15)) {
		t.Fatalf("lower<upper must be valid")
	}
	if !serializeBandsValid(bnbWei(10), bnbWei(10)) {
		t.Fatalf("lower==upper must be valid")
	}
	if serializeBandsValid(bnbWei(10), bnbWei(11)) {
		t.Fatalf("lower>upper must be INVALID (the divergence round-1 clamped away)")
	}
	if !serializeBandsValid(nil, nil) {
		t.Fatalf("nil bands must be valid (0<=0)")
	}
	if serializeBandsValid(nil, bnbWei(1)) {
		t.Fatalf("0 upper < positive lower must be invalid")
	}
}

// TestSerializeWithFallbackExcludesNotClamps pins the round-1 fix: a lower>upper
// group is EXCLUDED (ssDivergedGroups++) and contributes NOTHING to either
// aggregate — never clamped-and-added.
func TestSerializeWithFallbackExcludesNotClamps(t *testing.T) {
	r := newTestSerializeRunner()
	pair := rzTestPool

	// DIVERGED group (lower=20 > upper=10). Round-1 would have clamped lower->10 and
	// ADDED both bands (counts 1 and 2, wei 10 and 10). The fix excludes it entirely.
	r.serializeWithFallback(1, pair, bnbWei(10), bnbWei(20), 1, 2)
	if got := r.ssDivergedGroups.Load(); got != 1 {
		t.Fatalf("divergedGroups = %d, want 1", got)
	}
	if iu, sl := r.ssIndependentUpper.Load(), r.ssSerializedLower.Load(); iu != 0 || sl != 0 {
		t.Fatalf("excluded group must add no counts: upper=%d lower=%d, want 0/0", iu, sl)
	}
	if u, l := r.ssUpperTotalNetWei.Load(), r.ssLowerTotalNetWei.Load(); u.Sign() != 0 || l.Sign() != 0 {
		t.Fatalf("excluded group must add no wei: upper=%s lower=%s (round-1 would have clamped+added)", u, l)
	}

	// VALID group (lower=15 <= upper=20): both aggregates advance, diverged unchanged.
	r.serializeWithFallback(2, pair, bnbWei(20), bnbWei(15), 2, 1)
	if got := r.ssDivergedGroups.Load(); got != 1 {
		t.Fatalf("valid group must not bump divergedGroups, got %d", got)
	}
	if iu, sl := r.ssIndependentUpper.Load(), r.ssSerializedLower.Load(); iu != 2 || sl != 1 {
		t.Fatalf("valid group counts: upper=%d (want 2) lower=%d (want 1)", iu, sl)
	}
	if u, l := r.ssUpperTotalNetWei.Load(), r.ssLowerTotalNetWei.Load(); u.Cmp(bnbWei(20)) != 0 || l.Cmp(bnbWei(15)) != 0 {
		t.Fatalf("valid group wei: upper=%s (want 20) lower=%s (want 15)", u, l)
	}
}

// TestSerializeSharedSubstrateOvercountIsReal demonstrates, with the AMM math the
// live mode drives, that valuing two contending opportunities on a SHARED evolving
// substrate yields a strictly SMALLER sum than valuing each in isolation — i.e. the
// concurrency over-count (independentUpper > serializedLower) is a genuine
// mutual-exclusion effect, not the round-1 stripped-state artifact.
func TestSerializeSharedSubstrateOvercountIsReal(t *testing.T) {
	gamma := strategy.GammaPancakeV2
	s18 := func(v int64) *big.Int { return new(big.Int).Mul(big.NewInt(v), e18) }

	// A cross-pool gap: pool A (X->Y) and pool B (Y->X), each priced so a round trip
	// X->Y->X is profitable. (Same shape as the validated 2-pool closed form.)
	raIn, raOut := s18(1_000_000), s18(1_050_000) // pool A reserves (X, Y)
	rbIn, rbOut := s18(1_000_000), s18(1_050_000) // pool B reserves (Y, X)

	optIn0, gross0 := strategy.OptimalArb(raIn, raOut, rbIn, rbOut, gamma)
	if gross0.Sign() <= 0 || optIn0.Sign() <= 0 {
		t.Fatalf("setup: expected a profitable gap, got optIn=%s gross=%s", optIn0, gross0)
	}

	// INDEPENDENT (upper): two arbers each value the SAME fresh gap -> 2 * gross0.
	independentUpper := new(big.Int).Mul(gross0, big.NewInt(2))

	// SERIALIZED (lower): arber 1 executes optIn0 on the shared substrate; the
	// reserves move; arber 2 then values the RESIDUAL gap.
	yOut := strategy.GetAmountOut(optIn0, raIn, raOut, gamma)
	xBack := strategy.GetAmountOut(yOut, rbIn, rbOut, gamma)
	raIn2 := new(big.Int).Add(raIn, optIn0)
	raOut2 := new(big.Int).Sub(raOut, yOut)
	rbIn2 := new(big.Int).Add(rbIn, yOut)
	rbOut2 := new(big.Int).Sub(rbOut, xBack)

	_, gross1 := strategy.OptimalArb(raIn2, raOut2, rbIn2, rbOut2, gamma)
	if gross1.Sign() < 0 {
		gross1 = big.NewInt(0) // a non-positive residual contributes nothing.
	}
	serializedLower := new(big.Int).Add(gross0, gross1)

	// The second opportunity shrank on the shared substrate (the first arb consumed
	// the gap): this is the real over-count.
	if gross1.Cmp(gross0) >= 0 {
		t.Fatalf("shared substrate must SHRINK the second opportunity: gross1=%s gross0=%s", gross1, gross0)
	}
	if independentUpper.Cmp(serializedLower) <= 0 {
		t.Fatalf("independentUpper (%s) must exceed serializedLower (%s) — the concurrency over-count",
			independentUpper, serializedLower)
	}
}
