// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// sandwich_any.go is the ANY-POOL extension of the ground-truth sandwich
// valuator. The fixed-watch-set valuator (sandwich.go) only sandwiches the 12
// major deep pools, where sandwiching is correctly UNPROFITABLE (the victim's
// price impact is dwarfed by the 2x round-trip fee). Real BSC sandwich MEV lives
// in the LONG-TAIL thin/volatile pools (memecoins, new listings) with ARBITRARY
// tokens. To value it we must sandwich the VICTIM'S ACTUAL pool, on ANY pool,
// with ANY tokens — not a pre-registered watch set.
//
// Three capabilities are added here, all verified on-node (read-only eth_call +
// stateDiff overrides) in the research:
//
//  1. DYNAMIC TOKEN FUNDING — probe a token's balanceOf/allowance storage slots
//     at runtime (sentinel-write + eth_call match, slots 0..29) so the synthetic
//     attacker can be funded for ANY plain-mapping BEP20. Tokens whose slots
//     can't be found (proxies, packed/Vyper layouts, fee-on-transfer) are
//     notFundable -> skipped.
//
//  2. RUNTIME POOL METADATA — read token0()/token1()/getReserves() (V2) or
//     fee()/slot0() (V3) for ANY emitter at runtime, cached per pool, instead of
//     looking the pool up in the static registry.
//
//  3. DIRECT V2 pair.swap — sandwich the victim's ACTUAL V2 pair with no router:
//     fund the pair by crediting balanceOf[pair][tokenIn] += amountIn (== an
//     ERC20 transfer to the pair), then call pair.swap(amount0Out, amount1Out,
//     attacker, 0x). The amountOut is computed K-safely (under-quote by 1 wei) so
//     the pair's k-invariant never reverts ("Pancake: K"). Pancake V3 victims
//     reuse the existing router path; other V3 forks are skipped (unsupported).
//
// Strictly read-only: every probe/sandwich runs on a throwaway state.Copy that is
// discarded. Nothing is ever committed or submitted.
package simengine

import (
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/strategy"
	"github.com/holiman/uint256"
)

// ---------------------------------------------------------------------------
// ERC20 / V2 pair / V3 pool selectors (verified on-node).
// ---------------------------------------------------------------------------

var (
	selBalanceOf = []byte{0x70, 0xa0, 0x82, 0x31} // balanceOf(address)
	selToken0    = []byte{0x0d, 0xfe, 0x16, 0x81} // token0()
	selToken1    = []byte{0xd2, 0x12, 0x20, 0xa7} // token1()
	selSwapFee   = []byte{0x54, 0xcf, 0x2a, 0xeb} // swapFee()  (Biswap fork getter)
	selFactory   = []byte{0xc4, 0x5a, 0x01, 0x55} // factory()
	selFee       = []byte{0xdd, 0xca, 0x3f, 0x43} // fee()      (V3 fee tier)
	selPairSwap  = []byte{0x02, 0x2c, 0x0d, 0x9f} // swap(uint256,uint256,address,bytes)
)

// probeSlotMax is the highest storage base-slot index probed for a token's
// balanceOf mapping (and, derived from it, allowance). On-node research showed
// long-tail tokens place balanceOf at small indices (0..2); >30 has rapidly
// diminishing returns and proxy/packed/Vyper layouts never match a plain mapping
// at any small index regardless.
const probeSlotMax = 30

// slotProbeSentinel is the recognisable value written to a candidate balanceOf
// slot; we then call balanceOf(holder) and a match identifies the slot.
var slotProbeSentinel = common.HexToHash("0x00000000000000000000000000000000000000000000000000000000deadbeef")

// ---------------------------------------------------------------------------
// Known-fork V2 fee factors (factory -> gamma), used when a pool exposes no
// swapFee() getter so we can pick the EXACT (or a safely-higher) fee.
// ---------------------------------------------------------------------------

var (
	factoryPancakeV2 = common.HexToAddress("0xca143ce32fe78f1f7019d7d551a6402fc5350c73")
	factoryBiswap    = common.HexToAddress("0x858e3312ed3a876947ea49d572a7c42de08af7ee")
	factoryApeSwap   = common.HexToAddress("0xbcfccbde45ce874adcb698cc183debcf17952812")
)

// gammaGenericV2 is the SAFE upper-bound V2 fork fee (0.30% => 997/1000). On-node
// proof: assuming a fee >= the true fee under-quotes amountOut and is always
// k-safe; assuming a fee that is too LOW over-quotes and reverts "K". So when a
// pool exposes no fee getter and its factory is unknown we default to 0.30%.
var gammaGenericV2 = strategy.Gamma{Num: big.NewInt(997), Den: big.NewInt(1000)}

// ---------------------------------------------------------------------------
// Per-token slot cache (thread-safe; seeded with the verified known slots).
// ---------------------------------------------------------------------------

// tokenSlotResult is a cached probe outcome for one token.
type tokenSlotResult struct {
	slots    tokenSlots
	fundable bool
}

// tokenSlotCache memoises the balanceOf/allowance slot probe per token so the
// (state.Copy-heavy) probe runs at most once per token across the whole run.
type tokenSlotCache struct {
	mu sync.Mutex
	m  map[common.Address]tokenSlotResult
}

// globalTokenSlotCache is the process-wide token-slot cache, seeded with the
// verified WBNB/USDT/USDC slots so those tokens never incur a probe.
var globalTokenSlotCache = func() *tokenSlotCache {
	c := &tokenSlotCache{m: make(map[common.Address]tokenSlotResult)}
	for tok, sl := range knownTokenSlots {
		c.m[tok] = tokenSlotResult{slots: sl, fundable: true}
	}
	return c
}()

