// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_realizability.go is the IN-BLOCK COUNTERFACTUAL (realizability) detector,
// selectable with SIMENGINE_DRYRUN=realizability. It rides on the SAME single
// block replay the any-pool sandwich detector uses and answers a different
// question: for each EX-POST net-positive sandwich opportunity our detector
// surfaces in a canonical block, did a REAL competitor ALREADY capture it in that
// same block? A captured opportunity is unavailable to us; an unmatched one is
// "left on the table" (still an ex-post upper bound). It then attributes the
// captured ones to a builder/validator-internal cluster, a recurrent external
// searcher, or unknown.
//
// THE GOVERNING RULE (applied at every ambiguous decision): over-counting
// `captured` understates the realizable surface and is the DANGEROUS direction, so
// a landed-MEV capture is declared only on STRONG, CONJUNCTIVE evidence (a bracket
// structure on the same pool AND a shared actor across the two legs AND a delta
// corroboration that the actor round-tripped flat in the volatile token and came
// out net-positive in the hub asset, net of gas and any coinbase bribe). Any weak
// or ambiguous signal leaves the opportunity on the table. False negatives in
// capture detection are acceptable; false positives are not.
//
// It does NOT touch applyOnState / SimulateOnState / selftest.go (the validated
// 5/5 receipt-exact path). It rides ONLY on ApplyOnStateHooked, exactly like the
// sandwich-any and intrablock modes. Strictly read-only (state.Copy() only, never
// commits), every block and per-victim evaluation wrapped in defer/recover, and a
// complete no-op unless SIMENGINE_DRYRUN=realizability.
package simengine

import (
	"math/big"
	"os"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/strategy"
)

// ---------------------------------------------------------------------------
// Env knobs (mirror the SIMENGINE_SANDWICH_* style).
// ---------------------------------------------------------------------------

// realizabilityConfig holds the realizability-specific knobs. The ex-post side
// reuses the SIMENGINE_SANDWICH_* / SIMENGINE_DRYRUN_GASPRICES knobs unchanged.
type realizabilityConfig struct {
	dustUSD   float64 // net-of-gas-and-bribe dust floor for declaring a landed capture (§5.3)
	flatPct   float64 // volatile-token round-trip flatness tolerance, percent (§5.3)
	amtEpsPct float64 // log-amount reconciliation epsilon, percent (§5.4)
	repeatMin uint64  // blocks-seen threshold for repeatedAddr classification (§6.3)
}

func defaultRealizabilityConfig() realizabilityConfig {
	c := realizabilityConfig{dustUSD: 1.0, flatPct: 0.5, amtEpsPct: 2.0, repeatMin: 3}
	if v := os.Getenv("SIMENGINE_RZ_DUSTUSD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			c.dustUSD = f
		}
	}
	if v := os.Getenv("SIMENGINE_RZ_FLATPCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			c.flatPct = f
		}
	}
	if v := os.Getenv("SIMENGINE_RZ_AMTEPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			c.amtEpsPct = f
		}
	}
	if v := os.Getenv("SIMENGINE_RZ_REPEAT_MIN"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			c.repeatMin = n
		}
	}
	return c
}

// realizabilityTopN bounds the topSenders / topBuilders leaderboard rendered in
// the tally line.
const realizabilityTopN = 5

// rzHubBUSDSlot is the BUSD balanceOf base slot (verified: classic OZ layout 1).
// knownTokenSlots already carries WBNB/USDT/USDC; BUSD is added here as a local
// hub-asset entry (the ex-post side never funds BUSD, so it is not in that table).
// A read at this slot for an address gives its BUSD balance; if it ever proves
// wrong the hub delta for BUSD simply looks zero and the opp falls to
// left-on-the-table (the safe direction).
const rzHubBUSDSlot int64 = 1

// hubAsset describes one hub (numeraire-equivalent) asset and how to read an
// address's balance of it. native==true means read GetBalance; otherwise read the
// ERC20 balanceOf storage slot directly.
type hubAsset struct {
	token  common.Address
	native bool
	slot   int64
	kind   numeraireKind // numWBNB (BNB/WBNB) or numStable (USDT/USDC/BUSD)
}

// rzHubAssets is the fixed hub-asset set: native BNB, WBNB, USDT, USDC, BUSD.
// Native BNB and WBNB are both numWBNB and are summed into a single BNB-equivalent
// term so wrap/unwrap is value-neutral (§4.2). All ERC20 hubs have a known
// balanceOf slot, so reading them never incurs a runtime probe and cannot fail.
var rzHubAssets = []hubAsset{
	{token: common.Address{}, native: true, kind: numWBNB},                          // native BNB
	{token: strategy.WBNB, slot: knownTokenSlots[strategy.WBNB].balSlot, kind: numWBNB},
	{token: strategy.USDT, slot: knownTokenSlots[strategy.USDT].balSlot, kind: numStable},
	{token: strategy.USDC, slot: knownTokenSlots[strategy.USDC].balSlot, kind: numStable},
	{token: strategy.BUSD, slot: rzHubBUSDSlot, kind: numStable},
}

// rzKnownRouters are addresses that, when they appear as a Swap log's Topics[2]
// (the `to`/recipient), are NON-discriminating for same-actor matching: a shared
// router/aggregator `to` does NOT confirm two legs share an actor (§5.2). The
// header.Coinbase is excluded dynamically (per block).
var rzKnownRouters = map[common.Address]bool{
	pancakeV2Router:     true, // PancakeRouter02
	pancakeV3SwapRouter: true, // Pancake V3 SwapRouter
	common.HexToAddress("0x13f4EA83D0bd40E75C8222255bc855a974568Dd4"): true, // Pancake SmartRouter
	common.HexToAddress("0x1A0A18AC4BECDDbd6389559687d1A73d8927E416"): true, // Pancake universal router
	// Common cross-DEX aggregators on BSC (non-discriminating shared sender/`to`).
	common.HexToAddress("0x1111111254EEB25477B68fb85Ed929f73A960582"): true, // 1inch v5 AggregationRouter
	common.HexToAddress("0xDef1C0ded9bec7F1a1670819833240f027b25EfF"): true, // 0x ExchangeProxy
	common.HexToAddress("0x6352a56caadC4F1E25CD6c75970Fa768A3304e64"): true, // OpenOcean Exchange
}

// rzBuilderRegistry seeds the labeled builder-cluster registry with the verified
// builder/payment EOAs from the research. On BSC header.Coinbase is the VALIDATOR,
// NOT the builder, so coinbase is NEVER used as a builder identity here (only as a
// weak per-validator label, recorded separately).
var rzBuilderRegistry = func() map[common.Address]string {
	m := map[common.Address]string{
		common.HexToAddress("0x4848489f0b2BEdd788c696e2D79b6b69D7484848"): "48club",
		common.HexToAddress("0x1266C6bE60392A8Ff346E8d5ECCd3E69dD9c5F20"): "blockrazor-pay",
		common.HexToAddress("0x5532cdb3a2b6db5dB8e8C4B5b3D5Fef4f4Df5532"): "blockrazor",
		common.HexToAddress("0x49d91b1ab0CC6A1591f4dbe0E7B14a1e5C53d1fE"): "blockrazor",
		common.HexToAddress("0xba4233f6e478DB76698b0A5000972Af0196b7bE1"): "blockrazor",
		common.HexToAddress("0x539e2478E280844cd6d10C26d65e7E22C58Eb24a"): "blockrazor",
		common.HexToAddress("0x50061047B9c7150f0Dc105f79588D1B07D2be250"): "blockrazor",
		common.HexToAddress("0x0557E8CB169F90F6eF421a54e29d7dd0629Ca597"): "blockrazor",
	}
	return m
}()

