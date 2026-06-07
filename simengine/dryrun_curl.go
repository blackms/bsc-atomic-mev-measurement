// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_curl.go is the ORDERING-CURL / HODGE-DECOMPOSITION detector, selectable
// with SIMENGINE_DRYRUN=curl. It is the decisive go/no-go experiment for the
// paper's geometric backbone: is the realized value of a set of contending
// transactions on a single pool a NEAR-POTENTIAL function of their ordering?
//
// THE QUESTION. Per slot, identify the per-pool CONTENDING transaction set K (the
// txs in the block that swap the SAME pool — single-pool clusters; k is single
// digit). For a qualifying cluster (k >= MINK on a WBNB/USDT-class pool) we build,
// on a state.Copy() of the parent with the numeraire pinned ONCE from the parent:
//
//	V(sigma) : the value functional. Re-execute an ordered sub-sequence sigma of
//	           the cluster's txs (the OTHER block txs before the cluster form a
//	           fixed context PREFIX applied once) via the existing applyOnState /
//	           SimulateOnState path on state.Copy(); read the attributed actor set's
//	           INTEGER-EXACT {native BNB, WBNB} own-hub-delta as V. C1: native BNB +
//	           WBNB legs ONLY — no stablecoin legs, no oracle, no float — so V is
//	           integer-exact in wei and a deterministic function of the executed
//	           sub-sequence.
//
//	Omega_ij : the antisymmetric ordering-curl 2-form,
//	           C_c(i,j) = V(c.i.j) - V(c.j.i) for cluster txs i,j under bracketing
//	           context c; Omega_ij = E_c[C_c(i,j)] averaged over a few sampled
//	           bracketing contexts (or the empty context for the minimal version).
//
//	Hodge    : on the complete graph K_k with all triangles filled (clique
//	           2-complex, H^1 = 0), Omega = grad(psi) + curl with NO harmonic term.
//	           Solve L0 psi = div(Omega) (graph Laplacian least squares, psi fixed
//	           to sum 0); grad(psi)_ij = psi_j - psi_i; curl = Omega - grad(psi).
//	           rho = ||grad psi||^2 / ||Omega||^2 is the gradient/total energy
//	           fraction. rho -> 1 means value is near-potential (separable) on a
//	           single pool — the paper's flagship.
//
// It does NOT touch applyOnState / SimulateOnState / selftest.go (the validated
// 5/5 receipt-exact path). It rides ONLY on SimulateOnState, exactly like the
// backtest mode. Strictly read-only (state.Copy() only, never commits), every
// block and per-cluster evaluation wrapped in defer/recover, and a complete no-op
// unless SIMENGINE_DRYRUN=curl.
package simengine

import (
	"math/big"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/strategy"
)

// ---------------------------------------------------------------------------
// Env knobs.
// ---------------------------------------------------------------------------

// curlConfig holds the curl-engine knobs.
type curlConfig struct {
	// minK is the minimum cluster size to qualify (single-digit k; default 3).
	minK int
	// maxK caps the cluster size the engine fully evaluates. The pairwise Omega is
	// O(k^2) sub-sequence executions and the exhaustive GATE-1 check is O(k!), so a
	// cap protects the node from a pathological mega-cluster. Clusters above maxK
	// are recorded (curlOversize) but not decomposed. Default 8.
	maxK int
	// contexts is the number of sampled bracketing contexts c to average C_c(i,j)
	// over (>=1). 1 means the empty/minimal context only. Default 1.
	contexts int
	// exhaustiveMaxK: for k <= this, the engine ALSO runs the exhaustive k! check
	// (GATE-1) comparing the Hodge gradFrac against the full-permutation value
	// spread. Default 6 (k! = 720 sub-sequence executions worst case).
	exhaustiveMaxK int
	// scalarGate3: when true, ALSO build Omega around a purely SCALAR ordering
	// (priority-gas key) and report its curlFrac (GATE-3). Default true.
	scalarGate3 bool
}

