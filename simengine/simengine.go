// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Package simengine provides an in-process, speculative transaction execution
// engine for BSC (Parlia) built directly on top of go-ethereum v1.7.3 internals.
//
// Given the head/parent state and a list of transactions, SimEngine executes
// them via core.ApplyTransaction on a COPY of the canonical state. The canonical
// state is never mutated and nothing is ever committed to disk. It returns the
// resulting receipts, flattened logs, cumulative gas used, and balance deltas
// for an optional watch list.
//
// The execution path mirrors the canonical core.StateProcessor.Process and the
// miner worker's commitTransaction pattern:
//   - state at parent.Root, worked on via StateDB.Copy()
//   - gas pool seeded from header.GasLimit, minus Parlia's reserved system-tx gas
//   - per-tx snapshot/revert on error (matching miner/worker.applyTransaction)
//   - BSC system transactions (Parlia) are detected and skipped from the normal
//     loop, exactly as the canonical processor separates them out for Finalize.
package simengine

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/parlia"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// SimResult is the outcome of a speculative simulation.
type SimResult struct {
	// Receipts holds one receipt per executed (non-system) transaction, in
	// execution order. CumulativeGasUsed is accumulated across the run.
	Receipts []*types.Receipt
	// Logs is the flattened list of all receipt logs in execution order.
	Logs []*types.Log
	// GasUsed is the total gas used across all executed transactions.
	GasUsed uint64
	// BalanceDeltas maps each watched address to (postBalance - preBalance) in
	// wei. Populated only for the addresses passed in the watch list.
	BalanceDeltas map[common.Address]*big.Int
}

// SimEngine speculatively executes transactions on a copy of canonical state.
//
// It holds references to the already-open chain database, the path-scheme trie
// database, the derived state database, the chain configuration, and a Parlia
// consensus engine (used only for read-only helpers such as system-transaction
// detection and reserved-gas estimation). It never writes to disk.
type SimEngine struct {
	db         ethdb.Database      // open chaindata (read-only)
	triedb     *triedb.Database    // PBSS path-scheme trie database
	stateCache state.Database      // state.CachingDB wrapping triedb
	chainCfg   *params.ChainConfig // resolved chain configuration
	engine     consensus.Engine    // Parlia engine (read-only use)
	vmConfig   vm.Config           // EVM config (tracers, etc.)

	chainCtx *chainContext // ChainContext for NewEVMBlockContext / GetHashFn
}

// simChainContext is the union of the two read-only chain interfaces the
// execution loop depends on: core.ChainContext (for NewEVMBlockContext / the
// GetHashFn historical header lookups and blob base-fee calculation) and
// consensus.ChainHeaderReader (for Parlia's EstimateGasReservedForSystemTxs).
//
// Both the db-backed *chainContext used by Simulate and *core.BlockChain used by
// the in-process self-test satisfy this interface, so the shared applyOnState
// helper works identically against either.
type simChainContext interface {
	core.ChainContext
	consensus.ChainHeaderReader
}

// chainContext implements core.ChainContext on top of a raw ethdb.Database. It
// is sufficient for NewEVMBlockContext (GetHashFn historical header lookups and
// the blob base-fee calculation) without instantiating a full core.BlockChain.
type chainContext struct {
	db     ethdb.Database
	config *params.ChainConfig
	engine consensus.Engine
}

func (c *chainContext) Config() *params.ChainConfig { return c.config }

func (c *chainContext) Engine() consensus.Engine { return c.engine }

func (c *chainContext) GetHeader(hash common.Hash, number uint64) *types.Header {
	return rawdb.ReadHeader(c.db, hash, number)
}

func (c *chainContext) GetHeaderByNumber(number uint64) *types.Header {
	hash := rawdb.ReadCanonicalHash(c.db, number)
	if hash == (common.Hash{}) {
		return nil
	}
	return rawdb.ReadHeader(c.db, hash, number)
}

func (c *chainContext) GetHeaderByHash(hash common.Hash) *types.Header {
	number, ok := rawdb.ReadHeaderNumber(c.db, hash)
	if !ok {
		return nil
	}
	return rawdb.ReadHeader(c.db, hash, number)
}

