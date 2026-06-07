// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_sandwich_any.go is the ANY-POOL ground-truth sandwich detector,
// selectable with SIMENGINE_DRYRUN=sandwich-any. Unlike the fixed-watch-set
// sandwich mode (dryrun_sandwich.go), which only evaluates victims on the 12
// pre-registered major pools, this mode sandwiches the VICTIM'S ACTUAL pool on
// ANY emitter with ARBITRARY tokens — the long tail (memecoins, new listings)
// where the bulk of real BSC sandwich MEV lives.
//
// Per block it replays the txs tx-by-tx on the parent state; in the hook it
// decodes Swap logs on ANY pool by matching the V2/V3 Swap topics directly (NOT
// the static registry), reads token0/token1/fee for the emitter at runtime, gates
// by a minimum victim USD (or a min-input heuristic when the token has no USD
// reference), funds the synthetic attacker by dynamic storage-slot probing, sizes
// the frontrun via the ground-truth 3-step re-execution on the actual pool, and
// logs every net-positive opportunity. It maintains a full funnel (victimsSeen,
// skippedUnfundable, skippedUnsupported, belowThreshold, grossPositive,
// netPositive, totalNetWei) and reuses the GrossDist accumulator.
//
// Strictly read-only (state.Copy() only), every block and every per-victim
// evaluation wrapped in defer/recover, and a complete no-op unless the mode is set.
package simengine

import (
	"math/big"
	"os"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/strategy"
)

// sandwichAnyProbeLogCap bounds detailed per-victim probe lines (mirrors the
// fixed-set detector's cap).
const sandwichAnyProbeLogCap uint64 = 15

// minVictimInputHeuristicWei is the fallback dust floor (in wei of the victim's
// input token) when the input token has no USD reference: a victim that spends
// less than this raw amount is treated as dust. 1e16 (0.01 of an 18-dp token) is
// a conservative long-tail floor; with no price we cannot value it, so this only
// removes obvious micro-swaps. Tunable via SIMENGINE_SANDWICH_MININPUTWEI.
var minVictimInputHeuristicWei = func() *big.Int {
	def := new(big.Int).SetUint64(10_000_000_000_000_000) // 1e16
	if v := os.Getenv("SIMENGINE_SANDWICH_MININPUTWEI"); v != "" {
		if n, ok := new(big.Int).SetString(v, 10); ok && n.Sign() > 0 {
			return n
		}
	}
	return def
}()

// runSandwichAnyBacktest subscribes to chain heads and runs the any-pool
// ground-truth sandwich valuator on every imported block. Read-only, crash-safe.
func (r *dryRunner) runSandwichAnyBacktest() {
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	r.swCfg = defaultSandwichConfig()

	log.Info("SimEngine dry-run SANDWICH-ANY (any-pool ground-truth) loop started",
		"flashBps", r.swCfg.flashBps, "minVictimUSD", r.swCfg.minVictimUSD,
		"minInputWeiFallback", minVictimInputHeuristicWei.String(),
		"v3router", pancakeV3SwapRouter.Hex())

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run sandwich-any loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run sandwich-any recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.sandwichAnyBlock(head)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run sandwich-any loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// sandwichAnyBlock replays one block tx-by-tx on its parent state and, for each
// victim Swap log on ANY pool, evaluates the ground-truth sandwich on the EXACT
// pre-victim state. Read-only.
func (r *dryRunner) sandwichAnyBlock(head *types.Header) {
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

	// preState tracks the EXACT pre-victim state (post-state of the previously
	// applied tx); it starts as the parent copy and advances after every tx.
	preState := parentState.Copy()

	onTx := func(i int, tx *types.Transaction, receipt *types.Receipt, statedb *state.StateDB) {
		victimPreState := preState
		preState = statedb.Copy()

		if receipt == nil || len(receipt.Logs) == 0 {
			return
		}
		// One tx may emit several Swap logs (multi-hop); evaluate each distinct pool.
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
						log.Warn("SimEngine sandwich-any per-victim recovered from panic",
							"block", number, "txIndex", i, "tx", tx.Hash(), "panic", rec)
					}
				}()
				r.sandwichAnyEvaluateVictim(number, i, tx, head, victimPreState, pair, tokenIn, amountIn, isV3)
			}()
		}
	}

	if _, err := r.e.ApplyOnStateHooked(parentState.Copy(), r.bc, head, block.Transactions(), nil, onTx); err != nil {
		return
	}

	n := r.blocks.Add(1)
	if r.cfg.TallyEvery > 0 && n%r.cfg.TallyEvery == 0 {
		r.logSandwichAnyTally(n)
	}
}

