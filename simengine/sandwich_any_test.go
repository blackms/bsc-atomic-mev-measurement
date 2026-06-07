// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// sandwich_any_test.go unit-proves the ANY-POOL sandwich building blocks that do
// not need a fully-deployed PancakeSwap router: the dynamic balanceOf-slot prober
// (against a minimal hand-built ERC20 contract whose balanceOf reads a plain
// mapping at a chosen slot), the per-token slot cache (seeded with the verified
// known slots), and the K-safe amountOut under-quote. The full 3-step ground-truth
// re-execution is exercised at runtime under SIMENGINE_DRYRUN=sandwich-any.
package simengine

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/strategy"
	"github.com/holiman/uint256"
)

// ---------------------------------------------------------------------------
// Test harness: a SimEngine over an in-memory state, a stub chain context, and a
// minimal "ERC20" contract whose balanceOf(addr) returns sload(keccak256(addr ++
// slot)) for a compile-time slot constant.
// ---------------------------------------------------------------------------

// stubChainCtx satisfies simChainContext for a single EthCall that uses no
// BLOCKHASH opcode and no blob base fee (header.ExcessBlobGas == nil).
type stubChainCtx struct{}

func (stubChainCtx) Config() *params.ChainConfig                      { return params.TestChainConfig }
func (stubChainCtx) Engine() consensus.Engine                         { return nil } // unused (explicit author)
func (stubChainCtx) GetHeader(common.Hash, uint64) *types.Header      { return nil }
func (stubChainCtx) GetHeaderByNumber(uint64) *types.Header           { return nil }
func (stubChainCtx) GetHeaderByHash(common.Hash) *types.Header        { return nil }
func (stubChainCtx) CurrentHeader() *types.Header                     { return nil }
func (stubChainCtx) GenesisHeader() *types.Header                     { return nil }
func (stubChainCtx) GetTd(common.Hash, uint64) *big.Int               { return nil }
func (stubChainCtx) GetHighestVerifiedHeader() *types.Header          { return nil }
func (stubChainCtx) GetVerifiedBlockByHash(common.Hash) *types.Header { return nil }

// minimalERC20Runtime returns EVM runtime bytecode for a contract whose ANY call
// returns sload(keccak256( calldata[4:36] ++ slot )) — i.e. balanceOf(address)
// for a plain mapping at storage slot `slot`. This is the faithful "mock" of a
// standard BEP20/OpenZeppelin balanceOf the prober must recognise.
//
// Layout: mem[0x00] = calldataload(4) (the 32-byte left-padded address arg),
// mem[0x20] = slot; hash = keccak256(mem[0x00:0x40]); RETURN sload(hash).
func minimalERC20Runtime(slot byte) []byte {
	return []byte{
		0x60, 0x04, // PUSH1 0x04
		0x35,       // CALLDATALOAD     -> arg
		0x60, 0x00, // PUSH1 0x00
		0x52,       // MSTORE           -> mem[0x00] = arg
		0x60, slot, // PUSH1 slot
		0x60, 0x20, // PUSH1 0x20
		0x52,       // MSTORE           -> mem[0x20] = slot
		0x60, 0x40, // PUSH1 0x40 (len)
		0x60, 0x00, // PUSH1 0x00 (off)
		0x20,       // SHA3             -> keccak256(mem[0x00:0x40])
		0x54,       // SLOAD            -> value
		0x60, 0x00, // PUSH1 0x00
		0x52,       // MSTORE           -> mem[0x00] = value
		0x60, 0x20, // PUSH1 0x20 (len)
		0x60, 0x00, // PUSH1 0x00 (off)
		0xf3, // RETURN
	}
}

// setTestCode deploys runtime code at addr (v1.7.3 SetCode takes a code-change
// reason as its third argument).
func setTestCode(sdb *state.StateDB, addr common.Address, code []byte) {
	sdb.SetCode(addr, code, tracing.CodeChangeUnspecified)
}