// ---------------------------------------------------------------------------
// Per-block in-memory structures.
// ---------------------------------------------------------------------------

// exPostOpp is one ex-post net-positive sandwich opportunity surfaced by the
// (locally-copied) any-pool evaluator. Keyed for matching by (pool, victimTxIdx,
// token0Side). netBNBWei/grossBNBWei are OUR sizing (the ex-post upper bound); a
// match overrides the realized number with the competitor's measured deltas.
type exPostOpp struct {
	pool        common.Address // pair (the [20]byte emitter)
	isV3        bool
	token0Side  bool   // which side the victim spent (from decodeAnyVictim)
	victimTxIdx int    // original block index of the victim tx
	victimTx    common.Hash
	netBNBWei   *big.Int // our SandwichNet net profit (BNB wei)
	grossBNBWei *big.Int // our gross (BNB wei) — the upper bound
}

// rzSwapLeg is one decoded Swap log captured during the replay, annotated with the
// per-tx ledger inputs needed for the landed-MEV pass. It records the structural
// direction (which side went INTO the pool) and the actor identities so a bracket
// can be assembled and confirmed same-actor.
type rzSwapLeg struct {
	pool       common.Address
	txIdx      int         // original block index of the emitting tx
	txHash     common.Hash
	isV3       bool
	sender     common.Address // Topics[1]: executing contract / sender
	beneficiary common.Address // Topics[2]: V2 `to` / V3 `recipient`
	from       common.Address // recovered EOA `from` of the emitting tx
	// inToken0 reports the direction: true iff token0 was the token spent INTO the
	// pool (so token1 was bought out). This is the same "token0Side" decode the
	// ex-post side uses.
	inToken0 bool
	hasMintBurn bool // emitting tx also emitted a Mint/Burn on this pool (JIT flag)
	// Log amounts (raw, from the Swap Data) used for the log-amount fallback when
	// the volatile token's storage slot is unreadable.
	amt0In, amt1In, amt0Out, amt1Out *big.Int // V2: all four; V3: derived from signed amounts
}

// rzTxLedger is the per-tx hub-asset delta ledger plus gas/coinbase accounting for
// one applied tx, indexed by original block index. deltaHub maps an actor to its
// summed BNB-equivalent hub delta over this tx (native BNB and WBNB unified). For
// the volatile-token flatness check we additionally record per-actor per-token raw
// deltas in deltaTok.
type rzTxLedger struct {
	txIdx       int
	txHash      common.Hash
	from        common.Address
	gasBNBWei   *big.Int                          // receipt.GasUsed * EffectiveGasPrice
	coinbaseBNB *big.Int                          // native coinbase delta over this tx (bribe + tip)
	deltaHub    map[common.Address]*big.Int       // actor -> BNB-equivalent hub delta
	deltaTok    map[common.Address]map[common.Address]*big.Int // actor -> token -> raw delta
}

// ---------------------------------------------------------------------------
// Head subscription loop (mirrors runSandwichAnyBacktest).
// ---------------------------------------------------------------------------

// runRealizabilityBacktest subscribes to chain heads and runs the in-block
// counterfactual on every imported block. Read-only, crash-safe.
func (r *dryRunner) runRealizabilityBacktest() {
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	r.swCfg = defaultSandwichConfig()
	r.rzCfg = defaultRealizabilityConfig()

	log.Info("SimEngine dry-run REALIZABILITY (in-block counterfactual) loop started",
		"flashBps", r.swCfg.flashBps, "minVictimUSD", r.swCfg.minVictimUSD,
		"dustUSD", r.rzCfg.dustUSD, "flatPct", r.rzCfg.flatPct,
		"amtEpsPct", r.rzCfg.amtEpsPct, "repeatMin", r.rzCfg.repeatMin)

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run realizability loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run realizability recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.realizabilityBlock(head)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run realizability loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// realizabilityBlock replays one block ONCE on its parent state, collecting in the
// same hook both (a) the per-tx hub-asset delta ledger and the ordered Swap legs
// (for landed-MEV detection) and (b) the ex-post net-positive sandwich opps. After
// the replay it matches landed sandwiches to ex-post opps, attributes the captors,
// and tallies. Read-only.
func (r *dryRunner) realizabilityBlock(head *types.Header) {
	number := head.Number.Uint64()

	block := r.bc.GetBlockByHash(head.Hash())
	if block == nil {
		return
	}
	parent := r.bc.GetHeaderByHash(head.ParentHash)
	if parent == nil {
		return
	}
	parentState, err := r.bc.StateAt(parent.Root)
	if err != nil {
		return // parent state pruned during catch-up — skip silently.
	}

	var chainID *big.Int
	if r.e != nil && r.e.chainCfg != nil {
		chainID = r.e.chainCfg.ChainID
	}
	signer := types.LatestSignerForChainID(chainID)

	// Per-block accumulators (no cross-block state except the recurrence maps).
	var (
		opps    []*exPostOpp     // ex-post net-positive sandwiches surfaced this block
		legs    []rzSwapLeg      // every decoded Swap log, in execution order
		ledgers []rzTxLedger     // per-tx hub-asset ledger
	)

	// preState tracks the EXACT pre-tx state (post-state of the previously applied
	// tx); it starts as the parent copy and advances after every tx (mirror
	// sandwich-any's hook).
	preState := parentState.Copy()

	onTx := func(i int, tx *types.Transaction, receipt *types.Receipt, statedb *state.StateDB) {
		victimPreState := preState
		preState = statedb.Copy()

		if receipt == nil {
			return
		}

		// Recover the EOA `from` (skip-and-continue on failure, never deref).
		from, sErr := types.Sender(signer, tx)
		if sErr != nil {
			from = common.Address{}
		}

		// (A) Per-tx hub-asset delta ledger + gas/coinbase accounting.
		led := r.buildTxLedger(i, tx, receipt, from, head, victimPreState, statedb)
		ledgers = append(ledgers, led)
		// Decode this tx's Swap legs (and JIT Mint/Burn flag) for the landed scan.
		legs = append(legs, decodeRzLegs(i, tx.Hash(), from, receipt.Logs)...)

		// (B) Ex-post sandwich-any evaluation (UNCHANGED logic; captured into opps).
		if len(receipt.Logs) == 0 {
			return
		}
		seen := make(map[[20]byte]bool)
		for _, l := range receipt.Logs {
			pair, tokenIn, amountIn, isV3, vok := decodeAnyVictim(l)
			if !vok {
				continue
			}
			if seen[pair] {
				continue
			}
			seen[pair] = true

			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine realizability per-victim recovered from panic",
							"block", number, "txIndex", i, "tx", tx.Hash(), "panic", rec)
					}
				}()
				if opp := r.rzEvaluateExPostVictim(number, i, tx, head, victimPreState, pair, tokenIn, amountIn, isV3); opp != nil {
					opps = append(opps, opp)
				}
			}()
		}
	}

	if _, err := r.e.ApplyOnStateHooked(parentState.Copy(), r.bc, head, block.Transactions(), nil, onTx); err != nil {
		return
	}

	// Match landed sandwiches to ex-post opps + attribute, then tally. The dust
	// floor (net-of-costs capture threshold) and the per-pool hub denomination both
	// use the parent state's live WBNB/USD price (one read per block).
	wbnbUSD := liveWbnbPriceUSD(parentState)
	dust := r.rzDustBNB(wbnbUSD)
	r.rzMatchAndAttribute(number, head, opps, legs, ledgers, dust, wbnbUSD)

	n := r.rzProcessed.Add(1)
	r.blocks.Add(1)
	if r.cfg.TallyEvery > 0 && n%r.cfg.TallyEvery == 0 {
		r.logRealizabilityTally(n)
	}
}

// ---------------------------------------------------------------------------
// (A) Per-tx hub ledger + Swap-leg decode.
// ---------------------------------------------------------------------------

