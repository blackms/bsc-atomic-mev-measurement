// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// evmcall.go is the v4 in-process, read-only EVM-call helper: an eth_call
// equivalent that executes a single message against a *state.StateDB using the
// SAME EVM construction machinery the validated SimEngine already uses
// (core.NewEVMBlockContext + vm.NewEVM + evm.Call). It is the substrate of the
// quoter-chaining cycle valuator (see strategy/quoter.go and quoter_oracle.go):
// V3 hops are priced by calling the deployed PancakeSwap V3 QuoterV2 contract via
// EthCall against the intermediate state.Copy(), giving exact tick-math outputs
// without a custom on-chain executor.
//
// SAFETY: EthCall is strictly read-only. It NEVER commits and NEVER persists to
// disk. The caller MUST pass a state it is willing to have mutated locally —
// nested calls within the quoter (which does a real swap then reverts itself)
// touch storage, so the canonical contract is: pass a state.Copy() and discard
// it. EthCall additionally takes its own Snapshot/RevertToSnapshot around the
// call so the supplied statedb is left byte-identical on return, which lets the
// optimal-input search reuse a single Copy across many quotes. evm.Call bypasses
// nonce, balance, gas-price and intrinsic-gas checks (those are ApplyTransaction's
// job); we run with a generous gas cap on a copy, so a quote can never fail for
// want of gas the way a real tx would.
package simengine

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

// ethCallCaller is the synthetic origin used for read-only quoter calls. It is an
// arbitrary EOA-style address with no special privileges; since EthCall transfers
// zero value and the quoter performs no balance/allowance checks against the
// caller, any address works. We use a fixed, recognisable sentinel.
var ethCallCaller = common.HexToAddress("0x0000000000000000000000000000000000C0FFEE")

// ethCallGasCap is the default gas cap for read-only calls when the caller passes
// 0. QuoterV2 single hops cost ~92k-131k; multi-tick crossings can be higher. We
// run on a copy so the cap is free — a generous default avoids false "out of gas"
// negatives on deep tick walks.
const ethCallGasCap uint64 = 50_000_000

// EthCall executes input against contract `to` on `statedb` as a read-only
// message call and returns the raw ABI-encoded return data. It mirrors an
// eth_call: nonce/balance/gas-price/intrinsic-gas are NOT checked, zero value is
// transferred, and nothing is committed.
//
// statedb MUST be a copy the caller is willing to have transiently mutated;
// EthCall brackets the call in Snapshot/RevertToSnapshot so statedb is restored
// on return regardless of success or revert. chainCtx supplies the historical
// header lookups NewEVMBlockContext needs (GetHashFn / blob base fee). header is
// the block environment the call observes (number, time, base fee, coinbase).
//
// On execution failure (revert, out-of-gas, missing code path) err is non-nil and
// ret carries any returned revert payload; the quoter treats err!=nil as "no
// quote" and skips/penalises that edge.
func (e *SimEngine) EthCall(statedb *state.StateDB, chainCtx simChainContext, header *types.Header, to common.Address, input []byte, gas uint64) (ret []byte, err error) {
	if statedb == nil {
		return nil, fmt.Errorf("simengine: EthCall nil statedb")
	}
	if header == nil {
		return nil, fmt.Errorf("simengine: EthCall nil header")
	}
	if chainCtx == nil {
		return nil, fmt.Errorf("simengine: EthCall nil chainCtx")
	}
	if gas == 0 {
		gas = ethCallGasCap
	}

	// Build the block context and EVM exactly as the validated execution loop does
	// (applyOnState / ApplyOnStateHooked): pass header.Coinbase as the explicit
	// author so COINBASE resolves without a fully-initialised engine. Reuse the
	// engine's resolved chain config and the configured vm.Config (no tracer by
	// default for quoter reads).
	coinbase := header.Coinbase
	blockCtx := core.NewEVMBlockContext(header, chainCtx, &coinbase)
	evm := vm.NewEVM(blockCtx, statedb, e.chainCfg, e.vmConfig)

	// Bracket the call so the supplied state is left untouched on return; this lets
	// the optimal-input search reuse one Copy for many quotes (each quote is itself
	// atomic — the quoter reverts internally — so no cross-quote contamination).
	snap := statedb.Snapshot()
	defer statedb.RevertToSnapshot(snap)

	// value = 0 (common.U2560): no transfer, so the balance check at the top of
	// evm.Call is skipped and the synthetic caller needs no funds.
	ret, _, err = evm.Call(ethCallCaller, to, input, gas, common.U2560)
	return ret, err
}