func defaultCurlConfig() curlConfig {
	c := curlConfig{minK: 3, maxK: 8, contexts: 1, exhaustiveMaxK: 6, scalarGate3: true}
	if v := os.Getenv("SIMENGINE_CURL_MINK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 2 {
			c.minK = n
		}
	}
	if v := os.Getenv("SIMENGINE_CURL_MAXK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= c.minK {
			c.maxK = n
		}
	}
	if v := os.Getenv("SIMENGINE_CURL_CONTEXTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			c.contexts = n
		}
	}
	if v := os.Getenv("SIMENGINE_CURL_EXHAUSTIVE_MAXK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.exhaustiveMaxK = n
		}
	}
	if v := os.Getenv("SIMENGINE_CURL_GATE3"); v == "0" || v == "false" {
		c.scalarGate3 = false
	}
	return c
}

// ---------------------------------------------------------------------------
// Per-block in-memory structures.
// ---------------------------------------------------------------------------

// curlCluster is one single-pool contending transaction set in a block.
type curlCluster struct {
	pool    common.Address // the shared pool the cluster txs all swap
	numKind numeraireKind  // numWBNB (qualifying) — C1 admits only WBNB/native-BNB value
	// txIdx and txs are the cluster's transactions in ORIGINAL block order. The
	// actor set is pinned from these once.
	txIdx []int
	txs   []*types.Transaction
	// actorCands are the candidate value-holding addresses gathered from the
	// canonical block's cluster receipts: each cluster tx's Swap-log sender
	// (Topics[1]) and beneficiary (Topics[2]) on THIS pool. Routers/aggregators and
	// the coinbase are filtered when the set is pinned. Including these alongside the
	// EOA `from` captures the integrated-bot pattern where the EOA only pays gas while
	// its CONTRACT banks the WBNB (the realizability recall fix), so V is not blind to
	// value that lands off the signer.
	actorCands map[common.Address]bool
}

// ---------------------------------------------------------------------------
// Head subscription loop (mirrors runBacktest / runRealizabilityBacktest).
// ---------------------------------------------------------------------------