// decodeAnyVictim decodes a Swap log on ANY pool (not just the watch set) by
// matching the V2/V3 Swap topics directly. It returns the emitter pair, the token
// the victim SPENT (X), the amount spent, and whether it is a V3 pool. The token
// IDENTITIES (token0/token1) are resolved later at runtime against the pool; here
// we only learn WHICH side (token0 or token1) the victim spent, encoded as a
// sentinel in tokenIn==zero-with-side. To keep the hook self-contained we instead
// return the side via the amountIn sign convention is not possible, so we return
// the raw side index through a small struct path: decodeAnyVictim resolves the
// side and the caller maps it to the real token after reading token0/token1.
func decodeAnyVictim(l *types.Log) (pair common.Address, victimToken0Side bool, amountIn *big.Int, isV3 bool, ok bool) {
	if l == nil || len(l.Topics) == 0 {
		return common.Address{}, false, nil, false, false
	}
	switch l.Topics[0] {
	case strategy.SwapTopic0: // V2 Swap(sender, a0In, a1In, a0Out, a1Out, to)
		if len(l.Data) < 128 {
			return common.Address{}, false, nil, false, false
		}
		a0In := new(big.Int).SetBytes(l.Data[0:32])
		a1In := new(big.Int).SetBytes(l.Data[32:64])
		switch {
		case a0In.Sign() > 0:
			return l.Address, true, a0In, false, true // spent token0
		case a1In.Sign() > 0:
			return l.Address, false, a1In, false, true // spent token1
		default:
			return common.Address{}, false, nil, false, false
		}
	case strategy.V3SwapTopic0: // V3 Swap(...,amount0 int256, amount1 int256,...)
		if len(l.Data) < 64 {
			return common.Address{}, false, nil, false, false
		}
		a0 := signedWord(l.Data[0:32])
		a1 := signedWord(l.Data[32:64])
		switch {
		case a0.Sign() > 0:
			return l.Address, true, a0, true, true // token0 INTO pool = victim input
		case a1.Sign() > 0:
			return l.Address, false, a1, true, true // token1 INTO pool = victim input
		default:
			return common.Address{}, false, nil, false, false
		}
	default:
		return common.Address{}, false, nil, false, false
	}
}

// twoPow256 = 2^256 for two's-complement decoding.
var twoPow256 = new(big.Int).Lsh(big.NewInt(1), 256)

// signedWord interprets a 32-byte big-endian word as a two's-complement int256.
func signedWord(b []byte) *big.Int {
	v := new(big.Int).SetBytes(b)
	if len(b) == 32 && b[0]&0x80 != 0 {
		v.Sub(v, twoPow256)
	}
	return v
}

