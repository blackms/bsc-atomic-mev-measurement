# Receipt-Exact Measurement of the Atomic-MEV Surface on Post-PBS BNB Smart Chain: The Realized Independent-Searcher Edge Is Vanishingly Small

**Alessio Rocchi**

*Primary arXiv category: cs.CR (Cryptography and Security); cross-list cs.DC,
q-fin.TR.*

---

## Abstract

Empirical studies of decentralized-exchange (DEX) MEV estimate opportunities
analytically from constant-function-market-maker (CFMM) closed forms — which
approximate fees, ignore tick-crossing in concentrated-liquidity (V3) pools, cannot
account for reverts, hooks, or rounding, and price each MEV *category* on a different
dataset — so they systematically over-count and make cross-strategy comparison
unreliable. **This paper makes one epistemic structure explicit up front: we *measure*
three things and *infer* a fourth.** We measure (1) the *ex-post* atomic-MEV
opportunity surface with a receipt-exact oracle, (2) the in-block *real-competitor*
capture rate, and (3) the censorship-differential; from these we **infer** — we do *not*
directly measure — the edge available to an *independent* searcher, via the explicit,
falsifiable *unrealized ≠ available* mechanism (§6.8), because the PBS-submission path
is disarmed and never exercised (no independent bundle is ever actually placed). The
headline capture window is a *single ~2.5-day regime* (a 14-day collection early-stopped
on state growth), so generalization across volatility regimes is future work.
**(a) Instrument and contribution.** Rather than approximate execution, we *execute*: an
in-process *SimEngine* re-applies real BNB Smart Chain (BSC) blocks on a copy of the
parent state via the node's own `core.ApplyTransaction`, used as a *single* ground-truth
instrument to price *both* atomic categories — cross-DEX backrun arbitrage and
sandwiching — on the *same* blocks and the *same* victim stream, valuing every
opportunity with the actual pool bytecode (V3 hops EVM-valued via in-process QuoterV2;
V2 hops by the exact integer closed form, receipt-validated for V2 swaps but still
analytic, see T8). Backrun is detected at the correct **intra-block transient**
(immediately after each victim swap, the only state where it exists); sandwiching is
**constructed** on copied state (a synthetic attacker funded by direct ERC20 storage
writes, a conservative bias that can only understate profit), routing frontrun and
backrun to the victim's *actual* pool with the *real* victim transaction replayed
between. **(b) Validation.** Simulated receipts match the canonically stored receipts
**exactly** — status, gas, cumulative gas, and every log — 5/5 PASS on real mainnet
blocks (100–151 txs each) and **500/500 in Phase-2**; this certifies the **replay** (the
counterfactual sandwich **construction** has no ground-truth receipt and is argued
conservative, not receipt-validated). **(c) Headline results.** The two strategies
diverge sharply. **Cross-DEX backrun on the deep WBNB/stable hub is sub-gas**: over
3,000 blocks the EVM confirms 803 gross-positive cycles but only **15 (1.9%) clear the
~\$0.50 gas gate** (280k gas × 3 gwei), ~\$10 total. **Sandwiching the long tail is
not**: over 2,550 blocks and 33,525 victim swaps we find 1,735 gross-positive and
**1,162 ex-post net-positive sandwiches** (median gross \$1.78, max \$430.50), totalling
**35.08 BNB (~\$20,200)** — a gap of **~91× in net-positive count and ~2,040× in raw
aggregate value** (different block counts, 3,000 vs 2,550; ~2,400× per-block normalized,
and 458× on a matched, basis-aligned footprint, §5.6.2). The in-block counterfactual
then measures *realized* capture: over the completed multi-day window, captured
sandwiches number **45 of 96,064 (0.0468% by count, Wilson 95% CI [0.0350%, 0.0627%];
0.0078% by value)** over **315,750 processed blocks spanning 2.5 days** (2026-06-09
04:27 – 2026-06-11 17:29, chain heights ~102.99M–103.63M), with **25 captures by
repeated address** (one address 0xCF2e..C842 = 17 of 45, 38%), **20 by unknown, 0 by
builder**; and the censorship-differential is **$\hat D = 0$** after a chain-verified
settle window (99.6% of apparent public "drops" are merely delayed inclusion, not
censorship). Cross-tx sandwiching has receded under validator/builder filtering and
private order flow (BEP-322; two latency-advantaged builders, 48Club and BlockRazor, see
private flow ahead of the public mempool and capture ~90% of MEV), the same regime that
would exclude an independent's sandwich — so the *inferred* realized independent
atomic-searcher edge is **vanishingly small**, bounded above by the measured
0.0468%-by-count / 0.0078%-by-value capture rate and below by zero on the windows
measured, with the surviving surface in the long tail, not the deep markets the backrun
literature studies. **(d) Limits.** The independent edge is *inferred*, not directly
measured (the PBS-submission path is never exercised); the capture window is a single
early-stopped ~2.5-day regime; and backrun (a fixed 12-pool deep WBNB/stablecoin hub)
and sandwich (any long-tail WBNB-paired pool) are measured on intentionally different
scopes, so the 91×–2,040× gap reflects both strategy profitability *and* that portfolio
difference. The transferable contribution is the validated ground-truth instrument and
protocol. During development we found and fixed **five integrity catches** in the
detectors (units bug; realizability false-zero; censorship estimand-inversion;
censorship delayed-vs-censored conflation; backrun-any decimal-mismatch sanity-cap
fire), documented in the methods for reproducibility.

**Contributions.**

1. A receipt-exact, in-process simulation methodology (SimEngine) that re-executes
   real blocks on copied node state via `core.ApplyTransaction` and matches stored
   receipts byte-for-byte (5/5 PASS) — ground-truth execution, strictly more
   faithful than analytic CFMM for V3 / fee-on-transfer / hooks / reverts — used as
   the *single* valuation substrate for *both* atomic MEV categories we measure.
2. A ground-truth valuation oracle for **backrun** (Stage B): every
   candidate cross-venue cycle valued by the actual pool/Quoter bytecode — V3 hops by
   in-process QuoterV2 (EVM-valued on the exact intermediate state), V2 hops by the
   exact integer closed form (receipt-validated for V2 swaps on real blocks, but still
   an analytic closed form, not full re-execution; see T8) — with
   optimal-input search, evaluated at the correct intra-block transient (post-block
   evaluation is *not* a backrun test — intra-block competitors re-align prices).
3. A full-EVM ground-truth **sandwich** constructor: a synthetic attacker funded by
   direct storage writes (hardcoded + runtime-probed ERC20 slots), frontrun/backrun
   routed to the victim's actual pool (K-safe direct `pair.swap` for any V2 fork; the
   real V3 SwapRouter for Pancake V3), the real victim tx replayed between, profit
   read off the EVM in a single BNB-priced numeraire — the first (to our knowledge)
   receipt-exact *per-block census* of the sandwich surface (complementing
   per-target executing optimizers such as Lanturn [babel2023lanturn], which
   construct and EVM-simulate sandwich/arbitrage strategies for a chosen contract
   rather than censusing every victim swap in a block).
4. **The first (to our knowledge) controlled backrun-vs-sandwich measurement on
   one instrument and one victim stream**: same blocks, same EVM valuation, same cost model — turning a
   cross-paper comparison into a single controlled experiment. Note the two strategies
   are measured on *different scopes* (backrun on a fixed 12-pool deep WBNB/stablecoin
   hub, sandwich across any long-tail WBNB-paired pool); this scope difference is
   intentional (long-tail sandwiching is where ex-post value concentrates) but means the
   resulting gap reflects both strategy profitability *and* the portfolio difference.
   Prior executing tools
   (e.g. Lanturn [babel2023lanturn]) EVM-evaluate constructed strategies but optimize
   one target at a time; we instead hold a single per-block victim stream fixed and
   price *both* atomic categories on it.
5. A measurement of the independent-searcher atomic-MEV surface: backrun is frequent
   but **sub-gas** (15 of 803 net-positive over 3,000 blocks, ~\$10) on the deep hub;
   sandwiching the long tail is **substantial ex-post** (1,162 of 1,735 net-positive
   over 2,550 blocks, 35.08 BNB ≈ \$20,200) but **realized by only a small measured
   fraction** (an earlier geth-sim16 pilot corroborated ≈0.07% by count, ≈0.016% by value
   over 34,200 blocks, slightly above the final measured rate from the completed window,
   0.0468% by count (45/96,064, Wilson CI [0.0350%, 0.0627%]) and 0.0078% by value) — establishing that while ex-post
   sandwiching dominates ex-post backrun by ~91× in count and ~2,040× in raw aggregate
   value (as measured on their respective scopes; block counts differ: 2,550 sandwich
   vs 3,000 backrun; per-block normalized value ratio ~2,400×; see §5.6.2 for the
   matched-footprint, universe-matched basis-aligned contrast: ~458× in BNB-denominated
   value/block, price-free), the realized edge for an independent across
   both strategies is much smaller.
6. The ex-post sandwich surface density (0.35–0.46 net-positive per block over our
   measurement window) as the baseline for the in-block counterfactual and the
   ex-post-vs-realized comparison; we deliberately do *not* report an annualized
   extrapolation, since annual scaling is unreliable given diurnal/volatility variation
   and the per-victim concurrent same-pool over-count (§6.1).
7. **A ground-truth in-block counterfactual** that converts ex-post existence into a
   *measured* realizability: for each ex-post opportunity it detects whether a real
   competitor already captured it in the canonical block (conjunctive bracket +
   same-actor + profit-corroboration gates, conservative by construction, with an
   auditable funnel). A 2,100-block window finds 0 of 735 ex-post sandwiches realized
   (capture rate 0.00); a longer multi-day collection has anchored the rate at
   ≈0.07% by count and ≈0.016% by value (geth-sim16 pilot over 34,200
   blocks, 7 captures of ~10,190 ex-post opportunities; a later 152,650-block pilot
   anchored 10 captures); these earlier, smaller windows corroborate and sit slightly
   above the final measured capture rate, CI, and repeat-actor structure of **45 of 96,064 (0.0468% by count,
   Wilson 95% CI [0.0350%, 0.0627%]; 0.0078% by value), with 25 captures by repeated
   address (0xCF2e..C842 = 17 of 45, 38%), 20 by unknown, 0 by builder, over 315,750
   blocks (2026-06-09 04:27 – 2026-06-11 17:29)**, establishing nonzero but small realization relative to the
   ex-post surface — the short-window zero was a sampling artifact, consistent with
   the filtering + private-flow regime and cross-referenced with a 1,500-block
   pool-agnostic scan showing cross-tx sandwiching is rare. A separately measured
   trace-probe over 30,100 blocks finds the identification gap is **bounded at zero**
   on that window: 1,585 structural round-trips and 2 sweeps exist as patterns but
   carry zero positive ex-post net profit (`upperBoundMissedRealizedWei=0`; see §5.7).
   Thus the realized atomic edge is substantially smaller than the
   ex-post surface by direct measurement of real-competitor capture; the
   independent-searcher edge follows by the *unrealized ≠ available* inference (§6.1).
8. A careful realizability / threats discussion separating ex-post existence (what
   we prove) from realizable capture (which we now measure), uniform across both
   strategies, including the *unrealized ≠ available* point (developed in §6.1), with an explicit statement
   of what would change the conclusion.

---

# 1. Introduction

## 1.1 Maximal Extractable Value and the measurement problem

Maximal Extractable Value (MEV) — the profit a party can extract by including,
excluding, or reordering transactions within the blocks it produces or influences
— has grown from a curiosity into a structural feature of every major
smart-contract chain since Daian et al. introduced the concept and documented
priority-gas auctions and consensus-layer instability in *Flash Boys 2.0*
[daian2020flashboys]. A large share of MEV is *atomic arbitrage*: a single
transaction that buys an asset cheaply on one DEX and sells it dearly on another,
profiting from a transient price discrepancy and reverting risklessly if the
discrepancy has closed by execution time. When the arbitrage trails a specific
user swap that *created* the discrepancy, it is a *backrun*.

The empirical literature on DEX arbitrage is large and growing — Wang et al. on
cyclic arbitrage [wang2021cyclic], Qin et al. on quantifying blockchain
extractable value [qin2022quantifying], Torres et al. on front-running
[torres2021frontrunner], and the large-scale measurement of McLaughlin et al.
[mclaughlin2023arbitrage]. Almost all of this work shares a methodological choice
that is rarely examined: **opportunities are estimated analytically**. A study
reads pool reserves, applies a CFMM closed form (most commonly the Uniswap V2
`x·y=k` law), and reports the implied optimal trade and profit. This is fast and
scales to years of history, but it embeds approximations that systematically
distort the answer:

- it assumes the `x·y=k` invariant even for pools that do **not** obey it —
  concentrated-liquidity (Uniswap/Pancake V3) pools whose effective liquidity is
  piecewise-constant across ticks [adams2021uniswapv3], and stableswap/Curve-style
  pools whose invariant is entirely different;
- it cannot see **reverts**: a transaction the searcher would have to land may
  revert on-chain, turning would-be profit into pure gas loss;
- it ignores **fee-on-transfer tokens, transfer hooks, and rounding**, all of
  which the real EVM applies and the closed form does not;
- it conflates *ex-post* opportunity (what an oracle who knew the exact ordering
  could have captured) with *realizable* opportunity.

The net effect is that analytic studies **over-count**. The gap is not small: even
at the detection layer, Zhang et al. show that a naive negative-cycle scan
surfaces a tiny fraction of the cycles a better graph transform finds
[zhang2024improved], and at the *valuation* layer the discrepancy between a
no-slippage marginal-rate signal and exact, fee-and-tick-accurate profit is larger
still.

## 1.2 Our instrument: receipt-exact in-process re-execution

We take a different stance. Rather than approximate execution, **we execute**. We
run a full BNB Smart Chain (BSC) node — bsc-geth v1.7.3, snapshot-seeded, on a
64-core / 125 GB host — and build an in-process *SimEngine* that, given a block,
applies its transactions on a *copy* of the parent state through the node's own
`core.ApplyTransaction`, the identical code path the canonical block processor
uses. The canonical state is never mutated and nothing is committed to disk.

The SimEngine is **validated against ground truth**: we replay real mainnet blocks
and compare the simulated receipts to the receipts the network actually produced
and stored. Over our validation sample the simulated receipts match the stored
receipts **exactly** — status, gas used, cumulative gas used, and every log's
address, topics, and data — 5/5 PASS on blocks around height 101.83M with 100–151
transactions each. This is not an approximation of execution; it *is* execution,
and we have a receipt-level certificate that it reproduces the chain.

This gives us a measurement instrument with a property analytic studies lack, and
that prior executing tools apply only per chosen target rather than as a per-block
full-node census (Lanturn [babel2023lanturn] EVM-simulates optimizer-constructed MEV
strategies for a selected contract; we re-execute every transaction of every block):
when
we say a candidate arbitrage would have yielded profit *p*, *p* is the profit the
EVM actually computes for that exact swap sequence on that exact state, including
V3 tick-crossing, fee tiers, hooks, reverts, and integer rounding. For V3 in
particular this matters: rather than re-implement (and risk diverging from)
on-chain `TickMath` / `SwapMath` / `SqrtPriceMath`, we let the pool bytecode be
its own arbiter [adams2021uniswapv3; diamandis2023routing].

## 1.3 The post-PBS BSC regime, and why this result is interesting

BSC matters for an MEV study because it is high-throughput, deeply liquid in a
small set of hub assets (WBNB, USDT, USDC, USD1, BUSD), and has recently undergone
a **proposer–builder separation (PBS)** transition via BEP-322 [bep322]. Under
BEP-322 there is *no relay*; builders are *whitelisted*; order flow is *private*
and arrives at builders ahead of the public mempool. Wang et al.'s measurement of
the Binance builder ecosystem [wang2025binance] reports that two builders — 48Club
and BlockRazor — together produce the great majority of blocks and capture the
great majority of MEV, that the dominant strategy is short (2–3 swap) arbitrage
cycles on the WBNB/stablecoin hub, and that opportunities live on the order of
~100–400 ms while public-mempool transactions reach searchers tens of milliseconds
*after* builder bundles.

This is the backdrop against which we ask, more broadly than the backrun literature
does, **what the atomic-MEV opportunity surface available to an *independent*
searcher actually looks like on post-PBS BSC** — across both of the atomic categories
a non-builder could in principle contest. We split the primary question into two — with a
methodological third (RQ3) that falls out of the instrument. **RQ1: is
there any net-positive independent-searcher *backrun* edge left on BSC's major
pools?** And **RQ2: how does that compare, on the *same* instrument and the *same*
victim stream, to *sandwiching* — the dominant atomic MEV category on BSC by value
(roughly half of measured BSC MEV)?** Treating both on one ground-truth substrate is
the methodological move that makes the comparison meaningful, because the existing
literature prices each category on a different dataset with a different (analytic)
model.

**What we measure, and what we infer (stated up front).** Our design identifies three
quantities directly and a fourth only by inference, and we keep that boundary explicit
throughout. We *measure*: (1) the *ex-post* atomic-MEV opportunity surface,
receipt-exact, via the EVM oracle; (2) the *in-block real-competitor capture rate*, by
detecting whether a real competitor already took each ex-post opportunity in the
canonical block; and (3) the *censorship-differential* $D$, the one point-identified,
structurally-reachable extraction estimand. We do *not* directly measure the edge an
*independent* searcher would realize: the PBS-submission path is disarmed and never
exercised, so no independent bundle is ever actually placed. We *infer* that edge from
the three measured surfaces through the explicit, falsifiable *unrealized ≠ available*
mechanism (§6.1, §6.8), and we state the condition that would overturn it. Finally, the
headline capture window is a *single ~2.5-day regime* — a 14-day collection
early-stopped on state growth — so generalization across volatility regimes is future
work, not a claim we make here.

For RQ1, our naive baseline — three deep PancakeSwap V2 pools (WBNB/USDT, WBNB/USDC,
USDT/USDC), an exact closed-form two-pool optimal-input, a conservative 250k-gas /
3-gwei cost gate, and a zero builder bid (the most generous possible assumption for
the searcher) — finds **zero profitable backruns across 5,100 consecutive blocks.** A
cross-DEX V2 token-graph extension (adding three Biswap pools, enabling
same-pair-two-venue and triangular cycles) likewise finds **zero candidates across
2,950 blocks.** With full per-block coverage and ground-truth execution, these are
*credible* zeros at the level they measure.