// get returns a cached probe result and whether it is present.
func (c *tokenSlotCache) get(token common.Address) (tokenSlotResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.m[token]
	return r, ok
}

// put stores a probe result.
func (c *tokenSlotCache) put(token common.Address, r tokenSlotResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[token] = r
}

// ---------------------------------------------------------------------------
// Dynamic balanceOf/allowance slot probing.
// ---------------------------------------------------------------------------

// readAddressWord packs a single address argument into balanceOf-style calldata
// (selector ++ leftPad32(addr)).
func sel1Addr(selector []byte, addr common.Address) []byte {
	d := make([]byte, 0, 4+32)
	d = append(d, selector...)
	d = append(d, leftPad32(addr.Bytes())...)
	return d
}

// probeTokenSlots discovers a token's balanceOf base slot by writing a sentinel
// to keccak256(leftPad32(holder) ++ u256(s)) for s in 0..probeSlotMax on a
// THROWAWAY Copy and calling balanceOf(holder) via EthCall until the sentinel is
// returned. The allowance base slot of standard BEP20 tokens (OpenZeppelin/WETH9)
// is the balanceOf slot + 1; we verify that assumption with a second sentinel
// write to the allowance double-hash key and only accept it if it matches, else
// fall back to scanning. Returns (slots, true) when fundable.
//
// Strictly read-only: each candidate write happens on a fresh base.Copy() that is
// discarded after the single EthCall.
func (e *SimEngine) probeTokenSlots(base *state.StateDB, cc simChainContext, hdr *types.Header, token common.Address) (tokenSlots, bool) {
	holder := sandwichAttacker
	balCall := sel1Addr(selBalanceOf, holder)

	balSlot := int64(-1)
	for s := int64(0); s <= probeSlotMax; s++ {
		sdb := base.Copy()
		sdb.SetState(token, balanceOfKey(holder, s), slotProbeSentinel)
		ret, err := e.EthCall(sdb, cc, hdr, token, balCall, 0)
		if err == nil && len(ret) == 32 && common.BytesToHash(ret) == slotProbeSentinel {
			balSlot = s
			break
		}
	}
	if balSlot < 0 {
		return tokenSlots{}, false // unfundable: proxy/packed/fee-on-transfer/Vyper.
	}

	// Determine the allowance base slot. Standard layouts place it immediately
	// after balanceOf; verify with a sentinel on the double-hash key. If that
	// (cheap) guess fails, scan the full range.
	allowSlot := probeAllowanceSlot(e, base, cc, hdr, token, balSlot)
	if allowSlot < 0 {
		// Allowance slot not found. balanceOf alone is enough for the DIRECT
		// pair.swap path (which never needs an allowance — it credits the pair's
		// balance directly), so still report fundable with allowSlot = balSlot+1
		// as a harmless default; the direct path ignores it.
		allowSlot = balSlot + 1
	}
	return tokenSlots{balSlot: balSlot, allowSlot: allowSlot}, true
}

// probeAllowanceSlot finds a token's allowance base slot by sentinel-matching
// allowance(owner, spender). It first tries balSlot+1 (the standard layout) then
// scans 0..probeSlotMax. Returns -1 when no slot matches. owner=attacker,
// spender=attacker keeps the probe self-contained (the value read is whatever we
// wrote at the double-hash key).
func probeAllowanceSlot(e *SimEngine, base *state.StateDB, cc simChainContext, hdr *types.Header, token common.Address, balSlot int64) int64 {
	owner := sandwichAttacker
	spender := sandwichAttacker
	selAllowance := []byte{0xdd, 0x62, 0xed, 0x3e} // allowance(address,address)
	call := make([]byte, 0, 4+64)
	call = append(call, selAllowance...)
	call = append(call, leftPad32(owner.Bytes())...)
	call = append(call, leftPad32(spender.Bytes())...)

	try := func(s int64) bool {
		sdb := base.Copy()
		sdb.SetState(token, allowanceKey(owner, spender, s), slotProbeSentinel)
		ret, err := e.EthCall(sdb, cc, hdr, token, call, 0)
		return err == nil && len(ret) == 32 && common.BytesToHash(ret) == slotProbeSentinel
	}

	if try(balSlot + 1) {
		return balSlot + 1
	}
	for s := int64(0); s <= probeSlotMax; s++ {
		if s == balSlot+1 {
			continue
		}
		if try(s) {
			return s
		}
	}
	return -1
}

// resolveTokenSlots returns a token's storage slots, probing+caching on a miss.
// notFundable=false means the token cannot be funded (skip the pool, count it).
func (e *SimEngine) resolveTokenSlots(base *state.StateDB, cc simChainContext, hdr *types.Header, token common.Address) (tokenSlots, bool) {
	if r, ok := globalTokenSlotCache.get(token); ok {
		return r.slots, r.fundable
	}
	slots, ok := e.probeTokenSlots(base, cc, hdr, token)
	globalTokenSlotCache.put(token, tokenSlotResult{slots: slots, fundable: ok})
	return slots, ok
}

// ---------------------------------------------------------------------------
// Dynamic funding (storage writes on a Copy), any token.
// ---------------------------------------------------------------------------

// fundAttackerDyn funds the synthetic attacker on statedb to spend `amount` of
// `token`, using the dynamically-resolved (cached) storage slots: it sets
// balanceOf[attacker] = amount, allowance[attacker][spender] = MAX (only if a
// real spender is given — the direct pair.swap path passes the zero address and
// skips the allowance), and a BNB gas budget. Returns false (notFundable) when
// the token's slots can't be resolved.
func (e *SimEngine) fundAttackerDyn(statedb *state.StateDB, cc simChainContext, hdr *types.Header, token, spender common.Address, amount *big.Int) (ok bool) {
	slots, fundable := e.resolveTokenSlots(statedb, cc, hdr, token)
	if !fundable {
		return false
	}
	statedb.SetState(token, balanceOfKey(sandwichAttacker, slots.balSlot), common.BigToHash(amount))
	if (spender != common.Address{}) {
		statedb.SetState(token, allowanceKey(sandwichAttacker, spender, slots.allowSlot), maxUint256Hash)
	}
	statedb.SetBalance(sandwichAttacker, uint256.MustFromBig(attackerGasBudgetWei), tracing.BalanceChangeUnspecified)
	return true
}

