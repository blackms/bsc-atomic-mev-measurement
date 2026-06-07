// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun.go is the Phase 2 in-process DRY-RUN backrun harness. It mirrors the
// SimEngine self-test wiring (selftest.go): it subscribes to chain heads /
// pending txs, re-executes via the SimEngine on a COPY of live state, detects
// watched PancakeSwap V2 swaps, and computes the would-be profit of a 2-pool
// x*y=k arbitrage backrun via package strategy.
//
// IT NEVER SUBMITS ANYTHING. It is strictly read-only (state.Copy() only, never
// commits), every unit of work is wrapped in defer/recover so a bug can never
// panic or stall the node, and the whole thing is a no-op unless the env var
// SIMENGINE_DRYRUN is set.
//
// Two modes (SIMENGINE_DRYRUN = "backtest" | "live" | "both"; default backtest):
//   - BACKTEST (works during catch-up): on each imported block, re-execute the
//     block's txs on the PARENT state, scan the resulting logs for watched-pair
//     Sync events to get post-swap reserves, evaluate every shared-token 2-pool
//     cycle, and log opportunities with net would-be profit.
//   - LIVE (needs a synced node): subscribe to pending txs; for each watched-pair
//     candidate, simulate [target] on head state, read post-swap reserves, and
//     evaluate. Synthetic-backrun construction/submission is Phase 3.
package simengine

import (
	"math/big"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/strategy"
)

// DryRunConfig tunes the economic assessment. The defaults are conservative,
// BSC-realistic placeholders; all are read-only knobs (no submission). Costs are
// expressed in WEI of the cycle's start token. For WBNB-bridged cycles the start
// token is WBNB so GasCost can be taken directly as gasUsed*gasPrice in wei.
type DryRunConfig struct {
	// GasUnits is the assumed gas a 2-hop backrun would consume (research: budget 250k).
	GasUnits uint64
	// GasPriceWei is the assumed effective gas price in wei (BSC ~1-3 gwei).
	GasPriceWei *big.Int
	// BuilderBidWei is the assumed builder bid in wei (0 in pure dry-run).
	BuilderBidWei *big.Int
	// MarginWei is an extra safety margin subtracted from gross profit, in wei.
	MarginWei *big.Int
	// TallyEvery emits a periodic summary every N processed blocks/txs.
	TallyEvery uint64
}

// DefaultDryRunConfig returns conservative defaults: 250k gas at 3 gwei, no bid,
// a small margin, and a tally every 200 units.
func DefaultDryRunConfig() DryRunConfig {
	tally := uint64(200)
	if v := os.Getenv("SIMENGINE_DRYRUN_TALLY"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			tally = n
		}
	}
	return DryRunConfig{
		GasUnits:      250_000,
		GasPriceWei:   big.NewInt(3_000_000_000), // 3 gwei
		BuilderBidWei: big.NewInt(0),
		MarginWei:     big.NewInt(0),
		TallyEvery:    tally,
	}
}

// gasCostWei returns the assumed gas cost (GasUnits * GasPriceWei) in wei.
func (c DryRunConfig) gasCostWei() *big.Int {
	gp := c.GasPriceWei
	if gp == nil {
		gp = big.NewInt(0)
	}
	return new(big.Int).Mul(new(big.Int).SetUint64(c.GasUnits), gp)
}

// evalParams builds the strategy.EvalParams from the config. The start token of
// the evaluated cycles in the watch set is WBNB (18dp) for WBNB-bridged cycles,
// so wei costs are directly comparable to the wei-denominated gross profit. For
// stable/stable cycles (USDT start) the costs are an upper bound (gas is paid in
// BNB, conservatively counted 1:1 against an 18dp stable).
func (c DryRunConfig) evalParams() strategy.EvalParams {
	return strategy.EvalParams{
		GasCost:    c.gasCostWei(),
		BuilderBid: orZeroBig(c.BuilderBidWei),
		Margin:     orZeroBig(c.MarginWei),
	}
}

func orZeroBig(v *big.Int) *big.Int {
	if v == nil {
		return big.NewInt(0)
	}
	return v
}

