// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_panicfix_test.go documents the test status of the realizability panic-fix
// (simengine/dryrun_realizability.go: rzRecoveredPanics.Add(1) INSIDE the per-victim
// recover()). See TestRealizabilityPanicCounterRequiresPipeline for why this is a
// SKIP rather than a brittle unit test.
package simengine

import "testing"

// TestRealizabilityPanicCounterRequiresPipeline is intentionally SKIPPED.
//
// The fix adds rzRecoveredPanics.Add(1) INSIDE the defer/recover that wraps the
// per-victim ex-post evaluation (rzEvaluateExPostVictim) in realizabilityBlock's
// onTx hook. There is no unit-scope seam to inject a per-victim panic: the recover
// lives inside an anonymous closure in the block-replay loop, and
// rzEvaluateExPostVictim only panics through the EVM/state machinery
// (resolvePoolMeta / optimalFrontrunAny over a real StateDB + BlockChain). Forcing
// a panic would require standing up a full block pipeline (a live *core.BlockChain,
// which is a concrete type and cannot be mocked) and a victim tx crafted to fault
// mid-evaluation — a brittle, node-dependent test the task explicitly says to avoid.
//
// The increment is a single statement inside the recover() and is covered by the
// live realizability run (its tally surfaces recoveredPanics). The detection/
// matching core that DOES have a unit seam is covered by dryrun_realizability_test.go.
func TestRealizabilityPanicCounterRequiresPipeline(t *testing.T) {
	t.Skip("rzRecoveredPanics++ lives inside the per-victim recover() in the block-replay loop; " +
		"injecting a per-victim panic needs a full EVM/BlockChain pipeline (no hermetic seam) — " +
		"covered by the live SIMENGINE_DRYRUN=realizability run instead.")
}