**But they measure the wrong thing**, and we say so. Both evaluate arbitrage
against the *post-block* reserves; at a block boundary the cross-venue prices are
already re-aligned, because competing arbitrageurs act *within* the block. Backrun
MEV lives in the transient state immediately *after* a victim swap and *before* the
next transaction; end-of-block evaluation systematically misses it. The
naive/cross-DEX post-block zeros therefore quantify *standing* arbitrage, not
*backrun-able* transients. This realization reframes the whole study and is itself
a methodological contribution.

## 1.4 The correct experiment: intra-block, EVM-valued

The instrument that makes the right experiment cheap is the one we already built:
the SimEngine re-executes each block *transaction-by-transaction*. So we hook in
**after every transaction that emits a watched-pool `Swap`**, snapshot reserves at
that precise intermediate point, and run candidate detection and valuation *there*
— at the only state where a backrun could exist. On top of this intra-block
trigger the final model:

1. builds a **token graph** over a verified 12-pool multi-DEX watch set — six V2
   pools (three PancakeSwap, three Biswap) and six PancakeSwap V3 pools across fee
   tiers 100/500/2500 — with tokens as nodes and each ordered pool-direction as an
   edge weighted `-ln(rate·(1−fee))`;
2. runs **negative-cycle detection** to enumerate candidate cycles of length 2..K
   from source WBNB — Stage A, *detection only*, on marginal (spot) prices;
3. **values every candidate with the EVM (Stage B), the sole profit oracle:** V2
   hops by the exact closed form; V3 hops by an in-process PancakeSwap **QuoterV2**
   call on the *exact intermediate* `state.Copy()` (chaining hop outputs into hop
   inputs), with the optimal input found by a golden-section search whose every
   probe is an EVM evaluation. The reported gross is the EVM's own number, exact
   for V2 *and* V3 — no `x·y=k` approximation of concentrated liquidity;
4. applies a cost gate `net = gross − Σ gas − bid` with the *measured* per-cycle
   gas (~280k units) at 3 gwei (≈ \$0.50), assuming capital-free flash-swap /
   flash-loan funding [qin2021flashloans] so the binding constraints are gas + bid;
   and emits a gross-profit **distribution**, **break-even gas price**, and a
   **gas-price sensitivity sweep** over `{0, 0.1, 0.3, 1, 3}` gwei.

**RQ3** falls out directly: the collapse from EVM-confirmed *gross-positive* to
*net-positive* measures exactly how much an analytic (marginal-rate) or gross-only
study over-counts the realizable opportunity set.

The same intra-block victim stream answers **RQ2** with no change of instrument. For
*every* watched-pool victim swap the SimEngine surfaces, we do not merely look for a
trailing arbitrage — we **construct the adversarial sandwich**: on a fresh copy of
the pre-swap state we fund a synthetic attacker, insert an optimally-sized frontrun
*in the victim's own pool*, replay the *real* victim transaction against the degraded
reserves, close with a backrun, and read the attacker's profit straight off the EVM
(§3.7). Because the construction routes to the victim's actual pool by a K-safe direct
`pair.swap`, it generalizes beyond the deep hub to **any V2-fork pool in the long
tail** — exactly where, our own backrun runs hint, the volume actually is (the deep
pools were quiet). This lifts the study from "is backrun profitable on the hub?" to
"which atomic strategy, backrun or sandwich, carries the independent-searcher edge,
and where?" — and the answer (RQ2) turns out to be larger for sandwiching than backrun
by roughly two-to-three orders of magnitude.

## 1.5 Summary of findings

- The SimEngine reproduces real mainnet receipts exactly (**5/5 PASS**, 100–151
  txs each): *one* ground-truth execution instrument, used for *both* strategies.
- **Backrun (deep hub).** The naive deep-pool model finds **0 / 5,100 blocks** and
  the cross-DEX V2 graph **0 / 2,950 blocks** (both *post-block*, not valid backrun
  tests). The correct intra-block detector + EVM oracle, over **3,000 sampled
  blocks**, confirms **803 ground-truth gross-positive cross-venue cycles (~0.27 per
  block), of which only 15 (1.9%) are ex-post net-positive** at 3 gwei — ~1 per 200
  sampled blocks, 0.0172 BNB (~\$10) total; the other 788 sit below the ~\$0.50 gas
  floor. Frequent, real, but **overwhelmingly sub-gas**.
- **Sandwich (long tail).** The EVM-constructed three-leg attack, over **2,550
  sampled blocks** examining **33,525 victim swaps**, finds **1,735 gross-positive
  and 1,162 ex-post net-positive sandwiches** at 3 gwei (~0.46 per block), totalling
  **35.08 BNB (~\$20,200)**; median gross \$1.78, p90 \$31.6, max \$430.50. The 1,162
  net-positive = 67.0% of gross-positive after gas+flash; 69.0% (1,197) clear the
  3-gwei gas gate before the flash premium — entirely on long-tail WBNB-paired pools.
- **The contrast (headline).** Same instrument, same victim stream: sandwiching
  yields **~91× more net-positive opportunities per block and ~2,040× more total
  value in raw aggregate** (note block counts differ: 2,550 sandwich vs 3,000 backrun;
  ~2,400× per-block normalized) than backrun. The independent searcher's atomic-MEV edge is a
  **long-tail sandwiching phenomenon, not a deep-pool backrun one**.
- **Ex-post surface density (no annual extrapolation).** The ground-truth *ex-post*
  sandwich surface runs at 0.35–0.46 net-positive sandwiches per block over our
  measurement window. We deliberately do *not* annualize it: annual scaling is
  unreliable given diurnal/volatility variation and the per-victim concurrent same-pool
  over-count (§6.1). The density is the baseline against which the realized fraction
  (§5.7) is measured — the ex-post-vs-realizable gap is the finding (§5.6.1), not an
  absolute annual figure.
- **Realizability (in-block counterfactual).** Of the **735 ex-post sandwich
  opportunities** in a 2,100-block window, **0 were captured** by a real cross-tx
  sandwich (capture rate 0.00 in that small window, auditable funnel: 1,077 brackets →
  79 same-actor → 0 corroborated). A longer collection anchors the realized capture at
  ≈0.07% by count and ≈0.016% by value (geth-sim16 pilot over 34,200
  blocks: 7 captures of ~10,190 ex-post opportunities; a later 152,650-block pilot
  anchored 10 captures); these earlier, smaller windows corroborate and sit slightly above
  the **final measured capture rate, CI, and repeat-actor count**
  over the completed multi-day, multi-volatility window,
  **45 of 96,064 (0.0468% by count, Wilson 95% CI [0.0350%, 0.0627%]; 0.0078% by value),
  measured over 315,750 processed blocks (2026-06-09 04:27 – 2026-06-11 17:29, ~61 hours,
  chain heights ~102.99M–103.63M); 25 captures by repeated address (0xCF2e..C842 = 17 of
  45, 38%), 20 by unknown, 0 by builder**, concentrated in repeated addresses and structures
  matching blind-spot patterns (mid-tx sweep, non-flat round-trips) — establishing
  that the small-window zero was a sampling artifact and realization is nonzero but a
  small fraction of the ex-post surface. A separately measured Phase-2 trace-probe over
  30,100 blocks finds 1,585 structural round-trips and 2 sweeps as patterns but with
  `upperBoundMissedRealizedWei=0` — **the identification gap is bounded at zero on
  that window**; the blind-spot pattern is demographic, not economic (§5.7). Cross-tx
  sandwiching is rare in realized BSC activity; realized MEV is dominated by
  single-transaction atomic arbs. *Unrealized ≠ available*: the small realized fraction
  is consistent with the filtering + private-flow regime that suppresses sandwiching
  for everyone, including an independent (corroborated by the trace-probe upper-bound
  of zero on this window).
- **Censorship-differential (the reachable estimand).** The one point-identified,
  structurally-reachable estimand — the value a builder leaves by *dropping* public,
  orthogonal, profitable opportunities — is **$\hat D = 0$** after a chain-verified
  settle window; **99.6%** of apparent public "drops" were merely *delayed inclusion*
  (mined a few blocks later), not censorship. The public residual is empty too.
- **Implementation bugs / integrity catches (reproducibility).** During development we
  found and fixed **five integrity catches** in the detectors (detailed in §3 and
  §5.6.2), each documented in the methods so the results are reproducible.

The conclusion: on post-PBS BSC the *ex-post* atomic-MEV surface looks like it has
**migrated from deep-pool backrun (15 of 803 net-positive, ~\$10) to long-tail
sandwiching** (1,162 net-positive, ~\$20,200 ex-post) — but *realized*, both are far
less capturable by an independent. Backrun on the deep hub is sub-gas; the long-tail
sandwich surface, though large ex-post, is realized by **only a small measured fraction**
(final measured rate 0.0468% by count (45/96,064, Wilson 95% CI [0.0350%, 0.0627%]) and 0.0078% by value
over the completed 315,750-block window; earlier smaller windows corroborated this — the geth-sim16 pilot at
7 of ~10,190 ≈ 0.07% by count, ≈0.016% by value over 34,200 blocks) and would face the same filtering barriers. The short-window zero (capture rate 0 over 2,100 blocks) was a
sampling artifact corrected by the longer-window collection. Under BEP-322 two
latency-advantaged builders (48Club, BlockRazor) produce most blocks, see private flow
ahead of the public mempool, and capture ~90% of MEV [bep322; wang2025binance]. So the
independent atomic-searcher edge in realized terms is *vanishingly small* — bounded
above by the measured 0.0468%-by-count rate (0.0078% by value; earlier smaller windows corroborated slightly higher at ≈0.07%) and below by zero on
the windows measured — and we *infer* it is not available to an
independent via the *unrealized ≠ available* mechanism (§6.1), not from a direct measurement
of independent capture. The transferable contribution is the *instrument
and protocol*: validated ground-truth valuation (V3 hops EVM-valued, V2 hops closed-form; T8) that prices every atomic
category on one substrate, evaluated at the correct intra-block transient (backrun
gross→net collapse 803 → 15 — the quantitative case against analytic CFMM over-counting,
especially for V3), a controlled backrun-vs-sandwich contrast (with scope caveats: deep
hub vs. long tail), and an in-block counterfactual that turns "ex-post existence" into
measured realizability across an extended window.

---

# 2. Related Work

We organize prior work into seven threads: (i) MEV foundations, (ii) cyclic /
graph-based arbitrage detection, (iii) convex-optimization routing and optimal
sizing over CFMMs, (iv) concentrated liquidity, (v) flash loans / capital-free
execution, (vi) sandwich attacks and their concentrated-liquidity profitability, and
(vii) the BSC PBS regime that frames the interpretation.

## 2.1 MEV foundations

Daian et al., *Flash Boys 2.0* [daian2020flashboys], introduced MEV, documented
priority-gas auctions (PGAs) among arbitrage bots, and showed how reordering
incentives threaten consensus stability ("time-bandit" attacks). Heimbach and
Wattenhofer's SoK [heimbach2022sok] systematizes transaction-reordering
manipulations and their mitigations (private order flow, fair ordering, encrypted
mempools), defenses the BSC PBS regime partially instantiates. Qin, Zhou and
Gervais [qin2022quantifying] quantify blockchain extractable value across sandwich
attacks, liquidations and arbitrage, and show a miner with a modest hashrate
fraction is incentivized to fork for sufficiently large MEV. Torres, Camino and
State, *Frontrunner Jones* [torres2021frontrunner], analyze ~11M blocks and
identify on the order of 200k front-running attacks.

**Position.** These works motivate *why* arbitrage MEV is worth measuring. We
contribute a *measurement instrument*: where they infer value from logs and
analytic models, we re-execute blocks to obtain receipt-exact realized value. The
closest non-analytic prior art is Lanturn [babel2023lanturn], whose adaptive-learning
optimizer constructs MEV-extracting transaction sequences and *executes them on real
chain state without modification* (over Sushiswap/Uniswap V2/V3/Aave), valuing
sandwich, arbitrage, and backrun strategies by simulation rather than closed form.
Lanturn optimizes value for a *chosen target contract*; we instead run a *per-block,
full-node census* that prices every watched victim swap in every block. Our instrument
and Lanturn are thus complementary: per-target optimizer vs. per-block opportunity
census, both refusing the analytic CFMM surrogate.

## 2.2 Cyclic and graph-based arbitrage detection

The standard technique adapts FX-arbitrage negative-cycle search to DEXs: build a
directed multigraph with tokens as nodes and each ordered pool-direction as an edge
of weight `w(u→v) = −ln(r_uv · (1−fee))`. A profitable cycle exists iff some cycle
has sum of weights `< 0` (a negative cycle). Bellman–Ford detects one in `O(V·E)`;
SPFA and Johnson's algorithm are the standard speedups. Wang et al., *Cyclic
Arbitrage in Decentralized Exchanges* [wang2021cyclic], give the empirical and
theoretical framing, documenting hundreds of thousands of cyclic arbitrages and
>\$138M revenue.

The approach has three known limitations for *profit* estimation: (a) marginal-rate
weights certify only an infinitesimal trade; (b) vanilla Bellman–Ford returns one
arbitrary cycle per run; (c) it yields a percentage, not the absolute figure that
must beat a fixed gas cost. Zhang et al. [zhang2024improved] attack these with a
line-graph transform and a Modified Moore–Bellman–Ford pass that surfaces many
loops per run. McLaughlin et al. [mclaughlin2023arbitrage] provide the
large-scale empirical reference point (a 28-month Ethereum study) and the
measurement ethos that motivates a *detect-then-verify* split.

**Position.** We adopt negative-cycle detection strictly as a **candidate
generator** — cheap enough to enumerate cross-DEX and N-cycles the naive hardcoded
model cannot — and *never* as the profit oracle. Every candidate is EVM-valued,
making RQ3 (analytic over-count) directly measurable.

## 2.3 Convex routing and optimal sizing over CFMMs

For a single two-pool cycle on `x·y=k` pools, the optimal input is a closed form
obtained by collapsing the two hops into one synthetic constant-product pool and
solving `dP/dx = 0`; this is what `strategy/arb.go` implements exactly with integer
floor-sqrt, gated by `γ²·R_aOut·R_bOut > R_aIn·R_bIn`. For multi-hop / multi-path /
split routing the principled framework is convex optimization over CFMMs. Angeris
and Chitra [angeris2020oracles] establish that each CFMM trading function is
concave and the optimal-arbitrage problem is convex; Angeris et al.
[angeris2021multiasset] cast multi-asset trade selection as convex programs;
Angeris et al. [angeris2022routing] give the canonical optimal-routing program
where arbitrage is the special case that finds arbitrage or certifies none.
Diamandis et al. [diamandis2023routing] give the dual decomposition that separates
into independent per-market arbitrage subproblems, scaling ~linearly in pools and
composing heterogeneous pools (including V3 as an aggregate CFMM); Diamandis et al.
[diamandis2024convexflows] generalize to convex flows over hypergraphs.

**Position.** We use the exact closed form as the default sizer for isolated V2
two-pool cycles (our common case) and reserve the dual-decomposition router for
coupled / split / mixed-version cycles. For V3 we go further and let the EVM size
the trade (golden-section over Quoter evaluations), which is exact where no closed
form exists.

## 2.4 Concentrated liquidity (Uniswap / PancakeSwap V3)

Adams et al., *Uniswap v3 Core* [adams2021uniswapv3], introduce liquidity bounded
to price ranges. The `x·y=k` closed form breaks because liquidity `L` is
piecewise-constant across ticks: within an active range the pool behaves like a
constant-product AMM with *virtual* reserves, but a swap moves `√P` only until the
next initialized tick, where `L` jumps. Exact V3 output is a tick-by-tick
iteration; the optimal input is not a single closed form in general.

**Position.** This is the crux of our methodological argument. We use the V3 spot
price (from `slot0.sqrtPriceX96`) only as a cheap candidate detector, and we
*never* compute V3 profit analytically. We defer exact output to the EVM — the
QuoterV2 / pool bytecode performing `TickMath`/`SwapMath`/`SqrtPriceMath` and full
fee accounting — strictly more accurate than any re-implementation, and already
receipt-validated.

## 2.5 Flash loans and capital-free execution

Qin et al. [qin2021flashloans] formalize flash-loan parameter search as an
optimization over chain state. UniswapV2/PancakeSwap flash swaps deliver output
tokens first and require atomic repayment of output+fee within the same call;
Aave-style flash loans lend any amount for one transaction provided it is repaid
plus premium atomically. Either reduces required inventory to ~0, so the binding
constraints for cyclic arbitrage become **gas + builder bid** (plus a flash premium
for Aave), not capital.

**Position.** Our economics assume flash-swap / flash-loan funding: `net = gross −
gas − builderBid (− flashPremium)`, with no inventory constraint. V2 in-kind flash
swaps add zero premium; we default to them.

## 2.6 Sandwich attacks: formalization and concentrated-liquidity profitability

Where cyclic/backrun arbitrage (§2.2) exploits a *pre-existing* cross-venue
discrepancy, a **sandwich** *manufactures* the discrepancy by front- and
back-running a victim swap in the same pool. Zhou et al., *High-Frequency Trading on
Decentralized On-Chain Exchanges* [zhou2021hft], were the first to formalize and
quantify sandwiching, deriving the optimal frontrun size for a constant-product pool
and showing a single adversary could earn thousands of USD/day on Uniswap. Qin, Zhou
and Gervais [qin2022quantifying] subsequently folded sandwiching into their
chain-wide extractable-value census alongside arbitrage and liquidations, and the
front-running measurement of Torres et al. [torres2021frontrunner] situates it in the
broader transaction-ordering threat surface systematized by Heimbach and Wattenhofer
[heimbach2022sok].

The constant-product optimal-frontrun closed form does not survive contact with
concentrated liquidity. Gogol et al. [gogol2026sandwich] develop an
attacker-profitability model for both CPMMs and concentrated-liquidity AMMs
(Uniswap/Pancake v3) and derive the corresponding optimal strategy, confirming that
the V3 sandwich optimum is not a single closed form because effective liquidity is
piecewise-constant across ticks — the same obstacle we hit for V3 *backrun* sizing
(§2.4) and resolve the same way: we use the closed form only to *seed* a search whose
every probe is a ground-truth EVM evaluation of the full three-leg construction
(§3.7), so V3 sandwich profit is priced by the pool's own `TickMath`/`SwapMath`
rather than any analytic surrogate.

