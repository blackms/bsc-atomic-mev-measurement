#!/usr/bin/env python3
"""
TREE SOTA rung for the MEV-capacity ablation paper.

Fits XGBoost with a modest hyperparameter search under leakage-controlled
GroupKFold (by block) on two learnable targets:

  T1 curl-magnitude : target = curlFrac on NON-COMMUTING subset (omegaNorm2>0)
     features = PRE-STATE only: k, actors, pool frequency-encoding,
                log flow-magnitude (log omegaNorm2).
     EXCLUDES Hodge outputs (gradFrac, rho, scalarCurlFrac, harmonicFrac).

  T2 value-magnitude: target = log(V_BNBwei) on censorship_candidates.
     EXCLUDES grossBNBwei (V+gas, near-label leak) and leadTimeSec (post-treatment).

Reports CROSS-VALIDATED R2 (mean +/- std across folds), never train metrics.
Pool frequency-encoding is fit on TRAIN folds only (no leakage).
CPU only; data is tiny by design.
"""
import sys
import numpy as np
import pandas as pd
from itertools import product
from sklearn.model_selection import GroupKFold
from sklearn.metrics import r2_score
import xgboost as xgb

RNG = 42
np.random.seed(RNG)

# Tiny data: single-thread per fit is far faster than spinning up 64 cores
# for each of hundreds of fits, and is fully deterministic.
N_JOBS = 1

CURL = "./curl_clusters.csv"
CENS = "./censorship_candidates.csv"

# ------------------------------------------------------------------ helpers
def freq_encode(train_keys, eval_keys):
    """Frequency (count) encoding learned on TRAIN only; unseen -> 0."""
    counts = train_keys.value_counts()
    return eval_keys.map(counts).fillna(0.0).astype(float).values

def cv_r2_for_params(build_Xy, groups, y, params, n_splits):
    """GroupKFold CV. build_Xy(train_idx, eval_idx) returns (Xtr, Xev) with
    any in-fold (train-only) feature fitting done internally to prevent leakage."""
    gkf = GroupKFold(n_splits=n_splits)
    fold_r2 = []
    for tr, ev in gkf.split(np.zeros(len(y)), y, groups):
        Xtr, Xev = build_Xy(tr, ev)
        model = xgb.XGBRegressor(
            objective="reg:squarederror",
            random_state=RNG,
            n_jobs=N_JOBS,
            tree_method="hist",
            **params,
        )
        model.fit(Xtr, y[tr])
        pred = model.predict(Xev)
        fold_r2.append(r2_score(y[ev], pred))
    return np.array(fold_r2)

def search(build_Xy, groups, y, grid, n_splits, label):
    best = None
    keys = list(grid.keys())
    combos = list(product(*[grid[k] for k in keys]))
    print(f"\n=== {label}: searching {len(combos)} configs, "
          f"GroupKFold({n_splits}) over {len(np.unique(groups))} groups, n={len(y)} ===",
          flush=True)
    for i, combo in enumerate(combos):
        params = dict(zip(keys, combo))
        r2s = cv_r2_for_params(build_Xy, groups, y, params, n_splits)
        m, s = r2s.mean(), r2s.std()
        if best is None or m > best["mean"]:
            best = {"params": params, "mean": m, "std": s, "folds": r2s.tolist()}
        if (i + 1) % 16 == 0 or (i + 1) == len(combos):
            print(f"  [{i+1}/{len(combos)}] best-so-far R2={best['mean']:.4f}",
                  flush=True)
    print(f"BEST {label}: CV R2 = {best['mean']:.4f} +/- {best['std']:.4f}")
    print(f"  params: {best['params']}")
    print(f"  per-fold R2: {[round(x,4) for x in best['folds']]}")
    return best

