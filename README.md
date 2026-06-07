# Receipt-Exact Measurement of the Atomic-MEV Surface on Post-PBS BNB Smart Chain

**The independent-searcher edge is realized ≈ 0.**

Reproducibility artifact for the paper *"Receipt-Exact Measurement of the
Atomic-MEV Surface on Post-PBS BNB Smart Chain: The Independent-Searcher Edge
Is Realized ≈ Zero"* (Alessio Rocchi, 2026).

## Headline result

We measure two atomic-MEV categories — cross-DEX **backrun arbitrage** and
**sandwiching** — on a single ground-truth instrument and a single victim
stream, by re-executing real BNB Smart Chain (BSC) blocks inside a full node and
valuing every opportunity with the **actual EVM** (not a CFMM closed form). The
in-process *SimEngine* applies each block's transactions on a copy of the parent
state via the node's own `core.ApplyTransaction`, producing receipts that match
the canonically stored receipts **exactly** — status, gas, cumulative gas, and
every log (validated 5/5 on real mainnet blocks of 100–151 txs).

The central finding: on post-PBS (BEP-322) BSC, the **independent searcher's
realized atomic-MEV edge is approximately zero**. The result is **triangulated**
across independent detectors (intra-block backrun, sandwich, realizability /
in-block counterfactual, censorship-differential, and an ordering-curl / Hodge
analysis) and is **recall-validated** (a synthetic-injection harness confirms the
detectors find what they claim to find), so the near-zero edge is a property of
the regime, not a blind spot of the instrument.

## What's in this repository

```
paper/        paper.md, paper.tex, paper.pdf, references.bib
simengine/    the read-only measurement instrument: SimEngine + the dry-run detectors
strategy/     the arbitrage / sandwich math core (cycle search, sizing, quoting)
analysis/     XGBoost / AIPW / capacity-ladder / recall analysis scripts + small example data
```

`simengine/` and `strategy/` are **drop-in Go packages for the bnb-chain/bsc
source tree** (package paths `github.com/ethereum/go-ethereum/{simengine,strategy}`).
They are **not standalone-buildable**: they depend on internal go-ethereum
packages (`core`, `core/state`, `core/types`, `common`, `log`, …) and are
compiled as part of a full `bsc` node build (see below).

## Read-only / never-submits disclaimer

This instrument is **strictly read-only**. Every detector runs on `state.Copy()`,
never commits to the chain, and **never builds, signs, or submits a
transaction**. There is no wallet, no builder client, and no submission path in
this artifact — the upstream private fork's arm-gated submission orchestrator is
deliberately omitted here, and the only side effect of a detected opportunity is
a log line. Each per-block unit of work is wrapped in `defer/recover`, so the
detectors cannot panic or interfere with the host node.

## Reproduction

### 1. Get a matching node source tree

Clone upstream and check out the matching tag:

```bash
git clone https://github.com/bnb-chain/bsc.git
cd bsc
git checkout v1.7.3
```

### 2. Drop in the measurement packages and build

Copy the two packages from this repo into the bsc source tree, then build the
node. (`GOTOOLCHAIN=auto` lets the Go toolchain fetch the version pinned by the
bsc `go.mod`.)

```bash
cp -r /path/to/this/repo/simengine ./simengine
cp -r /path/to/this/repo/strategy  ./strategy
export GOTOOLCHAIN=auto
go build -o /tmp/geth ./cmd/geth
```

> Note: the detectors are invoked from the node's startup wiring (the same hook
> used by the in-node self-test). If your checkout does not already register the
> dry-run runner, add a single call to start it after the blockchain is
> available; the entry point is `StartDryRun(...)` in `simengine/dryrun.go`.

### 3. Run a synced node and select a detector

Run a fully synced BSC node built as above, then select a detector with the
`SIMENGINE_DRYRUN` environment variable. Each detector is strictly read-only and
emits per-block log lines plus periodic tallies:

```bash
# one detector per run; pick one of:
#   intrablock      cross-DEX backrun arbitrage at the intra-block transient
#   sandwich-any    sandwich opportunities against any victim swap
#   realizability   in-block realizability / counterfactual capture
#   censorship      censorship-differential (drop-then-mined-later)
#   recalltest      synthetic-injection recall validation of the detectors
# (also available: graph, sandwich, curl)

export SIMENGINE_DRYRUN=intrablock
export SIMENGINE_DRYRUN_TALLY=1          # emit running tallies
# optional knobs (see the per-detector source for the full list), e.g.:
#   SIMENGINE_DRYRUN_GASPRICES=0,0.1,0.3,1,3   gas-price sweep (gwei, CSV)
#   SIMENGINE_SANDWICH_MINUSD=...              sandwich victim USD floor
#   SIMENGINE_CS_SETTLE_BLOCKS=256            censorship settle window
/tmp/geth   # plus your usual node flags / datadir
```

Set `SIMENGINE_DRYRUN` to `sandwich-any`, `realizability`, `censorship`, or
`recalltest` to reproduce each corresponding result block in the paper.

### 4. Reproduce the ML / statistics from the detector logs

The detectors write tally lines and per-candidate rows; collect those into CSVs
(the column schemas are visible at the top of each script) and run the analysis
in `analysis/`. The scripts read their inputs from the working directory:

```bash
cd analysis
python3 -m venv venv && . venv/bin/activate
pip install numpy pandas scikit-learn xgboost   # (+ torch for the FT-Transformer ladder)
python3 tree_sota.py            # gradient-boosted-tree edge model
python3 censorship_aipw.py      # AIPW causal estimate for the censorship differential
python3 capacity_sweep.py       # capacity-ladder sweep
python3 linear_anchor.py        # linear anchor / baseline
```

Small example inputs and result JSONs are bundled (`curl_clusters.csv`,
`realizability_tally.csv`, the `*_results.json` / `degenerate_*` / `demo_*` /
`diag_*` outputs). Two larger inputs are **not** committed and must be
regenerated from a detector run:

- `censorship_candidates.csv` — the per-candidate censorship table (~2 MB),
  produced by the `censorship` detector;
- `sandwich_opps.csv` — the sandwich-opportunity table, which is **empty** in our
  measured window (zero net-positive independent sandwiches; that emptiness is
  itself a result).

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
            BNB Smart Chain: The Independent-Searcher Edge Is Realized
            Approximately Zero},
  year   = {2026},
  note   = {Reproducibility artifact},
  howpublished = {\url{https://github.com/blackms/bsc-atomic-mev-measurement}}
}
```
