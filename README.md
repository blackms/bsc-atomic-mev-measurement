# Receipt-Exact Measurement of the Atomic-MEV Surface on Post-PBS BNB Smart Chain

**The realized independent-searcher edge is *vanishingly small*.**

Reproducibility artifact for the paper *"Receipt-Exact Measurement of the
Atomic-MEV Surface on Post-PBS BNB Smart Chain: The Realized Independent-Searcher
Edge Is Vanishingly Small"* (Alessio Rocchi, 2026).

> Status: **data-complete**. The peer-review-driven major revision (Phase-1
> instrument extensions, Phase-2 multi-detector measurement) closed blockers
> **B1/B2/B3/M1/M2** with empirical numbers, and the headline capture-rate is now
> measured: **45 of 96,064 ex-post net-positive opportunities already captured =
> 0.0468% by count (Wilson 95% CI [0.0350%, 0.0627%]), 0.0078% by value, over
> 315,750 processed blocks spanning ~2.5 days** (2026-06-09 04:27 → 2026-06-11
> 17:29). The 14-day collection target was **early-stopped** when state growth
> exhausted the disk volume and a disk-guard watchdog gracefully halted the node,
> so the headline covers a **single ~2.5-day market regime** rather than a
> multi-regime average. The paper is data-complete (0 `TODO(revision)`).

## Headline result

Two atomic-MEV categories — cross-DEX **backrun arbitrage** and **sandwiching** —
measured on a single ground-truth instrument by re-executing real BNB Smart
Chain (BSC) blocks inside a full node and valuing every opportunity with the
**actual EVM** (not a CFMM closed form).

The in-process *SimEngine* applies each block's transactions on a copy of the
parent state via the node's own `core.ApplyTransaction`, producing receipts that
match the canonically stored receipts **byte-exactly** — validated **500/500 on
stratified in-range blocks** spanning V3-heavy and fee-on-transfer categories.

The central finding: on post-PBS (BEP-322) BSC, the realized atomic-MEV capture
rate is **vanishingly small** — **45 of 96,064 ex-post net-positive opportunities
were already captured on-chain = 0.0468% by count (Wilson 95% CI [0.0350%,
0.0627%]), 0.0078% by value**, over 315,750 processed blocks spanning ~2.5 days.
What little capture occurs is **not by block builders** (`byBuilder = 0`) and is
**concentrated in a few repeated addresses**: 25 of the 45 captures go to repeated
senders and a single address (`0xCF2e..C842`) accounts for **17 of 45 (38%)**; the
remaining 20 are unknown. The *missed-capture upper bound* from a trace-level
blind-spot probe is **empirically zero on 30,100 blocks**. The ex-post surface
exists; the realized capture is rare and concentrated.

> **The independent-searcher edge is an *inference*, not a direct measurement.**
> This instrument directly measures the *already-captured fraction* of the ex-post
> surface (0.0468% by count). The headline claim that an *independent* searcher's
> realized edge is vanishingly small is an **inference** from that capture
> structure (rare, `byBuilder = 0`, 38% concentrated in one address) via the
> *unrealized ≠ available* argument (paper §6.1) — we do **not** deploy a live
> searcher and measure its PnL. Three quantities are measured directly and the
> fourth (the independent edge) only by inference, and we keep that boundary
> explicit.

## Key Phase-2 measurements (paper revision)

| Blocker | Window | Result |
|---|---|---|
| **Headline realizability** | 315,750 blk / ~2.5 days (geth-sim20; 14-day target early-stopped on disk/state growth) | **45/96,064 already-captured = 0.0468% by count [Wilson 95% CI 0.0350%, 0.0627%], 0.0078% by value; `byBuilder = 0`, 25 by repeated addr (`0xCF2e..C842` = 17/45 = 38%), 20 unknown** |
| **B2** receipt-validation widened | 500 stratified blocks, in-range | passRate = 1.00, 0 dropped-tx, 0 mismatch on 47,586 txs |
| **B3** backrun matched-footprint | 16,100 blk on long-tail any-pool | 55 OPP, 0.484 BNB ≈ $291; rate-normalized contrast vs sandwich ≈ **440×** |
| **B1** blind-spot identification gap | 30,100 blk trace-probe | 1,585 round-trip + 2 sweep patterns exist, **upperBoundMissedRealized = 0 BNB** |
| **M1/M2** same-pool contention over-count | 23,300 blk INDEPENDENT vs SERIALIZED bands | η = (V_U − V_L)/V_U ≈ **0.17%** — sandwich aggregate is a legitimate upper band |
| **Catch #5** integrity discipline | 1 OPP on block 103,005,219 (BUSD→WBNB→BUSD 2-hop V2, $3.6·10¹⁵) | sanity-cap $100k/1000 BNB + separate counter + forensic REJECT log |

Raw `tally`/`dist` log snapshots for each window are in
[`datasets/`](datasets/).

## What's in this repository

```
paper/        paper.md, paper.tex, references.bib   (no PDF — rebuild from .tex)
simengine/    the read-only measurement instrument: SimEngine + dry-run detectors + tests
strategy/     the arbitrage / sandwich math core (cycle search, sizing, quoting)
analysis/     XGBoost / AIPW / capacity-ladder / recall analysis scripts + small example data
datasets/     gzipped tally/dist log snapshots from the Phase-2 measurement windows
```

### Detectors (all read-only)

