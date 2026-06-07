// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// pools_trigger_test.go pins the swap TRIGGER scope for the cross-DEX backrun
// detector. The trigger gate must cover the FULL verified extended watch set
// (PancakeSwap V2 + Biswap V2 + PancakeSwap V3), recognizing the V2 Swap/Sync
// topics for V2-style pools and the V3 Swap topic for V3 pools. These tests
// guard against the regression where the trigger only saw the 3 original
// WatchedPools (poolByPair) and so never fired on Biswap or V3 swaps.
package strategy

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// firstExtPool returns the first verified extended pool matching the predicate,
// failing the test if none exists (keeps the test honest if the registry shrinks).
func firstExtPool(t *testing.T, pred func(ExtPool) bool) ExtPool {
	t.Helper()
	for _, p := range ExtendedPools() {
		if pred(p) {
			return p
		}
	}
	t.Fatalf("no verified extended pool matched the predicate")
	return ExtPool{}
}

func swapLog(addr common.Address, topic0 common.Hash) *types.Log {
	return &types.Log{Address: addr, Topics: []common.Hash{topic0}}
}

// TestExtendedTriggerCoversV2BiswapV3 is the core regression guard: the extended
// detector must fire on Pancake V2, Biswap V2, and PancakeSwap V3 swaps, with V3
// matched specifically via the V3 Swap topic.
func TestExtendedTriggerCoversV2BiswapV3(t *testing.T) {
	pancakeV2 := firstExtPool(t, func(p ExtPool) bool { return p.DEX == DEXPancakeV2 && !p.IsV3 })
	biswapV2 := firstExtPool(t, func(p ExtPool) bool { return p.DEX == DEXBiswapV2 && !p.IsV3 })
	pancakeV3 := firstExtPool(t, func(p ExtPool) bool { return p.IsV3 })

	// V2-style pools fire on both Swap and Sync topics.
	for _, p := range []ExtPool{pancakeV2, biswapV2} {
		for _, topic := range []common.Hash{SwapTopic0, SyncTopic0} {
			got, ok := IsExtendedWatchedSwapLog(swapLog(p.Pair, topic))
			if !ok {
				t.Fatalf("%s %s: V2 pool should trigger on topic %s", p.DEX, p.Name, topic.Hex())
			}
			if got.Pair != p.Pair || got.IsV3 {
				t.Fatalf("%s %s: returned wrong pool (pair=%s isV3=%v)", p.DEX, p.Name, got.Pair.Hex(), got.IsV3)
			}
		}
		// A V2 pool must NOT be triggered by the V3 Swap topic.
		if _, ok := IsExtendedWatchedSwapLog(swapLog(p.Pair, V3SwapTopic0)); ok {
			t.Fatalf("%s %s: V2 pool must not trigger on the V3 Swap topic", p.DEX, p.Name)
		}
	}

	// V3 pool fires ONLY on the V3 Swap topic, never on V2 Swap/Sync (V3 emits no Sync).
	if got, ok := IsExtendedWatchedSwapLog(swapLog(pancakeV3.Pair, V3SwapTopic0)); !ok || !got.IsV3 {
		t.Fatalf("V3 pool %s should trigger on the V3 Swap topic (ok=%v isV3=%v)", pancakeV3.Name, ok, got.IsV3)
	}
	for _, topic := range []common.Hash{SwapTopic0, SyncTopic0} {
		if _, ok := IsExtendedWatchedSwapLog(swapLog(pancakeV3.Pair, topic)); ok {
			t.Fatalf("V3 pool %s must not trigger on V2 topic %s", pancakeV3.Name, topic.Hex())
		}
	}
}