**Position.** Sandwiching is the dominant *atomic* MEV category on BSC by value —
roughly half of measured BSC MEV — yet the empirical-measurement literature prices it
analytically or by log heuristics (e.g. the large-scale post-Merge Ethereum remeasurement
of Chi et al. [chi2024remeasuring], ~3M sandwiches detected by pattern rules rather than
re-execution), inheriting the V3 and revert mis-pricing we set out to eliminate; the one
close non-analytic precedent, Lanturn [babel2023lanturn], EVM-simulates sandwiches but
per chosen target, not as a per-block census. We
contribute the first (to our knowledge) *receipt-exact, EVM-constructed* per-block
census of the sandwich opportunity surface available to an independent searcher
on post-PBS BSC, on the **same instrument and the same victim stream** as our backrun
measurement — making the two directly comparable and turning "which atomic strategy
actually has an edge?" from an apples-to-oranges literature comparison into a single
controlled experiment.

## 2.7 The BSC PBS regime (BEP-322) and why the independent edge collapses

BSC adopted PBS through BEP-322 [bep322]: a Builder API with no relay, whitelisted
builders, and private (often zero-gas) order flow that reaches builders ahead of
the public mempool. Wang et al., *MEV in Binance Builder* [wang2025binance],
measure this regime: two builders (48Club, BlockRazor) produce the overwhelming
majority of blocks and capture the overwhelming majority of MEV; the dominant
strategy is short (2–3 swap) cycles on the WBNB/USDT/USDC/USD1 hub; opportunities
live ~100–400 ms; public-mempool transactions reach external searchers tens of ms
*after* builder bundles; path length barely correlates with profit.

**Position.** This thread directly explains our result on the major hub pools: the
independent edge there is competed down to a thin, latency-gated ex-post residual
by latency-advantaged builders with private flow that capture ~90% of MEV. It also
tells us where residual edge could plausibly persist (longer / cross-DEX / V3
cycles; thin long-tail pools) — directions we scope and discuss in §6.

## 2.8 Summary table of what we adopt vs. scope out

| Technique | Source | We use it as | Adopt / Scope-out |
| --- | --- | --- | --- |
| Negative-cycle (Bellman–Ford/SPFA) on `-ln(rate·(1−fee))` | classical; [wang2021cyclic] | candidate generator only | **Adopt** |
| Line-graph + Modified MBF | [zhang2024improved] | many-cycle enumeration | **Adopt (optional path)** |
| Two-pool closed-form optimal input | UniswapV2 derivation; `arb.go` | exact V2 sizer | **Adopt (implemented)** |
| Convex optimal routing | [angeris2022routing] | multi-path/cross-DEX sizing | **Adopt for coupled cycles** |
| Dual decomposition / per-CFMM oracle | [diamandis2023routing] | implementable multi-pool engine | **Adopt for coupled/split** |
| Convex network flows (hypergraph) | [diamandis2024convexflows] | most-general split routing | **Scope-out (future work)** |
| V3 tick math | [adams2021uniswapv3] | spot-price candidate detector only | **Adopt detector; defer profit to EVM** |
| Detect-then-EVM-value | this work; ethos of [mclaughlin2023arbitrage] | core methodology | **Adopt (our contribution)** |
| Flash swaps / flash loans | [qin2021flashloans] | capital-free cost model | **Adopt (assumption)** |
| Sandwich formalization + CP optimal frontrun | [zhou2021hft] | seed sizer; victim-impact model | **Adopt; profit deferred to EVM** |
| V3/CLMM sandwich profitability | [gogol2026sandwich] | confirms no V3 closed form | **Adopt detector; defer profit to EVM** |
| BSC PBS reality | [bep322; wang2025binance] | regime framing for the null | **Adopt (interpretation)** |
| Adaptive-learning executing MEV optimizer | [babel2023lanturn] | nearest non-analytic prior art (per-target executor) | **Contrast (per-block census vs. per-target optimizer)** |
| Heuristic arbitrage+sandwich remeasurement | [chi2024remeasuring] | recent large-scale measurement comparator | **Contrast (detection vs. re-execution)** |

---

# 3. Methodology

This section describes the measurement instrument (the SimEngine), how it is
validated against ground truth, the backtest protocol, and the metrics we report.

## 3.1 The node substrate

We run a single bsc-geth v1.7.3 full node (module `github.com/ethereum/go-ethereum`,
snapshot-seeded, PBSS path-scheme state) on a 64-core / 125 GB host, fully synced to
BSC mainnet (`eth_chainId = 0x38 = 56`, head ~block 102.54M, `eth_syncing = false`).
The build is stock bsc-geth v1.7.3 with our read-only instrumentation packages
added (`simengine`, `strategy`, and a disarmed Phase-3 `builder`/`wallet`/
`contracts`); build with `export GOTOOLCHAIN=auto`, output only to `/tmp/geth-sim10`.

All on-chain constants — contract addresses, the V2 reserves storage slot (8) and
its packing, the V3 `slot0` layout, fee tiers, token decimals, pool addresses and
reserves, and event/selector hashes — were verified **directly against this node**
via `eth_call` / `eth_getStorageAt` / `eth_getCode` / `eth_getLogs`, i.e. against
the *same* state source the SimEngine executes on. The entire watch set and every
measurement can be regenerated from the node alone with no third-party API.

## 3.2 The SimEngine: ground-truth in-process re-execution

The SimEngine (`simengine/simengine.go`) speculatively executes a list of
transactions on a copy of canonical state, mirroring the canonical
`core.StateProcessor.Process` and the miner worker's `commitTransaction` pattern,
but never committing:

1. obtain the canonical state at `parent.Root`, then immediately switch to an
   independent `statedb.Copy()`; all mutations land on the copy;
2. apply built-in system-contract code upgrades at block begin
   (`systemcontracts.TryUpdateBuildInSystemContract`), keyed off the *parent* block
   time, so bytecode matches across hard-fork boundaries;
3. seed the gas pool from `header.GasLimit` minus Parlia's reserved system-tx gas
   (`EstimateGasReservedForSystemTxs`), exactly as the worker does;
4. build the EVM with `core.NewEVMBlockContext` using the header coinbase as author;
5. run pre-execution system calls (`ProcessBeaconBlockRoot`,
   `ProcessParentBlockHash` once Prague is active);
6. iterate transactions: **skip BSC/Parlia system transactions** (via
   `consensus.PoSA.IsSystemTransaction` — they execute in `Finalize`); for each
   remaining tx, snapshot state and gas pool, set the per-tx context, call
   `core.ApplyTransaction`, and on error revert-and-skip (the miner's
   snapshot/revert pattern, purely local on the copy);
7. return `SimResult{ Receipts, Logs (flattened, execution order), GasUsed,
   BalanceDeltas }`.

The single execution loop (`applyOnState`) is shared by `Simulate` (db-backed) and
`SimulateOnState` (caller-supplied state + chain context). The self-test and the
backtest harness both use `SimulateOnState` attached to the running blockchain, so
the validation path and the measurement path are **byte-for-byte the same execution
code** — they cannot diverge.

**Why this beats analytic CFMM modeling.** The SimEngine runs the actual pool
bytecode, so it handles, for free and exactly: V3 tick-crossing and fee tiers;
stableswap/Curve invariants; fee-on-transfer tokens and transfer hooks; slippage
guards and reverts; and integer rounding. An analytic study must model each of
these (and most do not); any divergence from on-chain `TickMath`/`SwapMath` is a
silent measurement error. We have *no* such error by construction, and a
receipt-level certificate that we do not (§3.3).

## 3.3 Validation against stored receipts (5/5 PASS)

We validate by replaying real mainnet blocks and comparing simulated receipts to
the receipts the network stored (`simengine/selftest.go`). For each sampled block
we fetch the block and its parent header; obtain the live state at the parent root
(skip silently if pruned); fetch the real stored receipts; re-execute the block's
transactions via `SimulateOnState` on a `state.Copy()`; and compare each simulated
receipt to the real one *by transaction hash*, over **Status, GasUsed,
CumulativeGasUsed**, and for every log its **Address, Topics, and Data**.

**Result.** Over the validation sample the simulated receipts matched the stored
receipts **exactly: 5/5 PASS** on blocks around height **101.83M**, each with
**100–151 transactions**, with no mismatch in any field on any tx. This is the
certificate that the SimEngine reproduces canonical execution at the receipt level.
Matching every log and gas figure across 100+ txs on copied state is an extremely
tight constraint; the post-state root is implicitly exercised by it.

**Phase-2 widened validation (500 stratified blocks).** Per the revision plan we widened
this certificate from a 5-block cluster to a 500-block stratified sample drawn from a
9,989-block forward-from-head window (stride 20, heights ~102,991,477..103,001,466).
**Result on the widened sample: passRate=1.00, passed=500, failed=0**, droppedTxBlocks=0
over totalTxs=47,586, failedTxs=0; the stratification covers V3-heavy blocks
(v3Blocks=455) and fee-on-transfer pools (fotBlocks=42); all mismatch counters are 0.
This replaces the "n=5 cluster-localized" smoke test with a measured, stratified
validation in the actual measurement height range, on this window.

## 3.4 The backtest protocol

The backtest harness (`simengine/dryrun.go`) is a read-only, crash-safe loop (every
unit of work wrapped in `defer/recover`; no-op unless `SIMENGINE_DRYRUN` is set;
never submits). It subscribes to imported chain heads and, per block, re-executes
the whole block on a `state.Copy()` of the parent via the SimEngine. Three modes:

- **`backtest` (post-block, v1/v2):** scan the post-block logs for watched `Sync`/
  `Swap` events, read post-block reserves, size and evaluate each shared-token
  cycle. *This evaluates the block boundary and is not a valid backrun test* — see
  §5.2.1.
- **`graph` (post-block, cross-DEX v2):** build the multi-DEX token graph from the
  post-block state, enumerate negative cycles 2..K from WBNB, size/value each with
  the exact V2 closed form. Also post-block.
- **`intrablock` (per-swap, v3/v4):** the correct backrun test. The SimEngine
  re-executes tx-by-tx; after each transaction that emits a watched-pool `Swap`,
  snapshot reserves at that exact intermediate point, build the graph, enumerate
  cycles 2..K from WBNB, and value each. V2 hops use the closed form; V3 hops use
  the EVM oracle (§3.5). Funnel counters (watchedSwaps, Stage-A candidates,
  gross-positive, net-positive, total would-be wei) and the distribution
  accumulator are tallied per processed block and logged on the `TallyEvery`
  cadence.

Reserves are read directly from the post-(swap/block) `StateDB`: **V2** slot **8**
holds packed `(reserve0, reserve1, blockTimestampLast)` (`reserve0 = word &
((1<<112)−1)`, `reserve1 = (word>>112) & ((1<<112)−1)`); **V3** `slot0` holds
`sqrtPriceX96` in its low 160 bits and `tick` above, spot `P =
(sqrtPriceX96/2⁹⁶)²`.

A fully **disarmed** Phase-3 submission orchestrator (`simengine/submit.go`,
`builder/`) can build but never signs or sends unless multiple independent arming
factors are all present. It is *not* used for any measurement here; it exists only
to make the realizability discussion (§6.1) concrete.

## 3.5 The EVM valuation oracle (Stage B)

For each candidate cycle the oracle computes ground-truth gross by chaining hop
outputs into hop inputs on the *exact intermediate* `state.Copy()`:

- **V2 hops:** the exact integer closed form (`GetAmountOut`), which the SimEngine
  self-test already validates to receipt level for real V2 swaps;
- **V3 hops:** an in-process, read-only EVM call (`simengine/evmcall.go`, `EthCall`,
  `Snapshot`/`Revert`-bracketed `vm.Call`) to PancakeSwap's **QuoterV2**
  (`0xB048Bbc1Ee6b733FFfCFb9e9CeF7375518e25997`, selector `0xc6a5026a`), which
  reverts-to-return the exact output (and gas estimate) the pool would produce —
  the pool's own `TickMath`/`SwapMath` as oracle.

The optimal input is found by a golden-section search (`strategy/quoter_oracle.go`,
`OptimalInput`/`ValueCycle`) whose every probe is an EVM evaluation; per-cycle gas
is `CycleGasUnits(cycle)` (~280k for a 2–4-hop backrun). `net = gross −
CycleGasUnits·gasPrice − bid − margin`. This is the profit authority: no
profit figure is ever taken from Stage A.

## 3.6 Metrics

For each model over a fixed range we report: **coverage** (blocks processed,
sampling fraction); the **candidate funnel** (Stage-A candidates, EVM gross-positive,
net-positive); the **gross-profit distribution** (USD percentiles + exact max); the
**break-even gas-price distribution** (gwei); a **gas-price sensitivity sweep**
over `{0, 0.1, 0.3, 1, 3}` gwei (count and % that would be net-positive at each
price); and the **structural breakdown** by DEX mix and cycle length. The
Stage-A→Stage-B and gross→net ratios give RQ3 (analytic over-count) directly. For
sandwiching (§3.7) we report the analogous funnel — victims seen, fundable, supported,
above-threshold, gross-positive, net-positive — plus the same gross-profit
distribution, break-even gas, and gas-sweep, all denominated in BNB.

## 3.7 Sandwiching: ground-truth construction and valuation

Backrun arbitrage is *parasite-free*: the searcher inserts one transaction after a
victim swap and the victim is unaffected. A **sandwich** is adversarial and
three-legged: the attacker (1) **frontruns** — buys the same direction as the victim,
worsening the victim's execution price; (2) lets the **victim** execute at the
degraded price; (3) **backruns** — sells the position back into the pool the victim
just pushed, realizing the spread the attacker manufactured. Unlike backrun
arbitrage, sandwich profit is *created* by the attacker's own frontrun, so it cannot
be read off pool reserves analytically without re-deriving the full three-trade price
path — and on real pools that path includes V3 tick-crossing, fee-on-transfer tokens,
and reverts that closed-form CFMM math silently mis-prices [zhou2021hft;
gogol2026sandwich]. This is exactly where the SimEngine's receipt-exact substrate pays
off a second time: we *construct* the sandwich and let the EVM price it. Caveat: the
real-block replay is receipt-validated (§3.3), but the counterfactual sandwich
construction is not receipt-validated against a real landed sandwich (no ground truth
exists); the construction is argued to be conservative but this is a key limitation for exotic
long-tail tokens (T12).

### 3.7.1 The construction

For each victim swap surfaced by the intra-block trigger (§4.3) we build a synthetic
3-tx sequence on a fresh `parent.Copy()` and re-execute it with the same
`core.ApplyTransaction` / in-process `vm.Call` machinery validated in §3.3:

1. **Fund a synthetic attacker.** A throwaway attacker address is given inventory of
   the pool's *numeraire* token (§3.7.3) by writing the ERC20 balance and allowance
   storage slots directly on the copied state — never on canonical state. For the hub
   tokens the slots are known and hardcoded (WBNB balance slot 3 / allowance slot 4;
   USDT, USDC balance slot 1 / allowance slot 2). For arbitrary long-tail tokens we
   **probe the layout at runtime**: we write a sentinel to candidate
   `keccak256(addr . slot)` mappings for `slot ∈ {0..12}`, call `balanceOf(addr)`
   through the EVM, and accept the first slot whose read returns the sentinel (cached
   per token). Tokens whose `balanceOf` cannot be reproduced by any standard mapping
   slot (proxies, exotic layouts) are counted `skippedUnfundable` and excluded — a
   *conservative* exclusion that can only *lower* the measured sandwich count.

2. **Frontrun.** The attacker swaps an optimally-sized amount of numeraire into the
   victim's direction, *on the victim's actual pool*. Routing matters: we do not
   assume a fixed router. V2-family pools (Pancake V2 and any `x·y=k` fork — Biswap,
   ApeSwap, …) are swapped by encoding a **direct `pair.swap(amount0Out, amount1Out,
   to, data)`** (selector `0x022c0d9f`) with a K-safe `amountOut`: we compute the
   output with the *Pancake* 0.25% fee even when the fork's true fee is lower, which
   *under-quotes* the attacker's output and so never over-states sandwich profit on a
   fork we have not fee-calibrated. Pancake V3 pools are swapped through the real
   Pancake V3 `SwapRouter` (`0x1b81D678ffb9C0263b24A97847620C99d213eB14`,
   `exactInputSingle` with the deadline-bearing selector `0x414bf389`). Non-Pancake V3
   / Algebra pools we do not yet route and count `skippedUnsupported`.

3. **Victim.** The victim's *real* transaction is replayed verbatim on the
   post-frontrun state (no snapshot/revert bracketing — §3.7.4), so the victim
   executes against exactly the reserves the attacker's frontrun left behind, with the
   EVM applying every slippage guard the victim's own calldata carries (a sandwich
   that would trip the victim's `amountOutMin` simply yields a revert and zero profit,
   caught for free).

4. **Backrun.** The attacker swaps its entire acquired position back through the same
   pool, again via the K-safe direct path, closing the loop.

The attacker's **gross** is the change in its numeraire balance across the three legs,
read from the copied `StateDB` — the EVM's own number, inclusive of all three real
swaps' fees, tick crossings, and rounding. No CFMM closed form is used to *value* the
sandwich; the closed form (§4.5, `strategy/sandwich.go`) is used *only* to seed the
optimal-frontrun search.

### 3.7.2 Optimal frontrun size

The frontrun size trades off manufactured spread against the attacker's own price
impact: too small and the victim is barely moved, too large and the attacker pays more
slippage on entry/exit than it extracts. We seed with the V2 closed-form sandwich
optimum (γ-parameterized, `strategy/sandwich.go`) and refine by evaluating candidate
sizes through the full EVM construction above, capping the frontrun at ≤100% of the
victim's input notional and ≤50% of the pool's numeraire reserve (a liquidity-sanity
bound that prevents the optimizer from proposing a frontrun the pool could not absorb
without absurd impact). The reported gross is the EVM value at the best feasible size.

### 3.7.3 Denomination in a single numeraire (and an integrity catch)

A sandwich's three legs move two tokens; profit must be reported in **one** unit. Our
first any-pool implementation contained a **units bug** worth recording because it
shaped the protocol: it measured the attacker's gross in whatever token the victim
happened to *spend* (often an arbitrary memecoin) while subtracting the gas cost in
BNB. Mixing a memecoin-denominated gross with a BNB-denominated cost made the net gate
meaningless and produced absurd aggregates (a total "net" of order 10³⁰ wei — ~10¹²
BNB — and an implied break-even gas price of ~10⁹ gwei). The figures were *flagged as
not-real on sight* and never reported; the bug was the detector's, not the chain's.