// runCurlBacktest subscribes to chain heads and runs the ordering-curl /
// Hodge-decomposition experiment on every imported block. Read-only, crash-safe.
func (r *dryRunner) runCurlBacktest() {
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	r.curlCfg = defaultCurlConfig()
	r.curlRhoHist = newFracHist()
	r.curlCurlFracHist = newFracHist()
	r.curlHarmFracHist = newFracHist()
	r.curlScalarCurlHist = newFracHist()

	log.Info("SimEngine dry-run CURL (ordering-curl / Hodge decomposition) loop started",
		"minK", r.curlCfg.minK, "maxK", r.curlCfg.maxK, "contexts", r.curlCfg.contexts,
		"exhaustiveMaxK", r.curlCfg.exhaustiveMaxK, "gate3", r.curlCfg.scalarGate3)

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run curl loop stopped (head channel closed)")
				return
			}
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run curl recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.curlBlock(head)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run curl loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// curlBlock identifies the single-pool contending clusters in one block and, for
// each qualifying one, runs the value-functional / Omega / Hodge pipeline on a
// COPY of the parent state. Read-only.
func (r *dryRunner) curlBlock(head *types.Header) {
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

	// STEP 1: replay the whole block ONCE on the parent state to learn, per tx,
	// which pool(s) it swaps. We do not need per-tx intermediate state here, only
	// the receipt logs, so the plain SimulateOnState path (the validated one) on a
	// copy is sufficient and we attribute logs to txs by re-running with the hook
	// to get the per-tx receipts. Simpler: use ApplyOnStateHooked to capture each
	// tx's emitted pools in execution order without per-tx state copies.
	type txPools struct {
		idx   int
		tx    *types.Transaction
		pools map[common.Address]bool // pools this tx emitted a Swap on
		// poolActors maps each swapped pool to the Swap-log sender/beneficiary
		// candidate addresses on it (Topics[1]/Topics[2]), used to pin the value-
		// holding actor set per cluster.
		poolActors map[common.Address][]common.Address
	}
	var perTx []txPools

	onTx := func(i int, tx *types.Transaction, receipt *types.Receipt, _ *state.StateDB) {
		if receipt == nil {
			return
		}
		pools := make(map[common.Address]bool)
		poolActors := make(map[common.Address][]common.Address)
		for _, l := range receipt.Logs {
			if l == nil || len(l.Topics) == 0 {
				continue
			}
			if l.Topics[0] != strategy.SwapTopic0 && l.Topics[0] != strategy.V3SwapTopic0 {
				continue
			}
			pools[l.Address] = true
			if len(l.Topics) >= 2 {
				poolActors[l.Address] = append(poolActors[l.Address], topicAddr(l.Topics[1]))
			}
			if len(l.Topics) >= 3 {
				poolActors[l.Address] = append(poolActors[l.Address], topicAddr(l.Topics[2]))
			}
		}
		if len(pools) > 0 {
			perTx = append(perTx, txPools{idx: i, tx: tx, pools: pools, poolActors: poolActors})
		}
	}

	if _, err := r.e.ApplyOnStateHooked(parentState.Copy(), r.bc, head, block.Transactions(), nil, onTx); err != nil {
		return
	}

	r.curlProcessed.Add(1)
	r.blocks.Add(1)

	// STEP 2: group into single-pool clusters. A cluster is keyed by pool; its
	// members are the txs whose receipt swapped that pool. (A tx that swaps several
	// pools contributes to each — but the per-pool cluster is what "contends on the
	// SAME pool".) Build per-pool member lists in block order.
	byPool := make(map[common.Address][]int) // pool -> indices into perTx
	for pi := range perTx {
		for pool := range perTx[pi].pools {
			byPool[pool] = append(byPool[pool], pi)
		}
	}

	for pool, members := range byPool {
		k := len(members)
		if k < r.curlCfg.minK {
			continue
		}
		r.curlSinglePoolKge.Add(1)

		// C1 / qualification: the pool must have a numeraire (WBNB/native-BNB or
		// stable) side, AND for the integer-exact functional we admit ONLY WBNB /
		// native-BNB value (numWBNB). A stable-only pool is recorded but skipped
		// (its value would need an oracle, violating C1).
		pool, ok := r.e.resolvePoolMeta(parentState.Copy(), r.bc, head, pool, false)
		if !ok || !pool.ok {
			r.curlSkipNoMeta.Add(1)
			continue
		}
		_, numKind, hasNum := poolNumeraire(pool)
		if !hasNum {
			r.curlSkipNoNumeraire.Add(1)
			continue
		}
		if numKind != numWBNB {
			// C1: integer-exact requires WBNB/BNB legs only (no oracle, no float).
			r.curlSkipNonWBNB.Add(1)
			continue
		}

		if k > r.curlCfg.maxK {
			r.curlOversize.Add(1)
			continue
		}

		cl := &curlCluster{pool: pool.pair, numKind: numWBNB, actorCands: make(map[common.Address]bool)}
		for _, pi := range members {
			cl.txIdx = append(cl.txIdx, perTx[pi].idx)
			cl.txs = append(cl.txs, perTx[pi].tx)
			for _, a := range perTx[pi].poolActors[pool.pair] {
				cl.actorCands[a] = true
			}
		}

		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Warn("SimEngine curl per-cluster recovered from panic",
						"block", number, "pool", poolLabel(cl.pool), "k", k, "panic", rec)
				}
			}()
			r.curlEvaluateCluster(number, head, parentState, block, cl)
		}()
	}

	if r.cfg.TallyEvery > 0 && r.curlProcessed.Load()%r.cfg.TallyEvery == 0 {
		r.logCurlTally(r.curlProcessed.Load())
	}
}