// sandwichAnyEvaluateVictim values a single any-pool victim swap on its EXACT
// pre-victim state, DENOMINATED IN BNB. The whole profit calculus is carried in a
// single numeraire (BNB, with USD for reporting): the attacker starts/ends holding
// the pool's numeraire side (WBNB or a stable), so the measured gross is natively
// numeraire-denominated; a token/token pool (no numeraire) is skipped. preState is
// read-only here (sandwichProfitAny Copy()s it internally). Crash-safe via the
// caller's recover; every pool-metadata/reserve read is nil-guarded.
func (r *dryRunner) sandwichAnyEvaluateVictim(number uint64, txIndex int, victimTx *types.Transaction, head *types.Header, preState *state.StateDB, pair common.Address, token0Side bool, amountIn *big.Int, isV3 bool) {
	r.saVictimsSeen.Add(1)

	// Guard the inputs the per-victim path dereferences (nil-pointer fix): a nil
	// preState/header/victimTx/amountIn or an empty pair is unevaluable — skip
	// (count) rather than deref.
	if preState == nil || head == nil || victimTx == nil || amountIn == nil || amountIn.Sign() <= 0 || (pair == common.Address{}) {
		r.saSkippedUnsupported.Add(1)
		return
	}

	// Resolve the pool's metadata (token0/token1/fee) at runtime, cached. A
	// read-only probe Copy is used so preState is never mutated.
	pool, ok := r.e.resolvePoolMeta(preState.Copy(), r.bc, head, pair, isV3)
	if !ok || !pool.ok || (pool.token0 == common.Address{}) || (pool.token1 == common.Address{}) {
		// Couldn't read token0/token1/fee -> can't sandwich -> treat as unsupported.
		r.saSkippedUnsupported.Add(1)
		return
	}

	// Map the decoded side to the real VICTIM input token (the token the victim
	// spent) and its counterparty.
	victimTokenIn := pool.token0
	if !token0Side {
		victimTokenIn = pool.token1
	}
	if _, hasOther := poolOther(pool, victimTokenIn); !hasOther {
		r.saSkippedUnsupported.Add(1)
		return
	}

	// V3 fork support: only Pancake V3 is sandwichable (the existing router).
	if pool.isV3 && !pool.v3Supported {
		r.saSkippedUnsupported.Add(1)
		return
	}

	// NUMERAIRE: identify the WBNB/stable side and denominate the sandwich in it.
	// A token/token pool (no numeraire) cannot be valued in a single comparable
	// unit -> skip (this removes the cross-unit garbage). The attacker's spend/
	// recover token is the numeraire (so gross is natively numeraire-denominated);
	// the other token is the one it round-trips through.
	numToken, numKind, hasNum := poolNumeraire(pool)
	if !hasNum {
		r.saSkippedNoNumeraire.Add(1)
		return
	}
	attackerTokenIn := numToken
	attackerTokenOut, hasOther := poolOther(pool, attackerTokenIn)
	if !hasOther {
		r.saSkippedNoNumeraire.Add(1)
		return
	}

	// Fundability: both tokens must have resolvable storage slots (the attacker is
	// funded by storage writes). Probe (cached) up-front so the funnel is accurate.
	probeCopy := preState.Copy()
	if _, fundable := r.e.resolveTokenSlots(probeCopy, r.bc, head, attackerTokenIn); !fundable {
		r.saSkippedUnfundable.Add(1)
		return
	}
	if _, fundable := r.e.resolveTokenSlots(probeCopy, r.bc, head, attackerTokenOut); !fundable {
		r.saSkippedUnfundable.Add(1)
		return
	}

	wbnbUSD := liveWbnbPriceUSD(preState)

	// Victim notional in the numeraire (only when the victim spent the numeraire,
	// i.e. the profitable BUY direction). Used for the dust gate and the frontrun
	// <=100%-of-victim cap. When the victim spent the NON-numeraire token (the SELL
	// direction), the numeraire-denominated sandwich is run with no victim-size cap
	// and the ground-truth EVM decides feasibility/profit.
	var victimAmountInNumeraire *big.Int
	victimSpentNumeraire := victimTokenIn == numToken
	if victimSpentNumeraire {
		victimAmountInNumeraire = amountIn
	}

	// Dust gate, in BNB. Price the victim notional in USD when it is the numeraire
	// (WBNB via live spot, stables ~1.0); otherwise fall back to the raw-input
	// heuristic on the victim's (un-priceable) token.
	numTokenUSD := tokenUSDPrice(numToken, wbnbUSD)
	if victimSpentNumeraire && numTokenUSD > 0 {
		if !strategy.VictimAboveThreshold(amountIn, numTokenUSD, r.swCfg.minVictimUSD) {
			r.saBelowThreshold.Add(1)
			return
		}
	} else if amountIn.Cmp(minVictimInputHeuristicWei) < 0 {
		// No USD reference for the victim's spent token: raw-input dust heuristic.
		r.saBelowThreshold.Add(1)
		return
	}

	// Ground-truth optimal frontrun on the ACTUAL pool, denominated in the
	// numeraire (attacker spends/recovers numToken). grossNum is in numeraire wei.
	frontrun, grossNum, gasUnits := r.e.optimalFrontrunAny(preState, r.bc, head, victimTx, pool, attackerTokenIn, amountIn, victimAmountInNumeraire)

	// Per-victim probe diagnostics (first few above-threshold victims).
	if r.saProbeLogged.Load() < sandwichAnyProbeLogCap {
		r.logSandwichAnyProbe(number, txIndex, victimTx, head, preState, pool, attackerTokenIn, attackerTokenOut, amountIn, numTokenUSD, frontrun, grossNum)
	}

	if grossNum.Sign() <= 0 {
		return
	}
	r.saGrossPositive.Add(1)

	// Convert numeraire gross/frontrun to BNB so the net gate, totals and reporting
	// are all in BNB wei.
	grossBNB := numeraireToBNB(grossNum, numKind, wbnbUSD)
	frontrunBNB := numeraireToBNB(frontrun, numKind, wbnbUSD)
	if grossBNB.Sign() <= 0 {
		// A stable with no live WBNB price -> cannot convert -> drop (not garbage).
		return
	}

	grossUSD := weiToUSD(grossBNB, wbnbUSD) // grossBNB(wei) * wbnbUSD.
	dexLabel := "v2_any"
	if pool.isV3 {
		dexLabel = "pancake_v3"
	}
	r.saDist.Add(grossBNB, gasUnits, grossUSD, dexLabel, 2 /*2 attacker legs*/)

	// BNB net gate: net = grossBNB - gasBNB(gasUnits*gasPrice) -
	// flashFeeBNB(flashBps*frontrunBNB) - bid. Everything is in BNB wei.
	eval := strategy.SandwichNet(frontrunBNB, grossBNB, r.cfg.GasPriceWei, gasUnits, r.swCfg.flashBps, r.cfg.BuilderBidWei)
	if !eval.Profitable {
		return
	}
	r.saNetPositive.Add(1)
	r.opps.Add(1)
	r.addProfit(eval.NetProfit)
	r.addSandwichAnyNet(eval.NetProfit)

	log.Info("sandwich OPP @tx (any-pool)",
		"block", number,
		"txIndex", txIndex,
		"victimTx", victimTx.Hash().Hex(),
		"pool", dexLabel+":"+poolLabel(pair),
		"numeraire", shortAddr(numToken),
		"dir", shortAddr(attackerTokenIn)+"->"+shortAddr(attackerTokenOut),
		"victimSpentNumeraire", victimSpentNumeraire,
		"victimAmountWei", amountIn.String(),
		"frontrunNumWei", frontrun.String(),
		"grossBNBWei", eval.GrossProfit.String(),
		"netBNBWei", eval.NetProfit.String(),
		"grossUSD", strconv.FormatFloat(grossUSD, 'f', 4, 64),
		"gasBNBWei", eval.GasCost.String(),
		"flashFeeBNBWei", eval.FlashFee.String(),
	)
}

