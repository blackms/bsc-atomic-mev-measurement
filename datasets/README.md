# Phase-2 measurement snapshots

This directory contains the raw `tally` / `dist` log lines produced by the SimEngine detectors during the Phase-2 measurement windows for each of the major-revision blockers (B1/B2/B3/M1/M2).

| File | Window | Closes | Headline number |
|---|---|---|---|
| `backrun-any-pre-sanitycap-2026-06-08.log.gz` | ~10,400 blk (before integrity catch #5) | B3 — pre-fix | 16 OPP (1 outlier $3.6·10¹⁵, decimal-mismatch) |
| `backrun-any-postfix-final-2026-06-08.log.gz` | 16,100 blk (post sanity-cap) | **B3** matched-footprint | 55 OPP, 0.484 BNB ≈ $291 |
| `blindspot-final-2026-06-08.log.gz` | 30,100 blk | **B1** identification gap | `upperBoundMissedRealizedWei = 0` |
| `serialize-final-2026-06-09.log.gz` | 23,300 blk | **M1/M2** over-count | `η = 0.17%` (upper-vs-lower band) |

## Format

Each file is plain-text (gzipped) and contains the periodic `tally` and `dist` lines from a live node run, plus the per-opportunity `OPP @tx` lines and any `REJECT sanity cap` forensic warnings. The lines are emitted in JSON-like key=value format compatible with `grep` / `awk` / standard log parsers.

```bash
# example: extract the final tally
zcat backrun-any-postfix-final-2026-06-08.log.gz | grep 'backrun-any tally' | tail -1
```

## Reproducibility

Each window was measured on `geth-sim20` (the merged build with all Phase-1 instruments) running against a synced BSC mainnet node. The exact block-height ranges are documented in the paper §5.1–§6.1.

These files are the **frozen reference numbers** behind the paper's Phase-2 closures; the still-running 14-day headline window adds the final capture-rate stable estimate (16 `TODO(revision)` placeholders in the paper).

## What's NOT here

- The full real-time node log (typically `<datadir>/bsc.log.*` on the running node) — too large and contains operational details unrelated to the measurement.
- The pilot realizability window (geth-sim16, 152,650 blocks, 10 captures) — preserved on the original node but not part of the canonical paper run; the paper uses the geth-sim20 long-window as headline.
- Receipt-valid mode produces 0 mismatches by construction — no per-block records preserved separately (the tally is the result).