// dryRunner holds the long-lived state of a dry-run session.
type dryRunner struct {
	e   *SimEngine
	bc  *core.BlockChain
	cfg DryRunConfig

	// NOTE (public research artifact): the upstream private fork wires an
	// arm-gated backrun *submission* orchestrator here (a `sub *SubmitConfig`
	// field backed by submit.go + the builder/wallet packages). That submission
	// path is intentionally OMITTED from this read-only measurement artifact —
	// this code only ever simulates and LOGS would-be opportunities; it never
	// builds, signs, or submits a transaction. See README "Read-only / never
	// submits" disclaimer.

	// tallies (atomic, read-only reporting).
	blocks   atomic.Uint64
	opps     atomic.Uint64
	totalWei atomic.Pointer[big.Int] // accumulated net would-be profit (start-token wei)

	// IMPROVED-model (graph mode) candidate-funnel tallies (RQ3 over-count).
	gCandidates    atomic.Uint64 // Stage-A negative cycles enumerated
	gGrossPositive atomic.Uint64 // candidates with exact gross > 0 (Stage-B sizing)
	gNetPositive   atomic.Uint64 // candidates net-positive after gas+bid

	// v3 INTRA-BLOCK (per-swap) candidate-funnel tallies. These quantify how many
	// backrun opportunities exist in the TRANSIENT post-victim-swap state that the
	// post-block graph mode misses because competitors re-align prices within the
	// block. Distinct from the graph-mode counters so the two modes' funnels stay
	// independently auditable.
	ibWatchedSwaps  atomic.Uint64 // watched-pool Swap/Sync events observed mid-block
	ibCandidates    atomic.Uint64 // Stage-A negative cycles enumerated on transient state
	ibGrossPositive atomic.Uint64 // candidates with exact gross > 0 (Stage-B sizing)
	ibNetPositive   atomic.Uint64 // candidates net-positive after gas+bid

	// ibDist characterises the GROSS-POSITIVE population (how far below the gas
	// threshold the cross-venue cycles sit): grossUSD percentiles, breakeven gas
	// price, a gas-price sensitivity sweep, and per-dexMix/per-len breakdowns. It is
	// O(1) in memory (log-scale histograms + small counter maps) and is fed in the
	// intra-block valuation path right where gross>0 is determined. Read-only.
	ibDist *strategy.GrossDist

	// SANDWICH (ground-truth) detector state. swCfg carries the flash-loan/dust
	// knobs; the counters form the per-victim funnel (the paper's headline atomic
	// MEV); swDist characterises the gross-positive sandwich population (reusing the
	// same O(1) accumulator as intra-block); swTotalNetWei accumulates net would-be
	// sandwich profit. Distinct from the backrun counters so the two are auditable
	// independently.
	swCfg               sandwichConfig
	swVictimsConsidered atomic.Uint64 // watched-pool victim swaps above the dust floor
	swSkippedUnfundable atomic.Uint64 // victims skipped because a pool token has no known slot
	swBelowThreshold    atomic.Uint64 // victims skipped below the USD dust floor
	swGrossPositive     atomic.Uint64 // victims with ground-truth gross > 0
	swNetPositive       atomic.Uint64 // victims net-positive after gas + flash + bid
	swTotalNetWei       atomic.Pointer[big.Int]
	swDist              *strategy.GrossDist

	// DIAGNOSTICS (sim13): swSelftestDone gates the one-shot startup funding+swap
	// self-test (run once on the first processed block's pre-state). swProbeLogged
	// counts how many above-threshold victims have had a detailed per-step failure
	// line emitted (capped at sandwichProbeLogCap) so we can see WHICH leg fails
	// without flooding the log. Both are read-only diagnostics.
	swSelftestDone atomic.Bool
	swProbeLogged  atomic.Uint64

	// ANY-POOL sandwich detector (SIMENGINE_DRYRUN=sandwich-any) funnel. Distinct
	// from the fixed-watch-set sandwich counters above so the two modes stay
	// independently auditable. saVictimsSeen counts EVERY decoded Swap-log victim on
	// ANY pool (V2 or V3); the rest are the eligibility/profit funnel.
	saVictimsSeen        atomic.Uint64 // every V2/V3 Swap-log victim observed
	saSkippedUnfundable  atomic.Uint64 // skipped: a pool token's slot couldn't be probed
	saSkippedUnsupported atomic.Uint64 // skipped: non-Pancake V3 fork (no router path)
	saSkippedNoNumeraire atomic.Uint64 // skipped: neither pool token is WBNB/stable (can't value)
	saBelowThreshold     atomic.Uint64 // skipped: below the min-victim dust floor
	saGrossPositive      atomic.Uint64 // victims with ground-truth gross > 0
	saNetPositive        atomic.Uint64 // victims net-positive after gas + flash + bid
	saTotalNetWei        atomic.Pointer[big.Int]
	saDist               *strategy.GrossDist
	saProbeLogged        atomic.Uint64 // per-victim probe lines emitted (capped)

	// REALIZABILITY (in-block counterfactual) detector funnel
	// (SIMENGINE_DRYRUN=realizability). For each EX-POST net-positive sandwich opp
	// our detector surfaces, this mode decides whether a REAL competitor ALREADY
	// captured it in the SAME canonical block (a landed sandwich bracketing the same
	// victim on the same pool), turning the ex-post UPPER BOUND into a realizable
	// estimate (captured = unavailable; leftOnTable = the still-upper-bound slice).
	// The governing rule is "over-counting captured is the dangerous direction", so
	// every counter below is fed only on the conjunctive landed-MEV evidence in
	// dryrun_realizability.go. Kept fully separate from the sa* funnel so the two
	// are independently auditable; the ex-post numbers (rzExPostNetPos) reproduce
	// sandwich-any's netPositive on the same blocks by construction.
	rzProcessed           atomic.Uint64           // blocks processed in this mode
	rzExPostNetPos        atomic.Uint64           // ex-post net-positive opps surfaced (== sandwich-any netPositive)
	rzCaptured            atomic.Uint64           // opps matched to a landed competitor
	rzLeftOnTable         atomic.Uint64           // net-positive opps with NO landed match
	// Landed-detection FUNNEL counters (so we can never report a blind zero again):
	// bracketCandidates -> sameActorPass -> corroboratePass. Each is incremented in
	// detectLandedSandwiches as a candidate progresses through the §5.2/§5.3 gates.
	rzBracketCandidates atomic.Uint64 // opposite-dir cross-tx leg pairs on a pool that reached the §5.2 gate
	rzSameActorPass     atomic.Uint64 // candidates that passed rzSameActor (§5.2)
	rzCorroboratePass   atomic.Uint64 // candidates that also passed rzCorroborate (= confirmed landed sandwiches)
	// Corroboration-failure breakdown (diagnostic: prove WHY same-actor brackets are
	// rejected so a near-zero captureRate is auditable, not a blind zero). A bracket
	// that passed §5.2 but failed §5.3 increments exactly one of these.
	rzCorrFailNotFlat   atomic.Uint64 // failed the volatile-token flat round-trip check
	rzCorrFailHubNeg    atomic.Uint64 // per-pool bracket hub effect was <= 0 (round trip lost hub)
	rzCorrFailBelowDust atomic.Uint64 // net hub (after gas/bribe) was <= the dust floor
	rzCapturedNetWei      atomic.Pointer[big.Int] // sum of OUR netBNB over captured opps (our sizing of what was taken)
	rzCapturedRealizedWei atomic.Pointer[big.Int] // sum of COMPETITOR realizedNetBNB over captured opps
	rzLeftNetWei          atomic.Pointer[big.Int] // sum of OUR netBNB over left-on-table opps (realizable upper bound)
	rzByBuilder           atomic.Uint64           // captured opps attributed to a builder/validator-internal captor
	rzByRepeatedAddr      atomic.Uint64           // captured opps attributed to a recurrent external searcher
	rzByUnknown           atomic.Uint64           // captured opps with no attribution
	rzLeftDist            *strategy.GrossDist     // dist over left-on-table net-BNB
	rzCapturedDist        *strategy.GrossDist     // dist over captured net-BNB

	// rzMu guards the two process-wide recurrence maps below (the only non-atomic
	// realizability state). rzCaptureCount counts, per actor address, the number of
	// blocks in which that actor was the net-positive captor of a matched opp (the
	// repeatedAddr classifier); rzBuilderCount is the per-builder/validator capture
	// leaderboard. Both are read under lock when rendering the tally's topSenders /
	// topBuilders fields.
	rzMu           sync.Mutex
	rzCaptureCount map[common.Address]uint64
	rzBuilderCount map[common.Address]uint64
	rzCfg          realizabilityConfig

	// RECALL-VALIDATION harness (SIMENGINE_DRYRUN=recalltest). Measures the
	// landed-sandwich detector's RECALL by injecting genuine synthetic landed
	// sandwiches (real SimEngine swaps) of diverse structure into each block and
	// counting how many detectLandedSandwiches catches, PER structure, plus the
	// false-positive rate on injected clean victims. Kept on its own pointer so the
	// dryRunner struct only carries it when the mode is selected; all per-cell state
	// lives in rtCounters (dryrun_recalltest.go).
	rt *rtCounters

	// -----------------------------------------------------------------------
	// CENSORSHIP-DIFFERENTIAL (D) detector (SIMENGINE_DRYRUN=censorship).
	//
	// Estimates D = the receipt-exact BNB value the builder leaves on the table
	// by DROPPING public, private-flow-orthogonal, net-of-gas-profitable
	// opportunities. The NEW data dimension vs the realizability detector is the
	// PUBLIC MEMPOOL (cspub), which supplies the treatment assignment (a public
	// opportunity was INCLUDED vs DROPPED by the builder). Every counter below is
	// fed only on the conjunctive availability/orthogonality/profitability/
	// not-captured gate; the GOVERNING RULE is that over-stating D is the
	// dangerous direction, so any ambiguity EXCLUDES (increments a csSkip*) and
	// D-hat (csDhatWei) is a strict LOWER bound. Kept fully separate from the rz*/
	// sa* funnels so D's funnel stays independently auditable. The cspub ledger,
	// runCensorshipDetector, censorshipBlock, the gates, and logCensorshipTally
	// all live in dryrun_censorship.go.
	cspub *pubLedger // rolling public-mempool ledger (sender/nonce-indexed)
	csCfg censorshipConfig

	csProcessed      atomic.Uint64 // blocks processed in this mode
	csPublicOppsSeen atomic.Uint64 // drop candidates that decoded as a public opportunity
	// Funnel exclusions (each EXCLUDE increments exactly one).
	csSkipNonceMoved     atomic.Uint64 // account nonce already moved past the candidate (superseded/mined)
	csSkipNonceGap       atomic.Uint64 // candidate nonce > parent nonce (not executable at seal)
	csSkipInvalidAtSeal  atomic.Uint64 // failed static / stateful validation at the sealing parent
	csSkipReplaced       atomic.Uint64 // (sender,nonce) filled in the sealed block under a DIFFERENT hash
	csSkipNoNumeraire    atomic.Uint64 // neither pool token is WBNB/stable (cannot value)
	csSkipUnfundable     atomic.Uint64 // a pool token's slot could not be probed
	csSkipBelowThreshold atomic.Uint64 // below the min-victim dust floor
	csSkipNetNonPositive atomic.Uint64 // not net-of-gas profitable
	csSkipNonOrthogonal  atomic.Uint64 // pool in the candidate's path touched by a sealed tx (SUTVA / stale-reserve)
	csSkipAlreadyCaptured atomic.Uint64 // a landed competitor already captured it (value taken, not left)
	csSkipShortLead      atomic.Uint64 // (localSeal - firstSeen) below the builder lead-time floor (arrived too late)
	csSkipClosedByBlock  atomic.Uint64 // profitable on the parent but reverts/nets <=0 on the POST-SEALED-BLOCK state (the block already closed the opp)
	csSkipNotRoundTrip   atomic.Uint64 // single-leg one-way swap (< 2 directional swap legs): its positive hub delta is GROSS SALE PROCEEDS, not self-contained arb profit (not a dropped searcher opp)
	// Progress through the gate (informational; not exclusions).
	csOrthogonal         atomic.Uint64 // candidates that passed orthogonality
	csProfitable         atomic.Uint64 // candidates that passed net-of-gas
	csDropped            atomic.Uint64 // confirmed dropped (pre-not-captured count)
	csIncludedComparable atomic.Uint64 // public opps the builder INCLUDED (control group; D-contribution 0)
	csDhatCount          atomic.Uint64 // opps contributing to D-hat
	csDhatWei            atomic.Pointer[big.Int] // sum of V_i (BNB wei) = D-hat (the LOWER bound)
	csDist               *strategy.GrossDist     // BNB distribution of the dropped-D population
	// csCreditedDrops is the LIFETIME set of candidate tx hashes already credited to
	// D-hat. A public tx that stays pending across N heads would otherwise be flagged
	// "dropped" and have its V_i re-added once PER head it lingers (the per-block
	// creditedPoolSide map only dedups WITHIN a single block). De-duping by hash across
	// the whole run counts each unique dropped opportunity AT MOST ONCE, removing that
	// Nx multiplier. Strictly under-states D (one count, not N) — the safe direction.
	csCreditedDropsMu sync.Mutex
	csCreditedDrops   map[common.Hash]bool
	csSkipAlreadyCredited atomic.Uint64 // candidate already credited on an earlier head (cross-block repeat)
	// SETTLE WINDOW (deferred-drop finalization). A candidate that passes every gate
	// at block N is NOT credited to D immediately: it is enqueued (csPending) with a
	// finalizeHeight = N + settleBlocks and credited only if it is STILL un-mined K
	// blocks later. csMined is the rolling tx-hash -> mined-height index the finalizer
	// uses to tell delayed-inclusion (mined within the window -> csSkipMinedLater,
	// the dominant over-statement this fixes) from genuine censorship (never mined).
	// This collapses "delayed-inclusion" out of D-hat, the forbidden direction.
	csMined      *minedIndex
	csPendingMu  sync.Mutex
	csPending    []*pendingDrop // pending-drops queue (awaiting their settle window)
	csPendingMax atomic.Uint64  // high-water-mark of the queue depth (a memory covariate)

	csSkipMinedLater atomic.Uint64 // pending-drop MINED within (N, N+K] -> delayed-inclusion, NOT censored (the key finding)
	csSkipSuperseded atomic.Uint64 // pending-drop whose sender-nonce advanced before finalize (superseded/nonce moved on)

	// -----------------------------------------------------------------------
	// ORDERING-CURL / HODGE-DECOMPOSITION detector (SIMENGINE_DRYRUN=curl).
	//
	// The decisive go/no-go experiment for the paper's geometric backbone. Per
	// slot it finds single-pool contending transaction clusters (k>=MINK swaps on
	// the SAME WBNB/BNB pool), builds the integer-exact {BNB,WBNB} value functional
	// V over re-ordered sub-sequences (C1: no stables, no oracle, no float), forms
	// the antisymmetric ordering-curl 2-form Omega, and runs the discrete Hodge
	// decomposition on the filled clique 2-complex (H^1=0). The headline number is
	// median rho = ||grad psi||^2 / ||Omega||^2 (the gradient/potential fraction):
	// rho -> 1 means value is near-potential on a single pool. All curl* live in
	// dryrun_curl.go (engine) + dryrun_curl_hodge.go (math). Kept fully separate
	// from the other funnels so the experiment is independently auditable.
	curlCfg curlConfig

	curlProcessed     atomic.Uint64 // blocks processed in this mode
	curlSinglePoolKge atomic.Uint64 // single-pool clusters with k>=MINK seen (pre-qualification)
	curlClusters      atomic.Uint64 // qualifying clusters fully decomposed (fed rho)
	curlCommuting     atomic.Uint64 // clusters whose Omega==0 (value order-independent; rho undefined)
	curlOversize      atomic.Uint64 // clusters above maxK (recorded, not decomposed)
	curlSkipNoMeta    atomic.Uint64 // pool metadata unresolvable
	curlSkipNoNumeraire atomic.Uint64 // neither pool side is WBNB/stable
	curlSkipNonWBNB   atomic.Uint64 // pool numeraire is a stable (C1 admits WBNB/BNB only)
	curlSkipNoActor   atomic.Uint64 // no recoverable EOA actor in the cluster
	curlSkipPrefixFail atomic.Uint64 // the fixed context prefix failed to execute
	curlExhaustiveDone atomic.Uint64 // clusters that also ran the exhaustive k! GATE-1 check
	curlScalarDone    atomic.Uint64 // clusters that also ran the GATE-3 scalar-ordering curl

	// Exact-quantile accumulators (every cluster is rare, so we keep all samples and
	// report EXACT median/p10/p90; not log-bucketed).
	curlRhoHist        *fracHist // rho = gradFrac distribution (GATE-2: the decisive number)
	curlCurlFracHist   *fracHist // curlFrac = ||curl||^2/||Omega||^2 distribution
	curlHarmFracHist   *fracHist // GATE-1 orthogonality-residual (harmonic-proxy) distribution
	curlScalarCurlHist *fracHist // GATE-3 scalar-ordering curlFrac distribution
}

