// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// pools_ext.go is the multi-DEX extension of the watch set for the IMPROVED
// arbitrage model (paper/40-models.md): it adds PancakeSwap V3, Biswap, and
// Thena pools alongside the verified PancakeSwap V2 core in pools.go, plus a V3
// price reader (slot0 / sqrtPriceX96) to complement the V2 reserves reader.
//
// VERIFICATION STATUS (read this before trusting any address):
//   - The three PancakeSwap V2 pools and all four token addresses in pools.go
//     are VERIFIED on-chain (Phase 2).
//   - The DEX factory/router addresses below (Biswap, Thena, Pancake V3) are the
//     well-known canonical BSC deployments but are marked Verified=false until
//     re-checked against THIS node at bring-up (the design mandates a node-side
//     re-verification pass; see RegistryAudit()).
//   - The individual POOL addresses below are marked Verified per pool. Any pool
//     with Verified=false is a PLACEHOLDER: it is EXCLUDED from the live graph by
//     ExtendedPools() unless SIMENGINE_INCLUDE_UNVERIFIED=1. This keeps the
//     backtest honest (we never report profit from a pool we have not confirmed
//     exists with the assumed token order / fee / slot layout).
//
// To edit the watch set: add entries to extendedPoolSet below. Set Verified=true
// ONLY after confirming on the node: (a) the pair address has code, (b) token0 <
// token1 ordering, (c) the fee tier, (d) reserves at slot 8 (V2) or slot0 (V3).
package strategy

import (
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
)

// ---------------------------------------------------------------------------
// Additional fee factors for V2-style forks.
// ---------------------------------------------------------------------------

// GammaBiswapV2 is the Biswap V2 fee factor (0.20% fee => 998/1000). Biswap is a
// PancakeSwap-V2 fork with the same slot-8 reserves layout but a lower fee.
var GammaBiswapV2 = Gamma{Num: big.NewInt(998), Den: big.NewInt(1000)}

// GammaThenaV2 is the Thena (Algebra/Solidly-style) volatile-pair fee factor.
// Thena's classic AMM uses a ~0.20% volatile fee; concentrated-liquidity ("CL")
// pools are treated as V3 and priced through the EVM. PLACEHOLDER fee until the
// specific pool's fee is confirmed on-chain.
var GammaThenaV2 = Gamma{Num: big.NewInt(998), Den: big.NewInt(1000)}

// gammaForFeeTier converts a V3 fee tier (hundredths of a bip, e.g. 2500 =
// 0.25%) into the multiplicative kept factor gamma = 1 - feeTier/1e6.
func gammaForFeeTier(feeTier uint32) Gamma {
	// feeTier is in units of 1e-6 (1e6 = 100%). gamma = (1e6 - feeTier)/1e6.
	return Gamma{
		Num: big.NewInt(int64(1_000_000 - feeTier)),
		Den: big.NewInt(1_000_000),
	}
}

// ---------------------------------------------------------------------------
// Extra hub tokens (verified addresses where known).
// ---------------------------------------------------------------------------

var (
	// USD1 (World Liberty Financial USD) — a rising BSC stable hub in 2025-26.
	// PLACEHOLDER address: re-verify on node before enabling pools that use it.
	USD1 = common.HexToAddress("0x8d0D000Ee44948FC98c9B98A4FA4921476f08B0d")
)

// ---------------------------------------------------------------------------
// V3 price reader (slot0 / sqrtPriceX96).
// ---------------------------------------------------------------------------

// slot0Slot is storage slot 0 of a Uniswap/PancakeSwap V3 pool, which packs
// slot0: { uint160 sqrtPriceX96; int24 tick; ... } least-significant-first.
var slot0Slot = common.BigToHash(big.NewInt(0))

// mask160 = (1<<160)-1, extracts the sqrtPriceX96 field.
var mask160 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))

// mask24 = (1<<24)-1, extracts the int24 tick field.
var mask24 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 24), big.NewInt(1))

// two96 = 2^96, the Q64.96 scale of sqrtPriceX96.
var two96 = new(big.Int).Lsh(big.NewInt(1), 96)