// buildTxLedger computes, for this single applied tx, every candidate actor's
// summed BNB-equivalent hub delta (native BNB and WBNB unified), the per-actor
// per-token raw hub deltas (for the volatile-token flatness check), the gas paid,
// and the coinbase native delta. The candidate actor set is the tx's `from` plus
// every distinct Topics[1]/Topics[2] across the tx's Swap logs (both EOAs and bot
// contracts — bots that hold profit are legitimate actors). preState is the pre-tx
// state, statedb the post-tx state.
func (r *dryRunner) buildTxLedger(i int, tx *types.Transaction, receipt *types.Receipt, from common.Address, head *types.Header, preState, statedb *state.StateDB) rzTxLedger {
	led := rzTxLedger{
		txIdx:    i,
		txHash:   tx.Hash(),
		from:     from,
		deltaHub: make(map[common.Address]*big.Int),
		deltaTok: make(map[common.Address]map[common.Address]*big.Int),
	}

	// gas paid = GasUsed * EffectiveGasPrice (BNB wei).
	led.gasBNBWei = big.NewInt(0)
	if receipt.EffectiveGasPrice != nil {
		led.gasBNBWei = new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	}

	// coinbase native delta over this tx (tip + any direct bribe transfer).
	cbBefore := preState.GetBalance(head.Coinbase).ToBig()
	cbAfter := statedb.GetBalance(head.Coinbase).ToBig()
	led.coinbaseBNB = new(big.Int).Sub(cbAfter, cbBefore)

	// Candidate actor set for this tx.
	actors := make(map[common.Address]bool)
	if (from != common.Address{}) {
		actors[from] = true
	}
	for _, l := range receipt.Logs {
		if l == nil || len(l.Topics) == 0 {
			continue
		}
		if l.Topics[0] != strategy.SwapTopic0 && l.Topics[0] != strategy.V3SwapTopic0 {
			continue
		}
		if len(l.Topics) >= 2 {
			actors[topicAddr(l.Topics[1])] = true
		}
		if len(l.Topics) >= 3 {
			actors[topicAddr(l.Topics[2])] = true
		}
	}

	wbnbUSD := liveWbnbPriceUSD(preState)

	for actor := range actors {
		if (actor == common.Address{}) {
			continue
		}
		hubSum := big.NewInt(0)
		tokDeltas := make(map[common.Address]*big.Int)
		for _, h := range rzHubAssets {
			before := rzReadHubBalance(preState, actor, h)
			after := rzReadHubBalance(statedb, actor, h)
			d := new(big.Int).Sub(after, before)
			if d.Sign() == 0 {
				continue
			}
			// Track the raw per-token delta keyed by the hub token (WBNB used for
			// the native/WBNB pair; both map to WBNB for the flatness check).
			key := h.token
			if h.native {
				key = strategy.WBNB
			}
			if cur, ok := tokDeltas[key]; ok {
				tokDeltas[key] = new(big.Int).Add(cur, d)
			} else {
				tokDeltas[key] = new(big.Int).Set(d)
			}
			// BNB-denominate and sum.
			bnb := rzHubDeltaToBNB(d, h.kind, wbnbUSD)
			hubSum.Add(hubSum, bnb)
		}
		led.deltaHub[actor] = hubSum
		led.deltaTok[actor] = tokDeltas
	}
	return led
}

// rzReadHubBalance reads an actor's balance of one hub asset. Native BNB uses
// GetBalance; ERC20 hubs read the known balanceOf storage slot directly (never a
// runtime probe — they are all in the seeded slot table / local BUSD entry).
func rzReadHubBalance(sdb *state.StateDB, actor common.Address, h hubAsset) *big.Int {
	if h.native {
		return sdb.GetBalance(actor).ToBig()
	}
	word := sdb.GetState(h.token, balanceOfKey(actor, h.slot))
	return new(big.Int).SetBytes(word[:])
}

// rzActorHubDeltaBNB sums one actor's signed BNB-equivalent hub-asset delta
// (native BNB + WBNB + stables, all denominated in BNB wei) between preState and
// postState. This is the SAME numeraire and conversion buildTxLedger uses, factored
// out so the censorship-differential valuation can read the value an executor's OWN
// transaction realised (its arb/backrun profit) — the value the builder forwent by
// DROPPING it — rather than the value an attacker would extract by sandwiching it.
// A degenerate (non-positive) WBNB/USD oracle drops stable legs to 0 (the safe,
// D-under-stating direction). gasBNBWei (the tx's own gas, already in BNB wei) is
// NOT subtracted here; the caller nets it.
func rzActorHubDeltaBNB(preState, postState *state.StateDB, actor common.Address, wbnbUSD float64) *big.Int {
	sum := big.NewInt(0)
	if preState == nil || postState == nil || (actor == common.Address{}) {
		return sum
	}
	for _, h := range rzHubAssets {
		before := rzReadHubBalance(preState, actor, h)
		after := rzReadHubBalance(postState, actor, h)
		d := new(big.Int).Sub(after, before)
		if d.Sign() == 0 {
			continue
		}
		sum.Add(sum, rzHubDeltaToBNB(d, h.kind, wbnbUSD))
	}
	return sum
}

// rzHubDeltaToBNB converts a SIGNED hub-asset delta (which numeraireToBNB cannot,
// since it rejects non-positive amounts) to a signed BNB-wei amount. WBNB/BNB are
// 1:1; a stable is divided by the live WBNB/USD price. Returns 0 for a degenerate
// (non-positive) price on a stable leg (per the governing rule: unconvertible
// stable profit is not counted, so it cannot inflate captured).
func rzHubDeltaToBNB(delta *big.Int, kind numeraireKind, wbnbUSD float64) *big.Int {
	if delta == nil || delta.Sign() == 0 {
		return big.NewInt(0)
	}
	switch kind {
	case numWBNB:
		return new(big.Int).Set(delta)
	case numStable:
		if wbnbUSD <= 0 {
			return big.NewInt(0)
		}
		f := new(big.Float).Quo(new(big.Float).SetInt(delta), big.NewFloat(wbnbUSD))
		out, _ := f.Int(nil)
		if out == nil {
			return big.NewInt(0)
		}
		return out
	default:
		return big.NewInt(0)
	}
}

// topicAddr extracts an address from a 32-byte log topic (low 20 bytes).
func topicAddr(t common.Hash) common.Address {
	return common.BytesToAddress(t.Bytes()[12:])
}

// rzMintTopic0 / rzBurnTopic0 are the V2 pair Mint/Burn topics, used to flag
// JIT-liquidity provision so a JIT'd bracket is excluded from capture (§9).
var (
	rzMintTopic0 = common.HexToHash("0x4c209b5fc8ad50758f13e2e1088ba56a560dff690a1c6fef26394f4c03821c4f") // Mint(address,uint256,uint256)
	rzBurnTopic0 = common.HexToHash("0xdccd412f0b1252819cb1fd330b93224ca42612892bb3f4f789976e6d81936496") // Burn(address,uint256,uint256,address)
)

