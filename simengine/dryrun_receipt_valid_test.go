// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_receipt_valid_test.go unit-proves the ROUND-1 receipt-valid COUNT-GUARD
// fix (simengine/dryrun_receipt_valid.go). Round-1 only forward-diffed simulated
// receipts against canonical and had NO count guard, so a block where the simulator
// DROPPED a tx (fewer sim receipts) could still PASS as long as every PRESENT
// simulated receipt diffed clean — a false PASS.
//
// The fix counts the NON-SYSTEM canonical receipts (the population the simulator
// executes) and FAILs the block (droppedTxBlocks++) whenever that count differs
// from the simulated-receipt count, regardless of the present-receipt diffs. This
// test pins the count-guard DECISION and its inputs hermetically:
//
//	countNonSystemCanonical correctly counts the executed canonical population
//	(excluding txs with no canonical receipt; engine here is non-PoSA so nothing is
//	system-skipped), and the guard predicate (canonNonSystem != len(simReceipts))
//	is TRUE exactly when a tx is dropped EVEN THOUGH every present receipt matches.
//
// The droppedTxBlocks++ increment is the one-line consequence of this predicate
// inside receiptValidBlock; that full path drives a real BlockChain + SimulateOnState
// (a live node) and is exercised under SIMENGINE_DRYRUN=receipt-valid at runtime.
package simengine

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// newTestReceiptValidRunner builds a dryRunner with a non-PoSA SimEngine (engine
// nil), so countNonSystemCanonical treats every receipted tx as non-system.
func newTestReceiptValidRunner() *dryRunner {
	return &dryRunner{e: &SimEngine{}}
}

// rtvMkTx builds a distinct legacy tx (unique nonce -> unique hash).
func rtvMkTx(nonce uint64) *types.Transaction {
	return types.NewTransaction(nonce, common.HexToAddress("0x000000000000000000000000000000000000dEaD"), big.NewInt(0), 21000, big.NewInt(1), nil)
}

// rtvRcpt builds a minimal receipt for a tx.
func rtvRcpt(tx *types.Transaction, status, gas uint64) *types.Receipt {
	return &types.Receipt{TxHash: tx.Hash(), Status: status, GasUsed: gas, CumulativeGasUsed: gas}
}

// TestReceiptValidCountGuardCatchesDroppedTx pins the round-1 false-PASS: a dropped
// tx (sim count < canonical count) FAILs the count guard even though every PRESENT
// simulated receipt diffs clean.
func TestReceiptValidCountGuardCatchesDroppedTx(t *testing.T) {
	r := newTestReceiptValidRunner()
	hdr := &types.Header{Number: big.NewInt(1)}

	tx0, tx1, tx2 := rtvMkTx(0), rtvMkTx(1), rtvMkTx(2)
	txs := types.Transactions{tx0, tx1, tx2}

	realByHash := map[common.Hash]*types.Receipt{
		tx0.Hash(): rtvRcpt(tx0, 1, 21000),
		tx1.Hash(): rtvRcpt(tx1, 1, 21000),
		tx2.Hash(): rtvRcpt(tx2, 1, 21000),
	}

	// SIMULATED: tx1 dropped; the two PRESENT sim receipts are byte-identical to
	// canonical, so round-1's forward-only diff would have reported a clean PASS.
	simByHash := map[common.Hash]*types.Receipt{
		tx0.Hash(): rtvRcpt(tx0, 1, 21000),
		tx2.Hash(): rtvRcpt(tx2, 1, 21000),
	}
	for h, sim := range simByHash {
		if d := diffReceipt(sim, realByHash[h]); d != "" {
			t.Fatalf("present receipt %s must diff clean (false-PASS precondition), got %q", h.Hex(), d)
		}
	}

	// COUNT GUARD: 3 non-system canonical receipts != 2 simulated -> countDiverged ->
	// the block FAILs (droppedTxBlocks++). This is the predicate round-1 lacked.
	canonNonSystem := r.countNonSystemCanonical(hdr, txs, realByHash)
	if canonNonSystem != 3 {
		t.Fatalf("countNonSystemCanonical = %d, want 3", canonNonSystem)
	}
	if !(canonNonSystem != len(simByHash)) {
		t.Fatalf("count guard must fire on a dropped tx: canonNonSystem=%d simReceipts=%d", canonNonSystem, len(simByHash))
	}

	// CONTROL: no drop, all present -> guard does NOT fire (block PASSes).
	simAll := map[common.Hash]*types.Receipt{
		tx0.Hash(): rtvRcpt(tx0, 1, 21000),
		tx1.Hash(): rtvRcpt(tx1, 1, 21000),
		tx2.Hash(): rtvRcpt(tx2, 1, 21000),
	}
	if r.countNonSystemCanonical(hdr, txs, realByHash) != len(simAll) {
		t.Fatalf("no-drop control: count guard must NOT fire")
	}
}