// creditPairBalance credits balanceOf[pair][token] += amount on statedb — exactly
// what an ERC20 transfer(pair, amount) would do, but without invoking the token
// (so it is fee-on-transfer-immune on the INPUT side: the pair's k-check reads its
// own actual balance vs the stored reserve). Used by the direct pair.swap path to
// pay the pool. Returns false if the token's balance slot can't be resolved.
func (e *SimEngine) creditPairBalance(statedb *state.StateDB, cc simChainContext, hdr *types.Header, pair, token common.Address, amount *big.Int) bool {
	slots, fundable := e.resolveTokenSlots(statedb, cc, hdr, token)
	if !fundable {
		return false
	}
	key := balanceOfKey(pair, slots.balSlot)
	cur := new(big.Int).SetBytes(statedb.GetState(token, key).Bytes())
	cur.Add(cur, amount)
	statedb.SetState(token, key, common.BigToHash(cur))
	return true
}

// dynAttackerTokenBalance reads balanceOf[attacker] for `token` at its resolved
// slot. Returns (0,false) when the slot can't be resolved.
func (e *SimEngine) dynAttackerTokenBalance(statedb *state.StateDB, cc simChainContext, hdr *types.Header, token common.Address) (*big.Int, bool) {
	slots, fundable := e.resolveTokenSlots(statedb, cc, hdr, token)
	if !fundable {
		return big.NewInt(0), false
	}
	word := statedb.GetState(token, balanceOfKey(sandwichAttacker, slots.balSlot))
	return new(big.Int).SetBytes(word[:]), true
}

// ---------------------------------------------------------------------------
// Runtime pool metadata (any emitter), cached.
// ---------------------------------------------------------------------------

// anyPool is the runtime-resolved metadata for a victim's actual emitter pool.
// For V2 it carries the fee factor; for V3 it carries the fee tier and the
// supported flag (only Pancake V3 is sandwichable through the existing router).
type anyPool struct {
	pair        common.Address
	token0      common.Address
	token1      common.Address
	isV3        bool
	gamma       strategy.Gamma // V2 fee factor (1-fee)
	feeTier     uint32         // V3 fee tier (hundredths of a bip)
	v3Supported bool           // V3 only: true iff Pancake V3 (router path works)
	ok          bool           // metadata fully resolved
}

// poolMetaCache memoises anyPool metadata per emitter address.
type poolMetaCache struct {
	mu sync.Mutex
	m  map[common.Address]anyPool
}

var globalPoolMetaCache = &poolMetaCache{m: make(map[common.Address]anyPool)}

func (c *poolMetaCache) get(pair common.Address) (anyPool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.m[pair]
	return p, ok
}

func (c *poolMetaCache) put(pair common.Address, p anyPool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[pair] = p
}

// callAddr executes a no-arg getter returning a single address word, read-only.
func (e *SimEngine) callAddr(base *state.StateDB, cc simChainContext, hdr *types.Header, to common.Address, selector []byte) (common.Address, bool) {
	ret, err := e.EthCall(base, cc, hdr, to, selector, 0)
	if err != nil || len(ret) < 32 {
		return common.Address{}, false
	}
	return common.BytesToAddress(ret[12:32]), true
}

// callUint executes a no-arg getter returning a single uint word, read-only.
func (e *SimEngine) callUint(base *state.StateDB, cc simChainContext, hdr *types.Header, to common.Address, selector []byte) (*big.Int, bool) {
	ret, err := e.EthCall(base, cc, hdr, to, selector, 0)
	if err != nil || len(ret) < 32 {
		return nil, false
	}
	return new(big.Int).SetBytes(ret[0:32]), true
}

// resolvePoolMeta reads an emitter pool's metadata at runtime (token0/token1 plus
// either the V2 fee or the V3 fee tier) and caches it. isV3Hint comes from the
// decoded Swap topic (V2 vs V3). For V2 it determines the fee factor by:
//  1. swapFee() if present (Biswap-style; value in 0.1% units -> (1000-fee)/1000),
//  2. else factory() mapped to a known fork fee (Pancake 0.25%),
//  3. else the generic 0.30% safe upper bound.
//
// For V3 it reads fee() (the tier) and marks v3Supported iff the pool's factory
// is the Pancake V3 factory — only then does the existing SwapRouter path work.
func (e *SimEngine) resolvePoolMeta(base *state.StateDB, cc simChainContext, hdr *types.Header, pair common.Address, isV3Hint bool) (anyPool, bool) {
	if p, ok := globalPoolMetaCache.get(pair); ok {
		return p, p.ok
	}
	p := anyPool{pair: pair, isV3: isV3Hint}

	t0, ok0 := e.callAddr(base, cc, hdr, pair, selToken0)
	t1, ok1 := e.callAddr(base, cc, hdr, pair, selToken1)
	if !ok0 || !ok1 || (t0 == common.Address{}) || (t1 == common.Address{}) {
		globalPoolMetaCache.put(pair, p) // p.ok == false
		return p, false
	}
	p.token0, p.token1 = t0, t1

	if isV3Hint {
		feeTier, okf := e.callUint(base, cc, hdr, pair, selFee)
		if !okf || feeTier.Sign() <= 0 || feeTier.BitLen() > 24 {
			globalPoolMetaCache.put(pair, p)
			return p, false
		}
		p.feeTier = uint32(feeTier.Uint64())
		p.gamma = gammaForFeeTierAny(p.feeTier)
		// Only Pancake V3 pools are sandwichable through the existing SwapRouter.
		if fac, okfa := e.callAddr(base, cc, hdr, pair, selFactory); okfa {
			p.v3Supported = (fac == pancakeV3Factory)
		}
		p.ok = true
		globalPoolMetaCache.put(pair, p)
		return p, true
	}

	// V2 fee determination.
	p.gamma = e.resolveV2Gamma(base, cc, hdr, pair)
	p.ok = true
	globalPoolMetaCache.put(pair, p)
	return p, true
}