// Slot0 holds the decoded V3 slot0 price state.
type Slot0 struct {
	SqrtPriceX96 *big.Int
	Tick         int64 // int24, sign-extended
}

// toInt24 sign-extends a 24-bit two's-complement value held in the low 24 bits
// of v into a signed int64.
func toInt24(v *big.Int) int64 {
	t := new(big.Int).And(v, mask24)
	// If the sign bit (bit 23) is set, subtract 2^24.
	if t.Bit(23) == 1 {
		t.Sub(t, new(big.Int).Lsh(big.NewInt(1), 24))
	}
	return t.Int64()
}

// ReadSlot0 reads slot 0 of a V3 pool and decodes sqrtPriceX96 (low 160 bits)
// and tick (next 24 bits, two's-complement). Returns zero SqrtPriceX96 for an
// empty slot (caller should treat as "no pool / skip").
func ReadSlot0(statedb *state.StateDB, pool common.Address) Slot0 {
	word := statedb.GetState(pool, slot0Slot)
	packed := new(big.Int).SetBytes(word[:])
	sqrtP := new(big.Int).And(packed, mask160)
	tick := toInt24(new(big.Int).Rsh(packed, 160))
	return Slot0{SqrtPriceX96: sqrtP, Tick: tick}
}

// V3SpotPrice returns the spot price of token1 in terms of token0 as a *big.Rat
// from sqrtPriceX96: P = (sqrtPriceX96 / 2^96)^2 (token1/token0). This is the
// DETECTOR price only; V3 swap outputs and the optimal cycle size are computed
// by the EVM oracle, never analytically (L is tick-piecewise).
func V3SpotPrice(sqrtPriceX96 *big.Int) *big.Rat {
	if sqrtPriceX96 == nil || sqrtPriceX96.Sign() <= 0 {
		return new(big.Rat)
	}
	num := new(big.Int).Mul(sqrtPriceX96, sqrtPriceX96) // sqrtP^2
	den := new(big.Int).Mul(two96, two96)               // 2^192
	return new(big.Rat).SetFrac(num, den)
}

// v3VirtualReserves derives spot-equivalent "virtual reserves" (rIn, rOut) for a
// directed V3 hop tokenIn->tokenOut purely so the edge has a marginal rate for
// DETECTION. The ratio rOut/rIn equals the spot price in the swap direction; the
// magnitudes are arbitrary (we scale to 1e18) because V3 sizing is never done
// from these — only the ratio matters for the negative-cycle weight. zeroForOne
// is true when the swap sells token0 for token1.
func v3VirtualReserves(sqrtPriceX96 *big.Int, zeroForOne bool) (rIn, rOut *big.Int) {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	p := V3SpotPrice(sqrtPriceX96) // token1/token0
	if p.Sign() <= 0 {
		return big.NewInt(0), big.NewInt(0)
	}
	// price of out per in:
	//   zeroForOne (in=token0,out=token1): rate = P (token1 per token0)
	//   else        (in=token1,out=token0): rate = 1/P
	rate := new(big.Rat).Set(p)
	if !zeroForOne {
		rate = new(big.Rat).Inv(p)
	}
	// rOut/rIn == rate: set rIn = scale, rOut = scale*rate (rounded).
	rIn = new(big.Int).Set(scale)
	rOutRat := new(big.Rat).Mul(rate, new(big.Rat).SetInt(scale))
	rOut = new(big.Int).Quo(rOutRat.Num(), rOutRat.Denom())
	if rOut.Sign() <= 0 {
		return big.NewInt(0), big.NewInt(0)
	}
	return rIn, rOut
}

// ---------------------------------------------------------------------------
// Extended (multi-DEX) pool registry.
// ---------------------------------------------------------------------------

// DEX identifiers used in the registry and in opportunity logs.
const (
	DEXPancakeV2 = "pancake_v2"
	DEXPancakeV3 = "pancake_v3"
	DEXBiswapV2  = "biswap_v2"
	DEXThenaV2   = "thena_v2"
	DEXThenaCL   = "thena_cl" // concentrated liquidity, priced as V3
)

