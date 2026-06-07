// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// This file implements an in-process, read-only self-test for the SimEngine.
//
// It subscribes to newly imported chain heads and, for every Nth block,
// re-executes that block's transactions through the SimEngine on top of the
// LIVE node state at the parent block, then compares the simulated receipts and
// logs against the block's REAL stored receipts. This validates the SimEngine
// against real mainnet blocks using the node's own state, side-stepping the
// Pebble lock and missing state-history freezer that would block a standalone
// out-of-process replay tool.
//
// The self-test is strictly READ-ONLY: it executes only on a copy of the state
// (state.Copy()), never commits, and never mutates the blockchain. Every
// per-block unit of work is wrapped in defer/recover so a bug can never panic or
// stall the node. It is wired in behind an env var and is a no-op when disabled.
package simengine

import (
	"bytes"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// StartSelfTest launches the in-process SimEngine self-test loop. It subscribes
// to new chain heads and, for every everyN-th imported block (every block when
// everyN <= 1), re-executes the block's non-system transactions via the
// SimEngine on the parent state and compares the result to the real receipts.
//
// It blocks on the subscription channel and is meant to be run in its own
// goroutine. It returns only when the subscription is closed (node shutdown).
//
// bc is the live blockchain (used as the state source and chain context), cfg
// is the chain configuration, and engine is the consensus engine (Parlia on
// BSC) reused for author/system-tx detection. The blockchain is never mutated.
func StartSelfTest(bc *core.BlockChain, cfg *params.ChainConfig, engine consensus.Engine, everyN int) {
	if bc == nil {
		log.Warn("SimEngine self-test not started: nil blockchain")
		return
	}
	if everyN <= 1 {
		everyN = 1
	}

	// Build a SimEngine that reuses the live node's state cache and chain
	// config. We deliberately do NOT use simengine.New (which opens its own
	// trie database from raw chaindata); instead we attach to the running
	// blockchain so StateAt and the chain context come straight from the node.
	e := &SimEngine{
		chainCfg: cfg,
		engine:   engine,
	}

	ch := make(chan core.ChainHeadEvent, 16)
	sub := bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	var (
		validated uint64
		passed    uint64
		failed    uint64
	)

	log.Info("SimEngine self-test loop started", "everyN", everyN)

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine self-test loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			// Sample: only validate every Nth block by height.
			if everyN > 1 && head.Number.Uint64()%uint64(everyN) != 0 {
				continue
			}
			// Run the per-block work crash-safely: a bug must never panic or
			// stall the node.
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Warn("SimEngine self-test recovered from panic", "block", head.Number, "panic", r)
					}
				}()
				e.validateBlock(bc, head, &validated, &passed, &failed)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine self-test loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// validateBlock re-executes one block's transactions on the parent state and