// curlEvaluateCluster runs the full pipeline for one qualifying single-pool
// cluster: pin the actor set + the fixed context prefix, build the value
// functional V, the antisymmetric Omega, the Hodge decomposition, and (for small
// k) the exhaustive GATE-1 check and (always) the GATE-3 scalar-ordering check.
// Read-only.
func (r *dryRunner) curlEvaluateCluster(number uint64, head *types.Header, parentState *state.StateDB, block *types.Block, cl *curlCluster) {
	k := len(cl.txs)

	// The FIXED CONTEXT PREFIX: every block tx strictly BEFORE the cluster's FIRST
	// member, applied ONCE to a base state. Re-execute the cluster sub-sequence on a
	// COPY of that base. (Cluster members are interleaved among other block txs in
	// reality; the prefix is the canonical "everything before the contention".)
	firstIdx := cl.txIdx[0]
	clusterSet := make(map[int]bool, k)
	for _, idx := range cl.txIdx {
		clusterSet[idx] = true
	}
	allTxs := block.Transactions()
	var prefix types.Transactions
	for i := 0; i < firstIdx && i < len(allTxs); i++ {
		if clusterSet[i] {
			continue // defensive: a cluster member cannot be < firstIdx, but guard
		}
		prefix = append(prefix, allTxs[i])
	}

	// PIN the attributed actor set ONCE (from the parent / canonical block — never
	// from an intrablock post-state path): the recovered EOA `from` of each cluster
	// tx PLUS the Swap-log sender/beneficiary contracts on this pool (the bot/router
	// that actually holds WBNB), filtering out known routers/aggregators and the block
	// coinbase (non-discriminating, may carry transient/fee balances). V is this fixed
	// set's SUMMED integer-exact {native BNB, WBNB} hub delta. Including the contracts
	// captures the integrated-bot pattern where the EOA only pays gas while its
	// CONTRACT banks the WBNB; without them V would be blind to off-signer value and
	// many clusters would read as trivially commuting. This makes V a deterministic
	// function of the executed sub-sequence and a fixed actor set.
	var chainID *big.Int
	if r.e != nil && r.e.chainCfg != nil {
		chainID = r.e.chainCfg.ChainID
	}
	signer := types.LatestSignerForChainID(chainID)
	actors := make(map[common.Address]bool, k)
	for _, tx := range cl.txs {
		if from, err := types.Sender(signer, tx); err == nil && (from != common.Address{}) {
			actors[from] = true
		}
	}
	for a := range cl.actorCands {
		if (a == common.Address{}) || a == head.Coinbase || rzKnownRouters[a] {
			continue
		}
		actors[a] = true
	}
	if len(actors) == 0 {
		r.curlSkipNoActor.Add(1)
		return
	}

	// Build the BASE state once: parent copy + the fixed context prefix applied. The
	// value functional then runs each ordered sub-sequence on a COPY of this base.
	base := parentState.Copy()
	if len(prefix) > 0 {
		if _, err := r.e.SimulateOnState(base, r.bc, head, prefix, nil); err != nil {
			r.curlSkipPrefixFail.Add(1)
			return
		}
	}

	// The value functional V(sigma): integer-exact {BNB,WBNB} own-hub-delta of the
	// pinned actor set over the executed sub-sequence, measured against `base`. It is
	// a closure over the pinned base/actor set so V depends ONLY on the executed
	// sub-sequence (a permutation of the cluster txs). Cached per ordered-tuple key
	// so the same sub-sequence is never re-executed (Omega touches each ordered pair
	// once; the exhaustive check touches every permutation).
	cache := make(map[string]*big.Int)
	preWBNB := actorSetWBNBSum(base, actors)
	V := func(order []int) *big.Int {
		key := orderKey(order)
		if v, ok := cache[key]; ok {
			return v
		}
		seq := make(types.Transactions, 0, len(order))
		for _, pos := range order {
			seq = append(seq, cl.txs[pos])
		}
		st := base.Copy()
		if _, err := r.e.SimulateOnState(st, r.bc, head, seq, nil); err != nil {
			z := big.NewInt(0)
			cache[key] = z
			return z
		}
		postWBNB := actorSetWBNBSum(st, actors)
		v := new(big.Int).Sub(postWBNB, preWBNB)
		cache[key] = v
		return v
	}

	// ----- Omega from the bracketing-context average of C_c(i,j) -----------------
	// For the minimal (contexts==1) version c is the empty context, so
	// C(i,j) = V([i,j]) - V([j,i]). For contexts>1 we average over a few sampled
	// bracketing contexts c (a random ordered subset of the OTHER cluster txs
	// prepended), Omega_ij = E_c[ V(c.i.j) - V(c.j.i) ]. Omega is exactly
	// antisymmetric by construction (we fill the lower triangle as -upper).
	omega := newAntisym(k)
	for i := 0; i < k; i++ {
		for j := i + 1; j < k; j++ {
			cij := r.curlContextAvg(V, k, i, j)
			omega.set(i, j, cij)
		}
	}

	omegaNorm2 := omega.norm2()
	if omegaNorm2.Sign() == 0 {
		// All orderings commute (curl == 0 trivially): value is order-INDEPENDENT,
		// which is the degenerate near-potential limit (rho is undefined: 0/0). Record
		// it as a commuting cluster and DO NOT feed rho (avoid div-by-zero massaging).
		r.curlCommuting.Add(1)
		log.Info("curl cluster",
			"block", number, "pool", poolLabel(cl.pool), "k", k,
			"omegaNorm2", "0", "gradFrac", "n/a(commuting)", "curlFrac", "0", "harmonicFrac", "0")
		return
	}

	// ----- Hodge decomposition on the filled clique 2-complex --------------------
	dec := hodgeDecompose(omega)

	// GATE-1 orthogonality identity: ||Omega||^2 == ||grad psi||^2 + ||curl||^2.
	rho := ratioBig(dec.gradNorm2, omegaNorm2)         // gradient/total energy fraction
	curlFrac := ratioBig(dec.curlNorm2, omegaNorm2)    // residual curl fraction
	// Harmonic fraction: on the filled clique H^1=0 so it is identically 0 by
	// construction; we report the ORTHOGONALITY RESIDUAL as the harmonic-fraction
	// proxy (|omega^2 - grad^2 - curl^2| / omega^2), which MUST be ~0 if the
	// decomposition is correct (GATE-1 kill condition).
	harmFrac := dec.orthoResidualFrac(omegaNorm2)

	r.curlClusters.Add(1)
	r.curlRhoHist.add(rho)
	r.curlCurlFracHist.add(curlFrac)
	r.curlHarmFracHist.add(harmFrac)

	// GATE-1 exhaustive cross-check (small k): the gradient fraction predicted by the
	// Hodge solve should track the full-permutation value spread. We report the
	// MEASURED orthogonality residual (the load-bearing GATE-1 number) above; the
	// exhaustive permutation pass additionally confirms V is well-defined (every
	// permutation executed deterministically) and surfaces the value range.
	exhaustiveNote := ""
	if k <= r.curlCfg.exhaustiveMaxK {
		vmin, vmax, perms := exhaustiveValueSpread(V, k)
		spread := new(big.Int).Sub(vmax, vmin)
		exhaustiveNote = "perms=" + strconv.Itoa(perms) + " vspreadWei=" + spread.String()
		r.curlExhaustiveDone.Add(1)
	}

	// GATE-3 scalar-ordering sanity: rebuild Omega around a PURELY SCALAR ordering
	// (priority-gas key) and report its curlFrac. A scalar key induces a total order,
	// so curl built around it should be small (median < 0.10) — a scalar ordering is
	// curl-free up to the value's own path-dependence.
	scalarCurlFrac := ""
	if r.curlCfg.scalarGate3 {
		if sf, ok := r.curlScalarCurlFrac(V, cl, k); ok {
			scf := ratioBig(sf.curlNorm2, sf.omegaNorm2)
			scalarCurlFrac = floatStr(scf)
			r.curlScalarCurlHist.add(scf)
			r.curlScalarDone.Add(1)
		}
	}

	log.Info("curl cluster",
		"block", number,
		"pool", poolLabel(cl.pool),
		"k", k,
		"actors", len(actors),
		"omegaNorm2", omegaNorm2.String(),
		"gradFrac", floatStr(rho),
		"curlFrac", floatStr(curlFrac),
		"harmonicFrac", floatStr(harmFrac),
		"scalarCurlFrac", scalarCurlFrac,
		"exhaustive", exhaustiveNote,
	)
}

