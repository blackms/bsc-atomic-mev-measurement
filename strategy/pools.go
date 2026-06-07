// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// pools.go is the chain-facing half of package strategy: the registry of watched
// PancakeSwap V2 pools and the helpers that (a) read a pair's reserves directly
// out of a *state.StateDB and (b) recognise a watched swap from execution logs
// or from a pending tx's router/selector.
//
// All addresses, the reserves storage slot (8), the slot packing, the fee
// factor (0.25% => gamma 9975/10000), and the event/selector hashes below were
// VERIFIED on-chain against the live BSC node in the Phase 2 research. See the
// gotchas: reserves live at slot 8 (NOT 11); the slot is packed least-
// significant-first (reserve0 = low 112 bits, reserve1 = next 112 bits,
// blockTimestampLast = high 32 bits); token0 is the LOWER address (not WBNB);
// and most volume routes through aggregators/contracts so the log-based detector
// (Swap/Sync topic0 on a watched pair address) is the ground truth.
package strategy

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
)

// ---------------------------------------------------------------------------
// Core contracts and tokens (BSC mainnet, verified on-chain).
// ---------------------------------------------------------------------------

var (
	PancakeV2Factory = common.HexToAddress("0xcA143Ce32Fe78f1f7019d7d551a6402fC5350c73")
	PancakeV2Router  = common.HexToAddress("0x10ED43C718714eb63d5aA57B78B54704E256024E")

	WBNB = common.HexToAddress("0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c")
	USDT = common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")
	USDC = common.HexToAddress("0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d")
	BUSD = common.HexToAddress("0xe9e7CEA3DedcA5984780Bafc599bD69ADd087D56")
)

// reservesSlot is the storage slot (8) that holds the packed reserves on every
// PancakeSwap V2 pair. VERIFIED on-chain (NOT slot 11 — see package doc).
var reservesSlot = common.BigToHash(big.NewInt(8))

// mask112 is (1<<112)-1, used to extract each uint112 reserve field.
var mask112 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(1))

// ---------------------------------------------------------------------------
// Event topics and router selectors (computed via keccak256, verified).
// ---------------------------------------------------------------------------

var (
	// SwapTopic0 = keccak256("Swap(address,uint256,uint256,uint256,uint256,address)")
	SwapTopic0 = common.HexToHash("0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822")
	// SyncTopic0 = keccak256("Sync(uint112,uint112)") — fires on every reserve change.
	SyncTopic0 = common.HexToHash("0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1")
	// V3SwapTopic0 = keccak256("Swap(address,address,int256,int256,uint160,uint128,int24)")
	// — the Uniswap/PancakeSwap V3 Swap event (verified on node). V3 pools never
	// emit Sync; slot0 (sqrtPriceX96) is updated atomically with this event.
	V3SwapTopic0 = common.HexToHash("0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67")
)

// routerSwapSelectors is the set of PancakeRouter swap function selectors used as
// a cheap pre-filter for pending txs in LIVE mode. NOTE: this is a pre-filter
// only — most swaps route through aggregators / custom contracts / direct
// pair.swap calls, so always confirm with the log-based detector after
// simulation.
var routerSwapSelectors = map[[4]byte]string{
	{0x38, 0xed, 0x17, 0x39}: "swapExactTokensForTokens",
	{0x88, 0x03, 0xdb, 0xee}: "swapTokensForExactTokens",
	{0x7f, 0xf3, 0x6a, 0xb5}: "swapExactETHForTokens",
	{0x4a, 0x25, 0xd9, 0x4a}: "swapTokensForExactETH",
	{0x18, 0xcb, 0xaf, 0xe5}: "swapExactTokensForETH",
	{0xfb, 0x3b, 0xdb, 0x41}: "swapETHForExactTokens",
	{0x5c, 0x11, 0xd7, 0x95}: "swapExactTokensForTokensSupportingFeeOnTransferTokens",
	{0xb6, 0xf9, 0xde, 0x95}: "swapExactETHForTokensSupportingFeeOnTransferTokens",
	{0x79, 0x1a, 0xc9, 0x47}: "swapExactTokensForETHSupportingFeeOnTransferTokens",
}

// ---------------------------------------------------------------------------
// Pool registry.
// ---------------------------------------------------------------------------

// Pool describes one watched PancakeSwap V2 pair. Token0/Token1 follow the
// factory's address-sorted ordering (Token0 < Token1); reserve0 corresponds to
// Token0 and reserve1 to Token1. Gamma is the pair's fee factor.
type Pool struct {
	Name   string
	Pair   common.Address
	Token0 common.Address
	Token1 common.Address
	Gamma  Gamma
}