// pancakeV3Factory is the PancakeSwap V3 factory; only pools from it are
// sandwichable through pancakeV3SwapRouter.
var pancakeV3Factory = common.HexToAddress("0x0BFbCF9fa4f9C56B0F40a671Ad40E0805A091865")

// resolveV2Gamma determines a V2-style pair's fee factor with the K-safe rule:
// swapFee() getter if present, else map factory() to a known fork fee, else the
// generic 0.30% safe upper bound.
func (e *SimEngine) resolveV2Gamma(base *state.StateDB, cc simChainContext, hdr *types.Header, pair common.Address) strategy.Gamma {
	if fee, ok := e.callUint(base, cc, hdr, pair, selSwapFee); ok && fee.Sign() > 0 && fee.Cmp(big.NewInt(1000)) < 0 {
		// Biswap-style: fee is in 0.1% units; gamma = (1000-fee)/1000.
		return strategy.Gamma{Num: new(big.Int).Sub(big.NewInt(1000), fee), Den: big.NewInt(1000)}
	}
	if fac, ok := e.callAddr(base, cc, hdr, pair, selFactory); ok {
		switch fac {
		case factoryPancakeV2:
			return strategy.GammaPancakeV2 // 9975/10000 (0.25%)
		case factoryBiswap:
			return strategy.GammaBiswapV2 // 998/1000 (0.20%)
		case factoryApeSwap:
			return gammaGenericV2 // ApeSwap = 0.20% real, but 0.30% is the safe upper bound
		}
	}
	return gammaGenericV2 // unknown fork: 0.30% safe upper bound.
}

// gammaForFeeTierAny mirrors strategy.gammaForFeeTier (unexported there) for the
// any-pool path: gamma = (1e6 - feeTier)/1e6.
func gammaForFeeTierAny(feeTier uint32) strategy.Gamma {
	return strategy.Gamma{
		Num: big.NewInt(int64(1_000_000 - feeTier)),
		Den: big.NewInt(1_000_000),
	}
}

// ---------------------------------------------------------------------------
// Numeraire (single-unit denomination).
//
// The attacker's gross/net MUST be measured in a single, comparable unit so the
// BNB-wei net gate (gross - gasBNB - flashFeeBNB - bid) is meaningful. We pick a
// NUMERAIRE among the victim pool's two tokens — WBNB or a known stable
// (USDT/USDC) — and sandwich the pool denominated in it: the synthetic attacker
// starts and ends holding the NUMERAIRE, so the measured gross is natively in
// WBNB or a stable. A pool whose BOTH tokens are non-numeraire (token/token
// memecoin pool) cannot be valued and is SKIPPED (skippedNoNumeraire) — this is
// what removes the cross-unit garbage that produced the trillion-BNB artifacts.
// ---------------------------------------------------------------------------

// numeraireKind classifies a token for denomination.
type numeraireKind uint8

const (
	numNone   numeraireKind = iota // not a recognised numeraire.
	numWBNB                        // WBNB: already BNB; 1:1.
	numStable                      // USDT/USDC: ~$1, converted to BNB via live spot.
)

// numeraireOf returns the numeraire classification of a token.
func numeraireOf(token common.Address) numeraireKind {
	switch token {
	case strategy.WBNB:
		return numWBNB
	case strategy.USDT, strategy.USDC:
		return numStable
	default:
		return numNone
	}
}

// poolNumeraire returns the numeraire SIDE of a pool (the token that is WBNB or a
// stable) and its kind. If both sides are numeraires WBNB wins (it is the native
// unit). If NEITHER side is a numeraire it returns (zero, numNone, false) and the
// caller must skip the pool (skippedNoNumeraire). A nil-metadata pool is treated
// as no-numeraire rather than dereferenced.
func poolNumeraire(p anyPool) (numToken common.Address, kind numeraireKind, ok bool) {
	if !p.ok {
		return common.Address{}, numNone, false
	}
	k0 := numeraireOf(p.token0)
	k1 := numeraireOf(p.token1)
	switch {
	case k0 == numWBNB:
		return p.token0, numWBNB, true
	case k1 == numWBNB:
		return p.token1, numWBNB, true
	case k0 == numStable:
		return p.token0, numStable, true
	case k1 == numStable:
		return p.token1, numStable, true
	default:
		return common.Address{}, numNone, false
	}
}

// numeraireToBNB converts a numeraire-denominated wei amount to BNB wei:
//   - WBNB: identity (already BNB, 18dp).
//   - stable (USDT/USDC, 18dp on BSC): BNB = stableWei / wbnbPriceUSD, since
//     wbnbPriceUSD is USD per 1 WBNB and the stable is ~$1; both are 18dp so the
//     decimals cancel.
//
// Returns 0 for a non-positive amount or (for a stable) a non-positive price.
func numeraireToBNB(amount *big.Int, kind numeraireKind, wbnbPriceUSD float64) *big.Int {
	if amount == nil || amount.Sign() <= 0 {
		return big.NewInt(0)
	}
	switch kind {
	case numWBNB:
		return new(big.Int).Set(amount)
	case numStable:
		if wbnbPriceUSD <= 0 {
			return big.NewInt(0)
		}
		// bnbWei = stableWei / priceUSD (both 18dp).
		bnb := new(big.Float).Quo(new(big.Float).SetInt(amount), big.NewFloat(wbnbPriceUSD))
		out, _ := bnb.Int(nil)
		if out == nil || out.Sign() < 0 {
			return big.NewInt(0)
		}
		return out
	default:
		return big.NewInt(0)
	}
}

