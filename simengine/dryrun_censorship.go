// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_censorship.go is the CENSORSHIP-DIFFERENTIAL (D) detector, selectable
// with SIMENGINE_DRYRUN=censorship. It is the structural twin of the in-block
// counterfactual (dryrun_realizability.go) but adds ONE new data dimension that
// detector lacks: the PUBLIC MEMPOOL, which supplies the treatment-assignment
// variable (a public opportunity was INCLUDED vs DROPPED by the builder).
//
// It estimates the ONE point-identified estimand:
//
//   D = E[ V_i(1) - V_i(0) | o_i is a PUBLIC opportunity, o_i orthogonal to
//          private flow, builder DROPPED it ]
//     = the receipt-exact BNB value the builder leaves on the table by DROPPING
//       (not including / not internalising) public, private-flow-orthogonal,
//       net-of-gas-profitable opportunities.
//
// THE GOVERNING RULE (applied at every ambiguous decision): OVER-stating D is
// the dangerous direction (it would falsely suggest contestable public value
// exists — false hope). So at EVERY ambiguity — is it really public? really
// available at seal? really DROPPED (not replaced / nonced-out / mined /
// invalid)? really orthogonal to private flow? really net-of-gas profitable?
// really left-on-the-table (not captured by a landed competitor)? — we EXCLUDE
// and increment a csSkip* counter. D-hat (csDhatWei) is a strict LOWER bound.
// False negatives in D are acceptable; false positives are forbidden.
//
// It does NOT touch applyOnState / SimulateOnState semantics / selftest.go (the
// validated 5/5 receipt-exact path). It rides ONLY on ApplyOnStateHooked and
// SimulateOnState read-only on state.Copy(), exactly like the sandwich-any and
// realizability modes. Strictly read-only (never commits, never submits), every
// block and per-candidate unit wrapped in defer/recover, and a complete no-op
// unless SIMENGINE_DRYRUN=censorship.
package simengine

import (
	"math"
	"math/big"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/strategy"
)

// ---------------------------------------------------------------------------
// Env knobs.
// ---------------------------------------------------------------------------

// censorshipConfig holds the censorship-specific knobs. The valuation side
// reuses the SIMENGINE_SANDWICH_* / SIMENGINE_DRYRUN_GASPRICES knobs unchanged.
type censorshipConfig struct {
	ledgerCap int           // hard cap on the public ledger (FIFO eviction)
	ledgerTTL time.Duration // evict firstSeen older than this (rolling residual)
	// sealMargin is subtracted from the LOCALLY-observed seal time to form the
	// availability cutoff. firstSeen (local wall-clock at ingest) and the seal time
	// (local wall-clock at ChainHeadEvent receipt) are on the SAME clock; the margin
	// absorbs the gap between our head-observed instant and the proposer's true seal
	// instant. Larger => fewer admitted candidates => UNDER-states D (the safe dir).
	sealMargin time.Duration
	// leadFloor is the HARD minimum builder lead time: a candidate whose
	// (sealTime - firstSeen) is below this is excluded as "arrived too late for the
	// builder to include" (BSC builders cut bundle ingestion ~100-400ms pre-seal).
	leadFloor time.Duration
	// settleBlocks (K) is the SETTLE WINDOW: a candidate that passes every gate at
	// block N is NOT credited to D immediately. It is enqueued and finalized at
	// height N+K; it counts toward D ONLY IF it is still ABSENT from the canonical
	// chain after K subsequent blocks (i.e. it was never mined in (N, N+K]). A
	// candidate mined within the window was merely PENDING/delayed-inclusion, not
	// censored, and is discarded (csSkipMinedLater) — that conflation was the
	// over-statement this knob fixes. Larger K => stricter "never mined" proof =>
	// fewer credited drops => UNDER-states D (the safe direction).
	settleBlocks uint64
}

func defaultCensorshipConfig() censorshipConfig {
	c := censorshipConfig{
		ledgerCap:  200_000,
		ledgerTTL:  30 * time.Second,
		sealMargin:   0,                      // applied to the local seal anchor (see csSealCutoff)
		leadFloor:    400 * time.Millisecond, // conservative BSC builder bundle-cutoff
		settleBlocks: 256,                    // ~a couple minutes at BSC block time; a drop counts only if still un-mined after this many blocks
	}
	if v := os.Getenv("SIMENGINE_CS_LEDGER_CAP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.ledgerCap = n
		}
	}
	if v := os.Getenv("SIMENGINE_CS_LEDGER_TTL_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.ledgerTTL = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("SIMENGINE_CS_SEAL_MARGIN_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.sealMargin = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv("SIMENGINE_CS_LEAD_FLOOR_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.leadFloor = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv("SIMENGINE_CS_SETTLE_BLOCKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.settleBlocks = uint64(n)
		}
	}
	return c
}

// csSealCutoff returns the availability cutoff on the LOCAL wall-clock (the same
// clock as pubTx.firstSeen). It is the locally-observed seal instant (when we
// received the ChainHeadEvent) minus the safety margin. We DELIBERATELY do not use
// head.Time (proposer-set on-chain seconds, a different clock that a validator can
// post-date to make late-arriving txs look pre-seal); reconciling the two clocks is
// the whole point of this cutoff. A degenerate (zero) localSeal falls back to the
// on-chain header second so the detector still functions (and any error there only
// shifts the cutoff conservatively). head is accepted for that fallback only.
func (r *dryRunner) csSealCutoff(head *types.Header, localSeal time.Time) time.Time {
	if localSeal.IsZero() {
		if head != nil {
			return time.Unix(int64(head.Time), 0)
		}
		return time.Time{}
	}
	margin := time.Duration(0)
	if r.csCfg.sealMargin > 0 {
		margin = r.csCfg.sealMargin
	}
	return localSeal.Add(-margin)
}

// censorshipProbeLogCap bounds how many per-opportunity covariate lines we emit
// is unbounded by design (one per surviving dropped opp / included control) —
// these are the matching keys for offline caliper-matching and are the load-
// bearing output, so they are NOT capped here.

// ---------------------------------------------------------------------------
// Public-mempool snapshot layer (the new data dimension).
//
// Mechanism: HYBRID — a continuous SubscribeTransactions ingest into a rolling,
// sender/nonce-indexed in-memory public ledger, read per head. We do NOT poll
// Pending() as the primary source: it returns only the executable front-of-nonce
// per account under a MinTip/BaseFee filter (it silently hides gapped/queued
// public opportunities) and at BSC's sub-second block time a poll cadence cannot
// reliably capture "the set as it was just before seal." The event feed is the
// lowest-latency public-arrival signal and — load-bearing — gives a first-seen
// timestamp per hash, the field on which the public/availability tests hinge.
// ---------------------------------------------------------------------------

// pubTx is one public-mempool transaction observed via the event feed, with its
// first-seen timestamp (the public+availability margin) and its recovered sender
// (recovered at INGEST, off the head critical path).
type pubTx struct {
	hash      common.Hash
	tx        *types.Transaction // keep the FULL tx; we re-validate + re-execute it
	from      common.Address     // recovered at ingest (zero/unrecoverable => non-attributable)
	nonce     uint64
	firstSeen time.Time
}

// pubLedger is the rolling, sender/nonce-indexed public-tx ledger. It is fed by
// the ingest goroutine and read (under RLock) per head. TTL/cap eviction keeps it
// O(public residual) — small, because most BSC flow is private.
type pubLedger struct {
	mu            sync.RWMutex
	byHash        map[common.Hash]*pubTx
	bySenderNonce map[common.Address]map[uint64]*pubTx
	ring          []common.Hash // FIFO for cap/TTL eviction
	cap           int
	ttl           time.Duration
}

func newPubLedger(cap int, ttl time.Duration) *pubLedger {
	if cap <= 0 {
		cap = 200_000
	}
	return &pubLedger{
		byHash:        make(map[common.Hash]*pubTx),
		bySenderNonce: make(map[common.Address]map[uint64]*pubTx),
		cap:           cap,
		ttl:           ttl,
	}
}

// insert records a public tx, evicting the oldest entries beyond cap or older
// than ttl. A zero `from` is non-attributable and is NOT indexed by sender/nonce
// (it can never anchor a nonce check), but is still tracked by hash so it can
// match a sealed inclusion (only ever EXCLUDING it from D, never crediting it).
func (l *pubLedger) insert(p *pubTx) {
	if p == nil || (p.hash == common.Hash{}) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, exists := l.byHash[p.hash]; exists {
		return // keep the FIRST-seen record (earliest firstSeen is the load-bearing margin)
	}
	l.byHash[p.hash] = p
	if (p.from != common.Address{}) {
		m := l.bySenderNonce[p.from]
		if m == nil {
			m = make(map[uint64]*pubTx)
			l.bySenderNonce[p.from] = m
		}
		// Keep the earliest-seen tx for this (sender,nonce); a later repricing under
		// a different hash is a replacement (handled by the sealed-block index).
		if _, ok := m[p.nonce]; !ok {
			m[p.nonce] = p
		}
	}
	l.ring = append(l.ring, p.hash)
	l.evictLocked(time.Now())
}