// decodeRzLegs decodes every V2/V3 Swap log in one tx's receipt into rzSwapLeg
// records (in log order), recording the structural direction, the sender/
// beneficiary actor topics, and a JIT Mint/Burn flag per pool.
func decodeRzLegs(txIdx int, txHash common.Hash, from common.Address, logs []*types.Log) []rzSwapLeg {
	// First pass: collect pools that had a Mint/Burn in this tx (JIT detection).
	mintBurn := make(map[common.Address]bool)
	for _, l := range logs {
		if l == nil || len(l.Topics) == 0 {
			continue
		}
		if l.Topics[0] == rzMintTopic0 || l.Topics[0] == rzBurnTopic0 {
			mintBurn[l.Address] = true
		}
	}

	var out []rzSwapLeg
	for _, l := range logs {
		if l == nil || len(l.Topics) == 0 {
			continue
		}
		var leg rzSwapLeg
		switch l.Topics[0] {
		case strategy.SwapTopic0: // V2 Swap(sender, a0In, a1In, a0Out, a1Out, to)
			if len(l.Data) < 128 || len(l.Topics) < 3 {
				continue
			}
			a0In := new(big.Int).SetBytes(l.Data[0:32])
			a1In := new(big.Int).SetBytes(l.Data[32:64])
			a0Out := new(big.Int).SetBytes(l.Data[64:96])
			a1Out := new(big.Int).SetBytes(l.Data[96:128])
			leg = rzSwapLeg{
				pool: l.Address, txIdx: txIdx, txHash: txHash, isV3: false,
				sender: topicAddr(l.Topics[1]), beneficiary: topicAddr(l.Topics[2]), from: from,
				inToken0: a0In.Sign() > 0, // token0 went into the pool
				amt0In: a0In, amt1In: a1In, amt0Out: a0Out, amt1Out: a1Out,
				hasMintBurn: mintBurn[l.Address],
			}
			if a0In.Sign() <= 0 && a1In.Sign() <= 0 {
				continue // not a directional swap
			}
		case strategy.V3SwapTopic0: // V3 Swap(sender, recipient, amount0, amount1, ...)
			if len(l.Data) < 64 || len(l.Topics) < 3 {
				continue
			}
			a0 := signedWord(l.Data[0:32])
			a1 := signedWord(l.Data[32:64])
			// positive => that token went INTO the pool (pool received it).
			in0 := a0.Sign() > 0
			if a0.Sign() == 0 && a1.Sign() == 0 {
				continue
			}
			// Map signed amounts to the V2-style in/out fields for the fallback.
			a0In, a0Out := big.NewInt(0), big.NewInt(0)
			a1In, a1Out := big.NewInt(0), big.NewInt(0)
			if a0.Sign() > 0 {
				a0In = a0
			} else {
				a0Out = new(big.Int).Neg(a0)
			}
			if a1.Sign() > 0 {
				a1In = a1
			} else {
				a1Out = new(big.Int).Neg(a1)
			}
			leg = rzSwapLeg{
				pool: l.Address, txIdx: txIdx, txHash: txHash, isV3: true,
				sender: topicAddr(l.Topics[1]), beneficiary: topicAddr(l.Topics[2]), from: from,
				inToken0: in0,
				amt0In: a0In, amt1In: a1In, amt0Out: a0Out, amt1Out: a1Out,
				hasMintBurn: mintBurn[l.Address],
			}
		default:
			continue
		}
		out = append(out, leg)
	}
	return out
}

// ---------------------------------------------------------------------------
// (B) Ex-post sandwich-any evaluation (UNCHANGED logic; returns the opp).
//
// This is a byte-for-byte copy of sandwichAnyEvaluateVictim's body (same gates,
// same thresholds, same strategy.SandwichNet call) EXCEPT that, at the
// net-positive gate, it ALSO returns an exPostOpp record. The copy keeps the
// validated dryrun_sandwich_any.go untouched and keeps the realizability funnel
// separate and auditable. The ex-post numbers it produces are identical to
// sandwich-any's by construction.
// ---------------------------------------------------------------------------

func (r *dryRunner) rzEvaluateExPostVictim(number uint64, txIndex int, victimTx *types.Transaction, head *types.Header, preState *state.StateDB, pair common.Address, token0Side bool, amountIn *big.Int, isV3 bool) *exPostOpp {
	r.saVictimsSeen.Add(1)

	if preState == nil || head == nil || victimTx == nil || amountIn == nil || amountIn.Sign() <= 0 || (pair == common.Address{}) {
		r.saSkippedUnsupported.Add(1)
		return nil
	}

	pool, ok := r.e.resolvePoolMeta(preState.Copy(), r.bc, head, pair, isV3)
	if !ok || !pool.ok || (pool.token0 == common.Address{}) || (pool.token1 == common.Address{}) {
		r.saSkippedUnsupported.Add(1)
		return nil
	}

	victimTokenIn := pool.token0
	if !token0Side {
		victimTokenIn = pool.token1
	}
	if _, hasOther := poolOther(pool, victimTokenIn); !hasOther {
		r.saSkippedUnsupported.Add(1)
		return nil
	}

	if pool.isV3 && !pool.v3Supported {
		r.saSkippedUnsupported.Add(1)
		return nil
	}

	numToken, numKind, hasNum := poolNumeraire(pool)
	if !hasNum {
		r.saSkippedNoNumeraire.Add(1)
		return nil
	}
	attackerTokenIn := numToken
	attackerTokenOut, hasOther := poolOther(pool, attackerTokenIn)
	if !hasOther {
		r.saSkippedNoNumeraire.Add(1)
		return nil
	}

	probeCopy := preState.Copy()
	if _, fundable := r.e.resolveTokenSlots(probeCopy, r.bc, head, attackerTokenIn); !fundable {
		r.saSkippedUnfundable.Add(1)
		return nil
	}
	if _, fundable := r.e.resolveTokenSlots(probeCopy, r.bc, head, attackerTokenOut); !fundable {
		r.saSkippedUnfundable.Add(1)
		return nil
	}

	wbnbUSD := liveWbnbPriceUSD(preState)

	var victimAmountInNumeraire *big.Int
	victimSpentNumeraire := victimTokenIn == numToken
	if victimSpentNumeraire {
		victimAmountInNumeraire = amountIn
	}

	numTokenUSD := tokenUSDPrice(numToken, wbnbUSD)
	if victimSpentNumeraire && numTokenUSD > 0 {
		if !strategy.VictimAboveThreshold(amountIn, numTokenUSD, r.swCfg.minVictimUSD) {
			r.saBelowThreshold.Add(1)
			return nil
		}
	} else if amountIn.Cmp(minVictimInputHeuristicWei) < 0 {
		r.saBelowThreshold.Add(1)
		return nil
	}

	frontrun, grossNum, gasUnits := r.e.optimalFrontrunAny(preState, r.bc, head, victimTx, pool, attackerTokenIn, amountIn, victimAmountInNumeraire)

	if grossNum.Sign() <= 0 {
		return nil
	}
	r.saGrossPositive.Add(1)

	grossBNB := numeraireToBNB(grossNum, numKind, wbnbUSD)
	frontrunBNB := numeraireToBNB(frontrun, numKind, wbnbUSD)
	if grossBNB.Sign() <= 0 {
		return nil
	}

	grossUSD := weiToUSD(grossBNB, wbnbUSD)
	dexLabel := "v2_any"
	if pool.isV3 {
		dexLabel = "pancake_v3"
	}
	r.saDist.Add(grossBNB, gasUnits, grossUSD, dexLabel, 2)

	eval := strategy.SandwichNet(frontrunBNB, grossBNB, r.cfg.GasPriceWei, gasUnits, r.swCfg.flashBps, r.cfg.BuilderBidWei)
	if !eval.Profitable {
		return nil
	}
	r.saNetPositive.Add(1)
	r.rzExPostNetPos.Add(1)

	return &exPostOpp{
		pool:        pair,
		isV3:        pool.isV3,
		token0Side:  token0Side,
		victimTxIdx: txIndex,
		victimTx:    victimTx.Hash(),
		netBNBWei:   new(big.Int).Set(eval.NetProfit),
		grossBNBWei: new(big.Int).Set(eval.GrossProfit),
	}
}

// ---------------------------------------------------------------------------
// Landed-sandwich detection + matching + attribution (§5, §6).
// ---------------------------------------------------------------------------