// TestExtendedTriggerRejectsUnwatched confirms a swap on an unrelated address is
// never a trigger, and that nil/empty logs are handled safely.
func TestExtendedTriggerRejectsUnwatched(t *testing.T) {
	stranger := common.HexToAddress("0xDeaDBeefDeAdBeefdEAdbEEFdeadbEEF00000000")
	if _, ok := IsExtendedWatchedSwapLog(swapLog(stranger, SwapTopic0)); ok {
		t.Fatalf("unwatched address must not trigger")
	}
	if _, ok := IsExtendedWatchedSwapLog(nil); ok {
		t.Fatalf("nil log must not trigger")
	}
	if _, ok := IsExtendedWatchedSwapLog(&types.Log{Address: stranger}); ok {
		t.Fatalf("topic-less log must not trigger")
	}
}

// TestExtendedTriggerSupersetOfLegacy verifies the extended trigger strictly
// supersets the legacy IsWatchedSwapLog: every original WatchedPool still fires,
// and Biswap/V3 (which the legacy trigger misses) now fire too.
func TestExtendedTriggerSupersetOfLegacy(t *testing.T) {
	for _, p := range WatchedPools {
		legacy, lok := IsWatchedSwapLog(swapLog(p.Pair, SwapTopic0))
		ext, eok := IsExtendedWatchedSwapLog(swapLog(p.Pair, SwapTopic0))
		if !lok || !eok {
			t.Fatalf("%s: both legacy(%v) and extended(%v) must fire on original pool", p.Name, lok, eok)
		}
		if legacy.Pair != ext.Pair {
			t.Fatalf("%s: legacy/extended disagree on pair", p.Name)
		}
	}

	// Biswap and V3 must fire on the extended trigger but NOT on the legacy one.
	biswap := firstExtPool(t, func(p ExtPool) bool { return p.DEX == DEXBiswapV2 })
	if _, ok := IsWatchedSwapLog(swapLog(biswap.Pair, SwapTopic0)); ok {
		t.Fatalf("legacy trigger should NOT know Biswap pool (it indexes only WatchedPools)")
	}
	if _, ok := IsExtendedWatchedSwapLog(swapLog(biswap.Pair, SwapTopic0)); !ok {
		t.Fatalf("extended trigger must fire on Biswap swap")
	}
	v3 := firstExtPool(t, func(p ExtPool) bool { return p.IsV3 })
	if _, ok := IsExtendedWatchedSwapLog(swapLog(v3.Pair, V3SwapTopic0)); !ok {
		t.Fatalf("extended trigger must fire on V3 swap")
	}
}

// TestExtendedPairsTouched checks the flat-log scan de-duplicates by pair in
// first-touch order across a mixed V2/Biswap/V3 log stream.
func TestExtendedPairsTouched(t *testing.T) {
	pancakeV2 := firstExtPool(t, func(p ExtPool) bool { return p.DEX == DEXPancakeV2 && !p.IsV3 })
	biswapV2 := firstExtPool(t, func(p ExtPool) bool { return p.DEX == DEXBiswapV2 && !p.IsV3 })
	pancakeV3 := firstExtPool(t, func(p ExtPool) bool { return p.IsV3 })
	stranger := common.HexToAddress("0xDeaDBeefDeAdBeefdEAdbEEFdeadbEEF00000000")

	logs := []*types.Log{
		swapLog(stranger, SwapTopic0),         // ignored
		swapLog(pancakeV2.Pair, SwapTopic0),   // 1st
		swapLog(pancakeV2.Pair, SyncTopic0),   // dup
		swapLog(biswapV2.Pair, SyncTopic0),    // 2nd
		swapLog(pancakeV3.Pair, V3SwapTopic0), // 3rd
		swapLog(pancakeV3.Pair, SyncTopic0),   // ignored (wrong topic for V3)
	}
	touched := ExtendedPairsTouched(logs)
	if len(touched) != 3 {
		t.Fatalf("expected 3 distinct touched pools, got %d: %+v", len(touched), touched)
	}
	want := []common.Address{pancakeV2.Pair, biswapV2.Pair, pancakeV3.Pair}
	for i, p := range touched {
		if p.Pair != want[i] {
			t.Fatalf("touched[%d] = %s, want %s", i, p.Pair.Hex(), want[i].Hex())
		}
	}
}