// compares the simulated receipts to the real stored receipts. It updates the
// running tallies and logs a single INFO line per validated block (plus a WARN
// with the first mismatch on FAIL). It is read-only.
func (e *SimEngine) validateBlock(bc *core.BlockChain, head *types.Header, validated, passed, failed *uint64) {
	blockHash := head.Hash()
	number := head.Number.Uint64()

	block := bc.GetBlockByHash(blockHash)
	if block == nil {
		log.Debug("SimEngine self-test: block not found, skipping", "block", number, "hash", blockHash)
		return
	}

	parent := bc.GetHeaderByHash(head.ParentHash)
	if parent == nil {
		log.Debug("SimEngine self-test: parent header not found, skipping", "block", number, "parent", head.ParentHash)
		return
	}

	// Obtain the live state at the parent root. If it is unavailable (e.g.
	// pruned), skip silently — this is expected, not a failure.
	statedb, err := bc.StateAt(parent.Root)
	if err != nil {
		log.Debug("SimEngine self-test: parent state unavailable, skipping", "block", number, "root", parent.Root, "err", err)
		return
	}

	real := bc.GetReceiptsByHash(blockHash)
	if real == nil {
		log.Debug("SimEngine self-test: real receipts unavailable, skipping", "block", number)
		return
	}

	// Re-execute on a COPY of the live state via the shared execution loop.
	// bc serves as the chain context (header lookups + reserved-gas). Nothing
	// is ever committed.
	res, err := e.SimulateOnState(statedb.Copy(), bc, head, block.Transactions(), nil)
	if err != nil {
		log.Warn("SimEngine self-test: simulation failed, skipping", "block", number, "err", err)
		return
	}

	// Compare simulated receipts to the real ones over NON-system txs, matched
	// by transaction hash. The simulated set already excludes system txs; build
	// a lookup from the real receipts by tx hash.
	realByHash := make(map[common.Hash]*types.Receipt, len(real))
	for _, r := range real {
		realByHash[r.TxHash] = r
	}

	mismatch := ""
	matched := 0
	for _, sim := range res.Receipts {
		r, ok := realByHash[sim.TxHash]
		if !ok {
			mismatch = fmt.Sprintf("simulated tx %s has no matching real receipt", sim.TxHash.Hex())
			break
		}
		if d := diffReceipt(sim, r); d != "" {
			mismatch = d
			break
		}
		matched++
	}

	*validated++
	if mismatch == "" {
		*passed++
	} else {
		*failed++
	}

	status := "PASS"
	if mismatch != "" {
		status = "FAIL"
	}
	log.Info("SimEngine self-test",
		"block", number,
		"txs", matched,
		"status", status,
		"passed", *passed,
		"failed", *failed,
	)
	if mismatch != "" {
		log.Warn("SimEngine self-test mismatch", "block", number, "detail", mismatch)
	}
}

// diffReceipt compares a simulated receipt against the real one and returns a
// human-readable description of the FIRST mismatch, or "" if they match over
// the consensus-relevant fields (Status, GasUsed, CumulativeGasUsed, and per-log
// Address/Topics/Data).
func diffReceipt(sim, real *types.Receipt) string {
	if sim.Status != real.Status {
		return fmt.Sprintf("tx %s status mismatch: sim=%d real=%d", sim.TxHash.Hex(), sim.Status, real.Status)
	}
	if sim.GasUsed != real.GasUsed {
		return fmt.Sprintf("tx %s gasUsed mismatch: sim=%d real=%d", sim.TxHash.Hex(), sim.GasUsed, real.GasUsed)
	}
	if sim.CumulativeGasUsed != real.CumulativeGasUsed {
		return fmt.Sprintf("tx %s cumulativeGasUsed mismatch: sim=%d real=%d", sim.TxHash.Hex(), sim.CumulativeGasUsed, real.CumulativeGasUsed)
	}
	if len(sim.Logs) != len(real.Logs) {
		return fmt.Sprintf("tx %s log count mismatch: sim=%d real=%d", sim.TxHash.Hex(), len(sim.Logs), len(real.Logs))
	}
	for i := range sim.Logs {
		sl, rl := sim.Logs[i], real.Logs[i]
		if sl.Address != rl.Address {
			return fmt.Sprintf("tx %s log[%d] address mismatch: sim=%s real=%s", sim.TxHash.Hex(), i, sl.Address.Hex(), rl.Address.Hex())
		}
		if len(sl.Topics) != len(rl.Topics) {
			return fmt.Sprintf("tx %s log[%d] topic count mismatch: sim=%d real=%d", sim.TxHash.Hex(), i, len(sl.Topics), len(rl.Topics))
		}
		for t := range sl.Topics {
			if sl.Topics[t] != rl.Topics[t] {
				return fmt.Sprintf("tx %s log[%d] topic[%d] mismatch: sim=%s real=%s", sim.TxHash.Hex(), i, t, sl.Topics[t].Hex(), rl.Topics[t].Hex())
			}
		}
		if !bytes.Equal(sl.Data, rl.Data) {
			return fmt.Sprintf("tx %s log[%d] data mismatch: sim=%x real=%x", sim.TxHash.Hex(), i, sl.Data, rl.Data)
		}
	}
	return ""
}