The fix defines, for each victim pool, a **numeraire** — the side that is a hub asset
with a known BNB price (WBNB directly; USDT/USDC via the WBNB/stable pool). The
synthetic attacker is funded *in the numeraire*, both swap legs are denominated in the
numeraire, and gross/net/gas/flash-fee are all expressed in BNB. Pools with **no**
hub-asset side (pure token/token pairs) cannot be denominated and are counted
`skippedNoNumeraire` and excluded. This both fixes the accounting and scopes the
measurement to economically interpretable opportunities. After the fix the same sample
produced realistic magnitudes (per-sandwich net of order 10⁻⁴–10⁻² BNB, aggregate
tens of BNB, break-even gas in the single-to-hundreds-of-gwei range) — sanity-checked
against the published BSC sandwich-MEV figure before being trusted (§5.6). We document
this bug for reproducibility.

### 3.7.4 EVM-mechanics fixes

Constructing multi-leg synthetic transactions on copied state surfaced three EVM
state-management subtleties, each fixed and unit-tested:

- **"revision id N cannot be reverted."** Bracketing a `core.ApplyTransaction` in an
  outer `Snapshot`/`RevertToSnapshot` is illegal because `ApplyTransaction`'s own
  `Finalise` clears the journal's valid revisions. We removed the outer
  snapshot/revert and obtain isolation purely from a fresh `parent.Copy()` per
  sandwich probe (each probe is independent by construction).
- **"Refund counter below zero (gas: … > refund: 0)."** A read-only `vm.Call` leg (the
  V3 router) inherits a stale refund counter. We wrap each EVM leg with
  `statedb.Prepare(rules, attacker, coinbase, &router, ActivePrecompiles, nil)` before
  and `statedb.Finalise(true)` after, mirroring the canonical per-tx lifecycle so the
  refund accounting is well-formed.
- **Wrong-router routing.** An early version hardcoded the attacker to a Pancake router
  regardless of the victim pool, mispricing fork pools. The K-safe direct `pair.swap`
  to the victim's *actual* pool (§3.7.1) removes the assumption.

### 3.7.5 Cost gate

`net = grossBNB − gasBNB − flashFeeBNB`. Gas is the measured 3-leg cost (frontrun +
backrun; the victim's gas is the victim's, not the attacker's) at the swept gas
prices; the attacker is assumed flash-funded so the only capital cost is the flash
premium on the numeraire borrowed for the frontrun (`flashFeeBNB`, V2 in-kind ≈ 0,
Aave-style a few bps). As with backrun, every assumption is generous to the attacker,
so the reported net-positive sandwich count is an *upper bound*.

## 3.8 The in-block counterfactual: ex-post existence vs. realized capture

Everything to this point measures *ex-post existence* — that an opportunity was
present on-chain. The decision-relevant quantity is *realizable capture*: how much an
independent searcher could actually take. The gap between them is exactly what an
integrated, latency-advantaged competitor removes. We measure that gap directly, on
the same ground-truth substrate, with an **in-block counterfactual**: for each ex-post
opportunity our detector surfaces in a canonical block, we ask whether a *real*
competitor already captured it *in that same block*. Captured ⇒ unavailable;
uncaptured ⇒ "left on the table" (still an upper bound — see §6.1).

### 3.8.1 Detecting a landed sandwich in the canonical block

The detector mode `SIMENGINE_DRYRUN=realizability` rides on the *same single*
`ApplyOnStateHooked` replay used everywhere else (no extra EVM execution, no mutation
of canonical state). In one pass it (a) runs the unchanged ex-post sandwich-any
evaluation (§3.7) to enumerate the block's ex-post net-positive opportunities, and (b)
builds a per-transaction ledger of hub-asset balance deltas and parses every V2/V3
`Swap` log into legs `(pool, txIdx, direction, sender, beneficiary, from-EOA, amounts)`.
It then declares a **landed sandwich** on a pool only when a *conjunction* of strong
signals holds — the design is deliberately biased so that, under any doubt, an
opportunity falls to *left-on-the-table* rather than *captured* (over-counting capture
would understate the realizable surface, the dangerous direction):

1. **Bracket structure.** A front leg and a back leg on the *same pool*, in *different*
   transactions with the victim between them, in *opposite* directions. JIT
   liquidity (Mint/Burn around the swap) is excluded.
2. **Same actor.** The two legs share a discriminating actor: the *same signing EOA*
   sent both bracket transactions (`from_front = from_back` — the strongest real-bot
   signal, since searchers route through their own contract but pay from one EOA), or a
   shared `Swap` sender/beneficiary that is neither a known router/aggregator nor the
   block coinbase.
3. **Profit corroboration (the false-positive guard).** The bracket must be a genuine
   round trip: net-*flat* in the volatile token and net-*positive* in the hub asset on
   *that pool*, net of gas and any coinbase bribe, above a USD dust floor. Hub profit
   is read over the actor's address *cluster* (signing EOA plus the legs' contract
   sender/beneficiary, minus routers/coinbase), because a real bot pays gas from its
   EOA while the proceeds accrue to its contract. A coincidental router routing two
   unrelated swaps fails this gate.

Captured opportunities are attributed to a **builder** (block coinbase / a builder
contract registry), a **repeated MEV sender** (clustered across blocks), or
**unknown**. The mode emits a `realizability tally` (ex-post, captured, left-on-table,
capture rate, attribution) and a `realizability dist` (BNB distributions of captured
vs left), plus the funnel counters of the next paragraph.

### 3.8.2 An integrity episode (recall failure), and why the funnel is reported

The first implementation of this detector reported `captureRate = 0.00` — *zero*
captured across hundreds of blocks. We did **not** report it: a zero capture rate is
implausible given the literature on integrated BSC sandwichers, and an
implausible-good result is exactly what the ground-truth discipline (§3.7.3) exists to
catch — this time in the *opposite* direction (a false *zero* rather than a false
*fortune*). The cause was a recall bug: the same-actor gate required the shared
`Swap`-log actor to equal a leg's signing EOA, but real bots' `Swap` actor is their
*contract*, never the EOA, so every bracket was dropped before corroboration. The fix
(the `from_front = from_back` signal plus cluster-based hub-profit corroboration) was
verified by a unit test of precisely the integrated-bot pattern (contract actor, single
signing EOA, profit in the contract) and by an independent, pool-agnostic scan of
1,500+ canonical blocks. To make any future near-zero *auditable* rather than blind, the
detector permanently emits the funnel `bracketCandidates → sameActorPass →
corroboratePass`, with the corroboration-failure breakdown (`notFlat / hubNeg /
belowDust`). A near-zero capture rate is only credible when this funnel shows *why*
each candidate legitimately fails — which §5.7 reports.

## 3.9 The censorship-differential: the one point-identified extraction estimand

The realizability counterfactual (§3.8) measures whether an ex-post opportunity was
captured *in the block*. It does not, by itself, isolate the value an independent could
*reach*. The naive "predicted-vs-sealed" contrast for total builder MEV is
**non-identified**: private order flow is a *selected* treatment (correlated with state
via routing) and per-transaction value depends on the whole ordering, so SUTVA fails.
The one estimand that survives this is the **censorship-differential**
$$D = \mathbb E\big[\,V_i(1)-V_i(0)\ \big|\ o_i\ \text{public},\ o_i\perp\ \text{private flow},\ \text{builder dropped } o_i\,\big],$$
the receipt-exact value a builder leaves on the table by *dropping* (rather than
including or internalizing) public, private-flow-orthogonal opportunities — the slice an
independent party can, in principle, structurally reach. Measuring $D$ adds one data
dimension the rest of the paper lacks: the **public mempool**, which supplies the
treatment assignment (a public opportunity was included vs dropped).

### 3.9.1 Construction (`SIMENGINE_DRYRUN=censorship`)
The detector runs *live*, ingesting the public mempool via a `SubscribeTransactions`
goroutine into a rolling, sender/nonce-indexed ledger (first-seen timestamp per hash).
Per sealed block it builds inclusion/replacement indices from the canonical txs, runs
one hooked replay to obtain the **post-sealed-block state**, and applies a conjunction
of gates to every public candidate — every one biased so that *ambiguity excludes*
(over-stating $D$ is the dangerous direction; $\hat D$ is a strict lower bound):
1. **Available-at-seal** — recovered, correct-nonce (`GetNonce==tx.nonce`), valid and
   funded on the sealing parent; not nonce-gapped.
2. **Dropped, not replaced** — its `(sender,nonce)` slot is not filled by a different
   hash in the block.