// landedSandwich is one confirmed landed sandwich: a same-actor opposite-direction
// bracket on `pool` around a victim, with the corroborated realized hub profit.
type landedSandwich struct {
	pool          common.Address
	frontTxIdx    int
	backTxIdx     int
	inToken0Front bool            // exploited direction (front's token0Side)
	actor         common.Address  // shared actor (the captor)
	executor      common.Address  // front leg's executing contract / tx.to-equivalent (sender)
	realizedGross *big.Int        // competitor's hub gross (BNB wei) before its own gas/bribe
	realizedNet   *big.Int        // realizedGross - gas(front+back) - coinbaseBribe(front+back)
	coinbaseBribe *big.Int        // direct coinbase bribe over the bracket (BNB wei)
}

// rzMatchAndAttribute detects landed sandwiches from the ordered Swap legs + the
// per-tx ledger, matches them to the ex-post opps, marks captured/leftOnTable,
// feeds the distributions, and attributes each captured opp. Read-only; all the
// recurrence-map mutation is lock-guarded.
func (r *dryRunner) rzMatchAndAttribute(number uint64, head *types.Header, opps []*exPostOpp, legs []rzSwapLeg, ledgers []rzTxLedger, dust *big.Int, wbnbUSD float64) {
	landed := r.detectLandedSandwiches(legs, ledgers, head, dust, wbnbUSD)

	// Builder-payment trailing-transfer hint: look at the last few txs' coinbase
	// deltas? On BSC the builder is not the coinbase, so we rely on the actor /
	// executor registry membership for builder attribution (the trailing-payment
	// arm is approximated by registry membership of the bracket executor).

	// Track per-block which actors were credited (so a repeated-block counter is
	// incremented at most once per actor per block).
	creditedActors := make(map[common.Address]bool)
	creditedBuilders := make(map[common.Address]bool)
	// Track which landed brackets have already contributed their realizedNet to the
	// COMPETITOR's realized-profit accumulator. A single wide bracket can straddle
	// (and thus capture) MULTIPLE distinct ex-post victims; the bracket's realized
	// profit is one number and must be counted ONCE, not once per matched opp. The
	// per-opp ourNetBNBWei (rzCapturedNetWei) stays per-opp — that is OUR sizing of
	// each distinct victim — but realizedNet (what the competitor actually made on
	// the bracket) is de-duplicated here. Keyed by the bracket's stable identity.
	creditedBracket := make(map[*landedSandwich]bool)

	for _, opp := range opps {
		// Find a landed sandwich whose bracket straddles this opp's victim on the
		// same pool and exploited direction (§6.1). Greedy nearest-enclosing.
		var match *landedSandwich
		bestSpan := int(^uint(0) >> 1)
		for li := range landed {
			ls := &landed[li]
			if ls.pool != opp.pool {
				continue
			}
			if !(ls.frontTxIdx < opp.victimTxIdx && opp.victimTxIdx < ls.backTxIdx) {
				continue
			}
			if ls.inToken0Front != opp.token0Side {
				continue
			}
			span := ls.backTxIdx - ls.frontTxIdx
			if span < bestSpan {
				bestSpan = span
				match = ls
			}
		}

		if match == nil {
			// Left on the table: the realizable (still-upper-bound) slice.
			r.rzLeftOnTable.Add(1)
			r.rzAddBig(&r.rzLeftNetWei, opp.netBNBWei)
			// The GrossDist percentile fields (GrossUSD*) are reused as the BNB-net
			// carriers: feed the net in BNB units (wei/1e18) as the "grossUSD" float
			// so the dist's p50/p90/p99/max are denominated in BNB.
			r.rzLeftDist.Add(absBig(opp.netBNBWei), strategy.SandwichGasUnits(opp.isV3), weiToBNBFloat(opp.netBNBWei), "left", 2)
			continue
		}

		// Captured.
		r.rzCaptured.Add(1)
		r.rzAddBig(&r.rzCapturedNetWei, opp.netBNBWei)
		realized := match.realizedNet
		if realized == nil {
			realized = big.NewInt(0)
		}
		// Credit the COMPETITOR's realizedNet AT MOST ONCE per landed bracket: a
		// multi-victim bracket matches N opps but realized its profit once.
		if !creditedBracket[match] {
			creditedBracket[match] = true
			r.rzAddBig(&r.rzCapturedRealizedWei, realized)
		}
		r.rzCapturedDist.Add(absBig(opp.netBNBWei), strategy.SandwichGasUnits(opp.isV3), weiToBNBFloat(opp.netBNBWei), "captured", 2)

		// Attribute (§6.3).
		class, builderLabel := r.rzClassifyCaptor(head, match)
		switch class {
		case rzClassBuilder:
			r.rzByBuilder.Add(1)
		case rzClassRepeated:
			r.rzByRepeatedAddr.Add(1)
		default:
			r.rzByUnknown.Add(1)
		}

		// Record the captor in the recurrence maps (once per actor per block).
		if (match.actor != common.Address{}) {
			creditedActors[match.actor] = true
		}
		_ = builderLabel
		if (head.Coinbase != common.Address{}) {
			creditedBuilders[head.Coinbase] = true
		}

		log.Info("realizability capture",
			"block", number,
			"pool", poolLabel(opp.pool),
			"victimTxIdx", opp.victimTxIdx,
			"victimTx", opp.victimTx.Hex(),
			"frontTxIdx", match.frontTxIdx,
			"backTxIdx", match.backTxIdx,
			"actor", shortAddr(match.actor),
			"executor", shortAddr(match.executor),
			"validator", shortAddr(head.Coinbase),
			"class", class.String(),
			"ourNetBNBWei", opp.netBNBWei.String(),
			"realizedNetBNBWei", realized.String(),
			"coinbaseBribeBNBWei", bigOrZero(match.coinbaseBribe).String(),
		)
	}

	// Apply the per-block recurrence-map increments under the lock.
	if len(creditedActors) > 0 || len(creditedBuilders) > 0 {
		r.rzMu.Lock()
		for a := range creditedActors {
			r.rzCaptureCount[a]++
		}
		for b := range creditedBuilders {
			r.rzBuilderCount[b]++
		}
		r.rzMu.Unlock()
	}
}

// detectLandedSandwiches scans the ordered Swap legs for confirmed cross-tx
// sandwiches (§5.1-§5.3) and, secondarily, single-tx atomic sandwiches (§5.5). It
// returns one landedSandwich per confirmed bracket. The conjunctive gates (bracket
// structure + same-actor + delta corroboration) implement the governing rule: any
// failing gate leaves the bracket out (so the opp falls to left-on-the-table).
func (r *dryRunner) detectLandedSandwiches(legs []rzSwapLeg, ledgers []rzTxLedger, head *types.Header, dust *big.Int, wbnbUSD float64) []landedSandwich {
	if len(legs) == 0 {
		return nil
	}
	ledByIdx := make(map[int]*rzTxLedger, len(ledgers))
	for i := range ledgers {
		ledByIdx[ledgers[i].txIdx] = &ledgers[i]
	}

	// Group legs by pool, preserving order.
	byPool := make(map[common.Address][]int) // pool -> indices into legs
	for i := range legs {
		byPool[legs[i].pool] = append(byPool[legs[i].pool], i)
	}

	var out []landedSandwich
	usedBack := make(map[int]bool) // back-leg log index already consumed by a bracket

	for _, idxs := range byPool {
		// Cross-tx pattern: for each opposite-direction (front, back) pair in
		// DIFFERENT txs (front.txIdx < back.txIdx), check same-actor + corroboration.
		for fi := 0; fi < len(idxs); fi++ {
			front := legs[idxs[fi]]
			for bj := fi + 1; bj < len(idxs); bj++ {
				if usedBack[idxs[bj]] {
					continue
				}
				back := legs[idxs[bj]]
				if back.txIdx <= front.txIdx {
					continue // need a strict cross-tx bracket
				}
				// §5.1 opposite directions.
				if front.inToken0 == back.inToken0 {
					continue
				}
				// §9 JIT exclusion.
				if front.hasMintBurn || back.hasMintBurn {
					continue
				}
				// FUNNEL: this is a real opposite-direction cross-tx bracket candidate
				// that reached the §5.2 gate.
				r.rzBracketCandidates.Add(1)
				// §5.2 same-actor confirmation.
				actor, executor, ok := rzSameActor(front, back, head.Coinbase)
				if !ok {
					continue
				}
				r.rzSameActorPass.Add(1)
				// §5.3 delta corroboration on the combined {front, back}.
				realizedGross, realizedNet, bribe, ok := r.rzCorroborate(front, back, actor, ledByIdx, head, dust, wbnbUSD)
				if !ok {
					continue
				}
				r.rzCorroboratePass.Add(1)
				usedBack[idxs[bj]] = true
				out = append(out, landedSandwich{
					pool:          front.pool,
					frontTxIdx:    front.txIdx,
					backTxIdx:     back.txIdx,
					inToken0Front: front.inToken0,
					actor:         actor,
					executor:      executor,
					realizedGross: realizedGross,
					realizedNet:   realizedNet,
					coinbaseBribe: bribe,
				})
				break // this front consumed; move on
			}
		}
	}
	return out
}