| `SIMENGINE_DRYRUN=` | Purpose |
|---|---|
| `intrablock` | Cross-DEX backrun arbitrage at the intra-block transient (hub universe) |
| `sandwich-any` | Sandwich opportunities against any victim swap (any-pool long tail) |
| `backrun-any` | **NEW** — backrun via cross-pool cycle on the same any-pool universe (matched footprint vs sandwich-any), with **marginal post-pre** rule and **sanity cap** |
| `realizability` | In-block counterfactual capture; same-actor + balance corroboration; **`rzRecoveredPanics`** counter for auditable per-victim recovery |
| `blindspot` | **NEW** — trace-level probe of recall-missed brackets; router/coinbase exclusion + nonce coldness; upper-bound on missed realized capture |
| `sandwich-serialize` | **NEW** — INDEPENDENT (upper) vs SERIALIZED (lower) sandwich bands on shared canonical substrate |
| `receipt-valid` | **NEW** — widened receipt-exact selftest (count-guard + stratified stride) |
| `censorship` | Censorship-differential (drop-then-mined-later, chain-verified settle window) |
| `recalltest` | Synthetic-injection recall validation of the detectors |
| `graph`, `sandwich`, `curl` | Earlier detectors retained for historical / appendix results |

`simengine/` and `strategy/` are **drop-in Go packages for the bnb-chain/bsc
source tree** (package paths `github.com/ethereum/go-ethereum/{simengine,strategy}`).
They are **not standalone-buildable**: they depend on internal go-ethereum
packages (`core`, `core/state`, `core/types`, `common`, `log`, …) and compile as
part of a full `bsc` node build.

## Read-only / never-submits disclaimer

This instrument is **strictly read-only**. Every detector runs on `state.Copy()`,
never commits to the chain, and **never builds, signs, or submits a
transaction**. There is no wallet, no builder client, and no submission path in
this artifact — the upstream private fork's arm-gated submission orchestrator is
deliberately omitted here, and the only side effect of a detected opportunity is
a log line. Each per-block unit of work is wrapped in `defer/recover`, and
recovered panics are surfaced via auditable counters (`rzRecoveredPanics`,
`brSkippedSanityOutlier`, `ssDivergedGroups`, `ssRevertedSteps`).

## Reproduction

### 1. Get a matching node source tree

```bash
git clone https://github.com/bnb-chain/bsc.git
cd bsc
git checkout v1.7.3
```

### 2. Drop in the measurement packages and build

```bash
cp -r /path/to/this/repo/simengine ./simengine
cp -r /path/to/this/repo/strategy  ./strategy
export GOTOOLCHAIN=auto
go build -o /tmp/geth ./cmd/geth
go test ./simengine/... ./strategy/...    # 83 simengine + 53 strategy tests
```

> The detectors are invoked from the node's startup wiring (the same hook used
> by the in-node self-test). Entry point: `StartDryRun(...)` in
> `simengine/dryrun.go`.

### 3. Run a synced node and select a detector

```bash
export SIMENGINE_DRYRUN=realizability             # or any from the table above
export SIMENGINE_DRYRUN_TALLY=50                  # tally every 50 processed blocks
export SIMENGINE_DRYRUN_GASPRICES=0,0.1,0.3,1,3   # gas-price sweep (gwei, CSV)
# detector-specific knobs (see source for full list), e.g.:
export SIMENGINE_BACKRUN_SANITY_USD_CAP=100000    # catch #5: outlier reject threshold (USD)
export SIMENGINE_SERIALIZE_SAMEPOOL=1             # enable serialized lower band
/tmp/geth   # plus your usual node flags / datadir
```

### 4. Cross-check vs the published snapshots

Each Phase-2 window in `datasets/` reproduces a specific table or table row in
the paper. Compare your tally output to the gzipped reference:

```bash
zcat datasets/backrun-any-postfix-final-2026-06-08.log.gz | grep 'backrun-any tally' | tail -1
zcat datasets/blindspot-final-2026-06-08.log.gz          | grep 'blindspot tally'    | tail -1
zcat datasets/serialize-final-2026-06-09.log.gz          | grep 'sandwich-serialize tally' | tail -1
```

### 5. Reproduce the ML / statistics

```bash
cd analysis
python3 -m venv venv && . venv/bin/activate
pip install numpy pandas scikit-learn xgboost   # (+ torch for the FT-Transformer ladder)
python3 tree_sota.py            # gradient-boosted-tree edge model
python3 censorship_aipw.py      # AIPW causal estimate
python3 capacity_sweep.py       # capacity-ladder sweep
python3 linear_anchor.py        # linear anchor / baseline
```

## Upstream

Built on **go-ethereum / bnb-chain/bsc**: <https://github.com/bnb-chain/bsc>.

## License

GNU General Public License v3.0 — see [LICENSE](LICENSE). The `simengine/` and
`strategy/` packages are derivative works of go-ethereum / bnb-chain/bsc. See
[NOTICE](NOTICE): this is an independent research artifact, not affiliated with
or endorsed by BNB Chain.

## Cite as

```bibtex
@misc{rocchi2026atomicmev,
  author = {Alessio Rocchi},
  title  = {Receipt-Exact Measurement of the Atomic-MEV Surface on Post-PBS
            BNB Smart Chain: The Realized Independent-Searcher Edge Is
            Vanishingly Small},
  year   = {2026},
  note   = {Reproducibility artifact (work in progress)},
  howpublished = {\url{https://github.com/blackms/bsc-atomic-mev-measurement}}
}
```