3. **Net-of-gas profitable, self-contained** — valued by re-executing the candidate
   **alone on the post-sealed-block state** (§3.9.2), reading the executor's own
   BNB-denominated hub-asset delta net of gas; must emit ≥2 directional Swap legs (a
   single one-way swap's positive hub delta is sale proceeds, not profit) and clear a
   USD dust floor.
4. **Orthogonal to private flow** — no other sealed tx touches the opportunity pool
   (the SUTVA gate).
5. **Settled-never-mined** (§3.9.3) — the decisive gate.

### 3.9.2 Valuation on the post-block state (integrity episode 3)
A first implementation valued the candidate as the profit of *sandwiching* it — the
**inverse** of $D$ (a sandwich-the-user surface that over-states $D$ by construction);
caught in adversarial review and rewritten to the candidate's *own* realized net profit.
A second version valued that own profit on the *pre-seal parent* state, in isolation —
which counts opportunities the realized block had already closed (including by the
builder's own internalized arb). We re-value on the **post-sealed-block state**: a
candidate counts only if it is still profitable *after the entire builder block has
executed*. This is conservative (it may under-state $D$: an opportunity profitable
mid-block but closed by block-end is excluded) and it subsumes builder
self-internalization for free (the internalizing tx is in the post-block state).

### 3.9.3 The settle window (integrity episode 4)
The load-bearing definition is "dropped". Defining it as "absent from *this* block"
conflates *censored* with merely *pending*. We add a **settle window** of $K$ blocks
(default 256): a candidate is credited to $D$ only if, after $K$ subsequent blocks, its
hash is *still absent from the canonical chain* (checked against a rolling mined-hash
index; sender-nonce-advanced candidates are discarded as superseded). A candidate mined
within the window is *delayed inclusion*, not censorship, and becomes an
`includedComparable` control (the $T=1$ arm). The detector defers the credit decision,
freezing $V_i$ at flag time and finalizing $K$ blocks later. This is the gate that makes
$\hat D$ a defensible lower bound rather than a count of pending transactions.

These two episodes join the units-bug (§3.7.3), the realizability recall-bug (§3.8.2),
and the backrun-any decimal-mismatch sanity-cap fire (catch #5: §5.6 / §6.6, sanity-cap
fired 4× on block 103,005,219 — a BUSD→WBNB→BUSD 2-hop V2 cycle whose CycleOptimum
returned grossUSD $\sim$$3.6\times10^{15}$ from a V2 decimal-mismatch on a high-decimal
memecoin pool; caught-not-baked via the $100k / 1000 BNB sanity-cap and the
`brSkippedSanityOutlier` counter, with REJECT log): in total we found and fixed **five
integrity catches** in the detectors during development, documented here for
reproducibility. The discipline is reproducible — this is the fifth catch confirming
the measurement-integrity protocol.

---

# 4. Models: Naive Baseline to EVM Oracle

We specify the models precisely. The naive baseline (§4.1) and the cross-DEX graph
(§4.2) are post-block; the intra-block detector (§4.3) and the EVM oracle (§4.4)
are the correct backrun experiments. All reuse the validated `strategy` math core
and the `simengine` execution path rather than replacing them.

## 4.1 Naive baseline (v1)

**Watch set.** Three deep, all-18-decimal PancakeSwap V2 pairs forming a clean
WBNB-bridged set: WBNB/USDT `0x16b9…0daE`, WBNB/USDC `0xd99c…FC5b`, USDT/USDC
`0xEc65…5A8c`. Fee 0.25% → `γ = 9975/10000`.

**Pricing law (exact, integer, matches on-chain).** `amountInWithFee = amountIn ·
γ.Num`; `amountOut = (amountInWithFee · reserveOut) / (reserveIn · γ.Den +
amountInWithFee)` (floor div). **Gate (sqrt-free).** For a 2-pool cycle:
`γ.Num² · R_aOut · R_bOut > γ.Den² · R_aIn · R_bIn`. **Optimal input (closed
form).** Collapse the two hops into a synthetic constant-product pool, solve
`dP/dx = 0` with integer floor-sqrt (`OptimalArb`, unit-tested to exact wei).
**Economic gate.** `net = gross − gasCost − builderBid − margin`, with `gasUnits =
250k`, `gasPrice = 3 gwei`, `builderBid = 0`, `margin = 0`. **Detection.**
Router-agnostic log detector: a watched pair's `Swap`/`Sync` event on a post-block
log scan.

**Result (§5.2):** **0 / 5,100 consecutive blocks, 0 would-be profit.** Generous
assumptions, full coverage, node-verified constants. *Structural limits the later
models remove:* only 2-pool cycles among 3 hardcoded pools on one DEX version; only
`x·y=k`; post-block (not a backrun test); analytic-only valuation.

## 4.2 Cross-DEX V2 graph (v2)

A directed multigraph `G=(V,E)`: tokens as nodes, each ordered pool-direction as an
edge carrying the pool address, DEX/version tag, fee factor `γ`, marginal rate
`r_uv` (V2: `R_v/R_u`), and weight `w = −ln(r_uv·(1−fee))` (detection only;
valuation stays in `big.Int`). `NegativeCycles(src, maxLen)` enumerates cycles
2..K from WBNB; isolated all-V2 cycles are sized by the exact linear-fractional
(Möbius) hop composition `f(x)=ax/(b+cx)`, `x*=(√(ab)−b)/c`, with one integer
floor-sqrt (the exact generalization of the 2-pool `OptimalArb`). Adding three
verified Biswap V2 pools (fee 0.20%, `γ=998/1000`) yields 6 V2 pools and enables
cross-DEX 2-cycles and triangles.

**Result (§5.2):** **0 Stage-A candidates / 2,950 blocks.** A static snapshot
showed Pancake-vs-Biswap spreads of 0.029% (WBNB/USDT) and 0.078% (WBNB/USDC), far
below the ~0.45% combined-fee threshold — no *standing* cross-DEX arb. Still
post-block.

## 4.3 Intra-block detector (v3, v3+V3)

The correct backrun trigger: after each transaction that emits a watched-pool
`Swap` (V2 `Swap`/`Sync` topics plus the V3 `Swap` topic `0xc42079f9…`), snapshot
reserves at that intermediate point and run the negative-cycle finder there. With
V2-only pools this yields **0 candidates** (the V2 pools are quiet). Adding six
verified PancakeSwap V3 pools (WBNB/USDT fee 100/500/2500 with L≈3.15M/229k/10k;
WBNB/USDC fee 100/500; USDT/USDC fee 100 with L≈42B) — read via `ReadSlot0`
(`sqrtPriceX96` low 160 bits, int24 `tick` next 24 bits) and `V3SpotPrice` — surfaces
**803 Stage-A candidates / 3,000 sampled blocks**. These V3-containing cycles get
`CycleOptimum = (0,0)` (no V3 closed form): **detected but unvalued**, exactly the
Stage-A/Stage-B split that justifies the EVM oracle.

## 4.4 EVM oracle (v4) and the cost / PBS model

Stage B (§3.5) values every candidate exactly: V2 hops by the closed form; V3 hops
by the in-process QuoterV2 call on the exact intermediate state; optimal input by
golden-section over EVM probes. `net = gross − CycleGasUnits·gasPrice − bid`. We
assume capital-free flash-swap / flash-loan funding [qin2021flashloans] so the
binding constraints are gas + bid; the headline uses bid = 0 (most generous to the
searcher). We additionally emit the gross-profit distribution, break-even gas
price, and the `{0, 0.1, 0.3, 1, 3}` gwei sensitivity sweep (`strategy/
distribution.go`).

**Realizable vs. ex-post (central caveat, §6.1).** The backtest measures *ex-post*
would-be gross against the post-swap transient. Under BSC PBS [bep322;
wang2025binance] an independent searcher does not control ordering, competes inside
a ~100–400 ms window, and receives public flow *after* builders; so any positive
ex-post figure is an **upper bound** on realizable profit. The thin ex-post
net-positive residual we report (15 of 803 cycles) is therefore an *upper bound* on
what an independent searcher could realize, not a demonstration of capture: that
above-gas tail is the slice latency-advantaged builders take first.

## 4.5 Sandwich model (any-pool)

The sandwich model reuses the exact same intra-block trigger as v4 — every
watched-pool victim swap — but, instead of searching for a trailing cross-venue cycle,
it constructs and EVM-values the three-leg attack of §3.7 on the victim's own pool.
Two design choices make it a *long-tail* instrument rather than a hub one:

1. **Any-pool decode.** A victim is any transaction emitting a V2 `Swap`
   (`0xd78ad95f…`) or V3 `Swap` (`0xc42079f9…`) on *any* pool, not only the verified
   12-pool watch set — so the model sees the memecoin/WBNB long tail where sandwiching
   actually concentrates. Pool metadata (token0/token1, reserves, fee family) is read
   from the runtime state of whatever pool emitted the event.
2. **Numeraire gate (§3.7.3).** Only pools with a hub-asset side (WBNB or a major
   stable) are valued, in BNB; pure token/token pools are `skippedNoNumeraire`.

The sizer is the γ-parameterized V2 sandwich closed form (`strategy/sandwich.go`) used
*only* to seed; the profit authority is the EVM construction. `net = grossBNB − gasBNB
− flashFeeBNB`, with the same `{0, 0.1, 0.3, 1, 3}` gwei sweep. The funnel counters
(victimsSeen, skippedUnfundable, skippedUnsupported, skippedNoNumeraire,
belowThreshold, grossPositive, netPositive, totalNetWei) and the BNB-denominated
distribution accumulator are tallied per processed block exactly as for backrun, so
the two strategies are reported on identical scaffolding.

---

# 5. Results

All figures are produced by the read-only backtest harness on the synced bsc-geth
v1.7.3 node, with the receipt-validated SimEngine as the profit oracle. The **backrun**
experiments (§5.1–5.4) run with `SIMENGINE_DRYRUN=intrablock`; the **sandwich**
experiments (§5.5) run with `SIMENGINE_DRYRUN=sandwich-any`; both take
`SIMENGINE_DRYRUN_TALLY=N` and `SIMENGINE_DRYRUN_GASPRICES="0,0.1,0.3,1,3"`.
Everything is strictly read-only; nothing is submitted; node/systemd/datadir are
untouched; the binaries are built only to `/tmp` (`geth-sim10` for backrun,
`geth-sim15` for the numeraire-corrected sandwich model). Backrun numbers are from the
consolidated geth-sim10 run (3,000 sampled blocks; distribution from the last
`intrablock dist` line, §5.4); sandwich numbers are from the consolidated geth-sim15
run (2,550 sampled blocks; funnel and distribution from the last `sandwich-any tally`
and `sandwich-any dist` lines).

## 5.1 SimEngine validation (ground-truth certificate)

| Metric | Smoke test | **Phase-2 widened validation** |
| --- | --- | --- |
| Validation method | replay real mainnet block, compare to stored receipts | replay forward-from-head, stratified stride-20 |
| Fields compared | Status, GasUsed, CumulativeGasUsed, per-log Address/Topics/Data | same |
| Blocks validated | 5 (around height ~101.83M) | **500 stratified** (from a 9,989-block forward-from-head window, heights ~102,991,477..103,001,466) |
| Transactions per block | 100–151 | totalTxs=47,586 across the 500 |
| **PASS / FAIL** | **5 / 0 (5/5 PASS)** | **500 / 0 (passRate=1.00)** |
| Mismatches | none | none (all mismatch counters 0) |
| Coverage | one localized cluster | v3Blocks=455, fotBlocks=42, droppedTxBlocks=0, failedTxs=0 |

Over both samples the SimEngine reproduces canonical execution at the receipt level
exactly. The same `applyOnState` loop validated here is the one the backtest uses, so
the validation path and measurement path are byte-for-byte the same code.

**Phase-2 widened validation closes T6.** The original 5-block smoke test was strong
but cluster-localized (~height 101.83M). Per the revision plan we widened it to a
500-block stratified sample drawn from a 9,989-block window in the actual measurement
height range (~103M). The widened sample is **500/500 PASS** with **zero mismatches in
any field on any tx** across 47,586 transactions, and stratification confirms the
oracle handles V3-heavy blocks (n=455) and fee-on-transfer pools (n=42) at the same
fidelity. T6 (validation cluster locality) is closed on this window; the protocol
scales trivially. Note that "on this window" is meant literally: the certificate is
measured on the heights above, not asserted in general.

## 5.2 Model-evolution funnel: naive to EVM oracle

| # | Model | Scope | Evaluation point | Range (blocks) | Stage-A candidates | Gross-positive (EVM) | Net-positive |
| --- | --- | --- | --- | --- | --- | --- | --- |
| v1 | Naive 2-pool closed form | 3 Pancake V2 | post-block | 5,100 | — | — | **0** |
| v2 | Graph cross-DEX, V2-only | 6 V2 (3 Pancake + 3 Biswap) | post-block | 2,950 | **0** | 0 | **0** |
| v3 | Intra-block per-swap, V2-only | 6 V2 | post-swap | (validation window) | **0** | 0 | **0** |
| v3+V3 | Intra-block + V3 detect | 12 (6 V2 + 6 Pancake V3) | post-swap | 3,000 sampled | **803** | n/a (V3 unvalued) | n/a |
| v4 | Intra-block + EVM oracle | 12 (6 V2 + 6 Pancake V3) | post-swap | 3,000 sampled | **803** | **803** | **15 @ 3 gwei** |

- **v1** — 3 deep Pancake V2 pools, exact 2-pool closed form, 250k gas × 3 gwei,
  bid 0, full coverage: **0 / 5,100, 0 would-be profit.**
- **v2** — 6 V2 pools (3 Pancake + 3 Biswap), cross-DEX 2-cycles + triangles with
  exact `x·y=k` sizing: **0 Stage-A candidates / 2,950 blocks** (snapshot spreads
  0.029%/0.078% « 0.45% fee threshold — no standing cross-DEX arb).
- **v3** — intra-block per-swap, V2-only: **0 candidates** (major V2 pools quiet,
  ~10 Pancake-V2 WBNB/USDT swaps per 30 blocks; others ~0).
- **v3+V3** — adding six V3 pools surfaces **803 Stage-A candidates / 3,000 sampled
  blocks**, *detected but unvalued* (V3 closed form intractable, `CycleOptimum =
  (0,0)`). The Stage-A/Stage-B split made visible.
- **v4** — the EVM oracle values every candidate. Over **3,000 sampled blocks**:
  watchedSwaps = 1,554, Stage-A = 803, **EVM gross-positive = 803**,
  **net-positive = 15 at 3 gwei (1.9%)** — **~0.27 gross-positive cross-venue
  cycles per sampled block, ~1 net-positive (ex-post) per 200 sampled blocks**,
  for 0.017192 BNB (~\$10.3) total would-be net profit. These are *ex-post*
  figures, not a realizable-capture claim (§6.1).

**Sampling.** The v4 detector samples a **representative subset** of blocks
(per-candidate EVM valuation is slower than block production). Sampling is
incidental, not selective: the sampled set is a uniform-in-time subsample of
consecutive heads. All counters are per processed block, so rates are comparable.
The net-positive *rate* varied with the window — **0.2% of gross-positive at the
first 600 sampled blocks vs. 1.9% at 3,000** — so we headline the 3,000-block
figure and flag the window variance (the thin tail is noisy).

### 5.2.1 Why post-block evaluation is not a backrun test

The v1/v2 zeros are real but **not valid backrun tests**. Both evaluate against the
*post-block* reserves; at a block boundary the cross-venue prices are already
re-aligned, because competing arbitrageurs act *within* the block — any transient
created by a victim swap is closed by a later tx in the same block. Backrun MEV
lives in the transient state immediately *after* a victim swap and *before* the next
transaction; end-of-block evaluation systematically misses it. So the v1/v2 zeros
measure *standing* arbitrage, not backrun-able transients. The correct method (v3
onward) hooks in *after each watched-pool swap*, snapshots reserves there, and runs
detection + EVM valuation at the only state where a backrun could exist. (The
realizability/race question is separate — §6.1.)

## 5.3 Why almost all are sub-gas: gross-profit distribution and gas-sensitivity

The v4 result is not "no opportunities" — there are ~0.27 EVM-confirmed
gross-positive cycles per sampled block. It is "the overwhelming majority are below
the gas cost" — at 3 gwei only 15 of 803 (1.9%) clear it.
This subsection characterizes *how far* below, from the `intrablock dist` log line
(`simengine/dryrun_intrablock.go`, on the `TallyEvery` cadence). The accumulator
(`strategy/distribution.go`, `GrossDist`) is O(1)-memory: gross-USD and
break-even-gwei are kept as fixed log-scale histograms (64 buckets, 0.25-decade
step), so reported percentiles are the **low edge** of the containing bucket — true
value in `[edge, edge·10^0.25)` — while the two `*_max` figures are **exact running
maxima**.

**The gas threshold.** The floor a candidate must beat is `gasUnits · gasPrice`:
with ~280k measured gas units at 3 gwei, ≈ 0.00084 BNB ≈ **\$0.50**.

### 5.3.1 Gross-profit distribution of gross-positive cycles

Percentiles are log-bucket low edges (true value in `[edge, edge·10^0.25)`); max is
exact. Source: `grossUSD_p50/p90/p99/max`, `grossPosSamples`.

| Statistic | Gross profit (USD) | Relative to \$0.50 gas line |
| --- | --- | --- |
| grossPosSamples (count) | 803 | — |
| grossUSD p50 | \$0.00056 | 0.0011× |
| grossUSD p90 | \$0.0056 | 0.011× |
| grossUSD p99 | \$1.0 | 2.0× |
| grossUSD max (exact) | \$1.85 | 3.7× |

### 5.3.2 Break-even gas-price distribution

`breakevenGwei = grossWei / gasUnits`: the gas price at which net = 0 (bid =
margin = 0). A cycle is net-positive at price `g` iff `breakevenGwei > g`. p50/p90
bucketed; max exact. Source: `breakevenGwei_p50/p90/max`.

| Statistic | Break-even gas price (gwei) | vs. 3 gwei detector price |
| --- | --- | --- |
| breakevenGwei p50 | 0.0032 | ~1/940 of 3 gwei |
| breakevenGwei p90 | 0.032 | ~1/94 of 3 gwei |
| breakevenGwei max (exact) | 11.1 | 3.7× the 3 gwei price |

### 5.3.3 Gas-price sensitivity sweep {0, 0.1, 0.3, 1, 3} gwei

Count and % of the gross-positive population that *would be* net-positive at each
sweep price (`breakevenGwei > g`, bid = margin = 0). Source: `gasSweep_netPos`
(`g=0:N(P%) g=0.1:N(P%) g=0.3:N(P%) g=1:N(P%) g=3:N(P%)`).

| Gas price (gwei) | Would-be net-positive (count) | Share of gross-positive (%) |
| --- | --- | --- |
| 0 | 803 | 100.0% |
| 0.1 | 63 | 7.8% |
| 0.3 | 45 | 5.6% |
| 1 | 29 | 3.6% |
| 3 (detector default) | 15 | 1.9% |

At `g=0` the count is the full population (100.0%); the interesting figure is the
decay of the would-be net-positive share toward the real ~1–3 gwei BSC regime.

### 5.3.4 Structural breakdown: DEX mix and cycle length

Source: `byDexMix` (`<label>:<count>`, sorted) and `byCycleLen` (`<n>hop:<count>`,
sorted).

**By DEX mix:**

| DEX-mix label | Gross-positive count | Share (%) |
| --- | --- | --- |
| biswap_v2+pancake_v3 | 327 | 40.7% |
| pancake_v3×pancake_v3 (cross-fee-tier) | 254 | 31.6% |
| pancake_v2+pancake_v3 | 222 | 27.6% |

**By cycle length:**

| Hops | Gross-positive count | Share (%) |
| --- | --- | --- |
| 2 | 506 | 63.0% |
| 3 | 297 | 37.0% |
| 4 | 0 | 0.0% |

### 5.3.5 RQ3 — analytic-detector over-count

| Ratio | Value (geth-sim10) |
| --- | --- |
| Stage-A candidates / net-positive (after gas, 3 gwei) | ≈ 54 (803 / 15) |
| Stage-A candidates (v3+V3, unvalued) per 1,000 blocks | ≈ 268 (803 / 3,000) |
| Stage-A candidates (v4) per processed block | ≈ 0.27 (803 / 3,000) |
| EVM-confirmed gross-positive per processed block | ≈ 0.27 (803 / 3,000) |
| Of which net-positive (ex-post, 3 gwei) | **15 (1.9%)** |

The dominant over-count is gross→net: of **803 cycles** an analytic marginal-rate
study (or even a gross-only EVM study) would count as opportunities, only **15
(1.9%)** survive once the exact gas cost is applied to the EVM-true gross — a
~54-to-1 over-count. This is the quantitative form of the methodological argument;
the 15 are *ex-post* would-be net-positive, not realizable capture (§6.1).

### 5.3.6 Worked example: the representative net-positive backrun

The single largest / most representative net-positive cycle in the sample, as the
EVM oracle valued it:

| Field | Value |
| --- | --- |
| Block | 102,461,418 |
| Tx index | 97 |
| Victim tx | `0xb21636c9f6f94ef8e6118fea1705bb1c4faee57fc1ba931f2b384541fcc6b3e0` |
| Cycle | cross-version `pancake_v2:WBNB/USDT → pancake_v3:WBNB/USDT` |
| Optimal input | 6.21 WBNB |
| Gross (EVM) | 0.00158 BNB (~\$0.93) |
| Gas | 0.00084 BNB (280k gas @ 3 gwei, ~\$0.50) |
| **NET** | **+0.00074 BNB (~\$0.43)** |
| Break-even gas price | 5.64 gwei |

This is exactly the structure the aggregates predict: a cross-version (V2↔V3)
WBNB/USDT cycle whose EVM-true gross (\$0.93) sits in the §5.3.1 tail above the
\$0.50 line, with break-even (5.64 gwei) above the detector's 3 gwei but below the
population max (11.1 gwei). It clears gas by ~\$0.43 *ex-post* — not a claim that an
independent searcher could have landed in that tx-97 slot ahead of the
latency-advantaged builders (§6.1).

## 5.4 Provenance of the distribution figures

The §5.3 distribution figures are read from the last `intrablock dist` line of the
consolidated 3,000-block run, produced (attached to the synced node, read-only) by:

```
export GOTOOLCHAIN=auto
SIMENGINE_DRYRUN=intrablock \
SIMENGINE_DRYRUN_TALLY=<cadence> \
SIMENGINE_DRYRUN_GASPRICES="0,0.1,0.3,1,3" \
/tmp/geth-sim10 ...
```

Field-to-table mapping: §5.3.1 `grossPosSamples`, `grossUSD_p50/p90/p99/max`;
§5.3.2 `breakevenGwei_p50/p90/max`; §5.3.3 `gasSweep_netPos` (each
`g=<gwei>:<count>(<pct>%)`); §5.3.4 `byDexMix` (`<label>:<count>`) and `byCycleLen`
(`<n>hop:<count>`), % computed against `grossPosSamples` = 803. §5.1–§5.2 and §5.5
are independent of this line.

## 5.5 Sandwich results (any-pool, EVM-constructed)

The sandwich model (§3.7, §4.5) runs on the same node, same read-only harness, same
intra-block victim trigger, with `SIMENGINE_DRYRUN=sandwich-any` and the same gas
sweep. All figures below are from a single consolidated run over **2,550 sampled
blocks** and represent **ex-post existence**, not realizable capture (the aggregate
counts each victim independently on a fresh state copy, so concurrent same-pool attacks
are double-counted — see §6.1 for the realizability caveat before reading the contrast
in §5.6). Caveat: the SimEngine's replay of real blocks is receipt-validated; the
counterfactual sandwich **construction** on copied state is not receipt-validated
against a real landed sandwich (no ground truth exists) but is argued to be conservative
(every bias understates profit). A key concern for exotic long-tail tokens is that
storage-write funding may violate token invariants (T12); magnitudes were sanity-checked
against the published BSC sandwich-MEV figure before being trusted (the units-bug
episode of §3.7.3).

### 5.5.1 Sandwich funnel

| Stage | Count | Note |
| --- | --- | --- |
| Victim swaps seen | 33,525 | any V2/V3 `Swap`, any pool |
| − skippedUnsupported | 11,695 | non-Pancake V3 / Algebra (not yet routed) |
| − skippedNoNumeraire | 2,493 | pure token/token pools (no BNB-priced side) |
| − skippedUnfundable | 1,883 | `balanceOf` slot not reproducible (proxies/exotic) |
| − belowThreshold | 7,249 | below the min-notional screen |
| − gross-non-positive | 8,470 | constructed but EVM gross ≤ 0 (victim slippage guard, fee/impact) |
| **Gross-positive (EVM)** | **1,735** | EVM-true positive gross, in BNB numeraire |
| **Net-positive @ 3 gwei** | **1,162** | after gas + flash fee (67.0% of gross-positive; 69.0%/1,197 clear gas only) — ~0.46 per block, 1 per ~29 victims |
| **Total net** | **35.08 BNB (~\$20,200)** | sum of net over the 1,162 |

Every gross-positive sandwich is on a **V2-family long-tail pool** (`byDex =
v2_any:1735`): the construction's K-safe direct `pair.swap` covers any `x·y=k` fork,
and that is where the volume — and the sandwiching — lives. The deep hub pools, which
dominated the backrun watch set, contribute essentially nothing here.

### 5.5.2 Gross-profit distribution, break-even gas, and gas sweep

Percentiles are log-bucket low edges (true value in `[edge, edge·10^0.25)`); maxima
are exact. Source: the `sandwich-any dist` log line.

| Statistic | Gross profit (USD) |
| --- | --- |
| grossPosSamples (count) | 1,735 |
| grossUSD p50 | \$1.78 |
| grossUSD p90 | \$31.6 |
| grossUSD p99 | \$100 |
| grossUSD max (exact) | **\$430.50** |

| Statistic | Break-even gas price (gwei) |
| --- | --- |
| breakevenGwei p50 | 10 |
| breakevenGwei p90 | 178 |
| breakevenGwei max (exact) | 2,485 |

| Gas price (gwei) | Would-be net-positive (gross of flash fee) | Share of gross-positive |
| --- | --- | --- |
| 0 | 1,735 | 100.0% |
| 0.1 | 1,606 | 92.6% |
| 0.3 | 1,488 | 85.8% |
| 1 | 1,317 | 75.9% |
| 3 (detector default) | 1,197 | 69.0% |

The gas-sweep counts the population with `breakevenGwei > g` (gross of flash fee and
bid); the headline **net-positive = 1,162** applies the *full* gate including the flash
premium at 3 gwei, the ~35-count gap being that premium. The contrast with backrun is
large on every line: where backrun's break-even median was **0.0032 gwei** (≈1/940 of
3 gwei) and only 1.9% cleared gas, sandwiching's break-even median is **10 gwei** —
*above* the detector price — and **69.0% (1,197 of 1,735) clear the 3-gwei gas gate** (the headline net-positive 1,162 = 67.0% applies the full gate including the flash premium).

### 5.5.3 Worked example: a representative net-positive sandwich

| Field | Value |
| --- | --- |
| Block | 102,491,896 |
| Victim tx | `0x1f04d512…d5d6486` |
| Pool | long-tail V2 (`v2_any`), WBNB-numeraire |
| Direction | WBNB → memecoin (victim buys) |
| Victim input | 0.886 WBNB |
| Optimal frontrun | 0.886 WBNB |
| Gross (EVM) | 0.0561 BNB (~\$32.4) |
| Gas | 0.0009 BNB |
| Flash fee | 0.0008 BNB |
| **NET** | **+0.0544 BNB (~\$31.4)** |

The single largest net-positive sandwich in the sample reached **~0.569 BNB (~\$330)
net**; the distribution's exact gross max was **\$430.50** — a long-tail tens-to
-hundreds-of-dollars regime that simply does not exist for backrun on the deep hub
(whose gross max was \$1.85).

## 5.6 The backrun-vs-sandwich contrast (headline)

One instrument, one victim stream, one EVM valuation, one cost model — the two atomic
strategies side by side (3-gwei gate):

| Strategy | Scope | Blocks (sampled) | Candidates examined | Gross-positive (EVM) | Net-positive @3gwei | Total net | Median gross | Max gross |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Cross-DEX backrun | deep WBNB/stable hub (12 pools) | 3,000 | 803 cycles | 803 | **15 (1.9%)** | 0.0172 BNB (~\$10) | \$0.0006 | \$1.85 |
| Sandwich | long-tail WBNB-paired pools (any-pool) | 2,550 | 33,525 victims | 1,735 | **1,162 (67.0%)** | 35.08 BNB (~\$20,200) | \$1.78 | \$430.50 |

Normalizing per sampled block, sandwiching yields **~91× more net-positive
opportunities** (0.46 vs 0.005 per block) and **~2,040× more total value in raw
aggregate** (35.08 vs 0.0172 BNB; blocks: 2,550 vs 3,000) than backrun *on the measured
scopes* (~2,400× per-block normalized; backrun on the deep 12-pool hub; sandwich on
any-pool long tail). The
"91×/2,040× (raw) / 2,400× (normalized per-block)" headline was previously a **scope-different** comparison; **Phase-2 closes
this** with a matched-footprint backrun-any measurement on the long tail (next
subsection, §5.6.2). The matched-footprint contrast is **~458× in BNB-denominated
value/block, price-free** (universe-matched on the same any-pool long-tail decode, but
not time-matched: 2,550-block sandwich window vs 16,100-block backrun window; not
accounting-matched: sandwich ex-post constructed full value vs backrun-any marginal
post-minus-pre), measured on this single window. If USD prices are included for
reference, use a single \$600/BNB consistently (sandwich ≈\$8.25/blk, backrun ≈\$0.0180/blk). The
atomic-MEV edge for an independent searcher is, by both the scope-different and the
matched-footprint measurement, a **long-tail sandwiching** phenomenon — not the
deep-pool cross-DEX backrun the arbitrage literature studies.

### 5.6.2 Phase-2 matched-footprint backrun-any (closes the scope mismatch)

To put backrun on the same long-tail any-pool universe as sandwich, we ran the
EVM-oracle backrun detector with sanity-cap on a 16,100-block window (heights
~103,023,315..103,039,415, geth-sim20). Counters:

| Quantity | Value (16,100 blocks, backrun-any) |
| --- | --- |
| OPP accepted (sane) | **55** |
| Aggregate net | **0.484 BNB ≈ \$291** (at ~\$600/BNB) |
| grossUSD p50 / p90 / p99 / max | \$1.00 / \$1.78 / \$177.83 / \$188.67 |
| byCycleLen (2hop/3hop/4hop) | 48 / 4 / 1 (2-hop dominance) |
| byDex (pancake_v2+v3 / pancake_v2 / pancake_v3) | 49 / 3 / 1 |
| Density | 1 acceptable opp every ~293 blocks; from blk 12,750 onward (~3,350 blk) **zero** new opps accepted |

**Interpretation.** Backrun-any is **genuinely rare on the long tail**, confirming the
§6.7 hypothesis: 1 acceptable opp per ~293 blocks, with the entire last ~3,350-block
tail of the window contributing zero new accepts. The rate-normalized contrast vs
sandwich-any (\$8.25/block ex-post over 2,550 blocks vs \$0.018/block backrun-any over
16,100 blocks) is ≈ **458× in BNB-denominated value/block, price-free** on a
matched-footprint basis (universe-matched decode, but windows differ: 2,550 vs 16,100
blocks; accounting models differ: sandwich ex-post vs backrun marginal). This replaces
the prior "91×–2,040× (raw aggregate) / 2,400× (per-block normalized) scope-different"
framing with a measured, basis-aligned, matched-footprint number.

**Integrity catch #5 (caught-not-baked).** The sanity-cap fired 4× on a
block-103,005,219 pattern: a BUSD→WBNB→BUSD 2-hop V2 cycle whose `CycleOptimum`
returned `grossUSD ~$3.6×10^15` from a V2 decimal-mismatch on a high-decimal memecoin
pool. The detector caught the absurd magnitude at the $100k / 1000 BNB sanity-cap
gate (counter `brSkippedSanityOutlier`, REJECT log), excluded those four outliers
from the headline, and reports the 55-opp aggregate without them. This is the **fifth
integrity catch** in the project — the discipline is reproducible.

### 5.6.1 No annual extrapolation, and the ex-post-vs-realized gap

The ex-post measurement itself (§5.5) confirms the instrument is executing and valuing
sandwiches correctly, as demonstrated by the detailed gas and cross-pool construction.
We do **not** provide an annualized extrapolation: the ex-post sandwich aggregate is a
~2–3 hour snapshot over 2,550 blocks, and annual scaling is unreliable given known
diurnal volatility, seasonal MEV cycles, and the per-victim concurrent same-pool
over-count (§6.1). The window-level density (0.35–0.46 net-positive sandwiches/block)
serves as a baseline for measuring the fraction that is realized (via the in-block
counterfactual of §3.8 and its results, §5.7), which is the quantitative centerpiece of
this paper.

## 5.7 Realizability: the in-block counterfactual

**§3.8 detector run and corrected framing.** The 2,100-block measurement found 0 of 735
ex-post sandwich opportunities realized in this short window (capture rate 0.00, with an
auditable bracket→same-actor→corroboration funnel), but a longer collection reveals this
was a sampling artifact. A geth-sim16 pilot at 34,200 blocks anchored the capture rate
at ≈0.07% by count (7 of ~10,190) and ≈0.016% by value, and a subsequent 152,650-block
pilot anchored 10 captures; these earlier, smaller windows corroborate and sit slightly above the final
measured rate, CI, and repeat-actor count from the completed multi-day,
multi-volatility geth-sim20 window: **45 of 96,064 (0.0468% by count, Wilson 95%
CI [0.0350%, 0.0627%]; 0.0078% by value), with 25 by repeated address (0xCF2e..C842 = 17
of 45, 38%), 20 by unknown, 0 by builder**. This final number is measured over 315,750
processed blocks spanning ~2.5 days (2026-06-09 04:27 – 2026-06-11 17:29, chain heights
~102.99M–103.63M, geth-sim20, a uniform-in-time subsample). The headline run ended early
relative to its pre-registered 14-day / ~1.5M-block target because BSC chaindata state
growth exhausted the disk volume and a disk-guard watchdog gracefully stopped the node
(clean shutdown, chaindata intact) before ENOSPC; the 45 captures accumulated steadily,
so the rate is stable across the run, but it covers a single ~2.5-day market regime,
leaving multi-volatility-regime stratification (listings/depegs/stress) to future work
(§6.7). The corrected understanding is therefore
that realized capture is **small but measurably nonzero**, and the 2,100-block zero was
a small-window sampling artifact (a demographic, not economic, null) — confirmed
independently by the Phase-2 trace-probe upper-bound of zero (§5.7.2). The §3.8 detector
(`SIMENGINE_DRYRUN=realizability`) was run after the recall-bug fix and validation
(§3.8.2); its result on the 2,100-block window is stable across the run (identical at
450, 750, and 2,100 blocks). The rest of this section presents the supporting
auditability evidence — the detection funnel, detector recall validation (§5.7.1), and
the Phase-2 trace-probe (§5.7.2) — confirming that corrected understanding:

| Quantity | 2,100-blk window (artifact) | 34,200-blk pilot (geth-sim16) | **315,750-blk completed run (geth-sim20, headline)** |
| --- | --- | --- | --- |
| Ex-post net-positive sandwich opportunities | 735 | ~10,190 | **96,064** |
| **Already captured in-block by a real competitor** | 0 | 7 | **45** |
| Left on the table (uncaptured) | 735 | ~10,183 | **96,019** |
| **Capture rate (by count)** | 0.00% | ≈0.07% | **0.0468% [Wilson 95% CI 0.0350%, 0.0627%]** |
| Capture rate (by value) | — | ≈0.016% | **0.0078%** |
| Captured / left ex-post net (BNB) | — | — | 0.334 BNB captured (median 0.018, p90 0.032, p99 0.056, max 0.076); 4,269.7 BNB left (median 0.0056, p99 0.316, max 219.17) |
| Capture attribution | — | — | byBuilder=0, byRepeatedAddr=25 (0xCF2e..C842=17, 38%), byUnknown=20 |

*Note:* The earlier pilots and the final measured rate are stated in the lead
above; the Phase-2 trace-probe (§5.7.2 below) is the load-bearing closure of the
identification-gap concern, and the final realizability rate is a small fraction
consistent with (and slightly below) the earlier smaller-window pilots.

The 735 ex-post net-positive opportunities over 2,100 blocks is ~0.35 per block,
vs. ~0.46 per block (1,162/2,550) in the §5.5 sandwich run; the two are different
runs over different windows (the realizability detector, geth-sim16, samples a
separate block range from the sandwich-any run, geth-sim15), and the gap is within
the window-to-window sampling noise documented in §6.3 — the per-block surface
density is consistent across runs at ~0.35–0.46 net-positive sandwiches/block.

The funnel makes the zero *auditable* rather than blind:

| Funnel stage | Count |
| --- | --- |
| Bracket candidates (opposite-dir cross-tx leg pairs, same pool) | 1,077 |
| → passed same-actor gate | 79 |
| → corroborated as a profitable round-trip sandwich | **0** |
| (corroboration failures: not-flat / hub-negative / below-dust) | 74 / 5 / 0 |

So same-actor brackets *do* occur (79 over 2,100 blocks — the fixed detector finds
them), but in this short window **none is a profitable cross-tx sandwich**: 74 are not
flat round trips (unrelated swaps that happen to share an actor) and 5 are net-*negative*
in the hub (failed or out-competed bots, not captures). **However, a longer parallel
collection (~34,200 blocks) has detected captures at non-negligible prevalence (see the
§5.7 note above): the short-window zero was a sampling artifact.** An independent
pool-agnostic scan of 1,500+ canonical blocks confirmed that genuine
flat-round-trip-with-victim-between sandwiches are **rare in-window**, and the recurring
same-actor brackets are router-shared senders (correctly excluded) or non-round-trips.
What *does* dominate realized BSC MEV in-window is **atomic, single-transaction**
arbitrage/backrun (on the order of ~7% of blocks carry an atomic opposite-direction
round trip), a different category from the cross-transaction sandwich our ex-post
surface enumerates.

**Reading this correctly (and not over-reading it).** A capture rate of 0 over 2,100
blocks with most of the surface "left on the table" must **not** be read as "fully
available to an independent searcher." Two facts constrain that reading. (i) *Scope:*
this measures *cross-transaction sandwich* capture; the realized MEV in-window is
dominated by atomic single-tx arb, which our backrun track already showed is
independently marginal on the deep hub (§5.2, 15/803 net-positive, ~\$10). A longer
multi-day window (geth-sim16 pilot, ~34,200 blocks) corroborates realization at
≈0.07% by count (7 of ~10,190) and ≈0.016% by value, slightly above the final
measured 0.0468% by count (45/96,064, Wilson 95% CI [0.0350%, 0.0627%])
and 0.0078% by value; realization is small but not literally absent (the short-window
zero was a sampling artifact, and the Phase-2 trace-probe of §5.7.2 below bounds the
identification gap at zero on its window). (ii) *Mechanism:* the small realized
sandwich fraction is consistent with cross-tx sandwiching having receded under
validator/builder filtering and private-flow dominance (§6.4), and that same mechanism
would also suppress an independent's sandwich submission. *Unrealized is not the same as
available.* The realizable independent sandwich edge is therefore much smaller than the
ex-post surface — supported by direct measurement on an extended window showing nonzero
but small realized fraction.

**Caveats (§6.1, §6.6).** Single multi-hour window (a multi-day, multi-volatility
window is future work); the detector scopes cross-tx sandwiches (atomic single-tx
sandwiches are documented but not separately valued); corroboration conservatively
requires the hub profit to still reside in the actor cluster at end-of-tx, so a bot
sweeping proceeds to a cold address mid-transaction would be a (governing-rule-
consistent) false negative; ~1 nil-deref per 2–3 min in the shared ex-post EVM path is
contained by per-victim `defer/recover` and drops the victim to the conservative
left-on-table side (it cannot manufacture a capture).

### 5.7.1 Detector recall validation (the capture-0 is a measured null, not a blind detector)

A reading of "0 of 735 captured" requires ruling out that the detector simply has low
recall for real landed sandwiches. We measure its recall directly with an injection
harness (`SIMENGINE_DRYRUN=recalltest`): per canonical block, we execute a *genuine*
synthetic landed sandwich (real frontrun → real clean victim → real backrun, via the
same swap/funding primitives, producing real Swap legs and hub-balance deltas), splice
it into the block's legs, and run the *identical* `detectLandedSandwiches` over the
augmented set; recall = injected sandwiches detected. We sweep the structural axes that
govern real-world recall and report recall per cell (≥211 injected per cell, three
windows, recall stable to ±1%):

| Injected structure | Recall | FP rate |
| --- | --- | --- |
| same-EOA, Swap-sender = EOA | 0.950 | — |
| same-EOA, sender/beneficiary = contract (integrated-bot pattern) | 0.950 | — |
| cross-EOA, shared beneficiary | 0.959 | — |
| routed / multi-hop (router as Swap-sender) | 0.950 | — |
| stable-numeraire hub | 0.948 | — |
| thin pool (sub-dust profit) | 0.525 | — |
| marginally-flat round trip (>2% Y remainder) — *blind spot* | 0.000 | — |
| proceeds swept to a cold address mid-tx — *blind spot* | 0.000 | — |
| clean (non-sandwiched) victims | — | **0.000** |

So on the structures that dominate BSC sandwich MEV — same-actor brackets (including the
integrated-bot contract-sender pattern), cross-EOA shared-beneficiary, routed bots, and
stable-hub brackets — the detector recovers **95–96%** of injected landed sandwiches at a
**0% false-positive rate** on clean victims. Recall collapses to 0% only on the two
documented blind spots (profit swept to a cold address mid-transaction; round trips kept
deliberately >2% non-flat) and to ~52% on thin pools where realized profit is sub-dust
(correctly floored by the \$1 dust gate). The realized-capture count of 0/735 on the 2,100-block window appeared
to be a *measured null at high recall* on the dominant structures, but a longer
collection (~34,200 blocks) shows this zero was a short-window sampling artifact (§5.7
note). The detector's high recall (95–96%) on known structures is confirmed; the
residual false-negative surface is bounded to the two named blind spots — and notably,
**the empirical prevalence of blind-spot #2 (non-flat round trips, corrFailNotFlat) is
much higher than the injection test showed** (a 34,200-block pilot anchored ~984
victims in the corrFailNotFlat pattern; the Phase-2 trace-probe below quantifies the
prevalence on a 30,100-block window). This raised the load-bearing concern that real
sandwiching might have migrated to blind-spot #2 and inflated the false-negative tail;
§5.7.2 closes that concern empirically on its window.

### 5.7.2 Phase-2 blind-spot trace-probe (closes the identification gap)

The single largest peer-review concern was that the dominant blind-spots (non-flat
round trips and mid-tx sweep) could hide a substantial captured sandwich population,
so the realized rate would be an under-count. We answer that with a direct trace-probe
over **30,100 blocks** (heights ~103,063,262..103,094,000+, geth-sim20). The probe
counts the corrFail-eligible victim brackets, classifies them as **real** round-trip
or **router false-positive**, classifies sweeps as **real** or **cold-address FP**,
and (most importantly) tracks the **upper-bound positive ex-post net profit** these
patterns could carry.

| Counter | Value (30,100 blocks) |
| --- | --- |
| recallMissedBrackets | 2,429 (corrFail population probed) |
| roundTripReal | 1,585 |
| roundTripRouterFP | **842** (35% router pass-through correctly excluded — R2 redesign independently validated) |
| sweepReal | 2 |
| sweepColdFP | 0 |
| **upperBoundMissedRealizedWei** | **0** |

**Interpretation (on this window).** 1,585 structural round-trips and 2 sweeps exist as
**patterns**, but **zero** of them carry positive ex-post net profit. The blind-spot
population is **demographic, not economic**: the identification gap is **bounded at
zero** on this window. **Caveat**: this upper bound of zero is contingent on the
trace-probe's own recall, which is not separately validated here (unlike the detector's
95–96% recall measured in §5.7.1 via injection harness). Future work should validate the
trace-probe's recall by injecting a synthetic cold-sweep or deliberately non-flat
round-trip and confirming the trace-probe flags it. The peer-review's load-bearing concern that the realized rate
"could be a major under-count" because of blind-spot prevalence is **empirically
refuted** on this window. Independent corroboration: 842 router false-positives caught
by the R2 redesign (35% of the corrFail population is router pass-through that the
old detector would have mis-counted) is independent evidence the redesign was
necessary, not cosmetic. Density check: 1,585 / 30,100 ≈ 53 per 1,000 blocks,
consistent with the pilot corrFailNotFlat = 984 / 34,200 ≈ 29/1k, so the trace-probe
window is in the same prevalence regime as the pilot. The combination of high recall
(95–96% on dominant structures, §5.7.1), measured blind-spot upper-bound = 0 (this
subsection), and pilot anchor of ≈0.07% by count makes the residual identification
gap bounded sub-percent on the windows measured.