// rzSameActor applies §5.2: confirm that the two bracket legs share a single,
// DISCRIMINATING actor. There are three accepted same-actor signals, checked in
// priority order (strongest first); confirmation requires ANY of them. The
// corroboration gate (rzCorroborate: flat-in-Y round trip + net-positive-in-hub on
// THIS pool's bracket, net of gas/bribe) is the proper false-positive guard and
// ALWAYS runs after this gate, so the same-actor test only has to recognise a real
// bot — it does NOT have to also prove the bracket is genuine on its own.
//
//  1. front.from == back.from — the SAME EOA signed BOTH bracket txs. This is the
//     STRONGEST real-bot signal: integrated/long-tail BSC sandwich bots send the
//     front and back from their own EOA while the Swap-log sender/beneficiary is the
//     bot's CONTRACT (or a router). We accept this REGARDLESS of the contract
//     address, and attribute to the EOA so repeated-address clustering keys by the
//     bot's signing key. (The earlier code wrongly rejected exactly this case by
//     requiring the Swap-log actor to equal a leg's `from`.)
//  2. front.sender == back.sender (Topics[1]) AND that sender is not a known
//     router/aggregator (rzKnownRouters) and not the block coinbase. On PancakeSwap
//     V2/V3 the Swap `sender` topic is the CALLER of pair.swap, so a shared router
//     would otherwise let two UNRELATED routed swaps pass — hence the router/coinbase
//     exclusion. Corroboration then rejects any coincidental shared-MM/router routing
//     two unrelated swaps (not a flat-Y net-positive-hub round trip).
//  3. front.beneficiary == back.beneficiary (Topics[2]) AND not a known router and
//     not the coinbase. Same reasoning as path 2.
//
// The victim can never be the shared actor since it sits strictly BETWEEN front and
// back by the bracket-straddle. Returns (actor, executor, ok); for signal 1 the
// actor is the shared EOA, for signals 2/3 the shared contract.
func rzSameActor(front, back rzSwapLeg, coinbase common.Address) (actor, executor common.Address, ok bool) {
	// (1) Same signing EOA on both bracket txs — the strongest real-bot signal.
	// Accept regardless of the Swap-log sender/beneficiary (which is the bot's
	// contract or a router). Attribute to the EOA so clustering keys by the bot.
	if f := front.from; (f != common.Address{}) && f == back.from && f != coinbase {
		exec := front.sender
		if (exec == common.Address{}) {
			exec = f
		}
		return f, exec, true
	}
	// (2) Topics[1] sender equality — only when the shared sender is discriminating
	// (not a known router/aggregator and not the coinbase). Corroboration guards FPs.
	if s := front.sender; (s != common.Address{}) && s == back.sender &&
		!rzKnownRouters[s] && s != coinbase {
		return s, s, true
	}
	// (3) Topics[2] beneficiary equality — same discriminating gate.
	if b := front.beneficiary; (b != common.Address{}) && b == back.beneficiary &&
		!rzKnownRouters[b] && b != coinbase {
		return b, front.sender, true
	}
	return common.Address{}, common.Address{}, false
}

// rzActorCluster returns the small set of addresses that, for one confirmed
// same-actor bracket, could legitimately HOLD the realized hub profit: the
// attribution actor (the EOA when same-actor signal 1 fired, else the contract) plus
// both legs' Swap-log sender and beneficiary (the bot's executor contract is one of
// these). Routers/aggregators and the block coinbase are excluded — they are
// non-discriminating and may carry transient/fee balances unrelated to this bracket.
// rzCorroborate reads the MAX single-member hub delta over this set so an integrated
// bot whose EOA only pays gas while its CONTRACT banks the profit is still measured
// correctly (the recall fix), without summing unrelated balances.
func rzActorCluster(actor common.Address, front, back rzSwapLeg, coinbase common.Address) map[common.Address]bool {
	cluster := make(map[common.Address]bool, 5)
	add := func(a common.Address) {
		if (a == common.Address{}) || a == coinbase || rzKnownRouters[a] {
			return
		}
		cluster[a] = true
	}
	add(actor)
	add(front.sender)
	add(front.beneficiary)
	add(back.sender)
	add(back.beneficiary)
	return cluster
}