// ---------------------------------------------------------------------------
// Direct V2 pair.swap (no router) — calldata + K-safe amountOut.
// ---------------------------------------------------------------------------

// encodePairSwap builds swap(amount0Out, amount1Out, to, bytes data="") calldata
// for a V2 pair (selector 0x022c0d9f). Exactly one of amount0Out/amount1Out is
// non-zero (the token the attacker BUYS). Empty data => no flash callback.
func encodePairSwap(amount0Out, amount1Out *big.Int, to common.Address) []byte {
	d := make([]byte, 0, 4+32*5)
	d = append(d, selPairSwap...)
	d = append(d, leftPad32(amount0Out.Bytes())...)       // amount0Out
	d = append(d, leftPad32(amount1Out.Bytes())...)       // amount1Out
	d = append(d, leftPad32(to.Bytes())...)               // to = attacker
	d = append(d, leftPad32(big.NewInt(0x80).Bytes())...) // data offset (4 head words)
	d = append(d, make([]byte, 32)...)                    // data.len = 0
	return d
}

// ksafeAmountOut computes the V2 amountOut for the direct pair.swap, then shaves 1
// wei so floor-division rounding can never make the pair's k-invariant fail
// ("Pancake: K"). On-node proof: exact GetAmountOut succeeds; over-quote reverts;
// any under-quote (incl. -1 wei) succeeds, leaving a negligible amount unclaimed.
func ksafeAmountOut(amountIn, reserveIn, reserveOut *big.Int, g strategy.Gamma) *big.Int {
	out := strategy.GetAmountOut(amountIn, reserveIn, reserveOut, g)
	if out.Sign() <= 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Sub(out, big.NewInt(1)) // shave 1 wei (K-safety cushion).
}

// ---------------------------------------------------------------------------
// V2 direct-pair swap leg (state-mutating, no router).
// ---------------------------------------------------------------------------

// directPairSwap executes ONE leg of a sandwich against the victim's ACTUAL V2
// pair (no router): it credits the pair's tokenIn balance by amountIn (== an
// ERC20 transfer to the pair), computes a K-safe amountOut from the pre-leg
// reserves with the pool's gamma, and calls pair.swap(amount0Out, amount1Out,
// attacker, 0x) as the attacker. State-MUTATING (reuses applyRouterSwap's
// Prepare/Finalise bracketing) so the price move persists for the next leg.
//
// Returns the amountOut credited to the attacker, or err on revert / 0 quote.
func (e *SimEngine) directPairSwap(sdb *state.StateDB, cc simChainContext, hdr *types.Header, pool anyPool, tokenIn common.Address, amountIn *big.Int) (amountOut *big.Int, err error) {
	zero := big.NewInt(0)
	// Read current reserves directly from slot 8 (V2 packed layout).
	rv := strategy.ReadReserves(sdb, pool.pair)
	if rv.Reserve0 == nil || rv.Reserve1 == nil || rv.Reserve0.Sign() <= 0 || rv.Reserve1.Sign() <= 0 {
		return zero, errNoReserves
	}
	var reserveIn, reserveOut *big.Int
	var amount0Out, amount1Out *big.Int
	switch tokenIn {
	case pool.token0:
		reserveIn, reserveOut = rv.Reserve0, rv.Reserve1
		amount1Out = ksafeAmountOut(amountIn, reserveIn, reserveOut, pool.gamma) // buying token1
		amount0Out = big.NewInt(0)
		if amount1Out.Sign() <= 0 {
			return zero, errZeroQuote
		}
	case pool.token1:
		reserveIn, reserveOut = rv.Reserve1, rv.Reserve0
		amount0Out = ksafeAmountOut(amountIn, reserveIn, reserveOut, pool.gamma) // buying token0
		amount1Out = big.NewInt(0)
		if amount0Out.Sign() <= 0 {
			return zero, errZeroQuote
		}
	default:
		return zero, errTokenNotInPool
	}

	// Pay the pair: credit balanceOf[pair][tokenIn] += amountIn (transfer to pair).
	if !e.creditPairBalance(sdb, cc, hdr, pool.pair, tokenIn, amountIn) {
		return zero, errNotFundable
	}

	calldata := encodePairSwap(amount0Out, amount1Out, sandwichAttacker)
	if _, err := e.applyRouterSwap(sdb, cc, hdr, pool.pair, calldata, sandwichGasCap); err != nil {
		return zero, err
	}
	if amount0Out.Sign() > 0 {
		return amount0Out, nil
	}
	return amount1Out, nil
}

// Sentinel errors for the direct-pair path (diagnostics only).
var (
	errNoReserves     = newSandwichErr("pair reserves unavailable")
	errZeroQuote      = newSandwichErr("amountOut quote is zero")
	errTokenNotInPool = newSandwichErr("tokenIn not in pool")
	errNotFundable    = newSandwichErr("token not fundable (slot unresolved)")
)

type sandwichErr struct{ s string }

func (e sandwichErr) Error() string       { return e.s }
func newSandwichErr(s string) sandwichErr { return sandwichErr{s: s} }

// ---------------------------------------------------------------------------
// Ground-truth ANY-POOL sandwich profit (3-step on a fresh Copy).
// ---------------------------------------------------------------------------