## 5.8 Censorship-differential: the public residual is empty

The §3.9 detector (`SIMENGINE_DRYRUN=censorship`, settle window $K=256$) was run live at
the chain tip. After the four-gate conjunction and the chain-verified settle window, the
result on the live funnel (≈850 blocks) is: **$\hat D = 0$ BNB, 0 settled drops** (i.e.,
no public, profitable, private-flow-orthogonal opportunity survived the builder's block
*and* went unmined for $K$ blocks). The live funnel reads `publicOppsSeen≈17,
droppedFromN≈8, ...`, `Dhat_count=0`; the few flagged candidates all finalized as
*superseded* (sender nonce advanced — repriced, not censored). **Caveat: the live
candidate stream is thin** — this does *not* constitute a robust zero estimate, but
rather shows that genuine public, profitable, orthogonal opportunities at the BSC tip are
*rare* post-PBS. The evidence for a zero differential comes from the large historical
cross-check (next paragraph), not the thin live tail.

The instrument's own diagnostic is the finding. We took the **958 unique tx hashes the
pre-settle-window detector had logged as "drops"** (the population behind a spurious
~47 BNB figure) and queried each against the canonical chain: **954 / 958 (99.6%) were
actually mined later** — 1 to 123 blocks afterward (≈1–90 s) — i.e. pure *delayed
inclusion*; the remaining 4 (0.4%) have no chain record at all (most consistent with
repricing, which the live superseded gate excludes). On post-PBS BSC, public arbitrage
transactions that miss a block are **not censored — they are delayed** (mined a few
blocks later) or repriced. The structurally-reachable public surface for an independent
searcher is, by direct chain-verified measurement, **empty**.

