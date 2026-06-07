// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// sandwich.go is the GROUND-TRUTH sandwich valuator: it measures the real profit
// of a sandwich attack by executing the attacker's two legs through the REAL
// deployed PancakeSwap router code, with the REAL victim transaction sandwiched
// between them, all on a single in-process state.Copy. There is no analytic
// shortcut — the V2 pair / V3 tick math, the fees, and (crucially) the victim's
// own amountOutMin slippage revert are all enforced by the EVM itself. This is
// the paper's selling point: the dominant atomic MEV on BSC (~51% of volume) is
// valued by real execution, not a formula.
//
// THE 3-STEP RE-EXECUTION (sandwichProfit), on a fresh Copy of the pre-victim
// state, for one candidate frontrun size f:
//
//	0. fund the synthetic attacker on the copy (storage writes: balanceOf[atk],
//	   allowance[atk][router], and BNB for gas) — a flash-loan stand-in: the
//	   attacker needs no inventory, so the flash fee is charged in the net gate.
//	1. FRONTRUN: applyRouterSwap(atk -> router, swap f of X for Y). State-MUTATING
//	   (no Snapshot/Revert): the SSTOREs persist on the copy.
//	2. VICTIM: core.ApplyTransaction(the REAL victim tx) on the frontrun-mutated
//	   copy. If f is too large the victim breaches its amountOutMin and ApplyTx
//	   reverts it — automatically collapsing the (oversize) sandwich.
//	3. BACKRUN: applyRouterSwap(atk -> router, sell the Y it bought back to X).
//	4. PROFIT = attacker's X-balance delta across steps 1..4 (ground truth).
//
// The non-reverting call mechanism is the ONLY difference from EthCall: evm.Call
// keeps all storage mutations on SUCCESS (core/vm/evm.go) — it self-reverts only
// on EVM failure — so a state-mutating swap is EthCall MINUS its outer
// Snapshot/RevertToSnapshot bracket, with caller = the attacker EOA.
//
// Strictly read-only w.r.t. the chain: everything happens on a state.Copy that
// is discarded. Nothing is ever committed or submitted.
package simengine

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/strategy"
	"github.com/holiman/uint256"
)

// ---------------------------------------------------------------------------
// Verified router addresses and selectors (live BSC node, block 0x61ba538).
// ---------------------------------------------------------------------------

var (
	// pancakeV2Router is PancakeRouter02 — swapExactTokensForTokens. VERIFIED
	// (code present; factory()=0xca143ce3...; WETH()=WBNB).
	pancakeV2Router = common.HexToAddress("0x10ED43C718714eb63d5aA57B78B54704E256024E")

	// pancakeV3SwapRouter is the PancakeSwap V3 SwapRouter — exactInputSingle.
	// VERIFIED (factory()=0x0BFbCF9f...091865, WETH9()=WBNB). NOT the SmartRouter
	// (0x13f4EA83...), which is the universal multicall wrapper.
	pancakeV3SwapRouter = common.HexToAddress("0x1b81D678ffb9C0263b24A97847620C99d213eB14")
)

var (
	// v2SwapSelector = swapExactTokensForTokens(uint256,uint256,address[],address,uint256).
	v2SwapSelector = []byte{0x38, 0xed, 0x17, 0x39}

	// v3ExactInputSingleSelector = the Pancake variant WITH a deadline field in the
	// struct: exactInputSingle((address,address,uint24,address,uint256,uint256,
	// uint256,uint160)). The no-deadline Uniswap variant (0x04e45aaf) REVERTS on
	// this router.
	v3ExactInputSingleSelector = []byte{0x41, 0x4b, 0xf3, 0x89}
)

// sandwichAttacker is the synthetic attacker EOA. It has no special privileges;
// it is funded entirely via storage writes on the state.Copy (flash-loan
// stand-in). A recognisable sentinel distinct from ethCallCaller.
var sandwichAttacker = common.HexToAddress("0x000000000000000000000000000000000000BEEF")

// sandwichGasCap is the per-leg gas cap for the attacker's router calls on the
// copy. Generous (the copy makes it free); a router swap costs ~120-160k.
const sandwichGasCap uint64 = 8_000_000