// evictLocked drops entries beyond the hard cap and those older than ttl. Caller
// holds the write lock.
func (l *pubLedger) evictLocked(now time.Time) {
	// Cap eviction (FIFO).
	for len(l.ring) > l.cap {
		h := l.ring[0]
		l.ring = l.ring[1:]
		l.removeLocked(h)
	}
	// TTL eviction (the ring is in arrival order, so prefix-trim the oldest).
	if l.ttl > 0 {
		cutoff := now.Add(-l.ttl)
		for len(l.ring) > 0 {
			h := l.ring[0]
			p := l.byHash[h]
			if p == nil { // already removed; drop the dangling ring entry
				l.ring = l.ring[1:]
				continue
			}
			if p.firstSeen.After(cutoff) {
				break // ring is ordered; nothing older remains
			}
			l.ring = l.ring[1:]
			l.removeLocked(h)
		}
	}
}

// removeLocked deletes one tx from both indices. Caller holds the write lock.
func (l *pubLedger) removeLocked(h common.Hash) {
	p := l.byHash[h]
	if p == nil {
		return
	}
	delete(l.byHash, h)
	if (p.from != common.Address{}) {
		if m := l.bySenderNonce[p.from]; m != nil {
			if cur, ok := m[p.nonce]; ok && cur != nil && cur.hash == h {
				delete(m, p.nonce)
			}
			if len(m) == 0 {
				delete(l.bySenderNonce, p.from)
			}
		}
	}
}

// has reports whether a hash is in the ledger.
func (l *pubLedger) has(h common.Hash) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.byHash[h]
	return ok
}

// firstSeen returns the first-seen time of a hash (and whether it is present).
func (l *pubLedger) firstSeenOf(h common.Hash) (time.Time, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if p, ok := l.byHash[h]; ok {
		return p.firstSeen, true
	}
	return time.Time{}, false
}

// snapshotBefore returns a COPY of every pubTx whose firstSeen is strictly before
// `t`. The firstSeen<t gate is the load-bearing public+availability margin (a tx
// that arrived after seal but before we processed H is conservatively excluded
// here — under-stating D, the safe direction). Returned slice is owned by the
// caller (the *pubTx pointers are immutable after insert).
func (l *pubLedger) snapshotBefore(t time.Time) []*pubTx {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*pubTx, 0, len(l.byHash))
	for _, p := range l.byHash {
		if p.firstSeen.Before(t) {
			out = append(out, p)
		}
	}
	return out
}

// size returns the current ledger depth (a congestion covariate).
func (l *pubLedger) size() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.byHash)
}

// ---------------------------------------------------------------------------
// Settle-window layer (the deferred-drop finalization).
//
// THE FLAW THIS FIXES: the original gate labelled a candidate "dropped" the
// instant it was simply NOT IN THE FLAGGING BLOCK N. A chain check proved that
// 14 of 15 such "drops" were actually MINED 21-123 blocks LATER — they were just
// PENDING, not censored. So the old D-hat conflated DELAYED-INCLUSION with
// CENSORED value (an over-statement, the forbidden direction).
//
// THE FIX: a candidate counts toward D ONLY IF it is still ABSENT from the
// canonical chain after K (settleBlocks) subsequent blocks. Instead of crediting
// at block N, the candidate is ENQUEUED with finalizeHeight = N + K. At each new
// head height h we finalize every pending-drop with finalizeHeight <= h: if its
// hash was mined in (N, h] (looked up in the rolling minedAt index) it was
// delayed-inclusion → discard + csSkipMinedLater; if it was NEVER mined → it is
// genuinely abandoned/censored → NOW credit V_i (the value frozen on block-N
// post-block state, never re-valued) to D-hat. All read-only, lower-bound dir.
// ---------------------------------------------------------------------------

// minedIndex is the rolling tx-hash -> mined-height index. It is populated from
// every canonical sealed block the detector sees (the same block whose txs the
// per-head pass already iterates) and is the lookup the settle-window finalizer
// uses to decide delayed-inclusion (mined within (N, N+K]) vs genuine censorship
// (never mined). Entries older than (K + buffer) blocks are evicted to bound
// memory. Single-writer (the head goroutine) but lock-guarded for safety.
type minedIndex struct {
	mu      sync.Mutex
	at      map[common.Hash]uint64
	retain  uint64 // evict entries with height < (maxHeight - retain)
	maxSeen uint64
}

func newMinedIndex(retain uint64) *minedIndex {
	if retain == 0 {
		retain = 512
	}
	return &minedIndex{at: make(map[common.Hash]uint64), retain: retain}
}

// recordBlock records every tx hash in a sealed block at its height, then evicts
// entries older than the retention window. Keeps the EARLIEST height for a hash
// (a tx mined once cannot be "mined again later" — a re-org resurrection must not
// post-date it), which biases toward classifying a candidate as mined (NOT
// crediting it), the safe lower-bound direction.
func (m *minedIndex) recordBlock(height uint64, txs types.Transactions) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tx := range txs {
		if tx == nil {
			continue
		}
		h := tx.Hash()
		if prev, ok := m.at[h]; !ok || height < prev {
			m.at[h] = height
		}
	}
	if height > m.maxSeen {
		m.maxSeen = height
	}
	// Evict entries older than the retention window (bounded memory). Only worth a
	// sweep once we have advanced past the window.
	if m.maxSeen > m.retain {
		cutoff := m.maxSeen - m.retain
		for h, ht := range m.at {
			if ht < cutoff {
				delete(m.at, h)
			}
		}
	}
}

// minedHeight returns the height at which a hash was mined (and whether it is in
// the index). Absence means EITHER never mined OR evicted past the retention
// window; the finalizer only ever queries hashes whose mined-or-not is decided
// within the window, so eviction never causes a false "never mined".
func (m *minedIndex) minedHeight(h common.Hash) (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ht, ok := m.at[h]
	return ht, ok
}

// pendingDrop is a candidate that passed EVERY gate at its flagging block (it is
// a profitable, orthogonal, dropped-from-N, not-already-credited public opp) but
// is NOT yet credited to D: it is held until finalizeHeight (= flagBlock+K) so we
// can prove it was never mined in (flagBlock, finalizeHeight]. V_i is FROZEN at
// enqueue time (valued on the block-N post-block state) and never re-valued.
type pendingDrop struct {
	hash           common.Hash
	from           common.Address
	nonce          uint64
	flagBlock      uint64
	finalizeHeight uint64
	netBNBWei      *big.Int // V_i (frozen at enqueue; the value the builder forwent)
	grossBNBWei    *big.Int
	gasUnits       uint64
	isV3           bool
	dexLabel       string
	// Covariate + logging carriers (kept verbatim so the finalized "censorship
	// drop" line is identical to the old immediate one — same matching keys).
	headSnapshot *types.Header
	opp          *csOpp
	cov          csCovariates
	wbnbUSD      float64
	firstSeen    time.Time
}

// ---------------------------------------------------------------------------
// Detector entry point: ingest goroutine + head loop.
// ---------------------------------------------------------------------------