// ExtPool describes one watched pool across any supported DEX. For V2-style
// pools (IsV3=false) reserves are read from slot 8 and Gamma is the fixed fork
// fee. For V3-style pools (IsV3=true) the price is read from slot0/sqrtPriceX96,
// Gamma is derived from FeeTier, and all sizing/valuation is deferred to the EVM
// oracle. Token0/Token1 follow address-sorted ordering (Token0 < Token1).
type ExtPool struct {
	Name     string
	DEX      string
	Pair     common.Address
	Token0   common.Address
	Token1   common.Address
	Gamma    Gamma
	IsV3     bool
	FeeTier  uint32 // V3 fee tier (hundredths of a bip); 0 for V2
	Verified bool   // true ONLY after on-node confirmation of addr/order/fee/slot
}

// Other returns the token of the pool that is not `t`, and whether `t` belongs to
// the pool at all. Mirrors Pool.Other for the extended (multi-DEX) registry; used
// by the sandwich valuator to find the victim's output token (Y) from its input X.
func (p ExtPool) Other(t common.Address) (common.Address, bool) {
	switch t {
	case p.Token0:
		return p.Token1, true
	case p.Token1:
		return p.Token0, true
	default:
		return common.Address{}, false
	}
}

// Has reports whether token t is one of the pool's two tokens.
func (p ExtPool) Has(t common.Address) bool {
	return t == p.Token0 || t == p.Token1
}