// attackerGasBudgetWei seeds the attacker's BNB balance so gas/value checks never
// fail on the copy. The router swaps transfer zero native value (token->token),
// but evm.Call still needs the account to exist with a positive balance for some
// code paths; 1000 BNB is ample and is never spent (the copy is discarded).
var attackerGasBudgetWei = new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))

// maxUint256Hash is the MAX allowance value written to the token's allowance slot.
var maxUint256Hash = common.HexToHash("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")

// ---------------------------------------------------------------------------
// Token storage-slot table (verified on node).
// ---------------------------------------------------------------------------

// tokenSlots describes a BEP20/WETH9 token's storage layout for the two mappings
// we must write to fund the attacker: balanceOf[holder] and
// allowance[owner][spender].
type tokenSlots struct {
	balSlot   int64
	allowSlot int64
}

// knownTokenSlots is the VERIFIED balanceOf/allowance base-slot table. WBNB uses
// the classic WETH9 layout (3/4); the BEP20 stables use 1/2. A token absent here
// cannot be funded, so its sandwiches are SKIPPED (sandwichProfit returns ok=false
// with notFunded). Probed on-node by matching keccak256(leftPad32(holder) ++
// u256(slot)) against on-chain balanceOf and the double-hash allowance layout.
var knownTokenSlots = map[common.Address]tokenSlots{
	strategy.WBNB: {balSlot: 3, allowSlot: 4}, // 0xbb4c...095c, classic WETH9
	strategy.USDT: {balSlot: 1, allowSlot: 2}, // 0x55d3...7955
	strategy.USDC: {balSlot: 1, allowSlot: 2}, // 0x8ac7...580d
}

// TokenFundable reports whether a token's storage slots are known (so the
// attacker can be funded for a sandwich on a pool using it). Exposed for the
// detector's pool-eligibility filter.
func TokenFundable(token common.Address) bool {
	_, ok := knownTokenSlots[token]
	return ok
}

// ---------------------------------------------------------------------------
// Attacker funding (storage writes on a state.Copy).
// ---------------------------------------------------------------------------

// balanceOfKey returns the storage key of balanceOf[holder] for a token with the
// given base slot: keccak256( leftPad32(holder) ++ u256(balSlot) ).
func balanceOfKey(holder common.Address, balSlot int64) common.Hash {
	return crypto.Keccak256Hash(
		leftPad32(holder.Bytes()),
		common.BigToHash(big.NewInt(balSlot)).Bytes(),
	)
}

// allowanceKey returns the storage key of allowance[owner][spender] for a token
// with the given base slot (nested mapping): keccak256( leftPad32(spender) ++
// keccak256( leftPad32(owner) ++ u256(allowSlot) ) ).
func allowanceKey(owner, spender common.Address, allowSlot int64) common.Hash {
	inner := crypto.Keccak256Hash(
		leftPad32(owner.Bytes()),
		common.BigToHash(big.NewInt(allowSlot)).Bytes(),
	)
	return crypto.Keccak256Hash(leftPad32(spender.Bytes()), inner.Bytes())
}