// TestCountNonSystemCanonicalExcludesUnreceiptedTx confirms the canonical side of
// the guard counts only txs that actually executed canonically (have a receipt).
func TestCountNonSystemCanonicalExcludesUnreceiptedTx(t *testing.T) {
	r := newTestReceiptValidRunner()
	hdr := &types.Header{Number: big.NewInt(1)}

	tx0, tx1 := rtvMkTx(0), rtvMkTx(1)
	txs := types.Transactions{tx0, tx1}
	realByHash := map[common.Hash]*types.Receipt{
		tx0.Hash(): rtvRcpt(tx0, 1, 21000), // tx1 has NO canonical receipt
	}
	if n := r.countNonSystemCanonical(hdr, txs, realByHash); n != 1 {
		t.Fatalf("countNonSystemCanonical = %d, want 1 (tx with no canonical receipt excluded)", n)
	}
}

// TestRtvDeriveStrideSpansWholeRange pins the round-1 STRATIFICATION bug: the
// sampler must derive a stride so that `count` samples span the ENTIRE
// [start, start+stride*count] window, NOT a contiguous stride=1 tip window (which
// only covered the most-recent ~count blocks). The derived stride therefore reaches
// the END of the range, leaving at most one sub-stride remainder.
func TestRtvDeriveStrideSpansWholeRange(t *testing.T) {
	// Cleanly divisible: 2,000,000-block range / 500 samples => stride 4000, and the
	// 500 samples reach EXACTLY the end of the range.
	const start, end, count = uint64(100_000_000), uint64(102_000_000), uint64(500)
	stride := rtvDeriveStride(start, end, count)
	if stride != 4000 {
		t.Fatalf("rtvDeriveStride = %d, want 4000", stride)
	}
	if stride <= 1 {
		t.Fatalf("derived stride must NOT be a contiguous tip window (stride>1), got %d", stride)
	}
	span := stride * count
	if start+span != end {
		t.Fatalf("samples must span the whole range: start+stride*count=%d, want end=%d", start+span, end)
	}

	// Non-divisible range: the span must still reach the end up to a sub-stride
	// remainder (strictly less than one stride short), i.e. it covers the WHOLE
	// range, not a tip window.
	const e2 = uint64(101_999_137) // 1,999,137-block range
	s2 := rtvDeriveStride(start, e2, count)
	cover := start + s2*count
	if cover > e2 {
		t.Fatalf("coverage overshoot: %d > end %d", cover, e2)
	}
	if e2-cover >= s2 {
		t.Fatalf("coverage must be within one stride of the end (whole-range span); end-cover=%d stride=%d", e2-cover, s2)
	}

	// Degenerate guards: empty/inverted range or zero count -> stride 1 (defensive).
	if g := rtvDeriveStride(end, start, count); g != 1 {
		t.Fatalf("inverted range must yield stride 1, got %d", g)
	}
	if g := rtvDeriveStride(start, end, 0); g != 1 {
		t.Fatalf("zero count must yield stride 1, got %d", g)
	}
}

// TestResolveStrideStratifiesToFirstHead pins the deferred-end path: when endHeight
// is not pinned at parse time, the stride is derived from the FIRST head at/after
// startHeight so the sample stratifies [startHeight, firstHead] — again the whole
// range, not a tip window. Idempotent (only the first resolving head wins).
func TestResolveStrideStratifiesToFirstHead(t *testing.T) {
	s := &rtvSampling{startHeight: 100_000_000, maxCount: 500}

	// A head BELOW startHeight must not resolve (still out of range).
	s.resolveStride(99_000_000)
	if s.resolved.Load() {
		t.Fatalf("head below startHeight must not resolve the stride")
	}

	// First in-range head resolves to [start, head] with the stratified stride.
	const head = uint64(102_000_000)
	s.resolveStride(head)
	if !s.resolved.Load() {
		t.Fatalf("in-range head must resolve the stride")
	}
	if s.endHeight != head {
		t.Fatalf("endHeight = %d, want first head %d", s.endHeight, head)
	}
	want := rtvDeriveStride(s.startHeight, head, s.maxCount)
	if got := s.stride.Load(); got != want {
		t.Fatalf("resolved stride = %d, want %d", got, want)
	}
	if want <= 1 {
		t.Fatalf("resolved stride must stratify (>1), got %d", want)
	}
	if s.startHeight+want*s.maxCount != head {
		t.Fatalf("resolved sample must span [start, firstHead]; start+stride*count=%d head=%d", s.startHeight+want*s.maxCount, head)
	}

	// Idempotent: a later head does not re-resolve.
	s.resolveStride(200_000_000)
	if s.endHeight != head {
		t.Fatalf("resolveStride must be idempotent; endHeight changed to %d", s.endHeight)
	}
}

// TestDiffReceiptCatchesContentMismatch is the negative control for the guard's
// companion diff: a status/gas mismatch on a PRESENT receipt is surfaced (so a FAIL
// is not solely dependent on the count guard).
func TestDiffReceiptCatchesContentMismatch(t *testing.T) {
	tx0 := rtvMkTx(0)
	if d := diffReceipt(rtvRcpt(tx0, 1, 21000), rtvRcpt(tx0, 0, 21000)); d == "" {
		t.Fatalf("status mismatch must be reported")
	}
	if d := diffReceipt(rtvRcpt(tx0, 1, 21000), rtvRcpt(tx0, 1, 30000)); d == "" {
		t.Fatalf("gasUsed mismatch must be reported")
	}
	if d := diffReceipt(rtvRcpt(tx0, 1, 21000), rtvRcpt(tx0, 1, 21000)); d != "" {
		t.Fatalf("identical receipts must diff clean, got %q", d)
	}
}