// sandwichProfitAny measures the GROUND-TRUTH gross sandwich profit of
// frontrunning `victimTx` on the victim's ACTUAL pool with a frontrun of
// `frontrunIn`, on a FRESH Copy of preState. The attacker SPENDS and RECOVERS
// `tokenIn`, so the measured gross is denominated in `tokenIn` — the caller passes
// the pool's NUMERAIRE (WBNB or a stable) so the gross is natively in a single,
// comparable unit (never an arbitrary memecoin). It mirrors sandwichProfit's fund
// -> FRONTRUN -> VICTIM -> BACKRUN sequence and the same fresh-Copy /
// no-snapshot-across-ApplyTransaction invariant, but routes the attacker legs
// through:
//
//   - V2 victim: DIRECT pair.swap on the emitter pool (any fork; pool.gamma).
//   - Pancake V3 victim: the existing Pancake V3 SwapRouter (pool.feeTier).
//   - other V3 fork: unsupported -> ok=false (counted by the caller).
//
// gross = attacker tokenIn-balance delta over the 3 steps (numeraire wei). ok=false
// means infeasible (token not fundable, a leg reverted, the victim breached its
// amountOutMin, or the gross exceeded the pool's reserve — a bug guard).
func (e *SimEngine) sandwichProfitAny(preState *state.StateDB, cc simChainContext, hdr *types.Header, victimTx *types.Transaction, pool anyPool, tokenIn common.Address, frontrunIn *big.Int) (gross *big.Int, gasUnits uint64, ok bool) {
	zero := big.NewInt(0)
	if preState == nil || hdr == nil || victimTx == nil || frontrunIn == nil || frontrunIn.Sign() <= 0 || !pool.ok {
		return zero, 0, false
	}
	tokenOut, hasOther := poolOther(pool, tokenIn)
	if !hasOther {
		return zero, 0, false
	}
	gasUnits = strategy.SandwichGasUnits(pool.isV3)

	// V3: only Pancake V3 is sandwichable (existing router). Others are skipped.
	if pool.isV3 {
		if !pool.v3Supported {
			return zero, 0, false
		}
		return e.sandwichProfitV3Router(preState, cc, hdr, victimTx, pool, tokenIn, tokenOut, frontrunIn)
	}

	// V2 direct-pair path.
	sdb := preState.Copy()

	// Pre-balance of X: we credit the attacker with frontrunIn of X so profit is
	// measured net of the borrowed notional (gross = recovered - borrowed). The
	// frontrun pays the pair from a SEPARATE credit (directPairSwap), so the
	// attacker's own X balance starts at frontrunIn and ends at frontrunIn + gross.
	if !e.fundAttackerDyn(sdb, cc, hdr, tokenIn, common.Address{}, frontrunIn) {
		return zero, 0, false
	}
	preX, okp := e.dynAttackerTokenBalance(sdb, cc, hdr, tokenIn)
	if !okp {
		return zero, 0, false
	}

	// 1. FRONTRUN: buy Y with frontrunIn of X on the actual pair. directPairSwap
	// pays the pair (credits balanceOf[pair][X] += frontrunIn — a transfer to the
	// pair) and the pair's swap transfers yFront of Y to the attacker (a REAL EVM
	// write to balanceOf[attacker][Y]). The attacker's own X is NOT touched by the
	// swap (the pair was paid from the separate credit), so to make the final
	// X-delta the true gross we debit the attacker's X by frontrunIn here (the
	// borrowed notional it spent). post-frontrun: attacker X = preX - frontrunIn.
	yFront, err := e.directPairSwap(sdb, cc, hdr, pool, tokenIn, frontrunIn)
	if err != nil || yFront.Sign() <= 0 {
		return zero, 0, false
	}
	if !e.debitAttackerToken(sdb, cc, hdr, tokenIn, frontrunIn) {
		return zero, 0, false
	}

	// 2. VICTIM (real tx on the frontrun-mutated copy).
	if !e.applyVictimTx(sdb, cc, hdr, victimTx) {
		return zero, 0, false
	}

	// 3. BACKRUN: sell exactly yFront of Y back to X on the same pair. directPairSwap
	// credits balanceOf[pair][Y] += yFront (paying the pair from a separate credit),
	// but the attacker ACTUALLY holds that Y from the frontrun, so it must LEAVE the
	// attacker: debit the attacker's Y by yFront. The pair's swap then transfers
	// xBack of X to the attacker (a REAL EVM write to balanceOf[attacker][X]).
	if !e.debitAttackerToken(sdb, cc, hdr, tokenOut, yFront) {
		return zero, 0, false
	}
	xBack, err := e.directPairSwap(sdb, cc, hdr, pool, tokenOut, yFront)
	if err != nil || xBack.Sign() <= 0 {
		return zero, 0, false
	}

	// 4. PROFIT: attacker X delta. post-backrun attacker X = (preX - frontrunIn) +
	// xBack; gross = postX - preX = xBack - frontrunIn (recovered minus borrowed).
	postX, okp2 := e.dynAttackerTokenBalance(sdb, cc, hdr, tokenIn)
	if !okp2 {
		return zero, 0, false
	}
	gross = new(big.Int).Sub(postX, preX)

	// Bug guard: a gross exceeding the pool's reserve of the spent token X is
	// physically impossible (the attacker cannot extract more X than the pool
	// holds) and signals a unit/feasibility artifact — reject it as infeasible.
	if reserveX := poolReserveOf(sdb, pool, tokenIn); reserveX != nil && reserveX.Sign() > 0 && gross.Cmp(reserveX) > 0 {
		return zero, 0, false
	}
	return gross, gasUnits, true
}