// maxFrontrunReserveFrac caps the attacker's frontrun (in NUMERAIRE units) to a
// fraction of the pool's numeraire reserve. An oversized frontrun moves the price
// so far the victim tx reverts (and would be an unrealistic, capital-infeasible
// position); bounding it keeps the search on the feasible side and kills the
// long-tail oversize-frontrun artifacts. Tunable via SIMENGINE_SANDWICH_MAXRESFRAC.
var maxFrontrunReserveFracNum, maxFrontrunReserveFracDen = func() (int64, int64) {
	num, den := int64(1), int64(2) // 50% of the numeraire reserve.
	if v := os.Getenv("SIMENGINE_SANDWICH_MAXRESFRAC"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
			return int64(f * 1000), 1000
		}
	}
	return num, den
}()

// optimalFrontrunAny finds the gross-maximising frontrun for an any-pool victim
// via golden-section over the ground-truth 3-step re-execution, seeded by the V2
// closed-form candidates (for V2 pools) and bounded by sanity caps. tokenIn is the
// NUMERAIRE token (the attacker starts/ends holding it) so the returned gross is
// numeraire-denominated. victimAmountIn is the victim's input notional (in the
// victim's spent token); when the victim spent the numeraire it bounds the
// frontrun to <= 100% of the victim size. The frontrun is additionally bounded to
// <= maxFrontrunReserveFrac of the pool's numeraire reserve.
func (e *SimEngine) optimalFrontrunAny(preState *state.StateDB, cc simChainContext, hdr *types.Header, victimTx *types.Transaction, pool anyPool, tokenIn common.Address, victimAmountIn, victimAmountInNumeraire *big.Int) (frontrun, gross *big.Int, gasUnits uint64) {
	zero := big.NewInt(0)
	if victimAmountIn == nil || victimAmountIn.Sign() <= 0 {
		return zero, zero, 0
	}
	gasUnits = strategy.SandwichGasUnits(pool.isV3)

	probe := func(f *big.Int) (*big.Int, bool) {
		gr, _, ok := e.sandwichProfitAny(preState, cc, hdr, victimTx, pool, tokenIn, f)
		return gr, ok
	}

	// reserveNumeraire = the pool's reserve of the attacker's spent token (= the
	// numeraire). Used for both the V2 combined seed and the reserve-fraction cap.
	var reserveNumeraire *big.Int
	if !pool.isV3 {
		rv := strategy.ReadReserves(preState, pool.pair)
		switch tokenIn {
		case pool.token0:
			reserveNumeraire = rv.Reserve0
		case pool.token1:
			reserveNumeraire = rv.Reserve1
		}
	}

	// Seeds: half-victim and (V2) the combined-trade closed form, both in numeraire
	// units when the victim spent the numeraire.
	seedBasis := victimAmountInNumeraire
	if seedBasis == nil || seedBasis.Sign() <= 0 {
		seedBasis = victimAmountIn
	}
	var seeds []*big.Int
	seeds = append(seeds, strategy.HalfVictimSeed(seedBasis))
	if reserveNumeraire != nil && reserveNumeraire.Sign() > 0 {
		seeds = append(seeds, strategy.V2CombinedSeed(reserveNumeraire, seedBasis, pool.gamma))
	}

	// Sanity cap 1: <= 100% of the victim's size, in numeraire units (only binds
	// when the victim spent the numeraire — the profitable BUY direction).
	hi := new(big.Int).Mul(seedBasis, big.NewInt(8))
	if victimAmountInNumeraire != nil && victimAmountInNumeraire.Sign() > 0 {
		hi = new(big.Int).Set(victimAmountInNumeraire) // <= 100% of victim size.
	}
	// Sanity cap 2: <= maxFrontrunReserveFrac of the pool's numeraire reserve.
	if reserveNumeraire != nil && reserveNumeraire.Sign() > 0 {
		resCap := new(big.Int).Mul(reserveNumeraire, big.NewInt(maxFrontrunReserveFracNum))
		resCap.Quo(resCap, big.NewInt(maxFrontrunReserveFracDen))
		if resCap.Sign() > 0 && resCap.Cmp(hi) < 0 {
			hi = resCap
		}
	}
	if hi.Sign() <= 0 {
		return zero, zero, gasUnits
	}

	frontrun, gross = strategy.OptimalFrontrun(probe, seeds, hi)
	if gross.Sign() <= 0 {
		return zero, zero, gasUnits
	}
	return frontrun, gross, gasUnits
}