func (c *chainContext) CurrentHeader() *types.Header {
	hash := rawdb.ReadHeadHeaderHash(c.db)
	if hash == (common.Hash{}) {
		return nil
	}
	number, ok := rawdb.ReadHeaderNumber(c.db, hash)
	if !ok {
		return nil
	}
	return rawdb.ReadHeader(c.db, hash, number)
}

// The methods below round out the consensus.ChainHeaderReader interface so the
// same value can be handed to Parlia helpers such as
// EstimateGasReservedForSystemTxs (which only ever calls GetHeaderByHash).

func (c *chainContext) GenesisHeader() *types.Header {
	return c.GetHeaderByNumber(0)
}

func (c *chainContext) GetTd(hash common.Hash, number uint64) *big.Int {
	return rawdb.ReadTd(c.db, hash, number)
}

func (c *chainContext) GetHighestVerifiedHeader() *types.Header {
	return c.CurrentHeader()
}

func (c *chainContext) GetVerifiedBlockByHash(hash common.Hash) *types.Header {
	return c.GetHeaderByHash(hash)
}

// New wires a SimEngine from an already-open chaindata database and a PBSS
// path-scheme trie database. The chain configuration is resolved from the
// genesis stored in the DB. A Parlia engine is instantiated for read-only use.
//
// The caller retains ownership of db and tdb; SimEngine never closes them.
func New(db ethdb.Database, tdb *triedb.Database, vmConfig vm.Config) (*SimEngine, error) {
	// Resolve the genesis hash (canonical hash of block 0) and from it the
	// stored chain configuration.
	genesisHash := rawdb.ReadCanonicalHash(db, 0)
	if genesisHash == (common.Hash{}) {
		return nil, fmt.Errorf("simengine: could not read genesis hash from database")
	}
	chainCfg := rawdb.ReadChainConfig(db, genesisHash)
	if chainCfg == nil {
		return nil, fmt.Errorf("simengine: could not read chain config for genesis %s", genesisHash.Hex())
	}

	// Instantiate a Parlia engine for read-only helpers (system-tx detection,
	// reserved-gas estimation). ethAPI is nil: the helpers we use do not touch
	// it. If the chain isn't Parlia-based this stays nil and we fall back to
	// treating every transaction as a normal one.
	var engine consensus.Engine
	if chainCfg.Parlia != nil {
		engine = parlia.New(chainCfg, db, nil, genesisHash)
	}

	// state.NewDatabase builds the CachingDB that StateDB.New consumes. snap is
	// nil: we don't need the snapshot tree for read-only replay.
	stateCache := state.NewDatabase(tdb, nil)

	e := &SimEngine{
		db:         db,
		triedb:     tdb,
		stateCache: stateCache,
		chainCfg:   chainCfg,
		engine:     engine,
		vmConfig:   vmConfig,
		chainCtx: &chainContext{
			db:     db,
			config: chainCfg,
			engine: engine,
		},
	}
	return e, nil
}

// ChainConfig exposes the resolved chain configuration.
func (e *SimEngine) ChainConfig() *params.ChainConfig { return e.chainCfg }

// Simulate executes txs speculatively on top of the state at parent.Root using
// header as the block environment. It never mutates canonical state: a copy of
// the parent state is used and discarded.
//
// watch is an optional list of addresses whose balance delta (post - pre) is
// recorded in the result.
//
// System transactions (BSC/Parlia) are detected and skipped, mirroring the
// canonical processor which executes them separately during Finalize. Only the
// normal-transaction receipts are returned, in execution order.
func (e *SimEngine) Simulate(parent *types.Header, header *types.Header, txs types.Transactions, watch []common.Address) (*SimResult, error) {
	if parent == nil {
		return nil, fmt.Errorf("simengine: nil parent header")
	}
	if header == nil {
		return nil, fmt.Errorf("simengine: nil header")
	}

	// Build the canonical state at the parent root, then immediately switch to
	// an independent copy. All mutations land on the copy; the original is left
	// untouched and we never Commit anything to disk.
	canonical, err := state.New(parent.Root, e.stateCache)
	if err != nil {
		return nil, fmt.Errorf("simengine: state unavailable at parent root %s (block %d): %w",
			parent.Root.Hex(), parent.Number.Uint64(), err)
	}
	statedb := canonical.Copy()

	// Hand the copied state to the shared execution loop, using the db-backed
	// chain context for header lookups and reserved-gas estimation.
	return e.applyOnState(statedb, e.chainCtx, header, txs, watch)
}