// newTestEngineState builds a SimEngine + fresh in-memory StateDB + a child header
// suitable for read-only EthCall probing.
func newTestEngineState(t *testing.T) (*SimEngine, *state.StateDB, *types.Header) {
	t.Helper()
	sdb, err := state.New(common.Hash{}, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	e := &SimEngine{chainCfg: params.TestChainConfig}
	hdr := &types.Header{
		Number:     big.NewInt(1),
		Time:       1,
		GasLimit:   30_000_000,
		Difficulty: big.NewInt(1),
		Coinbase:   common.HexToAddress("0x000000000000000000000000000000000000c0de"),
	}
	return e, sdb, hdr
}

// TestProbeTokenSlotsDiscoversArbitrarySlot deploys the minimal ERC20 at several
// distinct slots and asserts the prober recovers each one — the core of dynamic
// funding for ARBITRARY long-tail tokens.
func TestProbeTokenSlotsDiscoversArbitrarySlot(t *testing.T) {
	e, sdb, hdr := newTestEngineState(t)
	cc := stubChainCtx{}

	for _, slot := range []byte{0, 1, 2, 5} {
		token := common.BigToAddress(big.NewInt(int64(0x1000) + int64(slot)))
		setTestCode(sdb, token, minimalERC20Runtime(slot))

		got, ok := e.probeTokenSlots(sdb, cc, hdr, token)
		if !ok {
			t.Fatalf("slot %d: probe reported not fundable", slot)
		}
		if got.balSlot != int64(slot) {
			t.Fatalf("slot %d: probed balSlot = %d, want %d", slot, got.balSlot, slot)
		}
	}
}

// TestProbeTokenSlotsUnfundable confirms a token with NO recognisable balanceOf
// mapping (here: an account with no code, so every probe SLOAD returns 0 and the
// sentinel never round-trips) is reported not fundable — the skip path for
// proxy/packed/Vyper/fee-on-transfer tokens.
func TestProbeTokenSlotsUnfundable(t *testing.T) {
	e, sdb, hdr := newTestEngineState(t)
	cc := stubChainCtx{}

	token := common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	// No code: EthCall returns empty, never the sentinel.
	if _, ok := e.probeTokenSlots(sdb, cc, hdr, token); ok {
		t.Fatalf("expected unfundable for a code-less token")
	}
}

// TestResolveTokenSlotsCacheKnownSeed confirms the known WBNB/USDT/USDC slots are
// served from the cache WITHOUT a probe (no code deployed for them here, so a
// probe would fail — a cache hit is the only way this can succeed).
func TestResolveTokenSlotsCacheKnownSeed(t *testing.T) {
	e, sdb, hdr := newTestEngineState(t)
	cc := stubChainCtx{}

	for tok, want := range knownTokenSlots {
		got, ok := e.resolveTokenSlots(sdb, cc, hdr, tok)
		if !ok {
			t.Fatalf("known token %s reported not fundable", tok.Hex())
		}
		if got.balSlot != want.balSlot || got.allowSlot != want.allowSlot {
			t.Fatalf("known token %s slots = %+v, want %+v", tok.Hex(), got, want)
		}
	}
}

// TestResolveTokenSlotsCachesProbe confirms the prober result is cached: after the
// first resolve, REMOVING the contract code still returns the cached slot (a second
// probe would now fail).
func TestResolveTokenSlotsCachesProbe(t *testing.T) {
	e, sdb, hdr := newTestEngineState(t)
	cc := stubChainCtx{}

	token := common.HexToAddress("0xabc0000000000000000000000000000000000abc")
	setTestCode(sdb, token, minimalERC20Runtime(7))

	first, ok := e.resolveTokenSlots(sdb, cc, hdr, token)
	if !ok || first.balSlot != 7 {
		t.Fatalf("first resolve: ok=%v balSlot=%d, want ok=true balSlot=7", ok, first.balSlot)
	}
	// Wipe the code: a fresh probe would now fail, but the cache should still hit.
	setTestCode(sdb, token, nil)
	second, ok2 := e.resolveTokenSlots(sdb, cc, hdr, token)
	if !ok2 || second.balSlot != 7 {
		t.Fatalf("cached resolve: ok=%v balSlot=%d, want cache hit balSlot=7", ok2, second.balSlot)
	}
}

// TestFundAttackerDynWritesBalance confirms dynamic funding writes the attacker's
// balanceOf at the discovered slot so the minimal ERC20's balanceOf returns it.
func TestFundAttackerDynWritesBalance(t *testing.T) {
	e, sdb, hdr := newTestEngineState(t)
	cc := stubChainCtx{}

	token := common.HexToAddress("0xfee0000000000000000000000000000000000fee")
	setTestCode(sdb, token, minimalERC20Runtime(3))
	// Give the attacker existence (the funder also sets BNB; harmless here).
	sdb.SetBalance(sandwichAttacker, uint256.NewInt(1), tracing.BalanceChangeUnspecified)

	amount := big.NewInt(123456789)
	if !e.fundAttackerDyn(sdb, cc, hdr, token, common.Address{}, amount) {
		t.Fatalf("fundAttackerDyn returned not fundable")
	}
	got, ok := e.dynAttackerTokenBalance(sdb, cc, hdr, token)
	if !ok || got.Cmp(amount) != 0 {
		t.Fatalf("attacker balance = %s (ok=%v), want %s", got, ok, amount)
	}
	// Confirm via the contract's own balanceOf path too.
	ret, err := e.EthCall(sdb, cc, hdr, token, sel1Addr(selBalanceOf, sandwichAttacker), 0)
	if err != nil || new(big.Int).SetBytes(ret).Cmp(amount) != 0 {
		t.Fatalf("balanceOf via EthCall = 0x%x (err=%v), want %s", ret, err, amount)
	}
}

// TestCreditPairBalance confirms crediting a pair's input balance adds to the
// existing balance at the resolved slot (the fee-on-transfer-immune funding leg).
func TestCreditPairBalance(t *testing.T) {
	e, sdb, hdr := newTestEngineState(t)
	cc := stubChainCtx{}

	token := common.HexToAddress("0xba70000000000000000000000000000000000ba7")
	setTestCode(sdb, token, minimalERC20Runtime(0))
	pair := common.HexToAddress("0x9a1f000000000000000000000000000000009a1f")

	// Seed an existing pair balance of 1000 at slot 0.
	sdb.SetState(token, balanceOfKey(pair, 0), common.BigToHash(big.NewInt(1000)))

	if !e.creditPairBalance(sdb, cc, hdr, pair, token, big.NewInt(500)) {
		t.Fatalf("creditPairBalance returned not fundable")
	}
	word := sdb.GetState(token, balanceOfKey(pair, 0))
	got := new(big.Int).SetBytes(word.Bytes())
	if got.Cmp(big.NewInt(1500)) != 0 {
		t.Fatalf("pair balance after credit = %s, want 1500", got)
	}
}

// ---------------------------------------------------------------------------
// K-safe amountOut under-quote.
// ---------------------------------------------------------------------------

// TestKsafeAmountOutUnderQuotes confirms ksafeAmountOut is exactly GetAmountOut-1
// (the 1-wei k-invariant cushion) for a positive quote, and 0 when the raw quote
// is non-positive. Over-quoting reverts on-chain ("Pancake: K"); under-quoting is
// always safe — this is the safety boundary proven on-node.
func TestKsafeAmountOutUnderQuotes(t *testing.T) {
	reserveIn := new(big.Int).Mul(big.NewInt(1000), e18)
	reserveOut := new(big.Int).Mul(big.NewInt(2_000_000), e18)
	amountIn := new(big.Int).Mul(big.NewInt(5), e18)

	for _, g := range []strategy.Gamma{strategy.GammaPancakeV2, strategy.GammaBiswapV2, gammaGenericV2} {
		raw := strategy.GetAmountOut(amountIn, reserveIn, reserveOut, g)
		ks := ksafeAmountOut(amountIn, reserveIn, reserveOut, g)
		want := new(big.Int).Sub(raw, big.NewInt(1))
		if ks.Cmp(want) != 0 {
			t.Fatalf("gamma %s/%s: ksafe = %s, want %s (raw-1)", g.Num, g.Den, ks, want)
		}
		// The K-safe quote is strictly below the raw quote (never over-quotes).
		if ks.Cmp(raw) >= 0 {
			t.Fatalf("gamma %s/%s: ksafe %s not below raw %s", g.Num, g.Den, ks, raw)
		}
	}

	// Non-positive raw quote -> 0.
	if got := ksafeAmountOut(big.NewInt(0), reserveIn, reserveOut, strategy.GammaPancakeV2); got.Sign() != 0 {
		t.Fatalf("zero amountIn ksafe = %s, want 0", got)
	}
}

// e18 is 10^18, shared with strategy tests' convention.
var e18 = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// ---------------------------------------------------------------------------
// Numeraire denomination (the UNITS fix): every sandwich is valued in a single
// comparable unit (WBNB, or a stable converted to BNB), and token/token pools
// with no numeraire are skipped instead of summed as cross-unit garbage.
// ---------------------------------------------------------------------------

// memecoinA / memecoinB are arbitrary non-numeraire long-tail tokens.
var (
	memecoinA = common.HexToAddress("0xaaaa000000000000000000000000000000000aaa")
	memecoinB = common.HexToAddress("0xbbbb000000000000000000000000000000000bbb")
)

// TestPoolNumeraireIdentifiesNumeraireSide confirms poolNumeraire picks the
// WBNB/stable side of a pool, prefers WBNB when both sides are numeraires, and
// reports no-numeraire for a token/token (memecoin/memecoin) pool — the skip path.
func TestPoolNumeraireIdentifiesNumeraireSide(t *testing.T) {
	cases := []struct {
		name     string
		t0, t1   common.Address
		wantTok  common.Address
		wantKind numeraireKind
		wantOk   bool
	}{
		{"wbnb/memecoin -> wbnb", strategy.WBNB, memecoinA, strategy.WBNB, numWBNB, true},
		{"memecoin/wbnb -> wbnb", memecoinA, strategy.WBNB, strategy.WBNB, numWBNB, true},
		{"usdt/memecoin -> usdt stable", strategy.USDT, memecoinA, strategy.USDT, numStable, true},
		{"memecoin/usdc -> usdc stable", memecoinA, strategy.USDC, strategy.USDC, numStable, true},
		{"wbnb/usdt -> wbnb wins", strategy.WBNB, strategy.USDT, strategy.WBNB, numWBNB, true},
		{"usdt/wbnb -> wbnb wins", strategy.USDT, strategy.WBNB, strategy.WBNB, numWBNB, true},
		{"memecoin/memecoin -> none (skip)", memecoinA, memecoinB, common.Address{}, numNone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := anyPool{token0: c.t0, token1: c.t1, ok: true}
			gotTok, gotKind, gotOk := poolNumeraire(p)
			if gotOk != c.wantOk || gotKind != c.wantKind || gotTok != c.wantTok {
				t.Fatalf("poolNumeraire = (%s,%d,%v), want (%s,%d,%v)",
					gotTok.Hex(), gotKind, gotOk, c.wantTok.Hex(), c.wantKind, c.wantOk)
			}
		})
	}

	// A non-resolved pool (ok=false) must never be valued (defends the nil-meta path).
	if _, _, ok := poolNumeraire(anyPool{token0: strategy.WBNB, token1: memecoinA, ok: false}); ok {
		t.Fatalf("poolNumeraire on an unresolved pool must report no numeraire")
	}
}