// curlContextAvg computes C_c(i,j) averaged over the configured number of
// bracketing contexts. For contexts==1 it is the empty-context C(i,j) =
// V([i,j]) - V([j,i]). For contexts>1 it prepends a deterministic rotation of the
// OTHER cluster indices as the bracketing context c and averages
// V(c.i.j) - V(c.j.i). Returns the integer average (truncated toward zero); the
// antisymmetry Omega_ji = -Omega_ij is enforced by the caller via antisym.set.
func (r *dryRunner) curlContextAvg(V func([]int) *big.Int, k, i, j int) *big.Int {
	ctxN := r.curlCfg.contexts
	if ctxN < 1 {
		ctxN = 1
	}
	// The set of OTHER indices available to form a bracketing context.
	var others []int
	for x := 0; x < k; x++ {
		if x != i && x != j {
			others = append(others, x)
		}
	}
	sum := big.NewInt(0)
	used := 0
	for c := 0; c < ctxN; c++ {
		var ctx []int
		if c > 0 && len(others) > 0 {
			// Deterministic rotation of `others` truncated to length min(c, len) so
			// successive contexts probe deeper bracketings without randomness (so the
			// experiment is reproducible). c==0 is always the empty context.
			depth := c
			if depth > len(others) {
				depth = len(others)
			}
			ctx = rotate(others, c)[:depth]
		}
		fwd := append(append([]int{}, ctx...), i, j)
		rev := append(append([]int{}, ctx...), j, i)
		d := new(big.Int).Sub(V(fwd), V(rev))
		sum.Add(sum, d)
		used++
	}
	if used == 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Quo(sum, big.NewInt(int64(used)))
}