// applyOnState runs the shared speculative execution loop against an already
// obtained *state.StateDB. The caller is responsible for passing a COPY of the
// canonical state: applyOnState mutates statedb in place and never commits, so
// the original must not be the live canonical state.
//
// chainCtx supplies the read-only header lookups (NewEVMBlockContext / GetHashFn)
// and Parlia's reserved-gas estimation. It is the single point of execution
// shared by the standalone Simulate path (db-backed chainContext) and the
// in-process self-test (*core.BlockChain), so the two can never diverge.
//
// System transactions (BSC/Parlia) are detected and skipped, per-tx errors are
// snapshot/reverted, and per-watch-address balance deltas are recorded.
func (e *SimEngine) applyOnState(statedb *state.StateDB, chainCtx simChainContext, header *types.Header, txs types.Transactions, watch []common.Address) (*SimResult, error) {
	// Apply built-in system contract code upgrades at block begin, exactly like
	// core.StateProcessor.Process does, so contract bytecode matches the real
	// execution at hard-fork boundaries. The fork-activation checks key off the
	// PARENT block time, so look it up via the chain context (matching the
	// canonical processor's lastBlock.Time) and fall back to header.Time-1 only
	// if the parent header is somehow unavailable.
	lastBlockTime := header.Time
	if parent := chainCtx.GetHeaderByHash(header.ParentHash); parent != nil {
		lastBlockTime = parent.Time
	} else if header.Time > 0 {
		lastBlockTime = header.Time - 1
	}
	systemcontracts.TryUpdateBuildInSystemContract(e.chainCfg, header.Number, lastBlockTime, header.Time, statedb, true)

	// Seed the gas pool from the header gas limit, reserving gas for Parlia
	// system transactions just as miner/worker.commitTransactions does.
	gp := new(core.GasPool).AddGas(header.GasLimit)
	if p, ok := e.engine.(*parlia.Parlia); ok {
		gasReserved := p.EstimateGasReservedForSystemTxs(chainCtx, header)
		// SubGas may fail if the reserved amount exceeds the limit; ignore the
		// error and clamp, matching the worker which logs-and-continues.
		_ = gp.SubGas(gasReserved)
	}

	// Create the EVM bound to the copied state. Pass the header coinbase as the
	// explicit author: for Parlia, Author(header) == header.Coinbase, and this
	// avoids needing a fully-initialised engine for the COINBASE opcode.
	coinbase := header.Coinbase
	blockCtx := core.NewEVMBlockContext(header, chainCtx, &coinbase)
	evm := vm.NewEVM(blockCtx, statedb, e.chainCfg, e.vmConfig)

	// Pre-execution system calls, mirroring core.StateProcessor.Process. On BSC
	// with Parlia, ProcessBeaconBlockRoot is a no-op for the zero beacon root;
	// ProcessParentBlockHash only applies once Prague is active.
	if beaconRoot := header.ParentBeaconRoot; beaconRoot != nil {
		core.ProcessBeaconBlockRoot(*beaconRoot, evm)
	}
	if e.chainCfg.IsPrague(header.Number, header.Time) || e.chainCfg.IsVerkle(header.Number, header.Time) {
		core.ProcessParentBlockHash(header.ParentHash, evm)
	}

	// Detect the consensus engine's system-transaction predicate, if any.
	posa, isPoSA := e.engine.(consensus.PoSA)

	// Record pre-balances for the watch list.
	pre := make(map[common.Address]*big.Int, len(watch))
	for _, addr := range watch {
		pre[addr] = statedb.GetBalance(addr).ToBig()
	}

	var (
		receipts = make([]*types.Receipt, 0, len(txs))
		allLogs  []*types.Log
		usedGas  uint64
		txIndex  int
	)

	for i, tx := range txs {
		// Skip BSC system transactions: they are executed by the consensus
		// engine during Finalize, not in the normal loop.
		if isPoSA {
			isSystemTx, sErr := posa.IsSystemTransaction(tx, header)
			if sErr != nil {
				return nil, fmt.Errorf("simengine: system-tx check failed for tx %d [%s]: %w", i, tx.Hash().Hex(), sErr)
			}
			if isSystemTx {
				continue
			}
		}

		// Snapshot/revert pattern from miner/worker.applyTransaction: on error
		// roll the copied state and gas pool back so a failing tx doesn't taint
		// subsequent ones. We run on a copy, so reverting is purely local.
		snap := statedb.Snapshot()
		gpSaved := gp.Gas()

		// Provide the per-tx context so logs are attributed to the right tx and
		// the receipt's TransactionIndex is correct.
		statedb.SetTxContext(tx.Hash(), txIndex)

		// usedGas is the running cumulative; ApplyTransaction adds this tx's gas
		// to it and stores it as CumulativeGasUsed in the receipt.
		receipt, aErr := core.ApplyTransaction(evm, gp, statedb, header, tx, &usedGas)
		if aErr != nil {
			// Roll back and skip this transaction, mirroring the worker.
			statedb.RevertToSnapshot(snap)
			gp.SetGas(gpSaved)
			continue
		}

		receipts = append(receipts, receipt)
		allLogs = append(allLogs, receipt.Logs...)
		txIndex++
	}

	// Record post-balances and compute deltas.
	deltas := make(map[common.Address]*big.Int, len(watch))
	for _, addr := range watch {
		post := statedb.GetBalance(addr).ToBig()
		deltas[addr] = new(big.Int).Sub(post, pre[addr])
	}

	return &SimResult{
		Receipts:      receipts,
		Logs:          allLogs,
		GasUsed:       usedGas,
		BalanceDeltas: deltas,
	}, nil
}

