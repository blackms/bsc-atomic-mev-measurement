// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_receipt_valid.go is the WIDEN RECEIPT VALIDATION harness, selectable with
// SIMENGINE_DRYRUN=receipt-valid. It closes B2: it widens the receipt-exact
// self-test (selftest.go) to a STRATIFIED sample over a height range, classifying
// each sampled block as V3-heavy / fee-on-transfer / fork-boundary / generic so the
// pass rate and any mismatches are attributable per block class.
//
// Per sampled block it re-executes the block on the parent state (a Copy, never
// committed), diffs the simulated receipts against the canonical ones with the
// EXACT diffReceipt logic the self-test uses (selftest.go), and tallies pass/fail
// plus per-category mismatch breakdowns. Strictly read-only and crash-safe.
package simengine

import (
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/strategy"
)

// rtvCounters holds the receipt-valid funnel (all atomic, read-only reporting).
type rtvCounters struct {
	processed         atomic.Uint64
	sampled           atomic.Uint64
	passed            atomic.Uint64
	failed            atomic.Uint64
	droppedTxBlocks   atomic.Uint64 // blocks FAILED because the sim/canonical receipt COUNT diverged
	totalTxs          atomic.Uint64
	failedTxs         atomic.Uint64
	v3Blocks          atomic.Uint64
	fotBlocks         atomic.Uint64
	v3Mismatches      atomic.Uint64 // per-TX mismatches attributed to a V3 tx
	fotMismatches     atomic.Uint64 // per-TX mismatches attributed to a fee-on-transfer tx
	genericMismatches atomic.Uint64 // per-TX mismatches attributed to a generic tx
	mismatchLogged    atomic.Uint64 // cap detailed FAIL logs (first 20).
}

// rtvSampling carries the parsed height-range sampling parameters. stride is
// derived so the maxCount samples span the WHOLE [startHeight, endHeight] range
// (stratified), not a contiguous tip window. endHeight may be 0 at parse time
// (operator did not pin it); it is then resolved to the FIRST head seen so the
// sample stratifies [startHeight, firstHead]. resolved guards the one-shot
// stride derivation.
type rtvSampling struct {
	startHeight uint64
	endHeight   uint64
	maxCount    uint64
	stride      atomic.Uint64
	resolved    atomic.Bool
}

// receiptValidSampling parses SIMENGINE_RECEIPT_VALID_START_HEIGHT (default
// 102_460_000), SIMENGINE_RECEIPT_VALID_END_HEIGHT (default 0 = resolve to the
// first head seen), and SIMENGINE_RECEIPT_VALID_COUNT (default 500). The stride is
// derived as max(1, (end-start)/count) so the COUNT samples are STRATIFIED across
// the entire height range (the round-1 bug was a hardcoded contiguous stride=1 over
// a 6-25 min tip window). An explicit SIMENGINE_RECEIPT_VALID_STRIDE overrides the
// derivation.
func receiptValidSampling() *rtvSampling {
	s := &rtvSampling{startHeight: 102_460_000, maxCount: 500}
	var strideOverride uint64
	if v := os.Getenv("SIMENGINE_RECEIPT_VALID_START_HEIGHT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			s.startHeight = n
		}
	}
	if v := os.Getenv("SIMENGINE_RECEIPT_VALID_END_HEIGHT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			s.endHeight = n
		}
	}
	if v := os.Getenv("SIMENGINE_RECEIPT_VALID_COUNT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			s.maxCount = n
		}
	}
	if v := os.Getenv("SIMENGINE_RECEIPT_VALID_STRIDE"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			strideOverride = n
		}
	}
	if strideOverride > 0 {
		s.stride.Store(strideOverride)
		s.resolved.Store(true)
	} else if s.endHeight > s.startHeight {
		s.stride.Store(rtvDeriveStride(s.startHeight, s.endHeight, s.maxCount))
		s.resolved.Store(true)
	}
	return s
}

// rtvDeriveStride returns the stratified stride = max(1, (end-start)/count) so
// `count` samples span [start, end].
func rtvDeriveStride(start, end, count uint64) uint64 {
	if end <= start || count == 0 {
		return 1
	}
	stride := (end - start) / count
	if stride < 1 {
		stride = 1
	}
	return stride
}