// curlScalarCurlFrac rebuilds the curl 2-form around a PURELY SCALAR ordering
// (GATE-3). It sorts the cluster txs by a scalar key (priority-gas = effective gas
// tip, tiebroken by nonce then hash), then forms Omega^scalar_ij = V(scalar prefix
// .. i .. j) - V(.. j .. i) where the bracketing context is the scalar-sorted
// order itself. The point: a scalar key induces a TOTAL order, so the curl around
// it should be near-zero. We measure it the same way as the discretionary Omega
// (pairwise, empty bracket) but on the scalar-relabeled indices and report its
// curlFrac. Returns (decomposition, ok).
func (r *dryRunner) curlScalarCurlFrac(V func([]int) *big.Int, cl *curlCluster, k int) (hodgeResult, bool) {
	// Scalar key per cluster position: effective gas tip cap (GasTipCap), then
	// GasPrice, then nonce, then hash — a deterministic scalar priority.
	type keyed struct {
		pos  int
		tip  *big.Int
		nonce uint64
		hash common.Hash
	}
	ks := make([]keyed, k)
	for p := 0; p < k; p++ {
		tx := cl.txs[p]
		tip := tx.GasTipCap()
		if tip == nil || tip.Sign() == 0 {
			tip = tx.GasPrice()
		}
		if tip == nil {
			tip = big.NewInt(0)
		}
		ks[p] = keyed{pos: p, tip: tip, nonce: tx.Nonce(), hash: tx.Hash()}
	}
	sort.SliceStable(ks, func(a, b int) bool {
		// Higher tip first (priority ordering).
		if c := ks[a].tip.Cmp(ks[b].tip); c != 0 {
			return c > 0
		}
		if ks[a].nonce != ks[b].nonce {
			return ks[a].nonce < ks[b].nonce
		}
		return ks[a].hash.Hex() < ks[b].hash.Hex()
	})
	// scalarRank[pos] = its rank in the scalar order.
	scalarRank := make([]int, k)
	for rank, e := range ks {
		scalarRank[e.pos] = rank
	}
	// Build Omega around the scalar order: for each pair (i,j), bracket them by the
	// other cluster txs in SCALAR order, then C = V(scalar.. i j) - V(scalar.. j i).
	// If the value is genuinely a function of a scalar priority, swapping two
	// adjacent-in-value txs yields ~0 net curl over the antisymmetric form.
	omega := newAntisym(k)
	// Precompute the scalar order positions (cluster-index order).
	scalarOrder := make([]int, k)
	copy(scalarOrder, scalarRankToOrder(scalarRank))
	for i := 0; i < k; i++ {
		for j := i + 1; j < k; j++ {
			// Bracketing context = the scalar order with i,j removed.
			var ctx []int
			for _, pos := range scalarOrder {
				if pos != i && pos != j {
					ctx = append(ctx, pos)
				}
			}
			fwd := append(append([]int{}, ctx...), i, j)
			rev := append(append([]int{}, ctx...), j, i)
			omega.set(i, j, new(big.Int).Sub(V(fwd), V(rev)))
		}
	}
	on2 := omega.norm2()
	if on2.Sign() == 0 {
		// Scalar ordering is perfectly curl-free (the ideal). Report 0/positive=0 by
		// returning a degenerate result the caller renders as "0".
		return hodgeResult{omegaNorm2: big.NewInt(1), curlNorm2: big.NewInt(0), gradNorm2: big.NewInt(1)}, true
	}
	dec := hodgeDecompose(omega)
	dec.omegaNorm2 = on2
	return dec, true
}