// poolReserveOf returns the pool's current reserve of `token` (V2 packed slot 8).
// Returns nil for a V3 pool (no packed reserves) or when `token` is not in the
// pool. nil-safe on `pool` fields.
func poolReserveOf(sdb *state.StateDB, pool anyPool, token common.Address) *big.Int {
	if pool.isV3 {
		return nil
	}
	rv := strategy.ReadReserves(sdb, pool.pair)
	if rv.Reserve0 == nil || rv.Reserve1 == nil {
		return nil
	}
	switch token {
	case pool.token0:
		return rv.Reserve0
	case pool.token1:
		return rv.Reserve1
	default:
		return nil
	}
}

// debitAttackerToken subtracts amount from balanceOf[attacker][token] (clamped at
// 0). Used to charge the attacker's borrowed X spent on the frontrun.
func (e *SimEngine) debitAttackerToken(sdb *state.StateDB, cc simChainContext, hdr *types.Header, token common.Address, amount *big.Int) bool {
	cur, ok := e.dynAttackerTokenBalance(sdb, cc, hdr, token)
	if !ok {
		return false
	}
	next := new(big.Int).Sub(cur, amount)
	if next.Sign() < 0 {
		next = big.NewInt(0)
	}
	return e.setAttackerToken(sdb, cc, hdr, token, next)
}

// setAttackerToken writes balanceOf[attacker][token] = value.
func (e *SimEngine) setAttackerToken(sdb *state.StateDB, cc simChainContext, hdr *types.Header, token common.Address, value *big.Int) bool {
	slots, fundable := e.resolveTokenSlots(sdb, cc, hdr, token)
	if !fundable {
		return false
	}
	sdb.SetState(token, balanceOfKey(sandwichAttacker, slots.balSlot), common.BigToHash(value))
	return true
}

// poolOther returns the other token of an anyPool, like ExtPool.Other.
func poolOther(p anyPool, t common.Address) (common.Address, bool) {
	switch t {
	case p.token0:
		return p.token1, true
	case p.token1:
		return p.token0, true
	default:
		return common.Address{}, false
	}
}

// sandwichAnyProbeDiag carries the per-step outcome of ONE any-pool 3-step probe
// at a fixed frontrun size, so the caller can log WHICH leg fails and why.
type sandwichAnyProbeDiag struct {
	frontrunIn *big.Int
	fundOk     bool
	frontrunOk bool
	yBought    *big.Int
	victimOk   bool
	backrunOk  bool
	gross      *big.Int
	reason     string // first failing step, or "ok"
}

// probeSandwichAnyDiag runs a single instrumented any-pool 3-step probe at
// frontrunIn and returns the step-by-step outcome. Read-only (fresh Copy). It
// covers the V2 direct-pair path; for V3 it defers to sandwichProfitAny and only
// reports the final gross/reason (the V3 router path is exercised in the
// fixed-set self-test).
func (e *SimEngine) probeSandwichAnyDiag(preState *state.StateDB, cc simChainContext, hdr *types.Header, victimTx *types.Transaction, pool anyPool, tokenIn common.Address, frontrunIn *big.Int) sandwichAnyProbeDiag {
	d := sandwichAnyProbeDiag{frontrunIn: frontrunIn, gross: big.NewInt(0)}
	if preState == nil || hdr == nil || victimTx == nil || frontrunIn == nil || frontrunIn.Sign() <= 0 || !pool.ok {
		d.reason = "bad args"
		return d
	}
	tokenOut, hasOther := poolOther(pool, tokenIn)
	if !hasOther {
		d.reason = "tokenIn not in pool"
		return d
	}

	if pool.isV3 {
		gr, _, ok := e.sandwichProfitAny(preState, cc, hdr, victimTx, pool, tokenIn, frontrunIn)
		d.gross = gr
		if ok {
			d.fundOk, d.frontrunOk, d.victimOk, d.backrunOk = true, true, true, true
			d.reason = "ok (v3 router path)"
		} else {
			d.reason = "v3 path infeasible (unsupported fork / leg revert / victim revert)"
		}
		return d
	}

	sdb := preState.Copy()
	if !e.fundAttackerDyn(sdb, cc, hdr, tokenIn, common.Address{}, frontrunIn) {
		d.reason = "fundAttacker failed (slot unresolved)"
		return d
	}
	d.fundOk = true
	preX, _ := e.dynAttackerTokenBalance(sdb, cc, hdr, tokenIn)

	yFront, err := e.directPairSwap(sdb, cc, hdr, pool, tokenIn, frontrunIn)
	if err != nil {
		d.reason = "frontrun revert: " + err.Error()
		return d
	}
	if !e.debitAttackerToken(sdb, cc, hdr, tokenIn, frontrunIn) {
		d.reason = "frontrun debit failed"
		return d
	}
	d.frontrunOk = true
	d.yBought = yFront
	if yFront.Sign() <= 0 {
		d.reason = "frontrun bought 0 Y"
		return d
	}

	vok, vreason := e.applyVictimTxDiag(sdb, cc, hdr, victimTx)
	if !vok {
		d.reason = vreason
		return d
	}
	d.victimOk = true

	if !e.debitAttackerToken(sdb, cc, hdr, tokenOut, yFront) {
		d.reason = "backrun debit failed"
		return d
	}
	xBack, berr := e.directPairSwap(sdb, cc, hdr, pool, tokenOut, yFront)
	if berr != nil {
		d.reason = "backrun revert: " + berr.Error()
		return d
	}
	d.backrunOk = true
	_ = xBack

	postX, _ := e.dynAttackerTokenBalance(sdb, cc, hdr, tokenIn)
	d.gross = new(big.Int).Sub(postX, preX)
	d.reason = "ok"
	return d
}