// runCensorshipDetector launches the public-mempool ingest goroutine and the
// per-head censorship pass. It blocks on the head subscription (meant to run in
// its own goroutine) and returns on node shutdown. Read-only, crash-safe.
func (r *dryRunner) runCensorshipDetector(pool *txpool.TxPool) {
	if pool == nil {
		log.Warn("SimEngine dry-run censorship mode disabled: nil txpool")
		return
	}

	r.swCfg = defaultSandwichConfig()
	r.csCfg = defaultCensorshipConfig()
	r.cspub = newPubLedger(r.csCfg.ledgerCap, r.csCfg.ledgerTTL)
	// The mined-hash index must retain at least the full settle window (so a drop
	// finalized at N+K can still find a tx mined anywhere in (N, N+K]) plus a buffer
	// for re-org slack; 2*K + 256 is a comfortable bound that stays O(K blocks).
	r.csMined = newMinedIndex(2*r.csCfg.settleBlocks + 256)

	var chainID *big.Int
	if r.e != nil && r.e.chainCfg != nil {
		chainID = r.e.chainCfg.ChainID
	}
	signer := types.LatestSignerForChainID(chainID)

	log.Info("SimEngine dry-run CENSORSHIP-DIFFERENTIAL (D) loop started",
		"flashBps", r.swCfg.flashBps, "minVictimUSD", r.swCfg.minVictimUSD,
		"ledgerCap", r.csCfg.ledgerCap, "ledgerTTLsec", int(r.csCfg.ledgerTTL/time.Second),
		"settleBlocks", r.csCfg.settleBlocks)

	// --- Ingest goroutine: subscribe to public pending txs, recover sender off
	// the head critical path, and roll them into the ledger. reorgs=false so a
	// resurrected tx never masquerades as fresh-public. ---
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Warn("SimEngine censorship ingest recovered from panic", "panic", rec)
			}
		}()
		txCh := make(chan core.NewTxsEvent, 256)
		txSub := pool.SubscribeTransactions(txCh, false)
		defer txSub.Unsubscribe()
		for {
			select {
			case ev, ok := <-txCh:
				if !ok {
					log.Info("SimEngine censorship ingest stopped (tx channel closed)")
					return
				}
				now := time.Now()
				for _, tx := range ev.Txs {
					if tx == nil {
						continue
					}
					from, sErr := types.Sender(signer, tx)
					if sErr != nil {
						from = common.Address{} // non-attributable; tracked by hash only
					}
					r.cspub.insert(&pubTx{
						hash:      tx.Hash(),
						tx:        tx,
						from:      from,
						nonce:     tx.Nonce(),
						firstSeen: now,
					})
				}
			case err := <-txSub.Err():
				log.Info("SimEngine censorship ingest stopped (subscription error)", "err", err)
				return
			}
		}
	}()

	// --- Main goroutine: per imported head, run the censorship pass. ---
	ch := make(chan core.ChainHeadEvent, 16)
	sub := r.bc.SubscribeChainHeadEvent(ch)
	defer sub.Unsubscribe()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				log.Info("SimEngine dry-run censorship loop stopped (head channel closed)")
				return
			}
			// LOCALLY-observed seal time: the wall-clock instant we received this
			// head, on the SAME clock as pubTx.firstSeen. This is the load-bearing
			// availability anchor — NOT head.Time (proposer-set on-chain seconds, a
			// different clock that a validator can post-date to make late-arriving txs
			// look pre-seal). A tx is "available before seal" iff it arrived before we
			// saw the block (minus a safety margin).
			localSeal := time.Now()
			head := ev.Header
			if head == nil {
				continue
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Warn("SimEngine dry-run censorship recovered from panic", "block", head.Number, "panic", rec)
					}
				}()
				r.censorshipBlock(head, localSeal, signer)
			}()
		case err := <-sub.Err():
			log.Info("SimEngine dry-run censorship loop stopped (subscription error)", "err", err)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Per-block in-memory structures.
// ---------------------------------------------------------------------------

// csReplayResult carries everything the single sealed-block replay produces that
// the gates need: the decoded Swap legs + per-tx ledgers (for the landed-capture
// scan, reused verbatim from realizability), the set of (from,nonce) inclusion
// slots, and the earliest tx-index at which a PRIVATE (not public-seen) sealed tx
// wrote each pool (the orthogonality index).
type csReplayResult struct {
	legs             []rzSwapLeg
	ledgers          []rzTxLedger
	includedHashes   map[common.Hash]bool
	includedSlots    map[csSenderNonce]common.Hash // (from,nonce) -> sealed hash
	privatePoolTouch map[common.Address]int        // pool -> earliest PRIVATE write tx-index
	anyPoolTouch     map[common.Address]int        // pool -> earliest ANY-sealed-tx write tx-index (public OR private)
	// postBlockState is the state AFTER the ENTIRE sealed block has executed (parent
	// state with all of block B's txs applied, on a copy — never committed). GATE 3
	// values each dropped candidate ALONE on a COPY of THIS state: an opportunity
	// counts toward D only if it survives the whole builder block still-profitable
	// (genuinely left-on-table). If the block — the builder's own txs, private flow,
	// or anyone — closed it, the candidate reverts / nets <= 0 here and is excluded.
	// This removes the isolation-overstatement (valuing the candidate on the stale
	// pre-seal parent counts opps the block already closed, e.g. the builder's own
	// internalized arb) and is conservative (it may UNDER-state D, the safe direction).
	postBlockState *state.StateDB
}

// csSenderNonce keys the replacement-detection index.
type csSenderNonce struct {
	from  common.Address
	nonce uint64
}

// ---------------------------------------------------------------------------
// Per-slot processing.
// ---------------------------------------------------------------------------

// censorshipBlock processes one sealed head: it replays the sealed block ONCE on
// the sealing parent state (building the inclusion / orthogonality / landed-
// capture indices), snapshots the public drop candidates, and runs each through
// the conjunctive gate. Read-only, crash-safe.
func (r *dryRunner) censorshipBlock(head *types.Header, localSeal time.Time, signer types.Signer) {
	number := head.Number.Uint64()

	// 3.1 Resolve the sealed block and the sealing parent + parent state.
	block := r.bc.GetBlockByHash(head.Hash())
	if block == nil {
		return
	}

	// SETTLE WINDOW step 1: record every tx hash in THIS sealed block into the
	// rolling mined-hash index BEFORE anything else (and outside the parent-state /
	// reorg-guard early-returns, so the index never has a hole for a canonical block
	// even when state is pruned). The finalizer queries this index to tell a delayed-
	// inclusion drop (mined within its window) from a genuinely-censored one.
	if r.csMined != nil {
		r.csMined.recordBlock(number, block.Transactions())
	}
	// SETTLE WINDOW step 2: finalize every pending-drop whose window has now elapsed
	// (finalizeHeight <= this height). Done up-front, on every head, independent of
	// whether the rest of this block's pass succeeds.
	r.csFinalizePending(number)

	parent := r.bc.GetHeaderByHash(head.ParentHash)
	if parent == nil {
		return
	}
	parentState, err := r.bc.StateAt(parent.Root)
	if err != nil {
		return // parent state pruned during catch-up — skip silently (ledger is empty anyway).
	}

	// Reorg guard: only process heads that are ancestor-or-equal of the current
	// tip at process time, so an orphaned block can never manufacture phantom D.
	if cur := r.bc.CurrentHeader(); cur != nil {
		if canon := r.bc.GetHeaderByNumber(number); canon == nil || canon.Hash() != head.Hash() {
			return // this head is not on the canonical chain (orphaned) — skip.
		}
	}

	// 3.2 + 3.3 ONE hooked replay of the sealed block on parent state, building the
	// inclusion indices, the orthogonality (private-pool-touch) index, and the
	// decoded legs + per-tx ledgers for the landed-capture scan.
	rep := r.censorshipReplay(head, localSeal, block, parentState, signer)
	if rep == nil {
		return
	}

	// 3.4 Snapshot the candidate dropped public set: public txs that arrived BEFORE
	// our locally-observed seal instant (minus a safety margin), whose hash is NOT in
	// the sealed block. The cutoff is on the LOCAL wall-clock (same clock as
	// firstSeen) — never the proposer-controlled head.Time — and a tx that arrived
	// after we saw the block is conservatively excluded (under-stating D).
	cutoff := r.csSealCutoff(head, localSeal)
	candidates := r.cspub.snapshotBefore(cutoff)

	wbnbUSD := liveWbnbPriceUSD(parentState)
	dust := r.rzDustBNB(wbnbUSD)
	landed := r.detectLandedSandwiches(rep.legs, rep.ledgers, head, dust, wbnbUSD)

	// Per-block de-dup: at most one opportunity per (pool, victim-side) per slot.
	creditedPoolSide := make(map[csPoolSide]bool)

	for _, c := range candidates {
		if c == nil || c.tx == nil {
			continue
		}
		// HARD lead-time gate: the builder must have had at least leadFloor between
		// the tx's first-seen and the (local) seal instant to physically include it.
		// A short-lead candidate "arrived too late", not "was dropped" — exclude.
		if r.csCfg.leadFloor > 0 && localSeal.Sub(c.firstSeen) < r.csCfg.leadFloor {
			r.csSkipShortLead.Add(1)
			continue
		}
		cc := c
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Warn("SimEngine censorship per-candidate recovered from panic",
						"block", number, "tx", cc.hash, "panic", rec)
				}
			}()
			r.censorshipCandidate(number, head, localSeal, parentState, rep, landed, wbnbUSD, creditedPoolSide, cc)
		}()
	}

	n := r.csProcessed.Add(1)
	r.blocks.Add(1)
	if r.cfg.TallyEvery > 0 && n%r.cfg.TallyEvery == 0 {
		r.logCensorshipTally(n)
	}
}

// csPoolSide keys the per-slot opportunity de-dup (one per pool + victim-side).
type csPoolSide struct {
	pool common.Address
	side bool
}