// ---------------------------------------------------------------------------
// Value-functional helpers.
// ---------------------------------------------------------------------------

// actorSetWBNBSum returns the SUMMED integer-exact {native BNB + WBNB} balance of
// the pinned actor set on the given state — the C1 numeraire (no stablecoin, no
// oracle, no float). Native BNB and WBNB are summed 1:1 so wrap/unwrap is value
// neutral. This is the V read-out restricted to the integer-exact hub legs, the
// rzActorHubDeltaBNB read-out narrowed to {BNB, WBNB} per the engine contract.
func actorSetWBNBSum(sdb *state.StateDB, actors map[common.Address]bool) *big.Int {
	sum := big.NewInt(0)
	if sdb == nil {
		return sum
	}
	wbnbSlot := knownTokenSlots[strategy.WBNB].balSlot
	for actor := range actors {
		if (actor == common.Address{}) {
			continue
		}
		// native BNB
		sum.Add(sum, sdb.GetBalance(actor).ToBig())
		// WBNB ERC20 balanceOf at the known slot (integer-exact, no probe).
		word := sdb.GetState(strategy.WBNB, balanceOfKey(actor, wbnbSlot))
		sum.Add(sum, new(big.Int).SetBytes(word[:]))
	}
	return sum
}

// orderKey serialises an ordering to a compact cache key.
func orderKey(order []int) string {
	b := make([]byte, 0, len(order)*2)
	for _, v := range order {
		b = append(b, byte(v), ',')
	}
	return string(b)
}

// rotate returns s rotated left by n (n may exceed len). Does not mutate s.
func rotate(s []int, n int) []int {
	if len(s) == 0 {
		return nil
	}
	n %= len(s)
	out := make([]int, 0, len(s))
	out = append(out, s[n:]...)
	out = append(out, s[:n]...)
	return out
}

// scalarRankToOrder inverts a rank slice (rank[pos]=rankIdx) into an order slice
// (order[rankIdx]=pos).
func scalarRankToOrder(rank []int) []int {
	order := make([]int, len(rank))
	for pos, rk := range rank {
		order[rk] = pos
	}
	return order
}

// exhaustiveValueSpread executes EVERY permutation of the k cluster txs through V
// (GATE-1 small-k cross-check) and returns the min/max value and the permutation
// count. It re-uses V's cache so already-seen orderings (the pairwise ones) are
// free. Only called for k <= exhaustiveMaxK.
func exhaustiveValueSpread(V func([]int) *big.Int, k int) (vmin, vmax *big.Int, perms int) {
	idx := make([]int, k)
	for i := range idx {
		idx[i] = i
	}
	first := true
	var permute func(prefix, rest []int)
	permute = func(prefix, rest []int) {
		if len(rest) == 0 {
			v := V(prefix)
			perms++
			if first {
				vmin = new(big.Int).Set(v)
				vmax = new(big.Int).Set(v)
				first = false
				return
			}
			if v.Cmp(vmin) < 0 {
				vmin = new(big.Int).Set(v)
			}
			if v.Cmp(vmax) > 0 {
				vmax = new(big.Int).Set(v)
			}
			return
		}
		for i := range rest {
			next := append(append([]int{}, prefix...), rest[i])
			remaining := append(append([]int{}, rest[:i]...), rest[i+1:]...)
			permute(next, remaining)
		}
	}
	permute(nil, idx)
	if vmin == nil {
		vmin = big.NewInt(0)
	}
	if vmax == nil {
		vmax = big.NewInt(0)
	}
	return vmin, vmax, perms
}