// leftPad32 left-pads b to a 32-byte big-endian word (mirrors strategy.leftPad32,
// kept local so simengine has no cross-package coupling for this trivial helper).
func leftPad32(b []byte) []byte {
	if len(b) >= 32 {
		return b[len(b)-32:]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// fundAttacker funds the synthetic attacker on `statedb` (a Copy) to execute a
// swap that SPENDS `token` via `router`: it sets balanceOf[attacker] = amount,
// allowance[attacker][router] = MAX, and gives the attacker a BNB budget for gas.
// It returns ok=false (notFunded) for a token whose slots are unknown — the
// caller then skips that pool's sandwich. Idempotent; safe to call per leg.
func fundAttacker(statedb *state.StateDB, token, router common.Address, amount *big.Int) (ok bool) {
	slots, known := knownTokenSlots[token]
	if !known {
		return false
	}
	// balanceOf[attacker] = amount.
	statedb.SetState(token, balanceOfKey(sandwichAttacker, slots.balSlot), common.BigToHash(amount))
	// allowance[attacker][router] = MAX.
	statedb.SetState(token, allowanceKey(sandwichAttacker, router, slots.allowSlot), maxUint256Hash)
	// BNB for gas (never spent on the discarded copy).
	statedb.SetBalance(sandwichAttacker, uint256.MustFromBig(attackerGasBudgetWei), tracing.BalanceChangeUnspecified)
	return true
}

// ---------------------------------------------------------------------------
// State-MUTATING router swap (the non-reverting call mechanism).
// ---------------------------------------------------------------------------

// applyRouterSwap executes `calldata` against `router` as the attacker EOA,
// KEEPING the state mutation on success. It is EthCall MINUS the outer
// Snapshot/RevertToSnapshot bracket: evm.Call persists all SSTOREs on success and
// only self-reverts on EVM failure, so a non-reverting variant is exactly the
// same construction without the discard bracket and with caller = the attacker.
//
// statedb MUST be the Copy the caller wants mutated (the frontrun/backrun must
// persist for the next step). Returns the raw return data and an error on revert
// / out-of-gas; the caller treats err!=nil as "this frontrun size is infeasible".
func (e *SimEngine) applyRouterSwap(statedb *state.StateDB, chainCtx simChainContext, header *types.Header, router common.Address, calldata []byte, gas uint64) (ret []byte, err error) {
	if statedb == nil || header == nil || chainCtx == nil {
		return nil, fmt.Errorf("simengine: applyRouterSwap nil arg")
	}
	if gas == 0 {
		gas = sandwichGasCap
	}
	coinbase := header.Coinbase
	blockCtx := core.NewEVMBlockContext(header, chainCtx, &coinbase)
	evm := vm.NewEVM(blockCtx, statedb, e.chainCfg, e.vmConfig)

	// Bracket this leg as a proper pseudo-transaction, exactly as core.ApplyTransaction
	// does, so the SSTORE gas/refund machinery is well-formed.
	//
	// WHY THIS IS REQUIRED (the V3 "Refund counter below zero" fix): a raw evm.Call
	// neither resets nor consumes the per-transaction refund counter, and neither
	// resets the EIP-2929 access list (warm/cold) nor promotes dirty storage to the
	// committed baseline. The SSTORE refund logic (core/vm/operations_acl.go,
	// makeGasSStoreFunc) reads `original = GetStateAndCommittedState(slot)` and the
	// slot's access-list warmth to decide whether to AddRefund(4800) or
	// SubRefund(4800). Without a per-leg Prepare() those inputs are stale across
	// legs (and across a state.Copy() that inherits a non-zero refund), so a V3 swap
	// — which churns many slots through non-zero -> 0 -> non-zero within one call
	// (slot0, tick bitmaps, fee growth, protocol fees) — can hit the "recreate slot"
	// branch (SubRefund(clearingRefund)) when the matching AddRefund was never
	// recorded, underflowing the counter and panicking in StateDB.SubRefund
	// (core/state/statedb.go:331). V2 router swaps touch far fewer slots and happen
	// not to trip it, which is why only the V3 self-test panicked.
	//
	// Prepare(): clears the access list (fresh warm/cold), resets transient storage,
	// and (via clearJournalAndRefund semantics in the next Finalise) re-bases the
	// refund. Finalise(true): promotes this leg's dirty storage to the pending
	// (committed) baseline AND resets the refund counter to 0 — so the NEXT leg sees
	// the correct `original` and starts from refund 0, identical to a real tx
	// boundary. The mutations themselves persist (Finalise promotes, it does not
	// revert), so the frontrun's price move is still in place for the victim/backrun.
	rules := e.chainCfg.Rules(header.Number, blockCtx.Random != nil, header.Time)
	statedb.Prepare(rules, sandwichAttacker, coinbase, &router, vm.ActivePrecompiles(rules), nil)

	// NO outer Snapshot/RevertToSnapshot here (unlike EthCall): SSTOREs persist on
	// success. value = 0 (token->token swap, no native transfer).
	ret, _, err = evm.Call(sandwichAttacker, router, calldata, gas, common.U2560)

	// Promote this leg's writes to the committed baseline and reset the per-tx refund
	// counter (mirrors ApplyTransaction's trailing Finalise). On an EVM error
	// evm.Call already self-reverted its snapshot, so there is nothing dirty to
	// promote; Finalise is a safe no-op then.
	statedb.Finalise(true)
	return ret, err
}

// describeRevert renders a human-readable reason for a failed evm.Call from its
// (ret, err) pair: it decodes the standard Error(string) revert payload when
// present, otherwise reports the raw return bytes and the error. Diagnostics only.
func describeRevert(ret []byte, err error) string {
	if err == nil {
		return "ok"
	}
	if len(ret) > 0 {
		if reason, uerr := abi.UnpackRevert(ret); uerr == nil && reason != "" {
			return fmt.Sprintf("%v: %q", err, reason)
		}
		// Non-string revert (custom error / Panic(uint256)) — show the selector.
		n := len(ret)
		if n > 36 {
			n = 36
		}
		return fmt.Sprintf("%v: ret=0x%x", err, ret[:n])
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// Calldata encoders for the verified router functions.
// ---------------------------------------------------------------------------

// farFutureDeadline is a deadline far past any block time so swaps never expire.
var farFutureDeadline = new(big.Int).SetUint64(1 << 62)

// encodeV2Swap builds swapExactTokensForTokens(amountIn, amountOutMin=0,
// path=[tokenIn,tokenOut], to=attacker, deadline) calldata. path is a dynamic
// array, so it is referenced by an offset (0xA0) after the 5 head words.
func encodeV2Swap(tokenIn, tokenOut common.Address, amountIn *big.Int) []byte {
	data := make([]byte, 0, 4+32*9)
	data = append(data, v2SwapSelector...)
	data = append(data, leftPad32(amountIn.Bytes())...)          // amountIn
	data = append(data, make([]byte, 32)...)                     // amountOutMin = 0
	data = append(data, leftPad32(big.NewInt(0xA0).Bytes())...)  // path offset (5 head words)
	data = append(data, leftPad32(sandwichAttacker.Bytes())...)  // to = attacker
	data = append(data, leftPad32(farFutureDeadline.Bytes())...) // deadline
	data = append(data, leftPad32(big.NewInt(2).Bytes())...)     // path.len = 2
	data = append(data, leftPad32(tokenIn.Bytes())...)           // path[0]
	data = append(data, leftPad32(tokenOut.Bytes())...)          // path[1]
	return data
}

// encodeV3ExactInputSingle builds the Pancake exactInputSingle calldata WITH the
// deadline field (selector 0x414bf389). The single struct argument is all static
// fields, encoded inline after the selector. amountOutMinimum=0, sqrtPriceLimitX96=0.
func encodeV3ExactInputSingle(tokenIn, tokenOut common.Address, feeTier uint32, amountIn *big.Int) []byte {
	data := make([]byte, 0, 4+32*8)
	data = append(data, v3ExactInputSingleSelector...)
	data = append(data, leftPad32(tokenIn.Bytes())...)                                  // tokenIn
	data = append(data, leftPad32(tokenOut.Bytes())...)                                 // tokenOut
	data = append(data, leftPad32(new(big.Int).SetUint64(uint64(feeTier)).Bytes())...)  // fee (uint24)
	data = append(data, leftPad32(sandwichAttacker.Bytes())...)                         // recipient
	data = append(data, leftPad32(farFutureDeadline.Bytes())...)                        // deadline
	data = append(data, leftPad32(amountIn.Bytes())...)                                 // amountIn
	data = append(data, make([]byte, 32)...)                                            // amountOutMinimum = 0
	data = append(data, make([]byte, 32)...)                                            // sqrtPriceLimitX96 = 0
	return data
}

// ---------------------------------------------------------------------------
// Attacker balance reading (ground-truth profit measurement).
// ---------------------------------------------------------------------------

// attackerTokenBalance reads balanceOf[attacker] for `token` directly from the
// state's storage at the known slot. Returns (balance, true) or (0,false) for an
// unknown token.
func attackerTokenBalance(statedb *state.StateDB, token common.Address) (*big.Int, bool) {
	slots, known := knownTokenSlots[token]
	if !known {
		return big.NewInt(0), false
	}
	word := statedb.GetState(token, balanceOfKey(sandwichAttacker, slots.balSlot))
	return new(big.Int).SetBytes(word[:]), true
}

// ---------------------------------------------------------------------------
// The ground-truth 3-step sandwich profit.
// ---------------------------------------------------------------------------

// sandwichProfit measures the ground-truth GROSS sandwich profit (in start-token
// X = the token the victim spent) of frontrunning `victimTx` on `pool` with a
// frontrun of `frontrunIn`, by re-executing FRONTRUN -> VICTIM -> BACKRUN on a
// FRESH Copy of the supplied pre-victim state. It returns the attacker's X-balance
// delta over the three steps, the assumed gas units for the two attacker txs, and
// ok.
//
// preState MUST be the exact pre-victim state (all block txs up to but excluding
// the victim already applied). It is Copy()'d internally, so the caller's state
// is never mutated and successive probes are independent. ok=false means the size
// is infeasible (token not fundable, a router leg reverted, or the victim tx
// reverted because the frontrun breached its amountOutMin) — the search treats
// that as the boundary of the feasible bracket.
func (e *SimEngine) sandwichProfit(preState *state.StateDB, chainCtx simChainContext, header *types.Header, victimTx *types.Transaction, victim strategy.SandwichVictim, frontrunIn *big.Int) (gross *big.Int, gasUnits uint64, ok bool) {
	zero := big.NewInt(0)
	if preState == nil || header == nil || victimTx == nil || frontrunIn == nil || frontrunIn.Sign() <= 0 {
		return zero, 0, false
	}
	tokenIn := victim.TokenIn // X
	tokenOut, hasOther := victim.Pool.Other(tokenIn)
	if !hasOther {
		return zero, 0, false
	}
	// Both tokens must be fundable: X for the frontrun spend + gas-budget approval,
	// and Y so the backrun's allowance[attacker][router] for Y is set (the Y
	// balance itself comes from the frontrun swap, but the router needs approval).
	if !TokenFundable(tokenIn) || !TokenFundable(tokenOut) {
		return zero, 0, false
	}

	isV3 := victim.Pool.IsV3
	router := pancakeV2Router
	if isV3 {
		router = pancakeV3SwapRouter
	}
	gasUnits = strategy.SandwichGasUnits(isV3)

	// Fresh independent copy per probe.
	sdb := preState.Copy()

	// 0. FUND the attacker: X for the frontrun, plus a MAX allowance on Y so the
	// backrun leg can spend the Y it receives. Fund Y with zero balance (its real
	// balance arrives from the frontrun output) but set its allowance.
	if !fundAttacker(sdb, tokenIn, router, frontrunIn) {
		return zero, 0, false
	}
	if !fundAttacker(sdb, tokenOut, router, big.NewInt(0)) {
		return zero, 0, false
	}

	// Pre-balance of X (start token) AFTER funding: the funded frontrunIn is the
	// attacker's X inventory at the start of step 1. Profit is measured relative to
	// this so the borrowed notional is netted out (the attacker repays the flash
	// loan from the recovered X; gross = recovered - borrowed).
	preX, _ := attackerTokenBalance(sdb, tokenIn)

	// 1. FRONTRUN (state-mutating).
	var frontCalldata []byte
	if isV3 {
		frontCalldata = encodeV3ExactInputSingle(tokenIn, tokenOut, victim.Pool.FeeTier, frontrunIn)
	} else {
		frontCalldata = encodeV2Swap(tokenIn, tokenOut, frontrunIn)
	}
	if _, err := e.applyRouterSwap(sdb, chainCtx, header, router, frontCalldata, sandwichGasCap); err != nil {
		return zero, 0, false
	}

	// The Y the frontrun bought (used as the exact backrun input).
	yBought, _ := attackerTokenBalance(sdb, tokenOut)
	if yBought.Sign() <= 0 {
		return zero, 0, false
	}

	// 2. VICTIM: the REAL tx, applied on the frontrun-mutated copy. Reuse the
	// validated ApplyTransaction path. If the frontrun was too large the victim
	// breaches its amountOutMin and ApplyTransaction reverts it -> infeasible.
	if !e.applyVictimTx(sdb, chainCtx, header, victimTx) {
		return zero, 0, false
	}

	// 3. BACKRUN: sell exactly yBought of Y back to X (state-mutating).
	var backCalldata []byte
	if isV3 {
		backCalldata = encodeV3ExactInputSingle(tokenOut, tokenIn, victim.Pool.FeeTier, yBought)
	} else {
		backCalldata = encodeV2Swap(tokenOut, tokenIn, yBought)
	}
	if _, err := e.applyRouterSwap(sdb, chainCtx, header, router, backCalldata, sandwichGasCap); err != nil {
		return zero, 0, false
	}

	// 4. PROFIT: attacker X-balance delta. (postX - preX) = recovered X - frontrun
	// spent = gross sandwich profit in X.
	postX, _ := attackerTokenBalance(sdb, tokenIn)
	gross = new(big.Int).Sub(postX, preX)
	return gross, gasUnits, true
}

// applyVictimTx applies the REAL victim transaction to `sdb` using the same
// core.ApplyTransaction path the validated loop uses. It returns true iff the
// victim tx executed successfully (a reverted victim — e.g. because an oversize
// frontrun breached its amountOutMin — returns false, which correctly collapses
// the sandwich). It builds a fresh gas pool from the header limit (we only apply
// one tx here, on the copy).
//
// CRITICAL: this MUST NOT bracket ApplyTransaction in Snapshot/RevertToSnapshot.
// core.ApplyTransaction calls statedb.Finalise(true) internally, which clears the
// journal AND validRevisions (reverting across a finalised tx is forbidden). A
// caller-level RevertToSnapshot(idTakenBeforeApplyTransaction) would then panic
// with "revision id N cannot be reverted" (core/state/journal.go). Isolation is
// instead provided by the FRESH preState.Copy() that sandwichProfit takes per
// probe: on failure sandwichProfit returns ok=false and DISCARDS the whole copy,
// so there is nothing to revert; on success the victim's mutations must persist
// for the backrun, which they do. The only snapshots in play are evm.Call's own
// internal self-revert-on-failure and ApplyTransaction's internal handling — no
// caller-level snapshot spans the ApplyTransaction.
func (e *SimEngine) applyVictimTx(sdb *state.StateDB, chainCtx simChainContext, header *types.Header, victimTx *types.Transaction) bool {
	ok, _ := e.applyVictimTxDiag(sdb, chainCtx, header, victimTx)
	return ok
}

// applyVictimTxDiag is applyVictimTx with a diagnostic reason string describing
// why the victim leg failed (ApplyTransaction error, or an on-chain revert with
// receipt status "failed"). reason is "ok" on success. Used by the per-victim
// probe logger; applyVictimTx wraps it for the hot path.
func (e *SimEngine) applyVictimTxDiag(sdb *state.StateDB, chainCtx simChainContext, header *types.Header, victimTx *types.Transaction) (bool, string) {
	coinbase := header.Coinbase
	blockCtx := core.NewEVMBlockContext(header, chainCtx, &coinbase)
	evm := vm.NewEVM(blockCtx, sdb, e.chainCfg, e.vmConfig)

	gp := new(core.GasPool).AddGas(header.GasLimit)
	var usedGas uint64

	sdb.SetTxContext(victimTx.Hash(), 0)
	receipt, err := core.ApplyTransaction(evm, gp, sdb, header, victimTx, &usedGas)
	if err != nil {
		// ApplyTransaction failed (e.g. invalid tx / gas). The copy is discarded by
		// the caller on a false return, so no revert is needed (and a revert here
		// would panic: Finalise already cleared the journal).
		return false, "victim ApplyTransaction err: " + err.Error()
	}
	// A victim whose status is "failed" (reverted on-chain, e.g. amountOutMin) is
	// NOT a usable sandwich: the price move the attacker bet on did not happen.
	if receipt == nil {
		return false, "victim nil receipt"
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return false, "victim reverted on-chain (status=failed; e.g. amountOutMin breached by frontrun)"
	}
	return true, "ok"
}

// ---------------------------------------------------------------------------
// Optimal frontrun (search wired to the ground-truth probe).
// ---------------------------------------------------------------------------

// optimalFrontrun finds the gross-profit-maximising frontrun for a victim swap by
// golden-section search over sandwichProfit, seeded (for V2) by the closed-form
// candidates and bounded above by a multiple of the victim size. Each probe is a
// full 3-step EVM re-execution on a fresh copy (ground truth), so the result is
// exact regardless of the analytic subtleties. It returns the optimal frontrun,
// the ground-truth gross at that size, and the gas units for the two attacker txs.
func (e *SimEngine) optimalFrontrun(preState *state.StateDB, chainCtx simChainContext, header *types.Header, victimTx *types.Transaction, victim strategy.SandwichVictim) (frontrun, gross *big.Int, gasUnits uint64) {
	zero := big.NewInt(0)
	if victim.AmountIn == nil || victim.AmountIn.Sign() <= 0 {
		return zero, zero, 0
	}
	isV3 := victim.Pool.IsV3
	gasUnits = strategy.SandwichGasUnits(isV3)

	// probe: ground-truth gross at a frontrun size (fresh copy each call).
	probe := func(f *big.Int) (*big.Int, bool) {
		gr, _, ok := e.sandwichProfit(preState, chainCtx, header, victimTx, victim, f)
		return gr, ok
	}

	// Seeds (V2 closed form). For V3 these are still reasonable starting points
	// (half-the-victim and the combined-trade estimate) and are merely probed; the
	// search owns the answer either way.
	var seeds []*big.Int
	seeds = append(seeds, strategy.HalfVictimSeed(victim.AmountIn))
	if !isV3 {
		// V2: derive reserves of X for the combined-trade seed.
		if reserveIn := poolReserveOfX(preState, victim); reserveIn != nil && reserveIn.Sign() > 0 {
			seeds = append(seeds, strategy.V2CombinedSeed(reserveIn, victim.AmountIn, victim.Pool.Gamma))
		}
	}

	// Upper bound: cap the frontrun at a generous multiple of the victim so the
	// bracket starts inside a sane region; OptimalFrontrun shrinks it to the
	// largest feasible (non-reverting) size automatically.
	hi := new(big.Int).Mul(victim.AmountIn, big.NewInt(8))

	frontrun, gross = strategy.OptimalFrontrun(probe, seeds, hi)
	if gross.Sign() <= 0 {
		return zero, zero, gasUnits
	}
	return frontrun, gross, gasUnits
}

// poolReserveOfX reads the V2 pool's reserve of the victim's input token X from
// the pre-victim state (slot-8 reserves), used only to compute the combined-trade
// frontrun seed. Returns nil for a V3 pool or when reserves are unavailable.
func poolReserveOfX(preState *state.StateDB, victim strategy.SandwichVictim) *big.Int {
	if victim.Pool.IsV3 {
		return nil
	}
	rv := strategy.ReadReserves(preState, victim.Pool.Pair)
	if rv.Reserve0 == nil || rv.Reserve1 == nil {
		return nil
	}
	switch victim.TokenIn {
	case victim.Pool.Token0:
		return rv.Reserve0
	case victim.Pool.Token1:
		return rv.Reserve1
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Diagnostics (sim13): per-probe step trace + a stand-alone funding+swap check.
// ---------------------------------------------------------------------------

// sandwichProbeDiag carries the per-step outcome of ONE 3-step sandwich probe at a
// fixed frontrun size, so the caller can log WHICH leg fails and why. It mirrors
// sandwichProfit exactly (same fund -> frontrun -> victim -> backrun on a fresh
// Copy) but never returns early without recording a reason. Diagnostics only.
type sandwichProbeDiag struct {
	frontrunIn *big.Int
	fundOk     bool
	frontrunOk bool
	yBought    *big.Int
	victimOk   bool
	backrunOk  bool
	gross      *big.Int
	reason     string // first failing step + revert message, or "ok"
}

// probeSandwichDiag runs a single instrumented 3-step probe at frontrunIn and
// returns the step-by-step outcome. It is a read-only clone of sandwichProfit's
// body (fresh preState.Copy(), discarded) used only to produce a human-readable
// failure trace for the first few above-threshold victims. It captures the EVM
// revert reason from each router leg via describeRevert.
func (e *SimEngine) probeSandwichDiag(preState *state.StateDB, chainCtx simChainContext, header *types.Header, victimTx *types.Transaction, victim strategy.SandwichVictim, frontrunIn *big.Int) sandwichProbeDiag {
	d := sandwichProbeDiag{frontrunIn: frontrunIn, gross: big.NewInt(0)}
	if preState == nil || header == nil || victimTx == nil || frontrunIn == nil || frontrunIn.Sign() <= 0 {
		d.reason = "bad args"
		return d
	}
	tokenIn := victim.TokenIn
	tokenOut, hasOther := victim.Pool.Other(tokenIn)
	if !hasOther {
		d.reason = "pool has no other token"
		return d
	}
	if !TokenFundable(tokenIn) || !TokenFundable(tokenOut) {
		d.reason = "token(s) not fundable (unknown slot)"
		return d
	}

	isV3 := victim.Pool.IsV3
	router := pancakeV2Router
	if isV3 {
		router = pancakeV3SwapRouter
	}

	sdb := preState.Copy()

	if !fundAttacker(sdb, tokenIn, router, frontrunIn) || !fundAttacker(sdb, tokenOut, router, big.NewInt(0)) {
		d.reason = "fundAttacker failed"
		return d
	}
	d.fundOk = true
	preX, _ := attackerTokenBalance(sdb, tokenIn)

	// 1. FRONTRUN.
	var frontCalldata []byte
	if isV3 {
		frontCalldata = encodeV3ExactInputSingle(tokenIn, tokenOut, victim.Pool.FeeTier, frontrunIn)
	} else {
		frontCalldata = encodeV2Swap(tokenIn, tokenOut, frontrunIn)
	}
	ret, err := e.applyRouterSwap(sdb, chainCtx, header, router, frontCalldata, sandwichGasCap)
	if err != nil {
		d.reason = "frontrun revert: " + describeRevert(ret, err)
		return d
	}
	d.frontrunOk = true

	yBought, _ := attackerTokenBalance(sdb, tokenOut)
	d.yBought = yBought
	if yBought.Sign() <= 0 {
		d.reason = "frontrun succeeded but bought 0 Y (wrong direction/token or balanceOf slot)"
		return d
	}

	// 2. VICTIM.
	vok, vreason := e.applyVictimTxDiag(sdb, chainCtx, header, victimTx)
	if !vok {
		d.reason = vreason
		return d
	}
	d.victimOk = true

	// 3. BACKRUN.
	var backCalldata []byte
	if isV3 {
		backCalldata = encodeV3ExactInputSingle(tokenOut, tokenIn, victim.Pool.FeeTier, yBought)
	} else {
		backCalldata = encodeV2Swap(tokenOut, tokenIn, yBought)
	}
	bret, berr := e.applyRouterSwap(sdb, chainCtx, header, router, backCalldata, sandwichGasCap)
	if berr != nil {
		d.reason = "backrun revert: " + describeRevert(bret, berr)
		return d
	}
	d.backrunOk = true

	postX, _ := attackerTokenBalance(sdb, tokenIn)
	d.gross = new(big.Int).Sub(postX, preX)
	d.reason = "ok"
	return d
}

// sandwichSelftestResult is one router's funding+swap self-test outcome.
type sandwichSelftestResult struct {
	label  string
	out    *big.Int // tokenOut wei received by the attacker (0 on failure)
	reason string   // "ok" or the revert/decode reason
}

// selftestRouterSwap funds a synthetic attacker with `amountIn` of `tokenIn`
// (+ allowance to `router` + BNB gas) on a FRESH copy of preState, then executes
// a single router swap tokenIn->tokenOut and reads the attacker's tokenOut
// balance. It isolates "does storage-funding + router calldata work in-process"
// from the sizing/sequence logic — this should match the on-node eth_call proof
// (1 WBNB -> ~585 USDT). Read-only: the copy is discarded.
func (e *SimEngine) selftestRouterSwap(preState *state.StateDB, chainCtx simChainContext, header *types.Header, label string, router, tokenIn, tokenOut common.Address, isV3 bool, feeTier uint32, amountIn *big.Int) sandwichSelftestResult {
	res := sandwichSelftestResult{label: label, out: big.NewInt(0)}
	if preState == nil || header == nil {
		res.reason = "nil preState/header"
		return res
	}
	if !TokenFundable(tokenIn) || !TokenFundable(tokenOut) {
		res.reason = "token(s) not fundable (unknown slot)"
		return res
	}
	sdb := preState.Copy()
	if !fundAttacker(sdb, tokenIn, router, amountIn) || !fundAttacker(sdb, tokenOut, router, big.NewInt(0)) {
		res.reason = "fundAttacker failed"
		return res
	}
	var calldata []byte
	if isV3 {
		calldata = encodeV3ExactInputSingle(tokenIn, tokenOut, feeTier, amountIn)
	} else {
		calldata = encodeV2Swap(tokenIn, tokenOut, amountIn)
	}
	ret, err := e.applyRouterSwap(sdb, chainCtx, header, router, calldata, sandwichGasCap)
	if err != nil {
		res.reason = describeRevert(ret, err)
		return res
	}
	out, _ := attackerTokenBalance(sdb, tokenOut)
	res.out = out
	if out.Sign() <= 0 {
		res.reason = "swap returned ok but attacker tokenOut balance is 0 (slot/direction bug)"
		return res
	}
	res.reason = "ok"
	return res
}