// logSandwichAnyProbe emits a single diagnostic line tracing a representative
// any-pool probe (frontrun = half the victim) for the first few above-threshold
// victims, so we can see WHICH leg fails and why. Read-only.
// tokenIn here is the NUMERAIRE (the attacker's spend token); searchGross is the
// numeraire-denominated gross from the search. numTokenUSD prices the numeraire.
func (r *dryRunner) logSandwichAnyProbe(number uint64, txIndex int, victimTx *types.Transaction, head *types.Header, preState *state.StateDB, pool anyPool, tokenIn, tokenOut common.Address, amountIn *big.Int, numTokenUSD float64, bestFrontrun, searchGross *big.Int) {
	r.saProbeLogged.Add(1)

	// Representative numeraire-denominated probe size: prefer the search's best
	// frontrun (already in numeraire units), else half the victim notional. The
	// victim notional is only in numeraire units when the victim spent the
	// numeraire; otherwise it is a coarse stand-in for a diagnostic line.
	size := bestFrontrun
	if size == nil || size.Sign() <= 0 {
		size = strategy.HalfVictimSeed(amountIn)
	}
	if size == nil || size.Sign() <= 0 {
		size = amountIn
	}
	d := r.e.probeSandwichAnyDiag(preState, r.bc, head, victimTx, pool, tokenIn, size)

	// searchGross is in numeraire wei; report its USD value.
	searchGrossUSD := 0.0
	if numTokenUSD > 0 && searchGross != nil {
		searchGrossUSD = weiToUSD(searchGross, numTokenUSD)
	}
	dexLabel := "v2_any"
	if pool.isV3 {
		dexLabel = "pancake_v3(fee=" + strconv.FormatUint(uint64(pool.feeTier), 10) + ")"
	}

	log.Info("sandwich-any probe",
		"block", number,
		"txIndex", txIndex,
		"victim", victimTx.Hash().Hex(),
		"pool", dexLabel+":"+poolLabel(pool.pair),
		"numeraire", shortAddr(tokenIn),
		"dir", shortAddr(tokenIn)+"->"+shortAddr(tokenOut),
		"victimAmtWei", amountIn.String(),
		"gammaNum", pool.gamma.Num.String(),
		"gammaDen", pool.gamma.Den.String(),
		"probeFrontrunNumWei", size.String(),
		"fundOk", d.fundOk,
		"frontrunOk", d.frontrunOk,
		"yBought", bigOrNil(d.yBought),
		"victimOk", d.victimOk,
		"backrunOk", d.backrunOk,
		"probeGrossNumWei", d.gross.String(),
		"searchBestFrontrunNumWei", bigOrNil(bestFrontrun),
		"searchGrossNumWei", bigOrNil(searchGross),
		"searchGrossUSD", strconv.FormatFloat(searchGrossUSD, 'f', 4, 64),
		"reason", d.reason,
	)
}