// StartDryRun launches the in-process dry-run backrun harness. It blocks on its
// subscription(s) and is meant to run in its own goroutine; it returns on node
// shutdown. mode is "backtest" (default), "live", or "both".
//
//   - bc:     the live blockchain (state source + chain context). Never mutated.
//   - pool:   the live transaction pool, used only by LIVE mode (may be nil for backtest).
//   - cfg:    the chain configuration.
//   - engine: the consensus engine (Parlia), reused for system-tx detection.
//   - mode:   "backtest" | "live" | "both".
func StartDryRun(bc *core.BlockChain, pool *txpool.TxPool, cfg *params.ChainConfig, engine consensus.Engine, mode string, drCfg DryRunConfig) {
	if bc == nil {
		log.Warn("SimEngine dry-run not started: nil blockchain")
		return
	}
	if drCfg.TallyEvery == 0 {
		drCfg = DefaultDryRunConfig()
	}

	// Reuse the live node's state cache and chain context by attaching to the
	// running blockchain, exactly like StartSelfTest (do NOT use simengine.New).
	r := &dryRunner{
		e:   &SimEngine{chainCfg: cfg, engine: engine},
		bc:  bc,
		cfg: drCfg,
		// (Submission orchestrator omitted from this read-only artifact — see note
		// on the dryRunner.sub field above.)
		// Gross-positive distribution accumulator for the intra-block characterisation.
		// The gas-price sweep is taken from SIMENGINE_DRYRUN_GASPRICES (CSV gwei),
		// default "0,0.1,0.3,1,3".
		ibDist: strategy.NewGrossDist(strategy.ParseGasPriceSweepGwei(os.Getenv("SIMENGINE_DRYRUN_GASPRICES"))),
		// Gross-positive distribution accumulator for the SANDWICH characterisation
		// (same gas-price sweep source as the intra-block detector).
		swDist: strategy.NewGrossDist(strategy.ParseGasPriceSweepGwei(os.Getenv("SIMENGINE_DRYRUN_GASPRICES"))),
		// Gross-positive distribution accumulator for the ANY-POOL sandwich detector.
		saDist: strategy.NewGrossDist(strategy.ParseGasPriceSweepGwei(os.Getenv("SIMENGINE_DRYRUN_GASPRICES"))),
		// REALIZABILITY net-BNB distributions (left-on-table vs captured), same
		// gas-price sweep source as the other detectors.
		rzLeftDist:     strategy.NewGrossDist(strategy.ParseGasPriceSweepGwei(os.Getenv("SIMENGINE_DRYRUN_GASPRICES"))),
		rzCapturedDist: strategy.NewGrossDist(strategy.ParseGasPriceSweepGwei(os.Getenv("SIMENGINE_DRYRUN_GASPRICES"))),
		rzCaptureCount: make(map[common.Address]uint64),
		rzBuilderCount: make(map[common.Address]uint64),
		// CENSORSHIP-DIFFERENTIAL (D) dropped-opportunity BNB distribution, same
		// gas-price sweep source as the other detectors.
		csDist: strategy.NewGrossDist(strategy.ParseGasPriceSweepGwei(os.Getenv("SIMENGINE_DRYRUN_GASPRICES"))),
	}
	r.totalWei.Store(big.NewInt(0))
	r.swTotalNetWei.Store(big.NewInt(0))
	r.saTotalNetWei.Store(big.NewInt(0))
	r.rzCapturedNetWei.Store(big.NewInt(0))
	r.rzCapturedRealizedWei.Store(big.NewInt(0))
	r.rzLeftNetWei.Store(big.NewInt(0))
	r.csDhatWei.Store(big.NewInt(0))
	r.csCreditedDrops = make(map[common.Hash]bool)

	switch mode {
	case "live":
		log.Info("SimEngine dry-run starting", "mode", "live")
		r.runLive(pool)
	case "both":
		log.Info("SimEngine dry-run starting", "mode", "both")
		go r.runLive(pool)
		r.runBacktest()
	case "graph":
		// IMPROVED model (Stage A negative-cycle detection + exact big.Int
		// sizing; SimEngine EVM-verify hook documented in dryrun_graph.go).
		log.Info("SimEngine dry-run starting", "mode", "graph")
		r.runGraphBacktest()
	case "intrablock":
		// v3 PER-SWAP intra-block detection: evaluate the transient state right
		// after each victim swap (before competitors re-align), catching backrun
		// MEV the post-block graph mode misses. See dryrun_intrablock.go.
		log.Info("SimEngine dry-run starting", "mode", "intrablock")
		r.runIntrablockBacktest()
	case "sandwich":
		// GROUND-TRUTH SANDWICH valuation: for each victim swap on a watched pool,
		// re-execute frontrun -> the REAL victim tx -> backrun through the deployed
		// PancakeSwap router on a state.Copy, size the frontrun optimally, and apply
		// the flash-loan + gas net gate. Sandwiching is the dominant atomic MEV on
		// BSC (~51% of volume). See dryrun_sandwich.go.
		log.Info("SimEngine dry-run starting", "mode", "sandwich")
		r.runSandwichBacktest()
	case "sandwich-any":
		// ANY-POOL GROUND-TRUTH SANDWICH valuation: detect victim swaps on ANY pool
		// (V2 or V3, arbitrary tokens) by matching the Swap topics directly, read the
		// pool's token0/token1/fee at runtime, fund arbitrary BEP20s by dynamic slot
		// probing, sandwich the victim's ACTUAL pool (direct V2 pair.swap or the
		// Pancake V3 router), and apply the flash + gas net gate. This is where the
		// real long-tail sandwich MEV lives. See dryrun_sandwich_any.go.
		log.Info("SimEngine dry-run starting", "mode", "sandwich-any")
		r.runSandwichAnyBacktest()
	case "realizability":
		// IN-BLOCK COUNTERFACTUAL: for each EX-POST net-positive sandwich opp the
		// any-pool detector surfaces in a canonical block, decide whether a REAL
		// competitor ALREADY captured it in the SAME block (a landed sandwich
		// bracketing the same victim on the same pool), and attribute the captor
		// (builder/validator-internal, recurrent searcher, or unknown). Converts the
		// ex-post upper bound into a realizable estimate. See dryrun_realizability.go.
		log.Info("SimEngine dry-run starting", "mode", "realizability")
		r.runRealizabilityBacktest()
	case "recalltest":
		// RECALL-VALIDATION HARNESS: per block, inject genuine synthetic LANDED
		// sandwiches (real SimEngine swaps) spanning the structural variety that
		// determines real-world recall, run the EXACT detectLandedSandwiches over the
		// augmented leg/ledger set, and tally recall PER structure plus the
		// false-positive rate on injected clean victims. This bounds the
		// realizability detector's false-negative rate so the "captured=0" headline is
		// a measured null at a known recall, not a blind zero. See dryrun_recalltest.go.
		log.Info("SimEngine dry-run starting", "mode", "recalltest")
		r.rt = &rtCounters{}
		r.runRecallTestBacktest()
	case "censorship":
		// CENSORSHIP-DIFFERENTIAL (D): subscribe to the PUBLIC mempool + new heads;
		// per slot, identify public opportunities, determine which the builder
		// DROPPED from the sealed block, value the dropped ones receipt-exactly, and
		// tally D-hat (a conservative LOWER bound on builder public-censorship
		// value). The public mempool supplies the included-vs-dropped treatment
		// assignment the realizability detector lacks. See dryrun_censorship.go.
		log.Info("SimEngine dry-run starting", "mode", "censorship")
		r.runCensorshipDetector(pool)
	case "curl":
		// ORDERING-CURL / HODGE DECOMPOSITION: per slot, find single-pool contending
		// transaction clusters (k>=MINK swaps on the SAME WBNB/BNB pool), build the
		// integer-exact {BNB,WBNB} value functional V over re-ordered sub-sequences,
		// form the antisymmetric ordering-curl 2-form Omega, and Hodge-decompose it on
		// the filled clique 2-complex. The decisive number is median rho =
		// ||grad psi||^2/||Omega||^2. See dryrun_curl.go + dryrun_curl_hodge.go.
		log.Info("SimEngine dry-run starting", "mode", "curl")
		r.runCurlBacktest()
	default: // "backtest" and anything else
		log.Info("SimEngine dry-run starting", "mode", "backtest")
		r.runBacktest()
	}
}