# ------------------------------------------------------------------ T1
def run_T1():
    d = pd.read_csv(CURL)
    # omegaNorm2 is a huge-int stored as object/string; parse robustly to float
    d["omegaNorm2"] = pd.to_numeric(d["omegaNorm2"], errors="coerce")
    nc = d[d["omegaNorm2"] > 0].copy().reset_index(drop=True)
    nc = nc[nc["actors"].notna()].reset_index(drop=True)  # complete on NC anyway
    y = nc["curlFrac"].astype(float).values
    groups = nc["block"].values

    # PRE-STATE numeric features (NO Hodge outputs / no function of label)
    log_flow = np.log(nc["omegaNorm2"].values)  # log flow-magnitude
    base = pd.DataFrame({
        "k": nc["k"].astype(float).values,
        "actors": nc["actors"].astype(float).values,
        "log_flow": log_flow,
    })
    pools = nc["pool"]

    def build_Xy(tr, ev):
        Xtr = base.iloc[tr].copy()
        Xev = base.iloc[ev].copy()
        # pool frequency-encoding fit on TRAIN fold only
        Xtr["pool_freq"] = freq_encode(pools.iloc[tr], pools.iloc[tr])
        Xev["pool_freq"] = freq_encode(pools.iloc[tr], pools.iloc[ev])
        return Xtr.values, Xev.values

    grid = {
        "max_depth": [2, 3, 4, 6],
        "n_estimators": [100, 300, 600],
        "learning_rate": [0.03, 0.1],
        "subsample": [0.7, 1.0],
        "colsample_bytree": [0.8, 1.0],
    }
    n_splits = 5
    best = search(build_Xy, groups, y, grid, n_splits, "T1 curlFrac (NC)")
    best["n"] = int(len(y))
    best["features"] = list(base.columns) + ["pool_freq"]
    return best

# ------------------------------------------------------------------ T2
def run_T2():
    c = pd.read_csv(CENS)
    y = np.log(c["V_BNBwei"].astype(float).values)  # log V
    groups = c["block"].values

    # EXCLUDE grossBNBwei (near-label leak), leadTimeSec (post-treatment),
    # V_BNBwei/V_USD (the label itself), tx/block (ids).
    drop = {"V_BNBwei", "V_USD", "grossBNBwei", "leadTimeSec",
            "tx", "block", "dropped"}
    cat_cols = ["dex", "numeraire", "validator"]  # pool handled via freq-encode
    num_cols = ["token0Side", "poolReserveLog10", "hops", "gas",
                "gasFeeCapWei", "gasTipCapWei", "blockFullness", "ledgerSize"]
    num_cols = [c0 for c0 in num_cols if c0 not in drop]

    base_num = c[num_cols].astype(float).reset_index(drop=True)
    # log-transform the fee/gas magnitudes (heavy tailed, pre-state)
    for col in ["gas", "gasFeeCapWei", "gasTipCapWei"]:
        base_num[col] = np.log1p(base_num[col])
    pools = c["pool"].reset_index(drop=True)

    def build_Xy(tr, ev):
        Xtr = base_num.iloc[tr].copy()
        Xev = base_num.iloc[ev].copy()
        # categorical: train-only frequency encoding (leakage-safe, simple)
        for col in cat_cols:
            ser = c[col].reset_index(drop=True)
            Xtr[col + "_freq"] = freq_encode(ser.iloc[tr], ser.iloc[tr])
            Xev[col + "_freq"] = freq_encode(ser.iloc[tr], ser.iloc[ev])
        Xtr["pool_freq"] = freq_encode(pools.iloc[tr], pools.iloc[tr])
        Xev["pool_freq"] = freq_encode(pools.iloc[tr], pools.iloc[ev])
        return Xtr.values, Xev.values

    grid = {
        "max_depth": [3, 4, 6, 8],
        "n_estimators": [200, 400, 800],
        "learning_rate": [0.03, 0.1],
        "subsample": [0.7, 1.0],
        "colsample_bytree": [0.8, 1.0],
    }
    n_splits = 5
    best = search(build_Xy, groups, y, grid, n_splits, "T2 log(V_BNBwei)")
    best["n"] = int(len(y))
    best["features"] = num_cols + [cc + "_freq" for cc in cat_cols] + ["pool_freq"]
    return best

if __name__ == "__main__":
    t1 = run_T1()
    t2 = run_T2()
    print("\n================ SUMMARY (CV R2, GroupKFold by block) ================")
    print(f"T1 curlFrac (NC, n={t1['n']}): R2 = {t1['mean']:.4f} +/- {t1['std']:.4f}  "
          f"[baseline ~0.21]")
    print(f"T2 log(V)   (n={t2['n']}):     R2 = {t2['mean']:.4f} +/- {t2['std']:.4f}  "
          f"[baseline ~0.72]")