// ---------------------------------------------------------------------------
// ANY-POOL BACKRUN as a CROSS-POOL CYCLE (round-1 redesign).
//
// A backrun is NOT a single-pool round trip (numeraire->other->numeraire on the
// SAME pool): under constant-product fee dynamics (gamma < 1) that is ALWAYS a
// structural loss — the victim's price move is already baked into the post-victim
// reserves, so a round trip at those reserves eats both legs of fees with NO
// arbitrage capture, and the detector fires zero times. A TRUE backrun is a
// CROSS-POOL cycle: the victim moves the price on pool P; the arb exploits the
// resulting gap via a DIFFERENT pool P' that shares a token. We therefore value
// the backrun by REUSING the existing cyclic-arbitrage detector (strategy.
// NegativeCycles / CycleOptimum / ValueCycle) on the POST-VICTIM state, seeding
// the graph from the pools the victim TOUCHED plus the verified hub set.
// ---------------------------------------------------------------------------

// anyPoolToExtPool converts a resolved anyPool into the strategy.ExtPool the graph
// builder consumes. The two share token0/token1/gamma/feeTier/isV3 semantics; the
// DEX label is a best-effort tag for the opportunity logs. Returns ok=false for an
// unresolved pool. V3 pools that are not Pancake-V3 (no router path) are still
// included for DETECTION purposes (the cross-pool gap they reveal is real); sizing
// of any cycle that includes them is deferred to the EVM oracle exactly as the hub
// graph mode does.
func anyPoolToExtPool(p anyPool) (strategy.ExtPool, bool) {
	if !p.ok || (p.token0 == common.Address{}) || (p.token1 == common.Address{}) {
		return strategy.ExtPool{}, false
	}
	dex := strategy.DEXPancakeV2
	if p.isV3 {
		dex = strategy.DEXPancakeV3
	}
	return strategy.ExtPool{
		Name:     "any:" + poolLabel(p.pair),
		DEX:      dex,
		Pair:     p.pair,
		Token0:   p.token0,
		Token1:   p.token1,
		Gamma:    p.gamma,
		IsV3:     p.isV3,
		FeeTier:  p.feeTier,
		Verified: true, // explicit-slice builder never filters on Verified.
	}, true
}

// BuildAnyPoolGraph seeds a strategy.Graph from the union of (a) the pools the
// victim TOUCHED (resolved from the per-block touched-pool set) and (b) the
// verified hub set (strategy.ExtendedPools), de-duplicated by pair address. The
// graph is built on the supplied POST-VICTIM state so cross-pool cycles reflect
// the gap the victim opened. Returns the graph plus the combined pool slice (the
// latter so callers can look up an edge's pool metadata for gas/V3 routing). The
// resolved touched pools come from the caller (it already decoded the victim's
// Swap logs); this keeps BuildAnyPoolGraph pure over the supplied inputs.
func BuildAnyPoolGraph(postState *state.StateDB, touched []anyPool) (*strategy.Graph, []strategy.ExtPool) {
	seen := make(map[common.Address]bool)
	combined := make([]strategy.ExtPool, 0, len(touched)+12)

	for _, tp := range touched {
		ep, ok := anyPoolToExtPool(tp)
		if !ok || seen[ep.Pair] {
			continue
		}
		seen[ep.Pair] = true
		combined = append(combined, ep)
	}
	// Union with the verified hub set (cross-touched-pool cycles close via the hub
	// stables/WBNB). Dedup against pools the victim already contributed.
	for _, hp := range strategy.ExtendedPools() {
		if seen[hp.Pair] {
			continue
		}
		seen[hp.Pair] = true
		combined = append(combined, hp)
	}

	return strategy.BuildGraphFromPools(postState, combined), combined
}

// sandwichProfitV3Router values a Pancake V3 victim via the existing SwapRouter
// (exactInputSingle), reusing the known funding path (the attacker holds the
// tokens and approves the router). It mirrors sandwichProfit's V3 branch but with
// dynamic funding so arbitrary Pancake-V3 token pairs are covered.
func (e *SimEngine) sandwichProfitV3Router(preState *state.StateDB, cc simChainContext, hdr *types.Header, victimTx *types.Transaction, pool anyPool, tokenIn, tokenOut common.Address, frontrunIn *big.Int) (gross *big.Int, gasUnits uint64, ok bool) {
	zero := big.NewInt(0)
	gasUnits = strategy.SandwichGasUnits(true)
	router := pancakeV3SwapRouter
	sdb := preState.Copy()

	// Fund X (frontrun spend + router approval) and approve the router for Y.
	if !e.fundAttackerDyn(sdb, cc, hdr, tokenIn, router, frontrunIn) {
		return zero, 0, false
	}
	if !e.fundAttackerDyn(sdb, cc, hdr, tokenOut, router, big.NewInt(0)) {
		return zero, 0, false
	}
	preX, okp := e.dynAttackerTokenBalance(sdb, cc, hdr, tokenIn)
	if !okp {
		return zero, 0, false
	}

	front := encodeV3ExactInputSingle(tokenIn, tokenOut, pool.feeTier, frontrunIn)
	if _, err := e.applyRouterSwap(sdb, cc, hdr, router, front, sandwichGasCap); err != nil {
		return zero, 0, false
	}
	yBought, _ := e.dynAttackerTokenBalance(sdb, cc, hdr, tokenOut)
	if yBought.Sign() <= 0 {
		return zero, 0, false
	}
	if !e.applyVictimTx(sdb, cc, hdr, victimTx) {
		return zero, 0, false
	}
	back := encodeV3ExactInputSingle(tokenOut, tokenIn, pool.feeTier, yBought)
	if _, err := e.applyRouterSwap(sdb, cc, hdr, router, back, sandwichGasCap); err != nil {
		return zero, 0, false
	}
	postX, _ := e.dynAttackerTokenBalance(sdb, cc, hdr, tokenIn)
	gross = new(big.Int).Sub(postX, preX)
	return gross, gasUnits, true
}