// ---------------------------------------------------------------------------
// BACKTEST mode.
// ---------------------------------------------------------------------------

// runBacktest subscribes to chain heads and, for every imported block, replays
// the block's txs on the parent state and evaluates backrun opportunities. It is
// the immediate go/no-go gate and works during catch-up. Read-only, crash-safe.
func (r *dryRunner) runBacktest() {
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	log.Info("SimEngine dry-run backtest loop started",
		"pools", len(strategy.WatchedPools), "cycles", len(strategy.SharedTokenPools()))

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run backtest loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run backtest recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.backtestBlock(head)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run backtest loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// backtestBlock re-executes one block on its parent state and evaluates backrun
// opportunities from the post-swap reserves. Read-only.
func (r *dryRunner) backtestBlock(head *types.Header) {
	number := head.Number.Uint64()

	block := r.bc.GetBlockByHash(head.Hash())
	if block == nil {
		return
	}
	parent := r.bc.GetHeaderByHash(head.ParentHash)
	if parent == nil {
		return
	}
	statedb, err := r.bc.StateAt(parent.Root)
	if err != nil {
		// Expected during catch-up if the parent state is pruned — skip silently.
		return
	}

	// Re-execute the whole block on a COPY of the parent state, mirroring the
	// self-test. The mutated copy and the flat log list are exactly the
	// post-block view we need: scanning Sync logs gives deterministic post-swap
	// reserves without per-tx snapshotting.
	res, err := r.e.SimulateOnState(statedb.Copy(), r.bc, head, block.Transactions(), nil)
	if err != nil {
		return
	}

	n := r.blocks.Add(1)

	// Which watched pools moved in this block?
	touched := strategy.WatchedPairsTouched(res.Logs)
	if len(touched) > 0 {
		// Resolve reserves for ALL watched pools that participate in any candidate
		// cycle, reading the mutated post-block state directly from storage. (The
		// block has been replayed onto the copy; re-derive a fresh copy at the
		// post-state by replaying again would be wasteful — instead read reserves
		// from the per-pair last Sync if present, else from storage.)
		r.evaluateBlock(number, statedb, head, block, res, touched)
	}

	if r.cfg.TallyEvery > 0 && n%r.cfg.TallyEvery == 0 {
		r.logTally("backtest", n)
	}
}

// evaluateBlock evaluates every shared-token 2-pool cycle against the post-swap
// reserves and logs profitable opportunities. Reserves are taken from the last
// Sync event of each pair when available (exact post-state), falling back to a
// direct storage read on a freshly post-executed state copy.
func (r *dryRunner) evaluateBlock(number uint64, parentState *state.StateDB, head *types.Header, block *types.Block, res *SimResult, touched []strategy.Pool) {
	// Build a map pair -> post reserves from the LAST Sync event per pair.
	syncReserves := lastSyncReserves(res.Logs)

	// For any watched pool involved in a cycle but not present in the Sync map
	// (e.g. unchanged counter-pool), read reserves from a post-executed state.
	// Re-run the block onto a fresh copy once and read storage for the misses.
	var post *state.StateDB
	reservesFor := func(p strategy.Pool) (strategy.Reserves, bool) {
		if rv, ok := syncReserves[p.Pair]; ok {
			return rv, true
		}
		if post == nil {
			fresh := parentState.Copy()
			if _, err := r.e.SimulateOnState(fresh, r.bc, head, block.Transactions(), nil); err != nil {
				return strategy.Reserves{}, false
			}
			post = fresh
		}
		rv := strategy.ReadReserves(post, p.Pair)
		if rv.Reserve0.Sign() <= 0 || rv.Reserve1.Sign() <= 0 {
			return strategy.Reserves{}, false
		}
		return rv, true
	}

	touchedSet := make(map[common.Address]bool, len(touched))
	for _, p := range touched {
		touchedSet[p.Pair] = true
	}

	params := r.cfg.evalParams()

	for _, pp := range strategy.SharedTokenPools() {
		// Only evaluate cycles where at least one leg's reserves moved this block.
		if !touchedSet[pp.A.Pair] && !touchedSet[pp.B.Pair] {
			continue
		}
		ra, okA := reservesFor(pp.A)
		rb, okB := reservesFor(pp.B)
		if !okA || !okB {
			continue
		}

		// Build the cycle around the shared token X: X -> other(A) on A? No —
		// the 2-pool x*y=k model arbitrages the SAME token pair across two pools.
		// For pools sharing exactly one token (the bridge X), the cycle is:
		//   start X -> buy the A-counter-token? not a 2-pool same-pair cycle.
		// We model the canonical shared-token cycle: enter pool A with the shared
		// token, exit pool B with the shared token, treating the OTHER tokens as
		// the intermediate. That requires A and B to share the SAME other token
		// too (i.e. identical pair on two venues). For the WBNB-bridged watch set
		// the pools form a triangular cycle; we approximate the opportunity using
		// the bridge token as start/end and the closed-form 2-pool formula over
		// the (sharedIn, otherOut) / (otherIn, sharedOut) legs. See notes.
		eval, fwd := r.evalCycle(pp, ra, rb, params)
		if !eval.Profitable {
			continue
		}

		r.opps.Add(1)
		r.addProfit(eval.NetProfit)

		dir := "A->B"
		if !fwd {
			dir = "B->A"
		}
		log.Info("backrun OPP",
			"block", number,
			"cycle", pp.A.Name+"|"+pp.B.Name,
			"dir", dir,
			"shared", shortAddr(pp.Shared),
			"profitWei", eval.NetProfit.String(),
			"grossWei", eval.GrossProfit.String(),
			"amountInWei", eval.OptimalAmountIn.String(),
			"gasWei", eval.GasCost.String(),
		)
	}
}

// evalCycle evaluates the 2-pool arb for a shared-token pool pair in both
// directions and returns the better Evaluation and whether it is the forward
// (A then B) direction. The cycle starts/ends in the shared token X:
//
//	forward: X --(buy other on A)--> sell other on B --> X
//
// Leg reserves: A leg in = R_A(X), A leg out = R_A(otherA); B leg in = R_B(otherB),
// B leg out = R_B(X). For an identical pair on two venues otherA == otherB and
// this is the exact x*y=k two-pool arb. For a bridged triangle the intermediate
// tokens differ and this is a first-order approximation (flagged in notes).
func (r *dryRunner) evalCycle(pp strategy.PoolPair, ra, rb strategy.Reserves, params strategy.EvalParams) (strategy.Evaluation, bool) {
	x := pp.Shared

	otherA, okA := pp.A.Other(x)
	otherB, okB := pp.B.Other(x)
	if !okA || !okB {
		return strategy.Evaluation{NetProfit: big.NewInt(0), GrossProfit: big.NewInt(0), OptimalAmountIn: big.NewInt(0), GasCost: big.NewInt(0)}, true
	}

	// forward A->B: buy otherA on A with X, sell otherB on B for X.
	aIn := pp.A.ReserveOf(ra, x)       // X in pool A
	aOut := pp.A.ReserveOf(ra, otherA) // other in pool A
	bIn := pp.B.ReserveOf(rb, otherB)  // other in pool B
	bOut := pp.B.ReserveOf(rb, x)      // X in pool B

	// gamma: both legs share the same fee in this watch set; use pool A's.
	return strategy.BestArb(aIn, aOut, bIn, bOut, pp.A.Gamma, params)
}

// ---------------------------------------------------------------------------
// LIVE mode.
// ---------------------------------------------------------------------------

// runLive subscribes to the pending mempool and evaluates watched-pair pending
// txs against head state. Works only once the node is synced. Read-only,
// crash-safe.
//
// NOTE (Phase 3): the synthetic-backrun transaction is NOT constructed or
// submitted here. LIVE mode simulates the TARGET tx on head, reads the resulting
// watched-pair reserves, and evaluates the would-be backrun profit with the same
// closed-form math as backtest. Building/signing/submitting the actual backrun
// (and the builder-bid bundle) is Phase 3 and intentionally left as a stub.
func (r *dryRunner) runLive(pool *txpool.TxPool) {
	if pool == nil {
		log.Warn("SimEngine dry-run live mode disabled: nil txpool")
		return
	}

	ch := make(chan core.NewTxsEvent, 64)
	sub := pool.SubscribeTransactions(ch, false)
	defer sub.Unsubscribe()

	log.Info("SimEngine dry-run live loop started", "pools", len(strategy.WatchedPools))

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run live loop stopped (tx channel closed)")
				return
			}
			for _, tx := range ev.Txs {
				tx := tx
				func() {
					defer func() {
						if rec := recover(); rec != nil {
							log.Warn("SimEngine dry-run live recovered from panic", "tx", tx.Hash(), "panic", rec)
						}
					}()
					r.liveTx(tx)
				}()
			}
		case err := <-sub.Err():
			log.Info("SimEngine dry-run live loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// liveTx pre-filters a pending tx, simulates it on head state, and (if it
// touches a watched pair) evaluates the would-be backrun. Submission is Phase 3.
func (r *dryRunner) liveTx(tx *types.Transaction) {
	// Cheap pre-filter: router+selector. Most volume routes elsewhere, so this is
	// advisory only; we still simulate-and-confirm via logs below. We do not gate
	// hard on the pre-filter because aggregator/contract swaps would be missed.

	head := r.bc.CurrentHeader()
	if head == nil {
		return
	}
	statedb, err := r.bc.StateAt(head.Root)
	if err != nil {
		return
	}

	// (Wallet nonce-tracker seeding omitted with the submission orchestrator —
	// this read-only artifact has no wallet and never sends.)

	// Simulate the single target tx on a copy of head state. We synthesise a
	// child header (head height+1) so the EVM block context is the next block.
	childHeader := nextHeader(head)
	res, err := r.e.SimulateOnState(statedb.Copy(), r.bc, childHeader, types.Transactions{tx}, nil)
	if err != nil || res == nil {
		return
	}

	touched := strategy.WatchedPairsTouched(res.Logs)
	if len(touched) == 0 {
		return
	}

	n := r.blocks.Add(1)

	// Reserves after the target tx: from the last Sync per pair.
	syncReserves := lastSyncReserves(res.Logs)
	params := r.cfg.evalParams()
	touchedSet := make(map[common.Address]bool, len(touched))
	for _, p := range touched {
		touchedSet[p.Pair] = true
	}

	for _, pp := range strategy.SharedTokenPools() {
		if !touchedSet[pp.A.Pair] && !touchedSet[pp.B.Pair] {
			continue
		}
		ra, okA := reservesOrRead(syncReserves, statedb, pp.A)
		rb, okB := reservesOrRead(syncReserves, statedb, pp.B)
		if !okA || !okB {
			continue
		}
		eval, fwd := r.evalCycle(pp, ra, rb, params)
		if !eval.Profitable {
			continue
		}
		r.opps.Add(1)
		r.addProfit(eval.NetProfit)
		dir := "A->B"
		if !fwd {
			dir = "B->A"
		}
		log.Info("backrun OPP (live)",
			"target", tx.Hash().Hex(),
			"cycle", pp.A.Name+"|"+pp.B.Name,
			"dir", dir,
			"profitWei", eval.NetProfit.String(),
			"amountInWei", eval.OptimalAmountIn.String(),
			"gasWei", eval.GasCost.String(),
		)
		// The opportunity is recorded and LOGGED above only. The upstream private
		// fork would hand it to an arm-gated submission orchestrator here; that
		// path is intentionally omitted from this read-only artifact (no wallet,
		// no builder, never sends).
	}

	if r.cfg.TallyEvery > 0 && n%r.cfg.TallyEvery == 0 {
		r.logTally("live", n)
	}
}

// reservesOrRead returns the post-tx reserves for a pool from the Sync map, or
// falls back to a direct storage read on the (pre-tx) head state. The fallback
// is only used for the COUNTER pool that the target tx did not touch, whose
// reserves are unchanged, so reading the pre-tx state is correct.
func reservesOrRead(syncMap map[common.Address]strategy.Reserves, headState *state.StateDB, p strategy.Pool) (strategy.Reserves, bool) {
	if rv, ok := syncMap[p.Pair]; ok {
		return rv, true
	}
	rv := strategy.ReadReserves(headState, p.Pair)
	if rv.Reserve0.Sign() <= 0 || rv.Reserve1.Sign() <= 0 {
		return strategy.Reserves{}, false
	}
	return rv, true
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// lastSyncReserves scans a flat log list (execution order) and returns, per
// watched pair, the reserves carried by that pair's LAST Sync event. Sync data
// is two uint112 words: reserve0 (token0) then reserve1 (token1).
func lastSyncReserves(logs []*types.Log) map[common.Address]strategy.Reserves {
	out := make(map[common.Address]strategy.Reserves)
	for _, l := range logs {
		if l == nil || len(l.Topics) == 0 || l.Topics[0] != strategy.SyncTopic0 {
			continue
		}
		if _, ok := strategy.PoolByPair(l.Address); !ok {
			continue
		}
		if len(l.Data) < 64 {
			continue
		}
		r0 := new(big.Int).SetBytes(l.Data[0:32])
		r1 := new(big.Int).SetBytes(l.Data[32:64])
		out[l.Address] = strategy.Reserves{Reserve0: r0, Reserve1: r1}
	}
	return out
}

// nextHeader builds a minimal child header for simulating a pending tx as if it
// were included in the next block. Fields beyond what NewEVMBlockContext needs
// (number, time, gas limit, base fee, coinbase, parent hash) are left zero.
func nextHeader(parent *types.Header) *types.Header {
	h := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number, big.NewInt(1)),
		GasLimit:   parent.GasLimit,
		Time:       parent.Time + 3, // BSC ~3s block time (approximate; read-only)
		Coinbase:   parent.Coinbase,
		Difficulty: new(big.Int).Set(parent.Difficulty),
	}
	if parent.BaseFee != nil {
		h.BaseFee = new(big.Int).Set(parent.BaseFee)
	}
	return h
}

func (r *dryRunner) addProfit(delta *big.Int) {
	for {
		cur := r.totalWei.Load()
		next := new(big.Int).Add(cur, delta)
		if r.totalWei.CompareAndSwap(cur, next) {
			return
		}
	}
}

func (r *dryRunner) logTally(mode string, processed uint64) {
	log.Info("SimEngine dry-run tally",
		"mode", mode,
		"processed", processed,
		"opportunities", r.opps.Load(),
		"totalWouldBeProfitWei", r.totalWei.Load().String(),
		"ts", time.Now().Format(time.RFC3339),
	)
}

func shortAddr(a common.Address) string {
	s := a.Hex()
	if len(s) <= 10 {
		return s
	}
	return s[:6] + ".." + s[len(s)-4:]
}