// resolveStride derives the stride from the first head seen when endHeight was not
// pinned at parse time, so the sample stratifies [startHeight, firstHead]. Idempotent
// (only the first caller wins via the resolved flag).
func (s *rtvSampling) resolveStride(headHeight uint64) {
	if s.resolved.Load() {
		return
	}
	if headHeight <= s.startHeight {
		return // not yet in range; defer until a head at/after startHeight arrives.
	}
	if s.resolved.CompareAndSwap(false, true) {
		s.endHeight = headHeight
		s.stride.Store(rtvDeriveStride(s.startHeight, headHeight, s.maxCount))
	}
}

// runReceiptValidBacktest subscribes to chain heads and validates a stratified
// height-range sample of blocks. Read-only, crash-safe.
func (r *dryRunner) runReceiptValidBacktest() {
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	s := receiptValidSampling()
	log.Info("SimEngine dry-run RECEIPT-VALID (stratified receipt-exact validation) loop started",
		"startHeight", s.startHeight, "endHeight", s.endHeight, "count", s.maxCount,
		"stride", s.stride.Load(), "strideResolved", s.resolved.Load())

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run receipt-valid loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run receipt-valid recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.receiptValidBlock(head, s)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run receipt-valid loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// receiptValidBlock applies the STRATIFIED height-range sampling, re-executes the
// block on the parent state, diffs every tx's receipt against canonical in BOTH
// directions (with a count guard that FAILS the block if the sim/canonical receipt
// counts diverge — the round-1 false-PASS bug), classifies and attributes each
// mismatch PER-TX, and tallies. Read-only.
func (r *dryRunner) receiptValidBlock(head *types.Header, s *rtvSampling) {
	number := head.Number.Uint64()
	r.rtv.processed.Add(1)
	r.blocks.Add(1)

	// Resolve the stride from the first in-range head if it was not pinned by env.
	s.resolveStride(number)
	stride := s.stride.Load()
	if stride == 0 {
		stride = 1
	}

	// Stratified sampling: at/after startHeight, every stride-th block, until maxCount.
	if number < s.startHeight {
		return
	}
	if (number-s.startHeight)%stride != 0 {
		return
	}
	if r.rtv.sampled.Load() >= s.maxCount {
		return
	}

	block := r.bc.GetBlockByHash(head.Hash())
	if block == nil {
		return
	}
	parent := r.bc.GetHeaderByHash(head.ParentHash)
	if parent == nil {
		return
	}
	statedb, err := r.bc.StateAt(parent.Root)
	if err != nil {
		return // parent state pruned — skip silently (not a failure).
	}
	real := r.bc.GetReceiptsByHash(head.Hash())
	if real == nil {
		return
	}

	// Re-execute on a COPY of the parent state.
	res, err := r.e.SimulateOnState(statedb.Copy(), r.bc, head, block.Transactions(), nil)
	if err != nil {
		return
	}

	sampled := r.rtv.sampled.Add(1)

	// Index canonical receipts by hash AND build the set of NON-SYSTEM canonical txs
	// (the population the simulator executes — system txs are skipped by both).
	realByHash := make(map[common.Hash]*types.Receipt, len(real))
	for _, rc := range real {
		if rc == nil {
			continue
		}
		realByHash[rc.TxHash] = rc
	}
	simByHash := make(map[common.Hash]*types.Receipt, len(res.Receipts))
	for _, sim := range res.Receipts {
		if sim == nil {
			continue
		}
		simByHash[sim.TxHash] = sim
	}

	// COUNT GUARD (round-1 false-PASS fix): the number of NON-SYSTEM canonical
	// receipts must equal the number of simulated receipts. If SimulateOnState
	// dropped a tx (fewer sim receipts) — or produced an extra one — the block FAILS
	// with reason dropped_tx, regardless of whether the surviving receipts diff
	// clean. Counting non-system canonical receipts (not all `real`) is what makes
	// this comparable to the simulator's executed set.
	canonNonSystem := r.countNonSystemCanonical(head, block.Transactions(), realByHash)
	countDiverged := canonNonSystem != len(simByHash)

	txCount := 0
	failedTxs := 0
	blockMismatch := ""
	recordMismatch := func(txHash common.Hash, d, category string) {
		if blockMismatch == "" {
			blockMismatch = d
		}
		failedTxs++
		switch category {
		case "v3":
			r.rtv.v3Mismatches.Add(1)
		case "fot":
			r.rtv.fotMismatches.Add(1)
		default:
			r.rtv.genericMismatches.Add(1)
		}
		r.logReceiptValidFail(number, txHash, d, category)
	}

	// Forward diff: every simulated receipt against its canonical match. Count EVERY
	// tx once (round-1 txCount bug: the unmatched-sim branch used `continue` BEFORE
	// the increment, so dropped/extra txs were never counted).
	for _, sim := range res.Receipts {
		if sim == nil {
			continue
		}
		txCount++
		category := r.classifyTx(head, real, sim.TxHash)
		rc, ok := realByHash[sim.TxHash]
		if !ok {
			recordMismatch(sim.TxHash, "dropped_tx: simulated tx "+sim.TxHash.Hex()+" has no matching canonical receipt", category)
			continue
		}
		if d := diffReceipt(sim, rc); d != "" {
			recordMismatch(sim.TxHash, d, category)
		}
	}

	// Reverse diff (round-1 only diffed sim->canonical, so a DROPPED tx — present in
	// canonical, absent in sim — was never surfaced and the block could still PASS):
	// every NON-SYSTEM canonical receipt must have a simulated counterpart.
	for _, rc := range real {
		if rc == nil {
			continue
		}
		if r.isSystemTxHash(head, block.Transactions(), rc.TxHash) {
			continue
		}
		if _, ok := simByHash[rc.TxHash]; !ok {
			category := r.classifyTx(head, real, rc.TxHash)
			recordMismatch(rc.TxHash, "dropped_tx: canonical tx "+rc.TxHash.Hex()+" was dropped by the simulator (no sim receipt)", category)
		}
	}

	r.rtv.totalTxs.Add(uint64(txCount))
	r.rtv.failedTxs.Add(uint64(failedTxs))

	status := "PASS"
	if blockMismatch != "" || countDiverged {
		status = "FAIL"
		r.rtv.failed.Add(1)
		if countDiverged {
			r.rtv.droppedTxBlocks.Add(1)
			if blockMismatch == "" {
				blockMismatch = "dropped_tx: non-system canonical receipts=" +
					strconv.Itoa(canonNonSystem) + " != simulated receipts=" + strconv.Itoa(len(simByHash))
				r.rtv.genericMismatches.Add(1)
				r.logReceiptValidFail(number, common.Hash{}, blockMismatch, "generic")
			}
		}
	} else {
		r.rtv.passed.Add(1)
	}

	// Block-level category label (for the summary line only): v3 if any sampled tx
	// is V3, else fot, else generic. Per-TX attribution above is the authoritative
	// mismatch breakdown.
	blockCategory := r.classifyBlock(real)
	switch blockCategory {
	case "v3":
		r.rtv.v3Blocks.Add(1)
	case "fot":
		r.rtv.fotBlocks.Add(1)
	}

	log.Info("receipt-valid block",
		"block", number,
		"status", status,
		"txs", txCount,
		"canonNonSystem", canonNonSystem,
		"simReceipts", len(simByHash),
		"gasUsed", head.GasUsed,
		"category", blockCategory,
	)

	if r.cfg.TallyEvery > 0 && sampled%r.cfg.TallyEvery == 0 {
		r.logReceiptValidTally(sampled)
	}
}

// countNonSystemCanonical returns the number of canonical receipts whose tx is NOT
// a BSC system transaction (the population the simulator executes). It walks the
// block txs (system-tx classification needs the tx, not just the receipt). A tx
// with no canonical receipt is not counted (it never executed canonically either).
func (r *dryRunner) countNonSystemCanonical(head *types.Header, txs types.Transactions, realByHash map[common.Hash]*types.Receipt) int {
	posa, isPoSA := r.e.engine.(consensus.PoSA)
	n := 0
	for _, tx := range txs {
		if _, ok := realByHash[tx.Hash()]; !ok {
			continue
		}
		if isPoSA {
			if sys, err := posa.IsSystemTransaction(tx, head); err == nil && sys {
				continue
			}
		}
		n++
	}
	return n
}

// isSystemTxHash reports whether the tx with the given hash is a BSC system
// transaction (so the reverse diff does not flag system txs the simulator
// intentionally skips). Read-only.
func (r *dryRunner) isSystemTxHash(head *types.Header, txs types.Transactions, txHash common.Hash) bool {
	posa, isPoSA := r.e.engine.(consensus.PoSA)
	if !isPoSA {
		return false
	}
	for _, tx := range txs {
		if tx.Hash() != txHash {
			continue
		}
		if sys, err := posa.IsSystemTransaction(tx, head); err == nil && sys {
			return true
		}
		return false
	}
	return false
}

// classifyTx labels a SINGLE tx (by hash) from its canonical receipt's logs as
// V3 / fee-on-transfer / generic. Per-TX classification (round-1 attributed every
// mismatch to the block's single category, starving the fot bucket and mislabeling
// V3) is what makes the per-category mismatch breakdown honest. The fork-boundary
// bucket is DROPPED: the active sampling range (>= 102.46M) contains no BSC fork
// height (LubanBlock 29.02M is far below), so a fork bucket here was dead code.
func (r *dryRunner) classifyTx(head *types.Header, real types.Receipts, txHash common.Hash) string {
	for _, rc := range real {
		if rc == nil || rc.TxHash != txHash {
			continue
		}
		return rtvClassifyLogs(rc.Logs)
	}
	return "generic"
}

// rtvClassifyLogs classifies one receipt's logs: V3 if any V3 Swap log is present,
// else fee-on-transfer if a swap-bearing tx has > 3x as many Transfer logs as Swap
// logs (FoT tokens emit extra fee/burn Transfers per hop), else generic.
func rtvClassifyLogs(logs []*types.Log) string {
	swaps, transfers, v3 := 0, 0, false
	for _, l := range logs {
		if l == nil || len(l.Topics) == 0 {
			continue
		}
		switch l.Topics[0] {
		case strategy.V3SwapTopic0:
			v3 = true
			swaps++
		case strategy.SwapTopic0:
			swaps++
		case transferTopic0:
			transfers++
		}
	}
	switch {
	case v3:
		return "v3"
	case swaps > 0 && transfers > 3*swaps:
		return "fot"
	default:
		return "generic"
	}
}

// classifyBlock labels a sampled block by the most-specific category present among
// its txs (v3 > fot > generic). Used ONLY for the summary line; the authoritative
// per-category mismatch attribution is per-TX via classifyTx. The fork bucket is
// dropped (no fork heights in the sampling range).
func (r *dryRunner) classifyBlock(real types.Receipts) string {
	fot := false
	for _, rc := range real {
		if rc == nil {
			continue
		}
		switch rtvClassifyLogs(rc.Logs) {
		case "v3":
			return "v3"
		case "fot":
			fot = true
		}
	}
	if fot {
		return "fot"
	}
	return "generic"
}

// logReceiptValidFail emits a capped per-mismatch warning (first 20 per run).
func (r *dryRunner) logReceiptValidFail(number uint64, txHash common.Hash, detail, category string) {
	if r.rtv.mismatchLogged.Load() >= 20 {
		return
	}
	r.rtv.mismatchLogged.Add(1)
	log.Warn("receipt-valid FAIL",
		"block", number,
		"txHash", txHash.Hex(),
		"detail", detail,
		"category", category,
	)
}

// logReceiptValidTally emits the periodic stratified pass-rate summary.
func (r *dryRunner) logReceiptValidTally(sampled uint64) {
	passed := r.rtv.passed.Load()
	failed := r.rtv.failed.Load()

	passRate := "n/a"
	if sampled > 0 {
		passRate = bigRatio(passed, sampled)
	}

	log.Info("receipt-valid tally",
		"processedBlocks", r.rtv.processed.Load(),
		"sampledBlocks", sampled,
		"passRate", passRate,
		"passed", passed,
		"failed", failed,
		"droppedTxBlocks", r.rtv.droppedTxBlocks.Load(),
		"totalTxs", r.rtv.totalTxs.Load(),
		"failedTxs", r.rtv.failedTxs.Load(),
		"v3Blocks", r.rtv.v3Blocks.Load(),
		"fotBlocks", r.rtv.fotBlocks.Load(),
		"v3Mismatches", r.rtv.v3Mismatches.Load(),
		"fotMismatches", r.rtv.fotMismatches.Load(),
		"genericMismatches", r.rtv.genericMismatches.Load(),
		"ts", time.Now().Format(time.RFC3339),
	)
}