// TestNumeraireToBNBDenominatesInBNB confirms the gross is correctly carried in
// BNB: WBNB is identity (already BNB), and a stable is converted via the live
// WBNB/USD spot (bnbWei = stableWei / priceUSD). This is the core of the units fix
// — gross is in BNB, never in an arbitrary memecoin unit.
func TestNumeraireToBNBDenominatesInBNB(t *testing.T) {
	// WBNB: 2.5 WBNB gross -> 2.5 BNB (identity), regardless of price.
	wbnbGross := new(big.Int).Mul(big.NewInt(25), new(big.Int).Div(e18, big.NewInt(10))) // 2.5e18
	if got := numeraireToBNB(wbnbGross, numWBNB, 600.0); got.Cmp(wbnbGross) != 0 {
		t.Fatalf("WBNB numeraire must be identity: got %s, want %s", got, wbnbGross)
	}

	// Stable: 600 USDT gross at $600/WBNB -> 1.0 BNB.
	stableGross := new(big.Int).Mul(big.NewInt(600), e18) // 600e18 (18dp stable)
	gotBNB := numeraireToBNB(stableGross, numStable, 600.0)
	wantBNB := new(big.Int).Set(e18) // 1.0 BNB
	// Allow a 1-wei float-rounding tolerance.
	diff := new(big.Int).Abs(new(big.Int).Sub(gotBNB, wantBNB))
	if diff.Cmp(big.NewInt(1_000_000)) > 0 { // < 1e6 wei tolerance
		t.Fatalf("600 USDT @ $600/WBNB -> %s wei BNB, want ~%s", gotBNB, wantBNB)
	}

	// Stable with no live price -> 0 (cannot convert; must not produce garbage).
	if got := numeraireToBNB(stableGross, numStable, 0); got.Sign() != 0 {
		t.Fatalf("stable with no price must convert to 0, got %s", got)
	}
	// Non-positive amount -> 0.
	if got := numeraireToBNB(big.NewInt(0), numWBNB, 600.0); got.Sign() != 0 {
		t.Fatalf("zero gross must convert to 0, got %s", got)
	}
}