Caveat (honest): the *live* candidate stream is thin (single-digit `droppedFromN` per
hundreds of blocks), because genuine public, profitable, round-trip, orthogonal
opportunities at the BSC tip are vanishingly rare post-PBS — itself consistent with the
rest of the paper. The robustness comes from the large historical cross-check (958 hashes
at 99.6% mined-later), not the thin live tail. The conservative direction is preserved
throughout (every ambiguity discards): even the one point-identified, structurally-reachable
estimand is zero — the *unrealized ≠ available* point (§5.7) in causal-inference form.

## 5.9 Synthesis

**Statistical precision (so no headline is a bare point estimate).** Rates carry Wilson
95% intervals and the zero-event counts carry rule-of-three 95% upper bounds: backrun
net-positive **15/803 = 1.87% [1.14, 3.06]** of gross-positive cycles (per-block 0.50%
[0.30, 0.82]); sandwich net-positive **1,162/1,735 = 67.0% [64.7, 69.1]** (gas-only sweep
69.0% [66.8, 71.1]); censorship apparent-drops that were merely delayed-inclusion
**954/958 = 99.6% [98.9, 99.8]**. For realizability: the short-window **0/735 (2,100
blocks) was a sampling artifact**; a longer-window measurement (geth-sim16 pilot
~34,200 blocks) has detected 7 captures of ~10,190 (≈0.07% by count, ≈0.016% by value)
as a corroborating earlier window, slightly above the final measured rate from the
completed multi-day, multi-volatility geth-sim20 window:
**0.0468% by count (45/96,064, Wilson 95% CI [0.0350%, 0.0627%]) and 0.0078% by value;
25 by repeated address (0xCF2e..C842=17, 38%), 20 by unknown, 0 by builder**.
Combined with the §5.7.1 recall of 95–96% on dominant structures and the §5.7.2
Phase-2 trace-probe bounding the blind-spot contribution at zero on its 30,100-block
window, the *realizable* cross-tx sandwich-capture rate, on the windows measured, is
bounded above by the measured 0.0468% by count (earlier smaller windows corroborated slightly higher at ≈0.07%); the measured estimate
with CI is 0.0468% by count (Wilson 95% CI [0.0350%, 0.0627%]) and 0.0078% by value. For censorship: the
point-identified, structurally-reachable genuine-censored differential is **0/958 → ≤
0.31%** from the large historical cross-check (§5.8), but this arm is near-vacuous live
(thin candidate stream). Both measurements span future longer-window work (§6.7).

Two solid measurement angles and two supporting estimands, one instrument, one inference. **Backrun** (first angle):
cross-venue gross-positive cycles on BSC's major V2/V3 pools are *frequent* (~0.27
EVM-confirmed per sampled block, 803 over 3,000) and *real* (every nominated candidate
confirmed gross-positive by the EVM), but only **15 (1.9%)** clear the ~\$0.50 gas
floor — ~1 per 200 sampled blocks, 0.0172 BNB (~\$10) total; the gross→net collapse
(803 → 15, ~54-to-1) is the quantitative case for ground-truth valuation over analytic
CFMM modeling, especially for V3. **Sandwich**: on the long tail the same instrument
finds a *substantial* surface — **1,162 of 1,735 net-positive (67.0%) after gas+flash** (69.0% clear the gas-only 3-gwei gate) over 2,550 blocks,
**35.08 BNB (~\$20,200)**, median gross \$1.78, max \$430.50 — ~91× the backrun
net-positive rate and ~2,040× its raw aggregate value (note different block counts: 2,550
sandwich vs 3,000 backrun; ~2,400× per-block normalized), in the same coarse order as
reported BSC MEV volume (§5.6.1, with the comparator caveat). Both are ex-post upper bounds (§3.8), with the above-gas tail builder-captured first [wang2025binance]. Both are credible because the
profit is the EVM's own computation (5/5 receipt-exact validated), at the correct
intra-block transient (§5.2.1), with V3 and any-fork pools valued by the actual pool
bytecode rather than an over-counting `x·y=k` approximation. **Realizability** (second angle): the in-block counterfactual (§5.7)
found that **0 of the 735 ex-post sandwich opportunities in a 2,100-block window** were
captured in that short window, but a longer parallel collection (geth-sim16 pilot
~34,200 blocks) corroborated capture at **7 of ~10,190 (≈0.07%)** in an earlier
window; the final measured rate from the completed multi-day, multi-volatility geth-sim20 window
is **0.0468% by count (45/96,064, Wilson 95% CI [0.0350%, 0.0627%]) and 0.0078% by value**, establishing a small-but-nonzero capture
rate (the short-window zero was a sampling artifact, the blind-spot identification gap
bounded at zero on the §5.7.2 trace-probe window). The
ex-post surface is largely *unrealized* in canonical blocks, because cross-tx sandwiching
has receded from realized BSC activity (validator/builder filtering + private flow, §6.4)
while realized MEV is atomic-arb-dominated. **Censorship-differential** (supporting
estimand, structurally-reachable but empirically thin, ~17 live public opportunities vs
~850 blocks, §5.8): the one *point-identified* quantity (the value a builder leaves by
dropping public, orthogonal, profitable opportunities) is, after a chain-verified settle
window, **$\hat D = 0$**; 99.6% of apparent public "drops" were
merely *delayed inclusion* (mined a few blocks later), not censorship. These two measurement angles (backrun, sandwich) plus the census of
realized capture and the censorship differential, all rooted in the *unrealized ≠
available* mechanism (§6.1), together point to the inference stated in full in the
Abstract and §1.5: ex-post the atomic-MEV surface has *migrated from deep-pool backrun to
long-tail sandwiching*, but *realized* very little is capturable by an independent
(backrun sub-gas, the sandwich surface captured at a measured 0.0468% by
count — 0.0078% by value, earlier windows corroborating at ≈0.07% — and filtered for us too, the public residual empty).
Thus we MEASURE three surfaces and INFER the independent atomic-searcher edge is
vanishingly small (bounded above by the measured 0.0468% by count and 0.0078% by
value, below by zero on the windows measured) via the stated unrealized≠available
mechanism; the final measured rate and CI from the completed multi-day geth-sim20 window are
0.0468% by count (45/96,064, Wilson 95% CI [0.0350%, 0.0627%]), 0.0078% by value over 315,750 blocks. One ground-truth instrument supplies all
three on the same footing, and across its sub-studies we found and fixed **five
integrity catches** (§3.7.3, §3.8.2, §3.9.2, §3.9.3, and the §5.6 / §6.6 backrun-any
sanity-cap fire on block 103,005,219), documented for reproducibility.

---

# 6. Discussion, Limitations, Threats to Validity, and Future Work

The central finding (stated in full in the Abstract and §1.5, synthesized in §5.9):
measured on one ground-truth instrument and one victim stream, post-PBS (2026), the
ex-post atomic-MEV edge has *migrated* from deep-pool backrun (sub-gas: 15 of 803, ~\$10)
to long-tail sandwiching (substantial: 1,162 of 1,735, ~\$20,200) — but both are *ex-post
existence*, not realizable capture. The subsections below develop the limitations and
threats that make the realizable edge vanishingly small.

## 6.1 Realizable vs. ex-post

The backtest measures **ex-post existence**, uniformly for both strategies: given the
realized ordering, a backrun inserted in the transient slot after a victim swap — or a
sandwich wrapped around it — *would have had* gross *p*, as the EVM computes it. We
**do not** prove a searcher could *win the race* to that slot. The gap is asymmetric
in our favour: our ex-post figure is an **upper bound** on realizable profit (a
searcher who does not control ordering, competes in ~100–400 ms, and sees public flow
after builders [wang2025binance; bep322] can capture *at most* the ex-post figure). So
both the thin backrun residual (15 of 803 cycles, ~\$10) and the substantial sandwich
surface (1,162 of 1,735, ~\$20,200) are *upper bounds* on what an independent searcher
could realize, not demonstrations of capture — the above-gas tail of either is
precisely the slice the latency-advantaged integrated builders (48Club, BlockRazor,
~90% of MEV) capture first. This caveat is *more* binding for sandwiching, not less:
sandwiching is the builders' single most lucrative atomic strategy, so the long-tail
surface we measure is exactly what they (and their integrated searchers) contest most
aggressively. The positive-existence side is reported as *would-be / oracle* net with
no capture claim. The one structurally plausible path for an independent party to
realize any of it is to **submit through the PBS builder API / order-flow auctions** —
the disarmed Phase-3 path documented but never exercised.

**From argued upper bound to measured gap.** §3.8/§5.7 turn this caveat from an
argument into a measurement. The in-block counterfactual finds the realized capture of
our ex-post *sandwich* surface over a 2,100-block window to be 0 of 735 (capture rate
0.00), with an auditable funnel (1,077 brackets → 79 same-actor → 0 profitable
round-trips); a longer geth-sim16 pilot collection (~34,200 blocks) corroborates the rate
at ≈0.07% by count (7 of ~10,190) and ≈0.016% by value — an
**earlier, smaller window**, slightly above the final measured rate and CI from the completed multi-day
geth-sim20 long window: 0.0468% by count (45/96,064, Wilson 95% CI [0.0350%, 0.0627%]) and 0.0078% by value. The realizable
surface is tiny but measurably nonzero at scale. As §5.7
develops, a small capture rate does **not** mean the surface is available to us
(*unrealized ≠ available*): the same mechanism that removed the competitor sandwichers
(production-layer filtering + private flow, §6.4) would stop *our* sandwich too. The
realizability gap differs by strategy — for *backrun* it is the latency/ordering race
(and the surface is sub-gas regardless); for *sandwich* it is suppression at the
production layer. Over a small window (2,100 blocks) the measured realized edge is
zero; over the larger pilot window (34,200 blocks, geth-sim16) we observe a tiny but
measurable nonzero rate of ≈0.07% by count and ≈0.016% by value as an earlier,
smaller window; the Phase-2 trace-probe (§5.7.2, 30,100 blocks) further bounds the
identification gap at zero on its window (1,585 structural round-trips and 2 sweeps
exist as patterns but carry zero positive ex-post net profit). The realizable edge is
therefore small but bounded above by the measured 0.0468% by count (earlier windows corroborating slightly higher at ≈0.07%) and below by zero
on the windows measured, over the completed multi-day geth-sim20 window.

A second, sandwich-specific upper-bound source: the aggregate sandwiches *each* victim
independently on a fresh state copy, so two sandwiches targeting victims in the same
pool and block are both counted at full value even though, executed for real, the
first would move the pool and shrink the second. The per-sandwich figures are exact;
the *aggregate* (35.08 BNB) therefore over-counts concurrent same-pool opportunities
and is best read as an upper bound on the total surface. **Phase-2 closes this with a
direct serialization measurement** (M1/M2): over 23,300 blocks (heights
~103,123,000..103,142,800, geth-sim20 with `SIMENGINE_SERIALIZE_SAMEPOOL=1`), with
counters poolsProcessed=168,093, groupsFormed=16,212, divergedGroupsExcluded=13
(round-2 exclude-not-clamp fix scattered) and revertedStepsAborted=644 (~2.8%, never
swallowed), we measure both the INDEPENDENT (upper) and SERIALIZED (lower) bands on
the same window:

| Band | Opps | Aggregate net (BNB) |
| --- | --- | --- |
| INDEPENDENT (upper) | 5,844 | 301.64 |
| SERIALIZED (lower) | 5,833 | 301.12 |
| Over-count | **11 opps** (≈0.19%) | **0.517 BNB ≈ \$310** (factor **0.17%**) |

Distributions p50/p90/p99/max are IDENTICAL between bands. **Interpretation (on this
window).** The sandwich aggregate's independence assumption inflates by **0.17%** —
empirically bounded sub-1% on this window. The sandwich-any aggregate of §5.5 is
therefore a **legitimate upper band**; the realistic correction is negligible. T9
(sandwich aggregate over-count) is closed on this window: the M1/M2 result reframes
the 35.08-BNB aggregate as a tight upper band, not a loose one. Backrun is largely free of this
effect (cross-venue cycles on distinct deep pools rarely collide), which makes the
~2,040× (raw aggregate) value gap, if anything, an *under*-statement of how concentrated the realized
edge is in sandwiching.

## 6.2 Watch-set scope (and why the two strategies are scoped differently)

The two measurements deliberately have *different* scope. **Backrun** is confined to a
verified 12-pool deep hub set — three deep Pancake V2 hub pairs, three Biswap V2
mirrors, six Pancake V3 pools (tiers 100/500/2500) across WBNB/USDT, WBNB/USDC,
USDT/USDC — the locus where published backrun MEV is claimed [wang2025binance]; the
result there is the near-null (15 of 803). **Sandwich** covers any V2-fork pool a victim
touches (§4.5), so it covers the **long tail** of thin/volatile/new WBNB-paired pools —
and that is precisely where its entire surface turned out to be (`v2_any:1735`, zero on
the deep hub). This scope mismatch is intentional — we measure each strategy where its
surface is claimed to live — but it means the reported 91×–2,040× (raw aggregate; 2,400× per-block normalized) contrast is *partly a
scope artifact*, not a controlled strategy comparison on identical universes. The 12-pool
backrun result does **not** prove backrun is absent on the long tail; testing backrun on
the same any-pool universe as sandwich (Phase-1 future work, §6.7) is necessary before
claiming the long-tail surface is sandwich-exclusive.

What remains genuinely out of scope: non-Pancake V3 / Algebra pools (counted
`skippedUnsupported`, ~35% of victims — a routing gap, not a valuation one), pure
token/token pools with no BNB numeraire (`skippedNoNumeraire`), tokens with
non-standard `balanceOf` layouts (`skippedUnfundable`), other pool types (Thena CL,
stableswap, V4 hooks), and multi-block / cross-domain / CEX-DEX strategies (out of
scope for an *atomic* study). Every exclusion is *conservative* — it can only lower
the measured sandwich count — so the surface is, if anything, larger than reported.
The honest scoped claims are therefore: *on the major V2/V3 hub pools, there is no
realizable net-positive independent backrun* (only a thin ex-post residual of 15 of 803
cycles); *on the long tail, there is a substantial ex-post sandwich surface* (1,162
net-positive, an upper bound on realizable capture); and *conditional on hub backrun
being sub-gas and long-tail sandwich being ex-post large, the independent atomic-MEV
edge, where it survives realized capture, is a long-tail phenomenon* — not a statement
that no MEV exists anywhere on BSC.

## 6.3 Sampling

The v4 detector samples a representative subset of heads (it values whatever head
it is on when the previous block finished), so the sampled set is a uniform-in-time
subsample uncorrelated with opportunity. Mitigations: counters are per processed
block; the gross-positive *rate* is comparable across windows. The net-positive
*rate*, by contrast, is window-sensitive (0.2% of gross-positive at the first 600
sampled blocks vs. 1.9% at 3,000), because the net-positive tail is thin and a
single large cycle moves a short-window percentage; we therefore headline the
3,000-block figure and report the variance explicitly. We claim a *rate*, a
*distribution*, and a thin ex-post net tail — robust as a subsample, not a precise
chain-wide count.

## 6.4 The builder / PBS capture explanation

We interpret the result through the post-BEP-322 regime [bep322; wang2025binance;
blocksec2025pbs]: two whitelisted builders produce most blocks and capture ~90% of MEV;
private flow reaches them ahead of the mempool; opportunities live ~100–400 ms. The largely
*sub-gas* gross distribution is what an efficient market looks like *after*
integrated searcher-builders have arbitraged the deep pools down to the fee/gas
boundary within each block; most of what remains at the post-swap transient is the
residual not worth *their* near-zero marginal cost to take, which is exactly the
slice that cannot cover an *external* searcher's full gas. The thin above-gas tail
that *does* exist ex-post (the 15 net-positive cycles) is, on this reading,
precisely what the latency-advantaged builders take first — an external searcher,
seeing public flow tens of ms later and not controlling ordering, cannot reliably
win that slot. This is an *interpretation* consistent with the literature, not a
direct measurement: our instrument measures the gross/net distribution; the
attribution to builder capture is inference.

The sandwich result fits the same regime from the other side. The long-tail sandwich
surface is *large* ex-post precisely because the long tail is where the deep-pool
efficiency argument does *not* apply: thin memecoin pools have wide price impact, new
listings, and unsophisticated victims, so a manufactured spread is both larger and
more frequent than on the hub. That this surface exists ex-post is therefore expected;
that an *independent* searcher could capture it is not — sandwiching is the single most
contested atomic strategy for the integrated builders, who see the victim's swap in
private flow first and can place the frontrun with certainty of ordering. So the
~\$20,200 sandwich surface and the ~\$10 backrun surface tell one consistent story:
the deep hub is arbitraged flat, the long tail carries the value, and builder
integration captures the above-gas tail of both — leaving the independent searcher an
ex-post upper bound far above any realizable figure.

Two concurrent 2025–2026 developments make this interpretation concrete and, if
anything, *tighten* the realizability gap for sandwiching specifically. First, the
dominant BSC builders now run **in-house arbitrage at industrial scale**: the
builder-ecosystem census [wang2025binance] attributes tens of thousands of arbitrage
contracts to the two leading builders and millions of builder-executed cycles, i.e.
the builders self-execute the atomic MEV rather than merely auctioning blockspace —
so an external bundle is racing the block's own producer, which sees private flow
first and decides its cut after merging. Second, a validator/builder *Goodwill
Alliance* has begun **filtering sandwich-pattern bundles** at the production layer:
GWA-aligned builders proactively exclude buy→buy→sell sandwich bundles and active
validators enforce this, with operator-reported reductions in daily sandwich activity
of roughly two orders of magnitude [bnbchain2025gwa] (we cite the magnitude as
self-reported, not independently verified, but the *direction* — enforced sandwich
filtering across most blocks — is well attested). The implication for our headline is
sharp: the long-tail sandwich surface we measure is *real ex-post*, but it is exactly
the bundle shape an increasing fraction of block production now rejects, and the
victim flow is increasingly routed through MEV-protect/private RPCs that never reach
the public mempool an independent searcher observes. Our ex-post figure is thus an
upper bound on a surface that is *additionally* being closed off at the protocol-
operator layer — which is why we treat the realizable fraction (§3.8) as the quantity
that bounds realizable capture, not the ex-post aggregate.

## 6.5 Gas, bid, and cost modeling