// TxHook is the per-transaction callback invoked by ApplyOnStateHooked after
// each successfully-applied (non-system) transaction. i is the index of the tx
// in the supplied txs slice (the original block index, NOT the executed-only
// counter), tx is the transaction, receipt is its receipt, and statedb is the
// LIVE intermediate state right after this tx — i.e. the transient state the v3
// per-swap intra-block detector evaluates. The hook MUST treat statedb as
// read-only (it is the live execution state; mutating it would corrupt the
// remaining txs). Any panic inside the hook is the caller's responsibility to
// recover; ApplyOnStateHooked does not wrap it.
type TxHook func(i int, tx *types.Transaction, receipt *types.Receipt, statedb *state.StateDB)

// ApplyOnStateHooked is a per-transaction-hooked variant of the validated
// applyOnState execution loop. It exists for the v3 intra-block detector, which
// must observe the TRANSIENT pool state right after each individual victim swap
// (before competing arbers re-align prices later in the same block) — something
// the whole-block applyOnState path cannot expose.
//
// It is a faithful DUPLICATE of applyOnState's loop: the system-contract
// upgrades, gas-pool seeding (incl. Parlia reserved-gas), EVM/block-context
// construction, EIP-4788 (beacon root) / EIP-2935 (parent block hash) pre-calls,
// system-tx skipping, snapshot/revert-on-error, and per-watch balance deltas are
// IDENTICAL. The ONLY addition is that, after each successfully-applied
// non-system tx, it invokes onTx(i, tx, receipt, statedb) with statedb being the
// live intermediate state at that point. When onTx is nil it behaves exactly
// like applyOnState.
//
// applyOnState itself is intentionally left untouched so the SimEngine self-test
// (selftest.go), which validates receipt-exactness 5/5 through applyOnState /
// SimulateOnState, can never be perturbed by this addition.
//
// The caller MUST pass a COPY of the canonical state: the loop mutates statedb
// in place and never commits.
func (e *SimEngine) ApplyOnStateHooked(statedb *state.StateDB, chainCtx simChainContext, header *types.Header, txs types.Transactions, watch []common.Address, onTx TxHook) (*SimResult, error) {
	if statedb == nil {
		return nil, fmt.Errorf("simengine: nil statedb")
	}
	if header == nil {
		return nil, fmt.Errorf("simengine: nil header")
	}

	// --- Block-begin setup: IDENTICAL to applyOnState. ---
	lastBlockTime := header.Time
	if parent := chainCtx.GetHeaderByHash(header.ParentHash); parent != nil {
		lastBlockTime = parent.Time
	} else if header.Time > 0 {
		lastBlockTime = header.Time - 1
	}
	systemcontracts.TryUpdateBuildInSystemContract(e.chainCfg, header.Number, lastBlockTime, header.Time, statedb, true)

	gp := new(core.GasPool).AddGas(header.GasLimit)
	if p, ok := e.engine.(*parlia.Parlia); ok {
		gasReserved := p.EstimateGasReservedForSystemTxs(chainCtx, header)
		_ = gp.SubGas(gasReserved)
	}

	coinbase := header.Coinbase
	blockCtx := core.NewEVMBlockContext(header, chainCtx, &coinbase)
	evm := vm.NewEVM(blockCtx, statedb, e.chainCfg, e.vmConfig)

	if beaconRoot := header.ParentBeaconRoot; beaconRoot != nil {
		core.ProcessBeaconBlockRoot(*beaconRoot, evm)
	}
	if e.chainCfg.IsPrague(header.Number, header.Time) || e.chainCfg.IsVerkle(header.Number, header.Time) {
		core.ProcessParentBlockHash(header.ParentHash, evm)
	}

	posa, isPoSA := e.engine.(consensus.PoSA)

	pre := make(map[common.Address]*big.Int, len(watch))
	for _, addr := range watch {
		pre[addr] = statedb.GetBalance(addr).ToBig()
	}

	var (
		receipts = make([]*types.Receipt, 0, len(txs))
		allLogs  []*types.Log
		usedGas  uint64
		txIndex  int
	)

	for i, tx := range txs {
		// Skip BSC system transactions: IDENTICAL to applyOnState.
		if isPoSA {
			isSystemTx, sErr := posa.IsSystemTransaction(tx, header)
			if sErr != nil {
				return nil, fmt.Errorf("simengine: system-tx check failed for tx %d [%s]: %w", i, tx.Hash().Hex(), sErr)
			}
			if isSystemTx {
				continue
			}
		}

		snap := statedb.Snapshot()
		gpSaved := gp.Gas()

		statedb.SetTxContext(tx.Hash(), txIndex)

		receipt, aErr := core.ApplyTransaction(evm, gp, statedb, header, tx, &usedGas)
		if aErr != nil {
			statedb.RevertToSnapshot(snap)
			gp.SetGas(gpSaved)
			continue
		}

		receipts = append(receipts, receipt)
		allLogs = append(allLogs, receipt.Logs...)
		txIndex++

		// ONLY addition over applyOnState: surface the live intermediate state
		// after this successfully-applied non-system tx. statedb is the live
		// execution state; the hook must treat it as read-only.
		if onTx != nil {
			onTx(i, tx, receipt, statedb)
		}
	}

	deltas := make(map[common.Address]*big.Int, len(watch))
	for _, addr := range watch {
		post := statedb.GetBalance(addr).ToBig()
		deltas[addr] = new(big.Int).Sub(post, pre[addr])
	}

	return &SimResult{
		Receipts:      receipts,
		Logs:          allLogs,
		GasUsed:       usedGas,
		BalanceDeltas: deltas,
	}, nil
}

// SimulateOnState runs the shared execution loop against a caller-supplied
// state and chain context. The caller MUST pass a copy of the canonical state
// (e.g. bc.StateAt(parent.Root) then .Copy()) — the loop mutates statedb in
// place and never commits. chainCtx is typically the live *core.BlockChain.
//
// This is the entry point used by the in-process self-test so it can reuse the
// exact execution path of Simulate without re-deriving the state database from
// raw chaindata.
func (e *SimEngine) SimulateOnState(statedb *state.StateDB, chainCtx simChainContext, header *types.Header, txs types.Transactions, watch []common.Address) (*SimResult, error) {
	if statedb == nil {
		return nil, fmt.Errorf("simengine: nil statedb")
	}
	if header == nil {
		return nil, fmt.Errorf("simengine: nil header")
	}
	return e.applyOnState(statedb, chainCtx, header, txs, watch)
}