// Other returns the token of the pair that is not `t`, and whether `t` belongs
// to the pool at all.
func (p Pool) Other(t common.Address) (common.Address, bool) {
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
func (p Pool) Has(t common.Address) bool {
	return t == p.Token0 || t == p.Token1
}

// WatchedPools is the initial BACKTEST go/no-go watch set: the three highest-
// hit-rate, all-18-decimal, slot-8-readable V2 pairs that form a clean WBNB-
// bridged cycle. token0/token1 ordering verified on-chain.
var WatchedPools = []Pool{
	{
		Name:   "WBNB/USDT",
		Pair:   common.HexToAddress("0x16b9a82891338f9bA80E2D6970FddA79D1eb0daE"),
		Token0: USDT, // 0x55d3.. sorts below WBNB 0xbb4c..
		Token1: WBNB,
		Gamma:  GammaPancakeV2,
	},
	{
		Name:   "WBNB/USDC",
		Pair:   common.HexToAddress("0xd99c7F6C65857AC913a8f880A4cb84032AB2FC5b"),
		Token0: USDC, // 0x8ac7.. sorts below WBNB
		Token1: WBNB,
		Gamma:  GammaPancakeV2,
	},
	{
		Name:   "USDT/USDC",
		Pair:   common.HexToAddress("0xEc6557348085Aa57C72514D67070dC863C0a5A8c"),
		Token0: USDT, // 0x55d3.. sorts below USDC 0x8ac7..
		Token1: USDC,
		Gamma:  GammaPancakeV2,
	},
}

// poolByPair indexes the watch set by pair address for O(1) log attribution.
var poolByPair = func() map[common.Address]Pool {
	m := make(map[common.Address]Pool, len(WatchedPools))
	for _, p := range WatchedPools {
		m[p.Pair] = p
	}
	return m
}()

// PoolByPair returns the watched pool for a pair address, if any.
func PoolByPair(pair common.Address) (Pool, bool) {
	p, ok := poolByPair[pair]
	return p, ok
}

// SharedTokenPools returns every distinct unordered pair of watched pools that
// share at least one common token, together with that shared token. These are
// the candidate 2-pool cycles to evaluate. For pools that share exactly one
// token (e.g. WBNB/USDT + WBNB/USDC) the shared token is the bridge asset.
func SharedTokenPools() []PoolPair {
	var out []PoolPair
	for i := 0; i < len(WatchedPools); i++ {
		for j := i + 1; j < len(WatchedPools); j++ {
			a, b := WatchedPools[i], WatchedPools[j]
			if a.Has(b.Token0) {
				out = append(out, PoolPair{A: a, B: b, Shared: b.Token0})
			} else if a.Has(b.Token1) {
				out = append(out, PoolPair{A: a, B: b, Shared: b.Token1})
			}
		}
	}
	return out
}

// PoolPair is two watched pools that share a common token.
type PoolPair struct {
	A      Pool
	B      Pool
	Shared common.Address
}

// ---------------------------------------------------------------------------
// Reserve reading from a *state.StateDB (direct storage, no EVM call).
// ---------------------------------------------------------------------------

// Reserves holds the decoded packed reserves of a pair (reserve0/reserve1 map to
// token0/token1) plus the last update timestamp.
type Reserves struct {
	Reserve0  *big.Int
	Reserve1  *big.Int
	Timestamp uint64
}

// ReadReserves reads slot 8 of a pair from the given state and decodes the
// packed (reserve0, reserve1, blockTimestampLast) per the verified layout:
//
//	reserve0           = word & ((1<<112)-1)          bits   0..111
//	reserve1           = (word >> 112) & ((1<<112)-1) bits 112..223
//	blockTimestampLast = (word >> 224) & 0xffffffff   bits 224..255
//
// Each reserve field is masked to 112 bits so the timestamp/neighbour never
// bleeds into the reserve value. Returns zero reserves for an empty slot (the
// caller should treat zero reserves as "no liquidity / skip").
func ReadReserves(statedb *state.StateDB, pair common.Address) Reserves {
	word := statedb.GetState(pair, reservesSlot)
	packed := new(big.Int).SetBytes(word[:])

	reserve0 := new(big.Int).And(packed, mask112)
	reserve1 := new(big.Int).And(new(big.Int).Rsh(packed, 112), mask112)
	ts := new(big.Int).Rsh(packed, 224)

	return Reserves{
		Reserve0:  reserve0,
		Reserve1:  reserve1,
		Timestamp: ts.Uint64(),
	}
}

// ReserveOf returns the reserve corresponding to a specific token of the pool.
func (p Pool) ReserveOf(r Reserves, token common.Address) *big.Int {
	switch token {
	case p.Token0:
		return r.Reserve0
	case p.Token1:
		return r.Reserve1
	default:
		return big.NewInt(0)
	}
}

// ---------------------------------------------------------------------------
// Swap recognition.
// ---------------------------------------------------------------------------

// IsWatchedSwapLog reports whether a log is a Swap/Sync event emitted by a
// watched pair — the robust, router-agnostic ground-truth detector. Sync alone
// is enough to know reserves moved; Swap additionally carries directional
// amounts.
func IsWatchedSwapLog(l *types.Log) (Pool, bool) {
	if l == nil || len(l.Topics) == 0 {
		return Pool{}, false
	}
	p, ok := poolByPair[l.Address]
	if !ok {
		return Pool{}, false
	}
	t0 := l.Topics[0]
	if t0 == SwapTopic0 || t0 == SyncTopic0 {
		return p, true
	}
	return Pool{}, false
}

// WatchedPairsTouched scans a flat log list (e.g. SimResult.Logs in execution
// order) and returns the set of watched pools whose reserves were changed,
// de-duplicated, in first-touch order.
func WatchedPairsTouched(logs []*types.Log) []Pool {
	seen := make(map[common.Address]bool)
	var out []Pool
	for _, l := range logs {
		if p, ok := IsWatchedSwapLog(l); ok && !seen[p.Pair] {
			seen[p.Pair] = true
			out = append(out, p)
		}
	}
	return out
}

// extPoolByPair indexes the FULL verified extended watch set (V2 + Biswap V2 +
// PancakeSwap V3) by pair address for O(1) log attribution. It is the trigger
// index for the multi-DEX detector and is kept in sync with ExtendedPools() — it
// is rebuilt on first use to honor the SIMENGINE_INCLUDE_UNVERIFIED env var that
// ExtendedPools() reads, so the trigger scope always matches the graph scope.
var extPoolByPair = func() map[common.Address]ExtPool {
	pools := ExtendedPools()
	m := make(map[common.Address]ExtPool, len(pools))
	for _, p := range pools {
		m[p.Pair] = p
	}
	return m
}()

// IsExtendedWatchedSwapLog reports whether a log is a swap on any pool in the
// FULL verified extended watch set used by the cross-DEX detector (the same set
// ExtendedPools()/BuildGraph use). It is the multi-DEX superset of
// IsWatchedSwapLog: it recognizes the V2 Swap/Sync topics for V2-style pools
// (Pancake V2, Biswap V2 — ExtPool.IsV3==false) and the V3 Swap topic for
// PancakeSwap V3 pools (ExtPool.IsV3==true). Returns the matched ExtPool so
// callers can branch on DEX / IsV3 / FeeTier.
func IsExtendedWatchedSwapLog(l *types.Log) (ExtPool, bool) {
	if l == nil || len(l.Topics) == 0 {
		return ExtPool{}, false
	}
	p, ok := extPoolByPair[l.Address]
	if !ok {
		return ExtPool{}, false
	}
	t0 := l.Topics[0]
	if p.IsV3 {
		if t0 == V3SwapTopic0 {
			return p, true
		}
		return ExtPool{}, false
	}
	if t0 == SwapTopic0 || t0 == SyncTopic0 {
		return p, true
	}
	return ExtPool{}, false
}

// ExtendedPairsTouched scans a flat log list and returns the set of extended
// (multi-DEX) pools whose price/reserves changed, de-duplicated, in first-touch
// order. It is the extended counterpart of WatchedPairsTouched: it fires on
// V2+Biswap+V3 swaps, so the intra-block detector's per-swap trigger covers the
// full verified watch set the graph already evaluates.
func ExtendedPairsTouched(logs []*types.Log) []ExtPool {
	seen := make(map[common.Address]bool)
	var out []ExtPool
	for _, l := range logs {
		if p, ok := IsExtendedWatchedSwapLog(l); ok && !seen[p.Pair] {
			seen[p.Pair] = true
			out = append(out, p)
		}
	}
	return out
}

// IsRouterSwapTx is the cheap pre-filter for LIVE mempool mode: it reports
// whether a pending tx targets the PancakeRouter with a known swap selector, and
// returns the selector's function name. This is ONLY a pre-filter — confirm with
// the log detector after simulation, since most volume routes elsewhere.
func IsRouterSwapTx(tx *types.Transaction) (string, bool) {
	if tx == nil || tx.To() == nil || *tx.To() != PancakeV2Router {
		return "", false
	}
	data := tx.Data()
	if len(data) < 4 {
		return "", false
	}
	var sel [4]byte
	copy(sel[:], data[:4])
	name, ok := routerSwapSelectors[sel]
	return name, ok
}