// extendedPoolSet is the editable multi-DEX watch set. The PancakeSwap V2 block
// reuses the VERIFIED pools from pools.go (Verified=true). Everything else is a
// canonical-but-unconfirmed PLACEHOLDER (Verified=false) and is excluded from
// the live graph until re-checked on this node (see ExtendedPools).
//
// EDIT HERE to grow the watch set. Keep the deepest pool per (token-pair, DEX,
// fee-tier) so cross-DEX and cross-version cycles are enumerable.
var extendedPoolSet = []ExtPool{
	// ---- PancakeSwap V2 (VERIFIED in Phase 2; mirrors WatchedPools) ----
	{
		Name: "WBNB/USDT", DEX: DEXPancakeV2,
		Pair:   common.HexToAddress("0x16b9a82891338f9bA80E2D6970FddA79D1eb0daE"),
		Token0: USDT, Token1: WBNB, Gamma: GammaPancakeV2, Verified: true,
	},
	{
		Name: "WBNB/USDC", DEX: DEXPancakeV2,
		Pair:   common.HexToAddress("0xd99c7F6C65857AC913a8f880A4cb84032AB2FC5b"),
		Token0: USDC, Token1: WBNB, Gamma: GammaPancakeV2, Verified: true,
	},
	{
		Name: "USDT/USDC", DEX: DEXPancakeV2,
		Pair:   common.HexToAddress("0xEc6557348085Aa57C72514D67070dC863C0a5A8c"),
		Token0: USDT, Token1: USDC, Gamma: GammaPancakeV2, Verified: true,
	},

	// ---- Biswap V2 (PLACEHOLDER pool addresses — re-verify on node) ----
	// Biswap is a Pancake-V2 fork (slot-8 reserves) with a 0.20% fee. Including a
	// second-venue WBNB/USDT pool is what makes cross-DEX cycles possible.
	{
		Name: "WBNB/USDT", DEX: DEXBiswapV2,
		Pair:   common.HexToAddress("0x8840c6252e2e86e545defb6da98b2a0e26d8c1ba"), // VERIFIED on node 2026-06-05 (Biswap factory getPair; swapFee=2 => 0.2%; ~$0.4M liq)
		Token0: USDT, Token1: WBNB, Gamma: GammaBiswapV2, Verified: true,
	},
	{
		Name: "WBNB/USDC", DEX: DEXBiswapV2,
		Pair:   common.HexToAddress("0x06cd679121ec37b0a2fd673d4976b09d81791856"), // VERIFIED on node 2026-06-05 (~$44k liq)
		Token0: USDC, Token1: WBNB, Gamma: GammaBiswapV2, Verified: true,
	},
	{
		Name: "USDT/USDC", DEX: DEXBiswapV2,
		Pair:   common.HexToAddress("0x1483767e665b3591677fd49f724bf7430c18bf83"), // VERIFIED on node 2026-06-05
		Token0: USDT, Token1: USDC, Gamma: GammaBiswapV2, Verified: true,
	},

	// ---- PancakeSwap V3 (VERIFIED on node 2026-06-05 via factory getPool + liquidity()) ----
	// Priced from slot0/sqrtPriceX96 for Stage-A DETECTION; profit valuation for
	// V3-containing cycles requires the EVM oracle (CycleOptimum returns 0 for V3).
	{
		Name: "WBNB/USDT", DEX: DEXPancakeV3,
		Pair:   common.HexToAddress("0x172fcd41e0913e95784454622d1c3724f546f849"), // fee 0.01% (deepest, L~3.15M)
		Token0: USDT, Token1: WBNB, IsV3: true, FeeTier: 100, Gamma: gammaForFeeTier(100), Verified: true,
	},
	{
		Name: "WBNB/USDT", DEX: DEXPancakeV3,
		Pair:   common.HexToAddress("0x36696169c63e42cd08ce11f5deebbcebae652050"), // fee 0.05% (L~229k)
		Token0: USDT, Token1: WBNB, IsV3: true, FeeTier: 500, Gamma: gammaForFeeTier(500), Verified: true,
	},
	{
		Name: "WBNB/USDT", DEX: DEXPancakeV3,
		Pair:   common.HexToAddress("0x1401ff943d08a7e098328c1d3a9d388923b115d2"), // fee 0.25% (L~10k)
		Token0: USDT, Token1: WBNB, IsV3: true, FeeTier: 2500, Gamma: gammaForFeeTier(2500), Verified: true,
	},
	{
		Name: "WBNB/USDC", DEX: DEXPancakeV3,
		Pair:   common.HexToAddress("0xf2688fb5b81049dfb7703ada5e770543770612c4"), // fee 0.01% (L~357k)
		Token0: USDC, Token1: WBNB, IsV3: true, FeeTier: 100, Gamma: gammaForFeeTier(100), Verified: true,
	},
	{
		Name: "WBNB/USDC", DEX: DEXPancakeV3,
		Pair:   common.HexToAddress("0x81a9b5f18179ce2bf8f001b8a634db80771f1824"), // fee 0.05% (L~24k)
		Token0: USDC, Token1: WBNB, IsV3: true, FeeTier: 500, Gamma: gammaForFeeTier(500), Verified: true,
	},
	{
		Name: "USDT/USDC", DEX: DEXPancakeV3,
		Pair:   common.HexToAddress("0x92b7807bf19b7dddf89b706143896d05228f3121"), // fee 0.01% (stable, L~42B)
		Token0: USDT, Token1: USDC, IsV3: true, FeeTier: 100, Gamma: gammaForFeeTier(100), Verified: true,
	},

	// ---- Thena (PLACEHOLDER — re-verify on node) ----
	{
		Name: "WBNB/USDT", DEX: DEXThenaV2,
		Pair:   common.HexToAddress("0x0000000000000000000000000000000000000000"), // PLACEHOLDER
		Token0: USDT, Token1: WBNB, Gamma: GammaThenaV2, Verified: false,
	},
}