// TestBNBNetGate confirms the net-profit gate is computed entirely in BNB wei:
// net = grossBNB - gasUnits*gasPrice - flashFee(frontrunBNB) - bid. With a
// realistic sub-$ sandwich the net is on the order of fractions of a BNB-cent, NOT
// millions — the symptom's totalNetWei ~1e30 is impossible once gross is in BNB.
func TestBNBNetGate(t *testing.T) {
	// A realistic small sandwich: grossBNB = 0.002 BNB (~ $1.2 at $600), frontrun
	// 0.5 BNB notional, 2 router legs at 3 gwei, 9 bps flash fee.
	grossBNB := new(big.Int).Mul(big.NewInt(2), new(big.Int).Div(e18, big.NewInt(1000))) // 0.002e18
	frontrunBNB := new(big.Int).Div(e18, big.NewInt(2))                                  // 0.5e18
	gasPrice := big.NewInt(3_000_000_000)                                                // 3 gwei
	gasUnits := strategy.SandwichGasUnits(false)
	const flashBps = 9

	eval := strategy.SandwichNet(frontrunBNB, grossBNB, gasPrice, gasUnits, flashBps, big.NewInt(0))

	// Net is computed in BNB wei: net = gross - gas - flash.
	wantGas := new(big.Int).Mul(new(big.Int).SetUint64(gasUnits), gasPrice)
	if eval.GasCost.Cmp(wantGas) != 0 {
		t.Fatalf("gasBNB = %s, want %s", eval.GasCost, wantGas)
	}
	wantFlash := strategy.FlashFee(frontrunBNB, flashBps)
	if eval.FlashFee.Cmp(wantFlash) != 0 {
		t.Fatalf("flashFeeBNB = %s, want %s", eval.FlashFee, wantFlash)
	}
	wantNet := new(big.Int).Sub(grossBNB, wantGas)
	wantNet.Sub(wantNet, wantFlash)
	if eval.NetProfit.Cmp(wantNet) != 0 {
		t.Fatalf("netBNB = %s, want %s", eval.NetProfit, wantNet)
	}

	// Sanity: the net magnitude is sub-BNB (here ~0.0017 BNB), not the trillion-BNB
	// (1e30) artifact the cross-unit bug produced. |net| < 1 BNB.
	if new(big.Int).Abs(eval.NetProfit).Cmp(e18) >= 0 {
		t.Fatalf("realistic sandwich net |%s| should be < 1 BNB", eval.NetProfit)
	}

	// A gross that does NOT clear gas+flash is correctly rejected (not profitable).
	tinyGross := big.NewInt(1) // 1 wei BNB gross.
	if e := strategy.SandwichNet(frontrunBNB, tinyGross, gasPrice, gasUnits, flashBps, big.NewInt(0)); e.Profitable {
		t.Fatalf("a 1-wei gross must not clear the BNB net gate")
	}
}

// TestPoolReserveOfNilSafe confirms poolReserveOf never dereferences nil pool
// fields and returns nil for tokens not in the pool / for V3 pools — the
// nil-pointer guard on the per-victim reserve read.
func TestPoolReserveOfNilSafe(t *testing.T) {
	_, sdb, _ := newTestEngineState(t)

	// V3 pool: no packed reserves -> nil.
	if got := poolReserveOf(sdb, anyPool{isV3: true, token0: strategy.WBNB, token1: memecoinA}, strategy.WBNB); got != nil {
		t.Fatalf("V3 pool reserve must be nil, got %s", got)
	}
	// Token not in the (V2) pool -> nil (no panic).
	pair := common.HexToAddress("0x9001000000000000000000000000000000009001")
	if got := poolReserveOf(sdb, anyPool{token0: strategy.WBNB, token1: memecoinA, pair: pair}, memecoinB); got != nil {
		t.Fatalf("token not in pool must yield nil reserve, got %s", got)
	}
}