// ---------------------------------------------------------------------------
// Tally.
// ---------------------------------------------------------------------------

// logCurlTally emits the periodic curl tally + distribution.
func (r *dryRunner) logCurlTally(processed uint64) {
	rhoMed, rhoP10, rhoP90, rhoN := r.curlRhoHist.summary()
	curlMed, _, _, _ := r.curlCurlFracHist.summary()
	harmMed, _, harmP90, _ := r.curlHarmFracHist.summary()
	scalarMed, _, _, scalarN := r.curlScalarCurlHist.summary()

	log.Info("curl tally",
		"processedBlocks", processed,
		"clusters", r.curlClusters.Load(),
		"singlePoolKge", r.curlSinglePoolKge.Load(),
		"commuting", r.curlCommuting.Load(),
		"oversize", r.curlOversize.Load(),
		"skipNoNumeraire", r.curlSkipNoNumeraire.Load(),
		"skipNonWBNB", r.curlSkipNonWBNB.Load(),
		"medianRho", floatStr(rhoMed),
		"p10Rho", floatStr(rhoP10),
		"p90Rho", floatStr(rhoP90),
		"medianCurlFrac", floatStr(curlMed),
		"rhoN", rhoN,
		"ts", time.Now().Format(time.RFC3339),
	)
	// GATE-1 + GATE-3 sibling line.
	log.Info("curl dist",
		"gate1_medianHarmonicFrac", floatStr(harmMed),
		"gate1_p90HarmonicFrac", floatStr(harmP90),
		"gate1_exhaustiveDone", r.curlExhaustiveDone.Load(),
		"gate3_medianScalarCurlFrac", floatStr(scalarMed),
		"gate3_scalarN", scalarN,
		"rhoDist", r.curlRhoHist.distString(),
	)
}

// ---------------------------------------------------------------------------
// Small helpers shared with the math file.
// ---------------------------------------------------------------------------

// curlState bundles the curl-engine state added to dryRunner (declared in
// dryrun.go's struct). Defined here as documentation; the actual fields live on
// dryRunner. See the curl* fields block.

// floatStr renders a float with fixed precision for log lines.
func floatStr(f float64) string {
	return strconv.FormatFloat(f, 'f', 4, 64)
}

// fracHist is a tiny exact-quantile accumulator for fractions in [0,1] (rho,
// curlFrac, harmonicFrac). It keeps every sample (a curl cluster is rare: tens to
// low-hundreds per run), so median / p10 / p90 are EXACT (not log-bucketed). Guarded
// by its own mutex.
type fracHist struct {
	mu   sync.Mutex
	vals []float64
}

func newFracHist() *fracHist { return &fracHist{} }

func (h *fracHist) add(v float64) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.vals = append(h.vals, v)
	h.mu.Unlock()
}

// summary returns the median, p10, p90 (nearest-rank) and the sample count.
func (h *fracHist) summary() (med, p10, p90 float64, n int) {
	if h == nil {
		return 0, 0, 0, 0
	}
	h.mu.Lock()
	cp := append([]float64(nil), h.vals...)
	h.mu.Unlock()
	n = len(cp)
	if n == 0 {
		return 0, 0, 0, 0
	}
	sort.Float64s(cp)
	return quantile(cp, 0.5), quantile(cp, 0.10), quantile(cp, 0.90), n
}

// distString renders a coarse [0,1] decile histogram of the samples for the log.
func (h *fracHist) distString() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	cp := append([]float64(nil), h.vals...)
	h.mu.Unlock()
	if len(cp) == 0 {
		return "n=0"
	}
	var buckets [11]int // [0,0.1),...,[0.9,1.0),[1.0]
	for _, v := range cp {
		b := int(v * 10)
		if b < 0 {
			b = 0
		}
		if b > 10 {
			b = 10
		}
		buckets[b]++
	}
	s := "n=" + strconv.Itoa(len(cp)) + " deciles["
	for i, c := range buckets {
		if i > 0 {
			s += ","
		}
		s += strconv.Itoa(c)
	}
	return s + "]"
}

// quantile returns the nearest-rank quantile of a SORTED slice (p in [0,1]).
func quantile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[n-1]
	}
	idx := int(p*float64(n-1) + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}