// censorshipReplay runs the single hooked replay and returns the inclusion /
// orthogonality / landed-scan indices. Read-only.
func (r *dryRunner) censorshipReplay(head *types.Header, localSeal time.Time, block *types.Block, parentState *state.StateDB, signer types.Signer) *csReplayResult {
	rep := &csReplayResult{
		includedHashes:   make(map[common.Hash]bool),
		includedSlots:    make(map[csSenderNonce]common.Hash),
		privatePoolTouch: make(map[common.Address]int),
		anyPoolTouch:     make(map[common.Address]int),
	}

	// preState tracks the EXACT pre-tx state (post-state of the previously applied
	// tx); starts as the parent copy and advances after every tx (mirror
	// realizability's hook). It is unused by the censorship gates (which value on
	// the pre-SEAL parent state), but kept so buildTxLedger sees correct deltas.
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

		// Inclusion indices.
		rep.includedHashes[tx.Hash()] = true
		if (from != common.Address{}) {
			rep.includedSlots[csSenderNonce{from: from, nonce: tx.Nonce()}] = tx.Hash()
		}

		// (A) Per-tx hub ledger + decoded legs (for detectLandedSandwiches reuse).
		led := r.buildTxLedger(i, tx, receipt, from, head, victimPreState, statedb)
		rep.ledgers = append(rep.ledgers, led)
		rep.legs = append(rep.legs, decodeRzLegs(i, tx.Hash(), from, receipt.Logs)...)

		// (B) ORTHOGONALITY index: a sealed tx is PUBLIC-SEEN iff its hash is in the
		// ledger with firstSeen < the LOCAL seal cutoff; every sealed tx that is NOT
		// public-seen is treated as PRIVATE flow (the conservative superset — over-
		// labeling private can only EXCLUDE more, never fabricate orthogonality).
		// Record the EARLIEST tx-index at which a PRIVATE sealed tx wrote each pool.
		isPublicSeen := false
		if fs, ok := r.cspub.firstSeenOf(tx.Hash()); ok && fs.Before(r.csSealCutoff(head, localSeal)) {
			isPublicSeen = true
		}
		for _, l := range receipt.Logs {
			if l == nil || len(l.Topics) == 0 {
				continue
			}
			if !csIsPoolWriteTopic(l.Topics[0]) {
				continue
			}
			// Record EVERY sealed write (public OR private): a dropped opp valued on
			// the block-top parent state is only realisable if its pools survive the
			// realized in-block trajectory. Any earlier sealed write (even public)
			// would have moved the pool before the dropped tx could execute, so the
			// block-top valuation is stale — EXCLUDE (the safe direction).
			if cur, ok := rep.anyPoolTouch[l.Address]; !ok || i < cur {
				rep.anyPoolTouch[l.Address] = i
			}
			// The PRIVATE-only index (a sealed tx that is NOT public-seen is treated as
			// private flow — the conservative superset) drives the strict SUTVA gate.
			if !isPublicSeen {
				if cur, ok := rep.privatePoolTouch[l.Address]; !ok || i < cur {
					rep.privatePoolTouch[l.Address] = i
				}
			}
		}
	}

	// applyState is the live copy ApplyOnStateHooked mutates in place; after the
	// loop it holds the EXACT post-sealed-block state. We freeze a COPY of it into
	// the replay result so GATE 3 can value each candidate on the post-block state
	// (the opportunity must survive the whole builder block to count toward D).
	applyState := parentState.Copy()
	if _, err := r.e.ApplyOnStateHooked(applyState, r.bc, head, block.Transactions(), nil, onTx); err != nil {
		return nil
	}
	rep.postBlockState = applyState.Copy()
	return rep
}