// rzCorroborate applies §5.3: using the per-tx ledger for the shared actor across
// {front tx, back tx}, require net-FLAT in the volatile token Y and net-POSITIVE
// in the hub net of gas and coinbase bribe. Returns (realizedGross, realizedNet,
// coinbaseBribe, ok). When the volatile token's storage delta is unavailable (it
// is not a hub asset, so deltaTok only carries hub tokens) we fall back to the
// log-amount reconciliation (§5.4): front amountOut(Y) ~= back amountIn(Y) within
// amtEps AND the actor recovered more hub than it spent.
//
// POOL ISOLATION (governing rule): the actor's WHOLE-TX hub delta folds in profit
// from other pools/swaps/transfers in the same two txs, so it cannot alone attribute
// realized profit to THIS pool's bracket. We additionally compute the bracket's OWN
// hub effect from the leg log amounts on THIS pool (back hub-out − front hub-in,
// BNB-denominated) and require THAT to be net-positive; the attributed realized
// gross is then min(perPoolBracketHub, wholeTxHub) so neither over-states the other.
// If the pool's numeraire side cannot be resolved (cache miss) we fall back to the
// whole-tx hub (the §5.2 same-actor/`from`-EOA gate still bounds false positives).
func (r *dryRunner) rzCorroborate(front, back rzSwapLeg, actor common.Address, ledByIdx map[int]*rzTxLedger, head *types.Header, dust *big.Int, wbnbUSD float64) (realizedGross, realizedNet, coinbaseBribe *big.Int, ok bool) {
	fled := ledByIdx[front.txIdx]
	bled := ledByIdx[back.txIdx]
	if fled == nil || bled == nil {
		return nil, nil, nil, false
	}

	// Combined WHOLE-TX hub delta for the bracket's ACTOR CLUSTER (BNB wei). This is
	// an UPPER BOUND on what the cluster could have realized; it includes unrelated
	// profit in the same txs and so must be intersected with the per-pool bracket
	// effect below.
	//
	// IMPORTANT (integrated-bot recall): for the dominant real-bot pattern the EOA
	// (actor, when same-actor signal 1 fired) only SIGNS and PAYS GAS — the hub
	// PROFIT accrues to the bot's executor CONTRACT (the Swap-log sender/beneficiary),
	// NOT to the EOA. Measuring deltaHub[EOA] alone would read ~0 and (capped by the
	// min() below) zero out every integrated bracket — the recall failure. We
	// therefore take the hub delta over the small actor CLUSTER {actor, both legs'
	// sender, both legs' beneficiary} and use the MAX single-address delta: a real
	// sandwich banks its hub gain in ONE address, and max picks whichever that is
	// without summing unrelated balances. Cluster members that are routers/coinbase
	// are excluded (non-discriminating; could carry transient/fee balances).
	cluster := rzActorCluster(actor, front, back, head.Coinbase)
	hub := big.NewInt(0)
	for member := range cluster {
		mh := big.NewInt(0)
		if d, okd := fled.deltaHub[member]; okd && d != nil {
			mh.Add(mh, d)
		}
		if d, okd := bled.deltaHub[member]; okd && d != nil {
			mh.Add(mh, d)
		}
		if mh.Cmp(hub) > 0 {
			hub = mh
		}
	}

	// Volatile-token flatness check (§5.3) via the LOG amounts (the volatile token
	// is generally not a hub asset, so storage deltas for it are unavailable in the
	// hub ledger — we use the log-amount reconciliation, which is the §5.4 path and
	// also serves the storage-unreadable case uniformly).
	if !rzVolatileFlat(front, back, r.rzCfg.flatPct, r.rzCfg.amtEpsPct) {
		r.rzCorrFailNotFlat.Add(1)
		return nil, nil, nil, false
	}

	// Per-pool bracket hub effect, isolated to THIS pool's two legs (BNB wei). When
	// resolvable it MUST be net-positive (the bracket itself made hub money on this
	// pool, not merely the whole tx) and it CAPS the attributed realized gross.
	perPoolHub, perPoolOK := rzPerPoolBracketHubBNB(front, back, wbnbUSD)
	if perPoolOK {
		if perPoolHub.Sign() <= 0 {
			r.rzCorrFailHubNeg.Add(1)
			return nil, nil, nil, false // this pool's bracket did not net hub-positive
		}
		if perPoolHub.Cmp(hub) < 0 {
			hub = perPoolHub // attribute at most the per-pool bracket effect
		}
	}

	// gas paid by the actor's legs (only count a leg's gas when the actor is that
	// leg's from — the searcher pays its own txs; a beneficiary-only actor whose
	// txs are someone else's is conservatively charged nothing, which can only
	// REDUCE captured via the dust gate below if hub is small).
	gas := big.NewInt(0)
	if fled.from == actor && fled.gasBNBWei != nil {
		gas.Add(gas, fled.gasBNBWei)
	}
	if bled.from == actor && bled.gasBNBWei != nil {
		gas.Add(gas, bled.gasBNBWei)
	}

	// Coinbase bribe over the bracket = the coinbase native delta above the gas tip
	// per leg. We approximate the bribe as max(coinbaseDelta - gasPaid, 0) per the
	// actor's own legs (the tip is part of gasPaid; anything beyond is a bribe).
	bribe := big.NewInt(0)
	for _, led := range []*rzTxLedger{fled, bled} {
		if led.from != actor {
			continue
		}
		if led.coinbaseBNB == nil {
			continue
		}
		extra := new(big.Int).Sub(led.coinbaseBNB, bigOrZero(led.gasBNBWei))
		if extra.Sign() > 0 {
			bribe.Add(bribe, extra)
		}
	}

	realizedGross = new(big.Int).Set(hub)
	realizedNet = new(big.Int).Sub(realizedGross, gas)
	realizedNet.Sub(realizedNet, bribe)

	// §5.3 net-POSITIVE-in-hub gate, net of all costs, above the dust floor.
	if realizedNet.Cmp(dust) <= 0 {
		r.rzCorrFailBelowDust.Add(1)
		return nil, nil, nil, false
	}
	return realizedGross, realizedNet, bribe, true
}

// rzVolatileFlat checks the §5.3/§5.4 round-trip-flat condition using the LOG
// amounts: the volatile token Y bought in the front leg must approximately equal
// the Y sold in the back leg, within max(flatPct, amtEps). The "Y" side is the
// non-hub side; we infer it from the front leg's direction. Both legs are on the
// same pool with opposite directions, so front buys Y (out) and back sells Y (in).
func rzVolatileFlat(front, back rzSwapLeg, flatPct, amtEps float64) bool {
	// front.inToken0 => token0 went INTO the pool, token1 came OUT (Y == token1).
	var yFrontOut, yBackIn *big.Int
	if front.inToken0 {
		// Y is token1: front OUT token1, back IN token1 (opposite direction).
		yFrontOut = front.amt1Out
		yBackIn = back.amt1In
	} else {
		// Y is token0.
		yFrontOut = front.amt0Out
		yBackIn = back.amt0In
	}
	if yFrontOut == nil || yBackIn == nil || yFrontOut.Sign() <= 0 || yBackIn.Sign() <= 0 {
		return false
	}
	tol := flatPct
	if amtEps > tol {
		tol = amtEps
	}
	return withinPct(yFrontOut, yBackIn, tol)
}

// rzPerPoolBracketHubBNB computes the bracket's OWN hub effect on THIS pool from the
// Swap log amounts, BNB-denominated: (hub the actor received OUT of the pool on the
// back leg) − (hub the actor put IN to the pool on the front leg). The hub side X is
// the numeraire side (WBNB / stable); the volatile side Y is the other one. The two
// legs are opposite-direction by construction, so the front leg spends X (X in) and
// the back leg recovers X (X out). Returns (perPoolHubBNB, ok); ok is false when the
// pool's numeraire side cannot be resolved from the meta cache (caller falls back to
// the whole-tx hub). This is the §5.3 pool-isolation guard against attributing
// whole-tx profit (other pools / unrelated transfers) to this victim's opp.
func rzPerPoolBracketHubBNB(front, back rzSwapLeg, wbnbUSD float64) (*big.Int, bool) {
	if front.pool != back.pool {
		return nil, false
	}
	pool, ok := globalPoolMetaCache.get(front.pool)
	if !ok || !pool.ok {
		return nil, false
	}
	numToken, numKind, hasNum := poolNumeraire(pool)
	if !hasNum {
		return nil, false
	}
	// Identify whether the hub (numeraire) token is token0 or token1.
	hubIsToken0 := numToken == pool.token0

	// Front leg: hub went INTO the pool (actor spent hub). Its direction is
	// front.inToken0 (token0 in) — for a real sandwich front spends the hub side, so
	// the hub-in amount is amt0In if hub is token0, else amt1In. Back leg: hub came
	// OUT (actor recovered hub): amt0Out if hub is token0, else amt1Out.
	var hubIn, hubOut *big.Int
	if hubIsToken0 {
		hubIn = front.amt0In
		hubOut = back.amt0Out
	} else {
		hubIn = front.amt1In
		hubOut = back.amt1Out
	}
	if hubIn == nil {
		hubIn = big.NewInt(0)
	}
	if hubOut == nil {
		hubOut = big.NewInt(0)
	}
	// Raw per-pool hub gain (numeraire-token units), then BNB-denominate.
	rawGain := new(big.Int).Sub(hubOut, hubIn)
	return rzHubDeltaToBNB(rawGain, numKind, wbnbUSD), true
}

// withinPct reports whether |a-b| <= tolPct% of max(a,b).
func withinPct(a, b *big.Int, tolPct float64) bool {
	diff := new(big.Int).Abs(new(big.Int).Sub(a, b))
	base := a
	if b.Cmp(a) > 0 {
		base = b
	}
	// diff*100 <= tolPct*base  =>  diff*10000 <= tolPct*100*base (integer-safe).
	lhs := new(big.Int).Mul(diff, big.NewInt(10000))
	tolScaled := int64(tolPct * 100)
	if tolScaled < 0 {
		tolScaled = 0
	}
	rhs := new(big.Int).Mul(base, big.NewInt(tolScaled))
	return lhs.Cmp(rhs) <= 0
}