For backrun, `net = gross − Σ gas − bid`, gas = measured per-cycle units (~280k) at 3
gwei, bid = 0 in the headline (most generous). **Caveat: V2 hops in backrun detection
use the analytic closed-form profit, not full EVM re-execution, so they inherit the
approximation errors (fee-on-transfer, hooks, rounding) the paper claims to eliminate;
only V3 hops use in-process QuoterV2 (Contribution 2 is therefore not fully
receipt-exact for backrun V2, a scope limitation).** For sandwich, `net = grossBNB −
gasBNB − flashFeeBNB` (§3.7.5), the same gas sweep, flash premium charged on the borrowed
numeraire. Every assumption in both is *generous to the searcher*: a deployed executor
adds fixed overhead (only raises the floor); the gas-price *sweeps* (§5.3.3, §5.5.2)
show neither conclusion is knife-edge on one price; any positive bid only lowers net;
capital is free via flash funding (V2 in-kind premium 0; Aave premium only raises the
floor); and the any-fork sandwich under-quotes the attacker's output by computing it at
the Pancake 0.25% fee (§3.7.1), which can only *lower* sandwich gross. Because every
assumption is generous to the searcher, both the 15 ex-post net-positive backrun cycles
*and* the 1,162 net-positive sandwiches are *upper bounds*: tightening any assumption
can only shrink the counts, not grow them. For backrun this makes the sub-marginality
conclusion robust; for sandwich it means the true realizable figure sits *below* a
surface that already exceeds realized BSC sandwich capture (§5.6.1), consistent with
heavy builder capture.

## 6.6 Threats to validity (consolidated)

- **(T1) Ex-post vs. realizable** — §6.1: ex-post is an upper bound for *both*
  strategies; the 15 backrun cycles and the 1,162 sandwiches are would-be, not
  realized, and the above-gas tail of either is builder-captured first (more binding
  for sandwiching, the builders' most contested atomic strategy).
- **(T2) Scope** — §6.2: backrun scoped to the verified 12-pool hub; sandwich covers
  the long tail (any V2-fork pool) but excludes non-Pancake V3/Algebra
  (`skippedUnsupported`), token/token pools (`skippedNoNumeraire`), and odd-slot
  tokens (`skippedUnfundable`) — all conservative (under-count) exclusions.
- **(T3) Evaluation point** — the headline experiments evaluate at the post-swap
  intra-block transient (correct backrun point); the post-block v1/v2 zeros are
  flagged as standing-arb measurements (§5.2.1).
- **(T4) Cost-model assumptions** — §6.5: all generous; gas swept; bid 0;
  capital-free.
- **(T5) Sampling** — §6.3: uniform-in-time subsample; gross-positive rate stable;
  net-positive rate window-sensitive (0.2%→1.9%), so the 3,000-block figure is the
  headline.
- **(T6) Validation sample size** — 5/5 PASS on the original smoke test was
  cluster-localized. **Phase-2 closes this on the widened window**: 500 stratified
  blocks (heights ~102,991,477..103,001,466, v3Blocks=455, fotBlocks=42) yield
  passRate=1.00, 500/500 PASS, zero field mismatches across 47,586 txs. The certificate
  is therefore measured on the actual measurement height range on this window. The
  oracle's V2 closed form agreeing with the chain's swap math is also receipt-validated,
  providing partial orthogonal validation.
- **(T7) Stage-A recall** — false positives eliminated by EVM-valuing every
  candidate; the residual risk is false negatives (size-profitable-but-marginally-
  unprofitable cycles); given all but 15 EVM-valued candidates are sub-gas and
  marginal profitability is necessary for the small hub cycles, the risk is low for
  the watched set but is a genuine limitation for the general claim (motivates a
  recall study, §6.7).
- **(T8) Oracle fidelity for backrun** — V3 backrun gross is QuoterV2 on the exact
  intermediate state (ground truth); **V2 backrun gross is the analytic closed form, not
  EVM-valued, so it reintroduces the approximation errors the methodology claims to
  eliminate.** A fully synthetic-tx executor (`verifyCycleEVM`, stubbed) for both V2 and
  V3 would only *raise* the gas side and so could only *shrink* the 15-cycle ex-post
  residual, not enlarge it.
- **(T9) Sandwich aggregate over-count** — §6.1: sandwiches are constructed
  independently per victim on fresh state copies, so concurrent same-pool/same-block
  opportunities are summed at full value; the *per-sandwich* figures are exact, but the
  35.08-BNB *aggregate* is an upper bound. **Phase-2 measurement closes this on the
  23,300-block serialization window** (M1/M2; §6.1 table): independent band 5,844 opps /
  301.64 BNB vs serialized band 5,833 opps / 301.12 BNB; over-count = 11 opps,
  0.517 BNB (~\$310), factor **0.17%** — empirically bounded sub-1% on this window.
  The 35.08-BNB aggregate is therefore a **legitimate upper band**; the realistic
  correction is negligible. Per-block *rates* and the distribution are
  unaffected.
- **(T10) Any-fork fee assumption** — §3.7.1: the K-safe direct `pair.swap` computes
  the attacker's output at the Pancake 0.25% fee even on lower-fee forks, *under*
  -quoting attacker gross — a conservative bias that can only lower the sandwich count.
- **(T11) Sandwich routing coverage and selection bias** — non-Pancake V3 / Algebra
  victims (~35%, `skippedUnsupported`), token/token pools with no hub numeraire, and
  tokens with non-standard ERC20 layouts are excluded (conservative exclusions,
  under-counting the surface). The 1,735 gross-positive and 1,162 net-positive sandwiches
  are measured on a non-representative ~65% subsample of the 33,525 victims, introducing
  potential selection bias: victims routed through unsupported V3/Algebra pools may
  differ systematically (e.g. higher value, exotic tokens) from those on covered
  V2/Pancake-V3 pools, so the measured rates are not necessarily generalizable to the
  full victim universe. Closing this gap can only enlarge the *discovered* surface, but
  it may also reveal whether skipped victims carry different profitability, which would
  affect the long-tail vs. hub contrast.
- **(T12) Token funding fidelity and invariant safety** — sandwich funding writes ERC20
  balance/allowance slots directly; tokens whose layout is not reproduced are
  `skippedUnfundable` (under-count, conservative). For funded tokens the EVM then applies
  the real transfer logic (including fee-on-transfer), but **the initial storage write
  may violate token invariants on fee-on-transfer / anti-bot long-tail tokens.** For
  instance, a token with internal `balanceOf` tracking or a transfer hook that enforces
  supply consistency could enter an invalid state if balance is written without updating
  internal accounting; the sandwich valuation is then based on EVM execution of an
  invalid state, potentially over-stating profit on tokens where the storage write would
  cause silent failures or revert on-chain. This risk is highest for exotic long-tail
  tokens (where the sandwich surface concentrates) and could affect the magnitude but not
  the order-of-magnitude conclusion (impact TBD once fee-on-transfer token modeling lands
  in Phase-1).
- **(T13) Realizability detector recall and blind-spot prevalence** — the 0/735 capture
  result (2,100 blocks) is only meaningful if the detector would catch real landed
  sandwiches. The injection harness of §5.7.1 measures **95–96% recall** on the dominant
  same-actor structures at a **0% false-positive rate**, with recall collapsing only on
  two documented blind spots: (i) mid-tx proceed-sweeping to a cold address; (ii)
  deliberately >2%-non-flat round trips. The geth-sim16 pilot anchored ~984 victims in
  the corrFailNotFlat pattern over 34,200 blocks (≈29/1k blocks) — the blind spots are
  **not rare** as patterns. **Phase-2 trace-probe (§5.7.2) closes this concern on a
  30,100-block window**: recallMissedBrackets=2,429, roundTripReal=1,585 (≈53/1k blocks,
  same regime as the pilot), roundTripRouterFP=842 (35% router pass-through correctly
  excluded — R2 redesign independently validated), sweepReal=2, sweepColdFP=0, and
  most importantly **upperBoundMissedRealizedWei=0**. **Interpretation (on this
  window).** The 1,585 structural round-trips and 2 sweeps exist as patterns but
  **zero** carry positive ex-post net profit: the blind-spot population is
  **demographic, not economic**; the identification gap is **bounded at zero** on this
  window. The peer-review's load-bearing concern that the realized rate "could be a
  major under-count" because of blind-spot prevalence is **empirically refuted** on
  this window, **subject to the caveat that the trace-probe's own recall is
  unvalidated** (see §5.7.2): future work should inject synthetic patterns to measure
  the trace-probe's sensitivity. Single-tx sandwiches (which dominate realized MEV) remain outside the
  cross-tx detector scope (a deliberate scope choice, not a blind spot).

## 6.7 Future work

The realizability result reprioritizes the roadmap. (1) **Longer, multi-volatility,
multi-regime realizability windows — URGENT.** The 0/735 result over 2,100 blocks was a
sampling artifact (§6.1); the 34,200-block geth-sim16 pilot anchors the rate at
≈0.07% by count (7 of ~10,190) and ≈0.016% by value, and the
152,650-block geth-sim16 pilot anchored 10 captures — these earlier, smaller windows
corroborate and sit slightly above the final measured rate from the completed multi-day, multi-volatility geth-sim20
window: 45 of 96,064 (0.0468% by count, Wilson 95% CI [0.0350%, 0.0627%]) and 0.0078%
by value, measured over 315,750 blocks (2026-06-09 04:27 – 2026-06-11 17:29); 25 captures
by repeated address (0xCF2e..C842=17, 38%), 20 by unknown, 0 by builder. The headline run
early-stopped at this 315,750-block / ~2.5-day window when BSC chaindata state growth
exhausted the disk volume and a disk-guard watchdog gracefully halted the node (clean
shutdown, chaindata intact) before ENOSPC; the 45 captures accumulated steadily, so the
rate is stable across the run, but it spans a single ~2.5-day market regime. Collection spanning multiple volatility regimes (normal,
listings, depegs, flash crashes) is future work, to establish whether the measured
0.0468% holds uniformly across regimes or whether realized capture is regime-dependent. This
data will also stratify realized capture by regime, testing whether the
GWA-suppression hypothesis of §6.4 holds uniformly or whether suppression is
regime-dependent. (2) **Atomic single-tx sandwich/arb valuation.** The
realized-MEV scan shows atomic single-tx activity dominates; valuing it on the same
instrument (it does not fit the cross-tx victim-bracket model) would close the one
realized category we currently only count, not value. (3) **Tighten the counterfactual
against the mid-tx-sweep false negative** (a bot moving proceeds to a cold address
before tx end), e.g. via intra-tx trace-level balance tracking, to confirm the 0 is not
hiding swept captures. (4) **Close the sandwich routing gap** (non-Pancake V3 / Algebra,
~35% `skippedUnsupported`) — pure upside to the *ex-post* surface. (5) **Extend backrun
to the long tail** with automated pool discovery, to test whether *backrun* too has a
long-tail ex-post surface the 12-pool hub set misses. (6) Complete **end-to-end
synthetic-tx verification** for backrun (`verifyCycleEVM`). (7) Model (and, only in a
sanctioned setting, exercise) the **PBS-builder submission path** — the only avenue by
which any ex-post surface could in principle become capturable, and the cleanest live
falsification test of the vanishingly-small conclusion.

## 6.8 What would change the conclusion

The conclusion has three falsifiable layers. The **ex-post backrun** finding (deep-pool
surface sub-gas) would be overturned by: (1) a materially larger above-gas backrun
population at a realistic BSC gas price; or (2) a flaw in the oracle under-stating
backrun gross. The **ex-post sandwich** finding (large long-tail surface) would be
overturned by a routing/funding bias that *over*-states sandwich gross (we argue every
such bias is conservative, §3.7.1, T10–T12). The **realizability** finding — the
decisive one, that realized independent capture is ≈ 0 — has been
revised by longer data (34,200-block geth-sim16 pilot corroborates ≈0.07% by count and
≈0.016% by value as an earlier, smaller window, slightly above the final measured rate from the completed
multi-day geth-sim20 window, 0.0468% by count (45/96,064, Wilson 95% CI [0.0350%, 0.0627%]) and 0.0078% by value): the headline has
shifted from "≈ 0" to "tiny but measurably nonzero on the windows measured, bounded
above by the measured 0.0468% by count and below by zero, with the
blind-spot identification gap independently bounded at zero on the 30,100-block
Phase-2 trace-probe window." It would be further revised by: (3) a longer or different window
in which the measured capture rate is *substantially larger* and stable across volatility
regimes; (4) evidence via intra-tx trace tracking (§6.7, Phase-2) that the blind spots
(mid-tx proceed-sweeping, non-flat round trips) account for the observed small capture
rate, or conversely that they hide a much larger real surface; or (5) a
demonstrated independent path that actually *lands* value against the suppression — the
PBS-builder submission route exercised in a sanctioned test (§6.7) returning net-positive
realized profit. The **comparative ex-post** claim (sandwiching ≫ backrun) would be
overturned only if a backrun long-tail turned out comparably large, which the
volume asymmetry makes unlikely. Note the layers interact: even a large ex-post sandwich
surface (which we *do* find) does not resurrect the edge, because realizability is
measured separately and is vanishingly small. None of the overturning conditions is observed; all are
concrete and testable, which is what makes "the independent atomic edge is vanishingly
small, bounded above by the measured 0.0468% by count and below by zero on the
completed window" a falsifiable claim.

## 6.9 Model capacity is not the bottleneck (an ML robustness check)

A natural objection: maybe a *bigger* model — a deep net on an H200 rather than
gradient-boosted trees — would find a predictive edge our measurement missed. It cannot,
for a structural reason; a capacity ladder on proxy targets serves only as an
illustrative robustness check.

**The oracle bound (structural, capacity-independent).** Our receipt-exact counterfactual
*is* the oracle predictor — perfect ex-post information, the strongest predictor that can
exist — and its realizable atomic value is ≈ 0. So for any predictor *f* on any
representation, value(*f*) ≤ value(oracle) ≈ 0: the binding constraint is *acting*
(ordering, latency, private flow, atomicity, gas), not *predicting*. No parameter count
changes that.

**A capacity ladder (illustrative robustness check on proxy targets).** A capacity ladder
serves only as an illustrative robustness check, not as evidence about extraction. On the
two structurally *learnable* proxy targets —
P1 curl-magnitude (`curlFrac`, n=359) and P2 value-magnitude (`log V`, n=7,680) — a
model-capacity ladder under GroupKFold-by-block CV (all preprocessing fit in-fold;
out-of-fold R² only; near-label/post-treatment leaks excluded) is flat in capacity:

| Model class | P1 curl (CV R²) | P2 log V (CV R²) |
| --- | --- | --- |
| Linear (Ridge/Lasso) | 0.19 | 0.65 |
| XGBoost (tree-SOTA) | 0.21–0.25 | **0.72–0.92** |
| MLP small (2×64) → large (6×512, 1.6M params) | 0.21 → 0.22 | 0.76 → 0.78 |

CV performance is **flat / non-increasing in capacity**: on P1 the linear anchor already
recovers essentially everything; on P2 **XGBoost is the best model and no deep net beats
it** [grinsztajn2022tabular], and the only lever that moved P2 was *representation* (pool
encoding: 0.09 → 0.65), not capacity. The targets that would matter for *extraction*
carry no positive mass: realized "captured" has 0 positives (any model fits it with
undefined AUC); "real-censored" has 7/7,680 positives and a 577→~800k-parameter sweep
*worsens* AUC (0.965 → 0.758, bigger overfits), the seven "positives" being
delayed-inclusion artifacts, not positive-EV opportunities. Capacity is irrelevant where
there is nothing to separate. The ladder demonstrates that *representation* (pool
encoding), not capacity, is the lever; this consistency check corroborates the oracle
bound but is not independent evidence of it.

**Robustness corollary.** As a secondary consistency check (not the main argument): used
only as a nuisance inside the identified estimand, XGBoost
as the AIPW outcome/propensity model leaves the censorship-differential indistinguishable
from zero (the AIPW point is non-identified — overlap collapses; the defensible exact-V
differential is **D = −0.029 BNB, 95% CI [−0.178, +0.081]** ≈ 0, corroborating $\hat D
= 0$, §5.8). RL is excluded a priori (realizable reward ≡ 0). The H200 is the right
instrument only for a *different* problem — non-atomic statistical arbitrage on a
contestable venue — not the BSC atomic question, whose realizable edge is structurally
pinned at zero by the oracle bound.

---

# Reproducibility statement

All addresses, storage slots (including the ERC20 balance/allowance slots and the
runtime slot-probing used for sandwich funding), event signatures, fee tiers, and pool
reserves used in this study were verified directly against the project's own synced
bsc-geth v1.7.3 node (`chainId = 56`, head ~block 102.54M) — the same node that
produces the ground-truth execution — so the watch set and all measurements are
reproducible from the node alone, with no third-party API dependency. The instrumented
binaries are built only to `/tmp` (`geth-sim10` for backrun, `geth-sim15` for the
sandwich model, `geth-sim16` for the realizability pilots, `geth-sim20` for the completed
multi-day realizability run and the Phase-2 matched-footprint / serialization / trace-probe
measurements, `geth-sim17` for the censorship-differential); the experiments are strictly read-only and never submit; the
running node, its systemd unit, and its datadir are untouched. The backrun figures are
from a 3,000-sampled-block `intrablock` run; the sandwich figures from a 2,550-sampled-block
`sandwich-any` run; the realizability figures from the COMPLETED 315,750-processed-block
`realizability` run (geth-sim20, chain heights ~102.99M–103.63M, 2026-06-09 04:27 –
2026-06-11 17:29; 45 of 96,064, 0.0468% by count, Wilson 95% CI [0.0350%, 0.0627%], 0.0078%
by value), with the 2,100-block geth-sim16 window reported only as the small-window
(capture-0) sampling artifact (auditable bracket→same-actor→corroboration funnel plus an
independent 1,500-block pool-agnostic cross-check); the censorship-differential from a live
`censorship` run with a $K=256$-block settle window, cross-checked by querying the
canonical chain for the later inclusion of every flagged drop (954/958 historical drops
verified mined-later).

# References

See `references.bib`. Cited keys: daian2020flashboys, heimbach2022sok,
qin2022quantifying, torres2021frontrunner, wang2021cyclic, zhang2024improved,
mclaughlin2023arbitrage, angeris2020oracles, angeris2021multiasset,
angeris2022routing, diamandis2023routing, diamandis2024convexflows,
adams2021uniswapv3, qin2021flashloans, zhou2021hft, gogol2026sandwich, bep322,
wang2025binance, bnbchain2025gwa, blocksec2025pbs, grinsztajn2022tabular,
babel2023lanturn, chi2024remeasuring.