// csPoolWriteTopics are the AMM events that constitute a "write" to a pool's
// price/liquidity state for the orthogonality test: Swap (V2/V3), Sync, Mint,
// Burn. Any private tx emitting one of these on a pool entangles that pool's
// state with private flow.
func csIsPoolWriteTopic(t common.Hash) bool {
	switch t {
	case strategy.SwapTopic0, strategy.V3SwapTopic0, strategy.SyncTopic0, rzMintTopic0, rzBurnTopic0:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// The conjunctive gate (§4). Count toward D only if ALL pass; else EXCLUDE +
// increment a csSkip* counter (the safe direction).
// ---------------------------------------------------------------------------

// csVerdict is the outcome of the (non-EVM) structural gates for one candidate.
type csVerdict uint8

const (
	csVerdictDropped          csVerdict = iota // passed all structural gates AND was dropped -> counts toward D
	csVerdictIncludedControl                   // passed value/orthogonality/not-captured but builder INCLUDED it -> control (D=0)
	csVerdictSkipNonceMoved                    // account nonce moved past the candidate (superseded/mined)
	csVerdictSkipNonceGap                      // nonce gap (predecessor missing)
	csVerdictSkipInvalidAtSeal                 // non-attributable sender / failed static+stateful validation
	csVerdictSkipReplaced                      // (sender,nonce) slot filled under a different hash (repricing)
	csVerdictSkipNonOrthogonal                 // opp pool written by a private sealed tx (SUTVA fails)
	csVerdictSkipAlreadyCaptured               // a landed competitor already took the value
)

// csStructuralVerdict applies the NON-EVM gates (1 availability/nonce, 2 replaced,
// 4 orthogonality, 5 already-captured + included-vs-dropped classification) for a
// candidate whose opportunity pool/side have ALREADY been resolved by the EVM
// valuation (GATE 3). It is pure (no atomics, no logging, no EVM) so the gate
// logic is unit-testable in isolation. validAtSeal carries the result of the
// static+stateful validation the caller ran on the live parent state.
//
// GOVERNING RULE: any ambiguity returns a Skip* verdict (EXCLUDE), so D-hat stays
// a strict lower bound.
func csStructuralVerdict(c *pubTx, parentNonce uint64, validAtSeal bool, rep *csReplayResult, oppPool common.Address, oppSide bool, pathPools []common.Address, landed []landedSandwich) csVerdict {
	// ---- GATE 1: AVAILABLE-AT-SEAL (valid + correct nonce + funded).
	if c == nil || (c.from == common.Address{}) {
		return csVerdictSkipInvalidAtSeal // non-attributable sender can't anchor a nonce check.
	}
	if parentNonce > c.nonce {
		return csVerdictSkipNonceMoved // superseded/mined earlier (account nonce advanced).
	}
	if c.nonce > parentNonce {
		return csVerdictSkipNonceGap // predecessor missing -> not executable at seal.
	}
	if !validAtSeal {
		return csVerdictSkipInvalidAtSeal
	}

	// ---- GATE 2: DROPPED, not replaced.
	includedThisHash := rep.includedHashes[c.hash]
	slot := csSenderNonce{from: c.from, nonce: c.nonce}
	sealedHash, slotFilled := rep.includedSlots[slot]
	if slotFilled && sealedHash != c.hash {
		return csVerdictSkipReplaced // repricing/replacement filled the slot.
	}
	dropped := !includedThisHash && !slotFilled

	// ---- GATE 4: ORTHOGONAL to private flow (the SUTVA gate), applied to EVERY pool
	// in the candidate's path (not just the chosen opp pool): a multi-hop candidate
	// whose value depends on a pool that private flow wrote is still entangled with
	// private flow. We additionally EXCLUDE if ANY pool in the path was written by
	// ANY sealed tx (public OR private) before/within the block — the block-top
	// parent valuation is stale once a sealed tx moved that pool, so the opp may not
	// have survived the realized in-block trajectory (issue: stale-reserve upward
	// bias not caught by the private-only gate). Both directions UNDER-state D.
	checkPools := pathPools
	if len(checkPools) == 0 {
		checkPools = []common.Address{oppPool}
	}
	for _, p := range checkPools {
		if (p == common.Address{}) {
			continue
		}
		if _, touched := rep.privatePoolTouch[p]; touched {
			return csVerdictSkipNonOrthogonal
		}
		if _, touched := rep.anyPoolTouch[p]; touched {
			return csVerdictSkipNonOrthogonal
		}
	}

	// ---- GATE 5: NOT ALREADY CAPTURED (left-on-table, not taken).
	if csAlreadyCaptured(landed, oppPool, oppSide) {
		return csVerdictSkipAlreadyCaptured
	}

	if !dropped {
		return csVerdictIncludedControl // builder included a comparable public opp (control, D=0).
	}
	return csVerdictDropped
}

// censorshipCandidate runs ONE drop candidate through the five-gate conjunction.
// A candidate that passes all five is a confirmed DROPPED public orthogonal
// profitable left-on-table opportunity; its V_i = eval.NetProfit is added to
// D-hat. A candidate that is INCLUDED (passed value/orthogonality/not-captured
// but the slot was in the sealed block) is counted as a control (contributes 0).
//
// The non-EVM gates are delegated to the pure csStructuralVerdict so the gate
// logic is unit-testable; GATE 3 (the receipt-exact EVM valuation) runs here.
func (r *dryRunner) censorshipCandidate(number uint64, head *types.Header, localSeal time.Time, parentState *state.StateDB, rep *csReplayResult, landed []landedSandwich, wbnbUSD float64, creditedPoolSide map[csPoolSide]bool, c *pubTx) {
	signer := types.LatestSignerForChainID(func() *big.Int {
		if r.e != nil && r.e.chainCfg != nil {
			return r.e.chainCfg.ChainID
		}
		return nil
	}())

	// Cheap pre-gates (availability) BEFORE the expensive EVM valuation, so an
	// unavailable candidate is excluded without a sim. These mirror GATE 1 but are
	// applied here (and re-checked structurally) to short-circuit; the skip counter
	// is incremented from the final verdict so each EXCLUDE is counted exactly once.
	if c == nil || c.tx == nil {
		return
	}
	if (c.from == common.Address{}) {
		r.csSkipInvalidAtSeal.Add(1)
		return
	}
	parentNonce := parentState.GetNonce(c.from)
	if parentNonce > c.nonce {
		r.csSkipNonceMoved.Add(1)
		return
	}
	if c.nonce > parentNonce {
		r.csSkipNonceGap.Add(1)
		return
	}
	validAtSeal := r.csValidateAtSeal(c.tx, head, parentState, signer) == nil
	if !validAtSeal {
		r.csSkipInvalidAtSeal.Add(1)
		return
	}

	// ---- GATE 3: IS A PUBLIC OPPORTUNITY (net-of-gas profitable), valued receipt-
	// exactly on the POST-SEALED-BLOCK state (parent + all of block B's txs). A
	// dropped candidate counts toward D ONLY IF it is still profitable AFTER the
	// whole real sealed block has executed — so opps the block already closed (incl.
	// the builder's OWN internalized arb) are excluded. Run BEFORE the cheap
	// structural gates 2/4/5 because those need the resolved opp pool/side.
	opp := r.censorshipValueOpp(number, head, rep.postBlockState, c, wbnbUSD)
	if opp == nil {
		return // a csSkip* was already incremented inside, or it is simply not an opp.
	}
	// censorshipValueOpp incremented csPublicOppsSeen for the decoded swap opp; a
	// non-nil return means it ALSO cleared the net-of-gas+inclusion-floor test, so
	// it is profitable. (publicOppsSeen >= profitable: the gap is opps that decoded
	// but were closed-by-block / not net-profitable.)
	r.csProfitable.Add(1)

	// Per-slot de-dup (at most one opportunity per pool + victim-side).
	ps := csPoolSide{pool: opp.pool, side: opp.token0Side}
	if creditedPoolSide[ps] {
		return
	}

	// ---- Structural gates 1(re-confirm)/2/4/5 + included-vs-dropped classification.
	verdict := csStructuralVerdict(c, parentNonce, validAtSeal, rep, opp.pool, opp.token0Side, opp.pathPools, landed)
	switch verdict {
	case csVerdictSkipNonceMoved:
		r.csSkipNonceMoved.Add(1)
		return
	case csVerdictSkipNonceGap:
		r.csSkipNonceGap.Add(1)
		return
	case csVerdictSkipInvalidAtSeal:
		r.csSkipInvalidAtSeal.Add(1)
		return
	case csVerdictSkipReplaced:
		r.csSkipReplaced.Add(1)
		return
	case csVerdictSkipNonOrthogonal:
		r.csSkipNonOrthogonal.Add(1)
		return
	case csVerdictSkipAlreadyCaptured:
		r.csOrthogonal.Add(1) // it WAS orthogonal; it just got captured.
		r.csSkipAlreadyCaptured.Add(1)
		return
	}

	// Passed orthogonality + not-captured.
	r.csOrthogonal.Add(1)
	creditedPoolSide[ps] = true
	cov := r.censorshipCovariates(head, localSeal, parentState, c, opp, wbnbUSD)

	if verdict == csVerdictIncludedControl {
		// INCLUDED comparable IN BLOCK N (the control group, treatment T=1): the builder
		// DID include this public opportunity in the very block it flagged. Contributes 0
		// to D; logged for offline matching. This is one of the two legitimate control
		// arms; the other (mined within the settle window) is credited at finalize.
		r.csIncludedComparable.Add(1)
		r.logCensorshipOpp("censorship included", number, head, c, opp, cov, wbnbUSD)
		return
	}

	// Confirmed DROPPED-FROM-BLOCK-N public orthogonal profitable opportunity. It is
	// NOT yet credited to D: under the SETTLE WINDOW it could still be merely PENDING
	// (delayed-inclusion), not censored. We ENQUEUE it (V_i FROZEN on the block-N
	// post-block state — never re-valued) and finalize at height N + settleBlocks.
	//
	// CROSS-BLOCK DE-DUP first: a public tx that stays pending across many heads would
	// otherwise be flagged "dropped" on every head it lingers, enqueuing the SAME
	// opportunity repeatedly. Mark it credited (claimed) at most once over the whole
	// run; a repeat is the SAME opportunity, not a new one. Claiming it once (and
	// finalizing once) strictly under-states D — the safe direction.
	if r.csMarkCreditedDrop(c.hash) {
		r.csSkipAlreadyCredited.Add(1)
		return
	}
	r.csDropped.Add(1) // "confirmed dropped-from-N" (pre-settle); D-hat is credited only at finalize.
	r.csEnqueuePending(&pendingDrop{
		hash:           c.hash,
		from:           c.from,
		nonce:          c.nonce,
		flagBlock:      number,
		finalizeHeight: number + r.csCfg.settleBlocks,
		netBNBWei:      new(big.Int).Set(opp.netBNBWei),
		grossBNBWei:    new(big.Int).Set(opp.grossBNBWei),
		gasUnits:       opp.gasUnits,
		isV3:           opp.isV3,
		dexLabel:       opp.dexLabel,
		headSnapshot:   head,
		opp:            opp,
		cov:            cov,
		wbnbUSD:        wbnbUSD,
		firstSeen:      c.firstSeen,
	})
}

// csEnqueuePending appends a passed-all-gates candidate to the settle-window queue.
// It is held until finalizeHeight and only THEN credited to D (iff still un-mined).
func (r *dryRunner) csEnqueuePending(p *pendingDrop) {
	if p == nil {
		return
	}
	r.csPendingMu.Lock()
	r.csPending = append(r.csPending, p)
	depth := uint64(len(r.csPending))
	r.csPendingMu.Unlock()
	for {
		cur := r.csPendingMax.Load()
		if depth <= cur || r.csPendingMax.CompareAndSwap(cur, depth) {
			break
		}
	}
}

// csFinalizePending settles every pending-drop whose finalize window has elapsed
// (finalizeHeight <= h, the current head height). For each:
//
//   - if its sender-nonce has ADVANCED past it on the canonical chain (the slot was
//     filled by a later tx, or the account moved on) -> superseded -> discard
//     (csSkipSuperseded); the opp it represented was not abandoned.
//   - else if its hash was MINED in (flagBlock, h] (looked up in csMined) -> it was
//     merely PENDING, i.e. DELAYED-INCLUSION, NOT censored -> discard
//     (csSkipMinedLater) AND count it as an included-comparable control (it WAS
//     eventually mined, so it is a legitimate T=1 control for the matching estimator).
//   - else (NEVER mined within the window) -> it is genuinely abandoned/censored ->
//     NOW credit V_i (the frozen block-N value) to D-hat, add to the dist, and emit
//     the "censorship drop" line.
//
// Read-only, lower-bound direction (any ambiguity discards rather than credits).
func (r *dryRunner) csFinalizePending(h uint64) {
	r.csPendingMu.Lock()
	ready := r.csPending[:0:0]
	keep := r.csPending[:0]
	for _, p := range r.csPending {
		if p != nil && p.finalizeHeight <= h {
			ready = append(ready, p)
		} else {
			keep = append(keep, p)
		}
	}
	r.csPending = keep
	r.csPendingMu.Unlock()

	for _, p := range ready {
		if p == nil {
			continue
		}
		number := p.flagBlock

		// Superseded check: if the sender's CURRENT canonical nonce is past the
		// candidate's nonce, that (sender,nonce) slot was filled by SOME later tx (the
		// account moved on), so the candidate was replaced/superseded — not censored.
		if r.bc != nil && (p.from != common.Address{}) {
			if cur := r.bc.CurrentHeader(); cur != nil {
				if st, err := r.bc.StateAt(cur.Root); err == nil && st != nil {
					if st.GetNonce(p.from) > p.nonce {
						r.csSkipSuperseded.Add(1)
						continue
					}
				}
			}
		}

		// Defensive: with no mined index we cannot prove "never mined", so we must NOT
		// credit (the safe lower-bound direction is to discard the ambiguous case).
		if r.csMined == nil {
			r.csSkipMinedLater.Add(1)
			continue
		}

		// Delayed-inclusion check: was the candidate's hash mined within its settle
		// window (flagBlock, h]? The mined index keeps the EARLIEST height; a hash mined
		// AT or BEFORE flagBlock cannot be this still-pending drop (it was dropped-from-N
		// at flagBlock), so we require minedHeight > flagBlock.
		if mh, ok := r.csMined.minedHeight(p.hash); ok && mh > p.flagBlock && mh <= h {
			// Mined later -> it was PENDING, not censored. The dominant over-statement.
			r.csSkipMinedLater.Add(1)
			// It WAS eventually included, so it is a legitimate included-comparable control
			// (treatment T=1) for the offline matching estimator: it passed the same
			// profitability + orthogonality gates and landed (just later than block N).
			r.csIncludedComparable.Add(1)
			r.logCensorshipOpp("censorship mined-later", number, p.headSnapshot, &pubTx{hash: p.hash, from: p.from, nonce: p.nonce, firstSeen: p.firstSeen}, p.opp, p.cov, p.wbnbUSD)
			continue
		}

		// NEVER mined within the settle window -> genuinely abandoned / censored. NOW
		// credit the frozen block-N value to D-hat (the conservative lower bound).
		r.csDhatCount.Add(1)
		r.rzAddBig(&r.csDhatWei, p.netBNBWei)
		r.csDist.Add(absBig(p.netBNBWei), strategy.SandwichGasUnits(p.isV3), weiToBNBFloat(p.netBNBWei), p.dexLabel, 2)
		r.logCensorshipOpp("censorship drop", number, p.headSnapshot, &pubTx{hash: p.hash, from: p.from, nonce: p.nonce, firstSeen: p.firstSeen}, p.opp, p.cov, p.wbnbUSD)
	}
}

// csValidateAtSeal applies the static (ValidateTransaction: type/blob/intrinsic/
// signature/gas-cap) and a self-contained stateful (nonce/gap/funded) validation
// against the sealing parent. Returns the first error (=> EXCLUDE). We do NOT use
// ValidateTransactionWithState's overdraft callbacks (they need the live pool's
// per-account expenditure bookkeeping); the self-contained nonce/balance check
// here is the conservative subset that matters for "executable at seal".
func (r *dryRunner) csValidateAtSeal(tx *types.Transaction, head *types.Header, parentState *state.StateDB, signer types.Signer) error {
	var cfg *params.ChainConfig
	if r.e != nil {
		cfg = r.e.chainCfg
	}
	if cfg == nil || head == nil {
		return errCensorNoConfig
	}
	opts := &txpool.ValidationOptions{
		Config: cfg,
		// Accept every standard type; type/fork gating is enforced inside
		// ValidateTransaction against the head rules. Blob (type 3) txs are
		// non-internalisable public swap opportunities here, so excluding them via a
		// zero bit is the conservative choice — but we accept them and let the value
		// gate (no numeraire / unfundable) drop them, keeping the funnel honest.
		Accept:       1<<types.LegacyTxType | 1<<types.AccessListTxType | 1<<types.DynamicFeeTxType | 1<<types.BlobTxType | 1<<types.SetCodeTxType,
		MaxSize:      txMaxSize,
		MinTip:       big.NewInt(0),
		MaxBlobCount: 1 << 20, // never the binding constraint here; intrinsic/funded gates dominate.
	}
	if err := txpool.ValidateTransaction(tx, head, signer, opts); err != nil {
		return err
	}
	// Self-contained stateful check (nonce already verified by the caller's
	// GetNonce gates; re-confirm funded for the exact cost at seal).
	from, err := types.Sender(signer, tx)
	if err != nil {
		return err
	}
	balance := parentState.GetBalance(from).ToBig()
	if balance.Cmp(tx.Cost()) < 0 {
		return errCensorUnderfunded
	}
	return nil
}

var (
	errCensorNoConfig    = newSandwichErr("censorship: nil chain config")
	errCensorUnderfunded = newSandwichErr("censorship: underfunded at seal")
)

// txMaxSize mirrors the legacy/blobpool max tx size used by ValidateTransaction's
// size sanity check. 4*32KB is a safe upper bound that never wrongly rejects a
// real swap tx (the binding gates are intrinsic gas / funded, not size).
const txMaxSize = 4 * 32 * 1024

// ---------------------------------------------------------------------------
// GATE 3 valuation — receipt-exact V_i on the POST-SEALED-BLOCK state.
//
// THE ESTIMAND (corrected). D is the value the builder FORWENT by DROPPING a
// public, private-flow-orthogonal opportunity — i.e. the OWN net profit the
// dropped tx would have realised had the builder included it. It is NOT (and must
// never be) the value an attacker could extract by SANDWICHING the dropped tx:
// sandwiching a public swap is predation ON a user, the antithesis of "value left
// on the table by not including a public opportunity", and it would OVER-state D
// in the dangerous direction (the false-hope failure the governing rule forbids).
//
// So V_i is the candidate executor's OWN summed BNB-equivalent hub-asset delta
// (its arb/backrun profit, the realizability buildTxLedger numeraire) over a
// single-tx simulation of the candidate, NET of the candidate's own gas AND a
// conservative builder inclusion-cost floor (a tx only "leaves value on the table"
// if its own profit exceeds what a realistic builder would charge to include it).
// Ordinary user swaps (negative or zero own hub delta — they SPEND the numeraire,
// they do not realise it) yield no opp and are excluded, which is exactly correct:
// a user swap is not a dropped searcher opportunity. Only self-contained arbs/
// backruns (positive own hub delta) survive.
//
// THE POST-BLOCK STATE FIX (the over-statement fix). The candidate is valued ALONE
// on a COPY of the POST-SEALED-BLOCK state (parent + ALL of block B's txs), NOT on
// the stale pre-seal parent state. Valuing in isolation on the parent counts opps
// that realistic sealed-block ordering had ALREADY closed — including by the
// builder's OWN internalized arb — manufacturing implausible "dropped" value (a
// genuinely-profitable arb left in a near-empty low-tip block that hundreds of
// public bots would instantly take). Rationale: if the opportunity survived the
// whole builder block uncaptured, it is genuinely left-on-table (censored); if the
// block — the builder's own txs, private flow, or anyone — closed it, the candidate
// reverts OR nets <= 0 post-block and is EXCLUDED (csSkipClosedByBlock). This is
// conservative: it may UNDER-state D (an opp profitable mid-block but closed by
// block-end is excluded), which is the correct safe direction. It also makes the
// sandwich-only GATE 5 redundant for arb-internalization: the post-block state
// already reflects any internalizing arb the builder ran.
// ---------------------------------------------------------------------------

// csOpp is one valued public opportunity surfaced by a candidate.
type csOpp struct {
	pool        common.Address // the candidate's largest Swap-log pool (the opp's pool, for orthogonality/dedup)
	isV3        bool
	token0Side  bool
	netBNBWei   *big.Int // V_i = own hub profit - own gas - builder-bid floor (BNB wei), strictly > 0
	grossBNBWei *big.Int // own hub profit before the candidate's own gas / bid floor (BNB wei)
	gasUnits    uint64   // the candidate's OWN gas used (receipt-exact)
	dexLabel    string
	hops        int                 // distinct Swap-log pools the candidate sim touched (path length covariate)
	numKind     numeraireKind       // numeraire of the opp pool (covariate label)
	pathPools   []common.Address    // every distinct Swap-log pool the candidate traversed (orthogonality set)
}

// censorshipValueOpp simulates the candidate ALONE on a COPY of the POST-SEALED-
// BLOCK state (parent + all of block B's txs; head-consistent block context, NOT a
// synthetic height+1 header) and values the candidate's OWN realised net profit —
// the value the builder forwent by dropping it, IFF the opportunity still survived
// the whole sealed block uncaptured. Returns nil (no opp) with the appropriate
// csSkip* incremented. This is the corrected estimand: it values what the dropped
// tx WOULD have earned on top of the realized block, never the sandwich an attacker
// could run on it, and never an opp the block already closed.
//
// postBlockState is the frozen post-sealed-block state from the per-block replay
// (rep.postBlockState). We copy it before simulating so the shared snapshot is
// never mutated (read-only, never committed).
func (r *dryRunner) censorshipValueOpp(number uint64, head *types.Header, postBlockState *state.StateDB, c *pubTx, wbnbUSD float64) *csOpp {
	if head == nil || c == nil || c.tx == nil || postBlockState == nil {
		return nil
	}

	// Single-tx detection+valuation sim on a HEAD-CONSISTENT block context: we use
	// `head` itself (NOT nextHeader/height+1) so NUMBER/TIMESTAMP/BaseFee match the
	// position at which availability claims the tx was includable, removing the
	// number/base-fee skew that a synthetic child header introduces. We value on the
	// state AFTER this single tx vs BEFORE, where BEFORE is the POST-SEALED-BLOCK
	// state: the candidate must still be profitable on top of the realized block to
	// count toward D (else the block already closed the opp -> csSkipClosedByBlock).
	preState := postBlockState.Copy()
	postState := preState.Copy()
	res, err := r.e.SimulateOnState(postState, r.bc, head, types.Transactions{c.tx}, nil)
	if err != nil || res == nil || len(res.Receipts) == 0 || len(res.Logs) == 0 {
		// Candidate reverts / yields no logs ON THE POST-BLOCK STATE: either it was
		// never an opp, or the sealed block closed it. Either way EXCLUDE; count the
		// closed-by-block case so the funnel is auditable (read-only, lower-bound dir).
		r.csSkipClosedByBlock.Add(1)
		return nil
	}
	receipt := res.Receipts[0]
	if receipt == nil || receipt.Status != types.ReceiptStatusSuccessful {
		r.csSkipClosedByBlock.Add(1)
		return nil // reverted on the post-block state — the block closed it (or it never was an opp).
	}

	// Recover the candidate's EOA `from` (the executor) and assemble the candidate's
	// path pools + the largest Swap leg (the opp pool/side for orthogonality+dedup).
	signer := types.LatestSignerForChainID(func() *big.Int {
		if r.e != nil && r.e.chainCfg != nil {
			return r.e.chainCfg.ChainID
		}
		return nil
	}())
	from, sErr := types.Sender(signer, c.tx)
	if sErr != nil {
		return nil // unattributable executor — cannot read an own-profit delta.
	}

	hopSet := make(map[common.Address]bool)
	var (
		oppPool      common.Address
		oppToken0    bool
		oppIsV3      bool
		bestLegIn    = big.NewInt(0)
		actors       = map[common.Address]bool{from: true}
		swapLegs     int // number of directional Swap legs the candidate emitted
	)
	for _, l := range res.Logs {
		if l == nil || len(l.Topics) == 0 {
			continue
		}
		if l.Topics[0] != strategy.SwapTopic0 && l.Topics[0] != strategy.V3SwapTopic0 {
			continue
		}
		swapLegs++
		hopSet[l.Address] = true
		// Candidate's beneficiary/sender topics are also candidate executors (a bot
		// contract that holds the arb profit), mirroring buildTxLedger's actor set.
		if len(l.Topics) >= 2 {
			actors[topicAddr(l.Topics[1])] = true
		}
		if len(l.Topics) >= 3 {
			actors[topicAddr(l.Topics[2])] = true
		}
		// Choose the opp pool/side as the leg with the largest input (the dominant
		// hop) for the orthogonality/dedup key.
		if pair, token0Side, amountIn, isV3, vok := decodeAnyVictim(l); vok && amountIn != nil {
			if amountIn.Cmp(bestLegIn) > 0 {
				bestLegIn = amountIn
				oppPool = pair
				oppToken0 = token0Side
				oppIsV3 = isV3
			}
		}
	}
	if (oppPool == common.Address{}) {
		return nil // no decodable directional swap leg — not a value-bearing opp.
	}

	// ---- ROUND-TRIP GATE (the gross-proceeds over-statement guard). A self-contained
	// arb/backrun — the only thing whose OWN positive hub delta is genuine MEV profit
	// the builder forwent — must both ACQUIRE and DISPOSE, i.e. emit >= 2 directional
	// Swap legs (cross-pool, or one pool in both directions). A SINGLE-leg swap is a
	// one-way trade: it BUYS a non-hub token (hub delta negative => already excluded by
	// the net gate) or SELLS one for a hub asset, in which case rzActorHubDeltaBNB reads
	// the FULL SALE PROCEEDS as a positive hub delta and mistakes it for profit. That is
	// an ordinary user swap, not a dropped searcher opportunity, and crediting it OVER-
	// states D (the forbidden direction). EXCLUDE every single-leg candidate. This is
	// conservative (a genuine single-pool arb is structurally impossible — it would just
	// round-trip the same pool at a net loss after fees — so no true opp is lost).
	if !csIsRoundTrip(swapLegs) {
		r.csSkipNotRoundTrip.Add(1)
		return nil
	}

	// FUNNEL: this candidate decoded as a directional public swap opportunity that
	// executes on the post-block state. Count it as a "public opp seen" HERE (before
	// the profitability test), so the funnel is a clean monotone chain
	// publicOppsSeen >= profitable >= orthogonal >= dropped (the caller increments
	// csProfitable only AFTER the net-of-gas test below passes). This is a pure
	// accounting move — it does NOT change the load-bearing D-hat logic.
	r.csPublicOppsSeen.Add(1)

	// V_i := the BEST (max) own net BNB-equivalent hub delta over the candidate's
	// executor set, measured on the POST-SEALED-BLOCK state (preState=post-block,
	// postState=post-block+candidate). We take the max over executors (the profit
	// accrues to ONE of from/bot-contract); a non-positive max means the candidate
	// realised no numeraire profit ON TOP OF the realized block — either an ordinary
	// user swap that SPENT the numeraire, or an arb the sealed block already closed.
	// Either way it is not a dropped opportunity that survived the block.
	ownProfit := big.NewInt(0)
	for actor := range actors {
		d := rzActorHubDeltaBNB(preState, postState, actor, wbnbUSD)
		if d.Cmp(ownProfit) > 0 {
			ownProfit = d
		}
	}
	if ownProfit.Sign() <= 0 {
		// No positive hub delta on the post-block state: the opp either never realised
		// numeraire profit or the sealed block closed it. EXCLUDE (lower-bound dir).
		r.csSkipClosedByBlock.Add(1)
		return nil
	}

	// Net of the candidate's OWN gas (receipt-exact) AND a conservative builder
	// inclusion-cost floor: gas at the conservative-HIGH price + a non-zero builder
	// bid. The value only counts as "left on the table" if it clears what a
	// realistic builder would itself charge to include the tx (per the governing
	// rule, a high gas price + positive bid UNDER-states D, the safe direction).
	ownGasBNB := big.NewInt(0)
	if receipt.EffectiveGasPrice != nil {
		ownGasBNB = new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	}
	inclusionFloor := r.csInclusionCostFloorWei(receipt.GasUsed)
	netBNB := new(big.Int).Sub(ownProfit, ownGasBNB)
	netBNB.Sub(netBNB, inclusionFloor)
	if netBNB.Sign() <= 0 {
		// Gross profit on the post-block state does not clear the candidate's own gas
		// + the conservative builder inclusion floor: not net-of-gas profitable once
		// the realized block is accounted for. EXCLUDE (lower-bound direction).
		r.csSkipNetNonPositive.Add(1)
		return nil
	}

	pathPools := make([]common.Address, 0, len(hopSet))
	for p := range hopSet {
		pathPools = append(pathPools, p)
	}

	// Numeraire label of the opp pool (best-effort, covariate only).
	numKind := numWBNB
	if pool, ok := r.e.resolvePoolMeta(preState.Copy(), r.bc, head, oppPool, oppIsV3); ok && pool.ok {
		if _, k, hasNum := poolNumeraire(pool); hasNum {
			numKind = k
		}
	}
	dexLabel := "v2_any"
	if oppIsV3 {
		dexLabel = "pancake_v3"
	}

	return &csOpp{
		pool:        oppPool,
		isV3:        oppIsV3,
		token0Side:  oppToken0,
		netBNBWei:   netBNB,
		grossBNBWei: new(big.Int).Set(ownProfit),
		gasUnits:    receipt.GasUsed,
		dexLabel:    dexLabel,
		hops:        len(hopSet),
		numKind:     numKind,
		pathPools:   pathPools,
	}
}

// csIsRoundTrip reports whether a candidate's directional-Swap-leg count is the
// structural signature of a self-contained arb/backrun (it both ACQUIRED and
// DISPOSED). A self-contained arb must emit >= 2 directional Swap legs; a single
// leg is a one-way user swap whose positive hub delta is gross sale proceeds, not
// MEV profit, and crediting it would OVER-state D (the forbidden direction). Pure
// so the round-trip over-statement guard is unit-testable in isolation.
func csIsRoundTrip(swapLegs int) bool { return swapLegs >= 2 }

// csInclusionCostFloorWei returns the conservative builder inclusion-cost floor in
// BNB wei: gasUsed * conservative-HIGH gas price + a non-zero builder bid floor.
// A dropped public opp must clear THIS hurdle (not the generous 3 gwei / 0-bid
// searcher defaults) to count toward D, so V_i is the value that exceeds what a
// realistic builder would itself charge to include the tx. Both knobs default to
// conservative-high and are env-overridable (swept as covariates offline). Per the
// governing rule, raising them UNDER-states D (the safe direction).
func (r *dryRunner) csInclusionCostFloorWei(gasUsed uint64) *big.Int {
	floor := new(big.Int).Mul(new(big.Int).SetUint64(gasUsed), csInclusionGasPriceWei)
	floor.Add(floor, csBuilderBidFloorWei)
	return floor
}

// csInclusionGasPriceWei / csBuilderBidFloorWei are the conservative-HIGH inclusion
// cost knobs (governing rule: high => under-state D). Defaults: 5 gwei effective
// inclusion price and a 0.0005 BNB builder-bid floor; both overridable via
// SIMENGINE_CS_INCLUSION_GWEI / SIMENGINE_CS_BUILDER_BID_WEI.
var (
	csInclusionGasPriceWei = func() *big.Int {
		if v := os.Getenv("SIMENGINE_CS_INCLUSION_GWEI"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
				return big.NewInt(int64(f * 1e9))
			}
		}
		return big.NewInt(5_000_000_000) // 5 gwei (conservative-high vs BSC's ~1-3 gwei)
	}()
	csBuilderBidFloorWei = func() *big.Int {
		if v := os.Getenv("SIMENGINE_CS_BUILDER_BID_WEI"); v != "" {
			if n, ok := new(big.Int).SetString(v, 10); ok && n.Sign() >= 0 {
				return n
			}
		}
		return big.NewInt(500_000_000_000_000) // 0.0005 BNB builder-bid floor
	}()
)

// csMarkCreditedDrop records a candidate hash as credited to D-hat and reports
// whether it had ALREADY been credited on an earlier head (a cross-block repeat of
// the same lingering pending candidate). Returns true if it was already credited
// (caller must skip — do not double-count the same opportunity). Lifetime-scoped,
// lock-guarded, so each unique dropped candidate adds V_i to D-hat at most once.
func (r *dryRunner) csMarkCreditedDrop(h common.Hash) (already bool) {
	r.csCreditedDropsMu.Lock()
	defer r.csCreditedDropsMu.Unlock()
	if r.csCreditedDrops == nil {
		r.csCreditedDrops = make(map[common.Hash]bool)
	}
	if r.csCreditedDrops[h] {
		return true
	}
	r.csCreditedDrops[h] = true
	return false
}

// csAlreadyCaptured reports whether a landed competitor bracket already captured
// the given pool/victim-side in the sealed block (GATE 5). A captured pool/side
// means the value was taken, not left on the table by dropping.
func csAlreadyCaptured(landed []landedSandwich, pool common.Address, token0Side bool) bool {
	for i := range landed {
		ls := &landed[i]
		if ls.pool == pool && ls.inToken0Front == token0Side {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Covariate logging (§5) — the matching keys for offline caliper-matching.
// ---------------------------------------------------------------------------

// csCovariates carries the observables an offline script pairs dropped vs
// included on, to difference out builder selection.
type csCovariates struct {
	poolReserveLog10 float64 // log10 of the numeraire reserve wei (pool depth / liquidity)
	hops             int     // path length / number of distinct Swap hops
	gas              uint64
	gasFeeCapWei     string
	gasTipCapWei     string
	blockFullness    float64 // head.GasUsed / head.GasLimit (congestion)
	ledgerSize       int     // public residual depth (congestion)
	leadTimeSec      float64 // localSeal - firstSeen (lead time the builder had, same clock)
	validator        common.Address
}

// censorshipCovariates assembles the matching-key covariates for one candidate.
func (r *dryRunner) censorshipCovariates(head *types.Header, localSeal time.Time, parentState *state.StateDB, c *pubTx, opp *csOpp, wbnbUSD float64) csCovariates {
	cov := csCovariates{
		hops:         opp.hops,
		gas:          c.tx.Gas(),
		gasFeeCapWei: bigOrZero(c.tx.GasFeeCap()).String(),
		gasTipCapWei: bigOrZero(c.tx.GasTipCap()).String(),
		ledgerSize:   r.cspub.size(),
		validator:    head.Coinbase,
	}
	if head.GasLimit > 0 {
		cov.blockFullness = float64(head.GasUsed) / float64(head.GasLimit)
	}
	// Pool depth: the numeraire reserve of the opp's pool (log10 of wei).
	rv := strategy.ReadReserves(parentState, opp.pool)
	var numReserve *big.Int
	if p, ok := globalPoolMetaCache.get(opp.pool); ok && p.ok {
		if numToken, _, hasNum := poolNumeraire(p); hasNum {
			if numToken == p.token0 {
				numReserve = rv.Reserve0
			} else {
				numReserve = rv.Reserve1
			}
		}
	}
	cov.poolReserveLog10 = log10Big(numReserve)
	// Lead time the builder had, measured on the SAME wall-clock for both anchors
	// (localSeal - firstSeen), NOT the head.Time/firstSeen cross-clock difference.
	if !c.firstSeen.IsZero() && !localSeal.IsZero() {
		cov.leadTimeSec = localSeal.Sub(c.firstSeen).Seconds()
	}
	return cov
}

// logCensorshipOpp emits one structured covariate line per surviving dropped
// opportunity (msg="censorship drop") or included control (msg="censorship
// included"), carrying the matching-key covariates and V_i (BNB wei + USD).
func (r *dryRunner) logCensorshipOpp(msg string, number uint64, head *types.Header, c *pubTx, opp *csOpp, cov csCovariates, wbnbUSD float64) {
	numKindLabel := "wbnb"
	if opp.numKind == numStable {
		numKindLabel = "stable"
	}
	log.Info(msg,
		"block", number,
		"tx", c.hash.Hex(),
		"pool", poolLabel(opp.pool),
		"dex", opp.dexLabel,
		"numeraire", numKindLabel,
		"token0Side", opp.token0Side,
		"V_BNBwei", opp.netBNBWei.String(),
		"V_USD", weiToUSD(opp.netBNBWei, wbnbUSD),
		"grossBNBwei", opp.grossBNBWei.String(),
		"poolReserveLog10", cov.poolReserveLog10,
		"hops", cov.hops,
		"gas", cov.gas,
		"gasFeeCapWei", cov.gasFeeCapWei,
		"gasTipCapWei", cov.gasTipCapWei,
		"blockFullness", cov.blockFullness,
		"ledgerSize", cov.ledgerSize,
		"leadTimeSec", cov.leadTimeSec,
		"validator", shortAddr(cov.validator),
	)
}

// ---------------------------------------------------------------------------
// Tally + dist (§6) — realizability funnel style.
// ---------------------------------------------------------------------------

// logCensorshipTally emits the censorship funnel + the dropped-D distribution.
// Crash-safe, read-only. The headline Dhat_BNB is the conservative LOWER bound
// on builder public-censorship value; it must NEVER be reported as total builder
// MEV — it is only "value of dropped public orthogonal opportunities."
func (r *dryRunner) logCensorshipTally(processed uint64) {
	r.csPendingMu.Lock()
	pendingDepth := len(r.csPending)
	r.csPendingMu.Unlock()
	log.Info("censorship tally",
		"processedBlocks", processed,
		"settleBlocks", r.csCfg.settleBlocks,
		"publicOppsSeen", r.csPublicOppsSeen.Load(),
		"orthogonal", r.csOrthogonal.Load(),
		"profitable", r.csProfitable.Load(),
		"droppedFromN", r.csDropped.Load(), // passed all gates at block N (pre-settle); D credited only after the window
		"includedComparable", r.csIncludedComparable.Load(), // control arm: mined in block N OR within the settle window (T=1)
		"csSkipMinedLater", r.csSkipMinedLater.Load(), // KEY FINDING: drops that were merely DELAYED-INCLUSION, not censored
		"pendingDrops", pendingDepth, // current settle-window queue depth
		"pendingDropsMax", r.csPendingMax.Load(),
		"Dhat_count", r.csDhatCount.Load(), // SETTLED, never-mined, genuinely-censored count ONLY
		"Dhat_BNB", r.csDhatWei.Load().String(), // SETTLED, never-mined, genuinely-censored value ONLY
		"skipSuperseded", r.csSkipSuperseded.Load(),
		"skipNonceMoved", r.csSkipNonceMoved.Load(),
		"skipNonceGap", r.csSkipNonceGap.Load(),
		"skipInvalidAtSeal", r.csSkipInvalidAtSeal.Load(),
		"skipReplaced", r.csSkipReplaced.Load(),
		"skipNoNumeraire", r.csSkipNoNumeraire.Load(),
		"skipUnfundable", r.csSkipUnfundable.Load(),
		"skipBelowThreshold", r.csSkipBelowThreshold.Load(),
		"skipNetNonPositive", r.csSkipNetNonPositive.Load(),
		"skipClosedByBlock", r.csSkipClosedByBlock.Load(),
		"skipNotRoundTrip", r.csSkipNotRoundTrip.Load(),
		"skipAlreadyCredited", r.csSkipAlreadyCredited.Load(),
		"skipNonOrthogonal", r.csSkipNonOrthogonal.Load(),
		"skipAlreadyCaptured", r.csSkipAlreadyCaptured.Load(),
		"skipShortLead", r.csSkipShortLead.Load(),
		"ledgerSize", r.cspub.size(),
		"ts", time.Now().Format(time.RFC3339),
	)

	D := r.csDist.Snapshot()
	log.Info("censorship dist",
		"processedBlocks", processed,
		"dropped_samples", D.Count,
		"dropped_BNB_p50", D.GrossUSDp50,
		"dropped_BNB_p90", D.GrossUSDp90,
		"dropped_BNB_p99", D.GrossUSDp99,
		"dropped_BNB_max", D.GrossUSDMax,
		"ts", time.Now().Format(time.RFC3339),
	)
}

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

// log10Big returns log10 of a wei amount (>=1), or 0 for nil/non-positive. Used
// as the pool-depth covariate so reserves of wildly different magnitudes compare
// on a stable scale.
func log10Big(v *big.Int) float64 {
	if v == nil || v.Sign() <= 0 {
		return 0
	}
	f := new(big.Float).SetInt(v)
	x, _ := f.Float64()
	if x <= 0 {
		return 0
	}
	return math.Log10(x)
}