// ExtendedPools returns the watch set used to build the Stage-A graph. By
// default it returns ONLY verified pools (the honest set). Setting the env var
// SIMENGINE_INCLUDE_UNVERIFIED=1 includes placeholders too (for development /
// once their addresses have been filled in and re-verified by editing the set).
func ExtendedPools() []ExtPool {
	includeUnverified := os.Getenv("SIMENGINE_INCLUDE_UNVERIFIED") == "1"
	out := make([]ExtPool, 0, len(extendedPoolSet))
	for _, p := range extendedPoolSet {
		if p.Verified || includeUnverified {
			// Skip obvious placeholder zero addresses even when including unverified.
			if (p.Pair == common.Address{}) {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// AllExtendedPools returns the full registry (verified + placeholder) for
// auditing / emission to file, regardless of env. Use ExtendedPools() to build
// the live graph.
func AllExtendedPools() []ExtPool {
	out := make([]ExtPool, len(extendedPoolSet))
	copy(out, extendedPoolSet)
	return out
}

// RegistryAudit is a human-readable, deterministic dump of the registry with
// verification flags, suitable for writing to a file at harness bring-up so the
// exact watch set is auditable alongside the results.
func RegistryAudit() string {
	s := "watch-set audit (DEX | pool | token0<token1 | fee | v3 | verified)\n"
	for _, p := range extendedPoolSet {
		v3 := "v2"
		if p.IsV3 {
			v3 = "v3"
		}
		ver := "PLACEHOLDER"
		if p.Verified {
			ver = "verified"
		}
		s += p.DEX + " | " + p.Name + " | " + p.Pair.Hex() + " | " +
			p.Token0.Hex() + "<" + p.Token1.Hex() + " | " + v3 + " | " + ver + "\n"
	}
	return s
}

// ---------------------------------------------------------------------------
// Graph construction from chain state.
// ---------------------------------------------------------------------------

// BuildGraph constructs the Stage-A token multigraph by reading each pool's
// current price out of the given (post-block) state: V2 reserves from slot 8,
// V3 spot from slot0/sqrtPriceX96 (as spot-equivalent virtual reserves for
// detection only). Only pools returned by ExtendedPools() are included. The
// graph is pure thereafter; the caller runs NegativeCycles on it.
func BuildGraph(statedb *state.StateDB) *Graph {
	return BuildGraphFromPools(statedb, ExtendedPools())
}

// BuildGraphFromPools is the explicit-pool-slice variant of BuildGraph: it builds
// the Stage-A token multigraph from the SUPPLIED pool slice instead of the fixed
// ExtendedPools() hub. It reuses the EXACT same V2 slot-8 / V3 slot0 readers and
// AddEdge/AddV2Pool edge construction as BuildGraph, so cycles enumerated over the
// resulting graph are sized/valued by the identical CycleOptimum / ValueCycle
// machinery. The any-pool backrun detector uses this to seed a graph from the set
// of pools a victim TOUCHED (plus the verified hub) so cross-pool cycles starting
// at the victim's input token are enumerable. Read-only.
func BuildGraphFromPools(statedb *state.StateDB, pools []ExtPool) *Graph {
	g := NewGraph()
	for _, p := range pools {
		if p.IsV3 {
			s0 := ReadSlot0(statedb, p.Pair)
			if s0.SqrtPriceX96 == nil || s0.SqrtPriceX96.Sign() <= 0 {
				continue
			}
			// Two directed edges with spot-equivalent virtual reserves.
			// zeroForOne edge: token0 -> token1.
			rin01, rout01 := v3VirtualReserves(s0.SqrtPriceX96, true)
			if rin01.Sign() > 0 && rout01.Sign() > 0 {
				g.AddEdge(Edge{
					Pool: p.Pair, DEX: p.DEX, FeeTier: p.FeeTier, Gamma: p.Gamma,
					TokenIn: p.Token0, TokenOut: p.Token1, IsV3: true,
					ReserveIn: rin01, ReserveOut: rout01,
					SqrtPriceX96: new(big.Int).Set(s0.SqrtPriceX96),
				})
			}
			// oneForZero edge: token1 -> token0.
			rin10, rout10 := v3VirtualReserves(s0.SqrtPriceX96, false)
			if rin10.Sign() > 0 && rout10.Sign() > 0 {
				g.AddEdge(Edge{
					Pool: p.Pair, DEX: p.DEX, FeeTier: p.FeeTier, Gamma: p.Gamma,
					TokenIn: p.Token1, TokenOut: p.Token0, IsV3: true,
					ReserveIn: rin10, ReserveOut: rout10,
					SqrtPriceX96: new(big.Int).Set(s0.SqrtPriceX96),
				})
			}
			continue
		}
		// V2-style: read slot-8 reserves.
		rv := ReadReserves(statedb, p.Pair)
		if rv.Reserve0 == nil || rv.Reserve1 == nil || rv.Reserve0.Sign() <= 0 || rv.Reserve1.Sign() <= 0 {
			continue
		}
		g.AddV2Pool(p.Pair, p.DEX, p.Gamma, p.Token0, p.Token1, rv.Reserve0, rv.Reserve1)
	}
	return g
}