// addSandwichAnyNet accumulates the any-pool net would-be profit.
func (r *dryRunner) addSandwichAnyNet(delta *big.Int) {
	for {
		cur := r.saTotalNetWei.Load()
		next := new(big.Int).Add(cur, delta)
		if r.saTotalNetWei.CompareAndSwap(cur, next) {
			return
		}
	}
}

// logSandwichAnyTally emits the any-pool candidate funnel + profit summary and the
// gross-positive distribution. Crash-safe and read-only.
func (r *dryRunner) logSandwichAnyTally(processed uint64) {
	seen := r.saVictimsSeen.Load()
	gross := r.saGrossPositive.Load()
	net := r.saNetPositive.Load()

	overcount := "n/a"
	if net > 0 {
		overcount = bigRatio(seen, net)
	}

	log.Info("sandwich-any tally",
		"processedBlocks", processed,
		"victimsSeen", seen,
		"skippedUnfundable", r.saSkippedUnfundable.Load(),
		"skippedUnsupported", r.saSkippedUnsupported.Load(),
		"skippedNoNumeraire", r.saSkippedNoNumeraire.Load(),
		"belowThreshold", r.saBelowThreshold.Load(),
		"grossPositive", gross,
		"netPositive", net,
		"seenPerNet", overcount,
		"opportunities", r.opps.Load(),
		"totalNetWei", r.saTotalNetWei.Load().String(),
		"ts", time.Now().Format(time.RFC3339),
	)

	s := r.saDist.Snapshot()
	log.Info("sandwich-any dist",
		"processedBlocks", processed,
		"grossPosSamples", s.Count,
		"grossUSD_p50", s.GrossUSDp50,
		"grossUSD_p90", s.GrossUSDp90,
		"grossUSD_p99", s.GrossUSDp99,
		"grossUSD_max", s.GrossUSDMax,
		"breakevenGwei_p50", s.BreakevenGweiP50,
		"breakevenGwei_p90", s.BreakevenGweiP90,
		"breakevenGwei_max", s.BreakevenGweiMax,
		"gasSweep_netPos", s.SweepString(),
		"byDex", s.DexMixString(),
		"ts", time.Now().Format(time.RFC3339),
	)
}