// rzDustBNB returns the net-of-costs dust floor (BNB wei) for declaring a capture:
// dustUSD converted to BNB via the supplied live WBNB/USD price. When the price is
// unavailable (<=0) it falls back to a tiny fixed floor (1e14 wei = 0.0001 BNB) so
// a degenerate oracle does not let everything count as captured.
func (r *dryRunner) rzDustBNB(wbnbUSD float64) *big.Int {
	floor := big.NewInt(100_000_000_000_000) // 1e14 wei = 0.0001 BNB fallback
	if wbnbUSD <= 0 || r.rzCfg.dustUSD <= 0 {
		return floor
	}
	// dustBNBwei = dustUSD / wbnbUSD * 1e18.
	bnb := r.rzCfg.dustUSD / wbnbUSD
	f := new(big.Float).Mul(big.NewFloat(bnb), big.NewFloat(1e18))
	out, _ := f.Int(nil)
	if out == nil || out.Sign() <= 0 {
		return floor
	}
	return out
}

// ---------------------------------------------------------------------------
// Attribution classification (§6.3).
// ---------------------------------------------------------------------------

type rzClass uint8

const (
	rzClassUnknown rzClass = iota
	rzClassBuilder
	rzClassRepeated
)

func (c rzClass) String() string {
	switch c {
	case rzClassBuilder:
		return "builder"
	case rzClassRepeated:
		return "repeatedAddr"
	default:
		return "unknown"
	}
}

// rzClassifyCaptor attributes a captured opp's captor in priority order:
// (1) builder/validator-internal if the actor or the executor is in the labeled
// builder-cluster registry; (2) repeatedAddr if the actor has captured in >=
// repeatMin blocks already; (3) unknown. header.Coinbase is NEVER used as a
// builder identity (on BSC it is the validator). gasPrice==0 is not consulted here
// (it is only a weak hint and never decides classification on its own).
func (r *dryRunner) rzClassifyCaptor(_ *types.Header, ls *landedSandwich) (rzClass, string) {
	if lbl, ok := rzBuilderRegistry[ls.actor]; ok {
		return rzClassBuilder, lbl
	}
	if lbl, ok := rzBuilderRegistry[ls.executor]; ok {
		return rzClassBuilder, lbl
	}
	// repeatedAddr: read the process-wide recurrence count (already-seen blocks).
	r.rzMu.Lock()
	seen := r.rzCaptureCount[ls.actor]
	r.rzMu.Unlock()
	if seen+1 >= r.rzCfg.repeatMin { // +1 includes this block's pending credit
		return rzClassRepeated, ""
	}
	return rzClassUnknown, ""
}

// ---------------------------------------------------------------------------
// Tally + dist log lines (§8.3).
// ---------------------------------------------------------------------------

// logRealizabilityTally emits the realizability funnel + attribution leaderboard
// and the left-on-table vs captured net-BNB distributions. Crash-safe, read-only.
func (r *dryRunner) logRealizabilityTally(processed uint64) {
	exPost := r.rzExPostNetPos.Load()
	captured := r.rzCaptured.Load()
	left := r.rzLeftOnTable.Load()

	rate := "n/a"
	if exPost > 0 {
		rate = bigRatio(captured, exPost)
	}

	r.rzMu.Lock()
	topSenders := topAddrsByCount(r.rzCaptureCount, realizabilityTopN)
	topBuilders := topAddrsByCount(r.rzBuilderCount, realizabilityTopN)
	r.rzMu.Unlock()

	log.Info("realizability tally",
		"processedBlocks", processed,
		"exPostNetPositive", exPost,
		"alreadyCaptured", captured,
		"leftOnTable", left,
		"captureRate", rate,
		"bracketCandidates", r.rzBracketCandidates.Load(),
		"sameActorPass", r.rzSameActorPass.Load(),
		"corroboratePass", r.rzCorroboratePass.Load(),
		"corrFailNotFlat", r.rzCorrFailNotFlat.Load(),
		"corrFailHubNeg", r.rzCorrFailHubNeg.Load(),
		"corrFailBelowDust", r.rzCorrFailBelowDust.Load(),
		"capturedOurNetWei", r.rzCapturedNetWei.Load().String(),
		"capturedRealizedNetWei", r.rzCapturedRealizedWei.Load().String(),
		"leftNetWei", r.rzLeftNetWei.Load().String(),
		"byBuilder", r.rzByBuilder.Load(),
		"byRepeatedAddr", r.rzByRepeatedAddr.Load(),
		"byUnknown", r.rzByUnknown.Load(),
		"topSenders", topSenders,
		"topBuilders", topBuilders,
		"ts", time.Now().Format(time.RFC3339),
	)

	L := r.rzLeftDist.Snapshot()
	C := r.rzCapturedDist.Snapshot()
	log.Info("realizability dist",
		"processedBlocks", processed,
		"left_samples", L.Count,
		"left_BNB_p50", L.GrossUSDp50,
		"left_BNB_p90", L.GrossUSDp90,
		"left_BNB_p99", L.GrossUSDp99,
		"left_BNB_max", L.GrossUSDMax,
		"capt_samples", C.Count,
		"capt_BNB_p50", C.GrossUSDp50,
		"capt_BNB_p90", C.GrossUSDp90,
		"capt_BNB_p99", C.GrossUSDp99,
		"capt_BNB_max", C.GrossUSDMax,
		"ts", time.Now().Format(time.RFC3339),
	)
}

// topAddrsByCount renders the top-N addresses by capture count as a compact
// "0xabcd..1234:count" comma-joined string via shortAddr.
func topAddrsByCount(m map[common.Address]uint64, n int) string {
	type kv struct {
		a common.Address
		c uint64
	}
	xs := make([]kv, 0, len(m))
	for a, c := range m {
		xs = append(xs, kv{a, c})
	}
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].c != xs[j].c {
			return xs[i].c > xs[j].c
		}
		return xs[i].a.Hex() < xs[j].a.Hex()
	})
	if len(xs) > n {
		xs = xs[:n]
	}
	s := ""
	for i, x := range xs {
		if i > 0 {
			s += ","
		}
		s += shortAddr(x.a) + ":" + strconv.FormatUint(x.c, 10)
	}
	if s == "" {
		return "none"
	}
	return s
}

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

// rzAddBig atomically adds delta to the big.Int behind an atomic.Pointer.
func (r *dryRunner) rzAddBig(p *atomic.Pointer[big.Int], delta *big.Int) {
	if delta == nil {
		return
	}
	for {
		cur := p.Load()
		next := new(big.Int).Add(cur, delta)
		if p.CompareAndSwap(cur, next) {
			return
		}
	}
}

func bigOrZero(v *big.Int) *big.Int {
	if v == nil {
		return big.NewInt(0)
	}
	return v
}

// absBig returns |v| (used to feed the GrossDist, which rejects non-positive; our
// net is generally positive here, but guard the sign).
func absBig(v *big.Int) *big.Int {
	if v == nil {
		return big.NewInt(0)
	}
	return new(big.Int).Abs(v)
}

// weiToBNBFloat converts a wei amount to a BNB float (wei / 1e18). Used to feed the
// GrossDist percentile (GrossUSD*) fields, which the realizability dist repurposes
// as BNB-net carriers.
func weiToBNBFloat(wei *big.Int) float64 {
	if wei == nil {
		return 0
	}
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(new(big.Int).Abs(wei)), big.NewFloat(1e18)).Float64()
	return f
}
