#!/usr/bin/env python3
"""
Attention-based tabular DL rung for the MEV-capacity ladder.

Fits a compact FT-Transformer (feature-tokenizer + transformer encoder, CLS
readout) on the two LEARNABLE targets under the SAME cross-validation protocol
used for the XGBoost baselines, with a small and a larger capacity variant.

  T1 curl-magnitude : target = curlFrac on the NON-COMMUTING subset
                      (omegaNorm2 > 0). Features = PRE-STATE only. GroupKFold
                      by block. Hodge outputs (gradFrac/rho/scalarCurlFrac/
                      harmonicFrac) EXCLUDED (they are functions of the label).
  T2 value-magnitude: target = log(V_BNBwei) on censorship_candidates.
                      grossBNBwei (= V + gas, near-label leak) and leadTimeSec
                      (post-treatment) EXCLUDED. GroupKFold by block.

rtdl is uninstallable in this env (it pins an ancient torch), so we use a
compact custom FT-Transformer that is architecturally faithful to Gorishniy
et al. 2021 (feature tokenizer + [CLS] + pre-norm transformer). CPU only.

We report ONLY cross-validated R2 (out-of-fold), never train metrics, and
compare against an XGBoost baseline trained under the identical folds.
"""
import os
# Cap thread fan-out BEFORE importing numpy/torch to avoid oversubscription
# on a heavily-loaded shared box (this run was spawning ~190 threads).
os.environ.setdefault("OMP_NUM_THREADS", "8")
os.environ.setdefault("MKL_NUM_THREADS", "8")
os.environ.setdefault("OPENBLAS_NUM_THREADS", "8")
import warnings
warnings.filterwarnings("ignore")
import sys
import numpy as np
import pandas as pd
import torch
import torch.nn as nn
from sklearn.model_selection import GroupKFold
from sklearn.preprocessing import StandardScaler
from sklearn.metrics import r2_score
import xgboost as xgb

SEED = 0
np.random.seed(SEED)
torch.manual_seed(SEED)
torch.set_num_threads(8)
torch.set_num_interop_threads(2)
DEV = torch.device("cpu")


# ----------------------------------------------------------------------------
# Compact FT-Transformer (faithful to Gorishniy et al. 2021)
# ----------------------------------------------------------------------------
class FeatureTokenizer(nn.Module):
    """One learned linear token per numeric feature + a [CLS] token."""
    def __init__(self, n_num, d_token):
        super().__init__()
        self.weight = nn.Parameter(torch.empty(n_num, d_token))
        self.bias = nn.Parameter(torch.empty(n_num, d_token))
        self.cls = nn.Parameter(torch.empty(1, 1, d_token))
        for p in (self.weight, self.bias, self.cls):
            nn.init.normal_(p, std=1.0 / (d_token ** 0.5))

    def forward(self, x_num):
        # x_num: (B, n_num) -> tokens (B, n_num, d)
        tokens = x_num[:, :, None] * self.weight[None] + self.bias[None]
        cls = self.cls.expand(x_num.shape[0], -1, -1)
        return torch.cat([cls, tokens], dim=1)


class FTTransformer(nn.Module):
    def __init__(self, n_num, d_token=32, depth=2, heads=4, ff_mult=2,
                 dropout=0.1):
        super().__init__()
        self.tok = FeatureTokenizer(n_num, d_token)
        layer = nn.TransformerEncoderLayer(
            d_model=d_token, nhead=heads, dim_feedforward=d_token * ff_mult,
            dropout=dropout, activation="gelu", batch_first=True,
            norm_first=True)
        self.enc = nn.TransformerEncoder(layer, num_layers=depth)
        self.head = nn.Sequential(
            nn.LayerNorm(d_token), nn.ReLU(), nn.Linear(d_token, 1))

    def forward(self, x_num):
        h = self.tok(x_num)
        h = self.enc(h)
        return self.head(h[:, 0]).squeeze(-1)  # CLS readout


def train_ft(Xtr, ytr, Xva, cfg, epochs=150, lr=1e-3, wd=1e-5, bs=512,
             patience=20):
    """Train one FT-Transformer; early-stop on an internal val split carved
    from the TRAIN fold only (the outer va fold stays untouched for scoring)."""
    n = len(Xtr)
    idx = np.random.RandomState(SEED).permutation(n)
    cut = max(1, int(0.85 * n))
    tr_i, es_i = idx[:cut], idx[cut:]
    Xt = torch.tensor(Xtr[tr_i], dtype=torch.float32)
    yt = torch.tensor(ytr[tr_i], dtype=torch.float32)
    Xe = torch.tensor(Xtr[es_i], dtype=torch.float32)
    ye = ytr[es_i]
    model = FTTransformer(n_num=Xtr.shape[1], **cfg).to(DEV)
    opt = torch.optim.AdamW(model.parameters(), lr=lr, weight_decay=wd)
    loss_fn = nn.MSELoss()
    best_es, best_state, bad = np.inf, None, 0
    for ep in range(epochs):
        model.train()
        perm = torch.randperm(len(Xt))
        for j in range(0, len(Xt), bs):
            b = perm[j:j + bs]
            opt.zero_grad()
            out = model(Xt[b])
            loss = loss_fn(out, yt[b])
            loss.backward()
            opt.step()
        model.eval()
        with torch.no_grad():
            pe = model(Xe).numpy()
        es = float(np.mean((pe - ye) ** 2)) if len(ye) else float(loss.item())
        if es < best_es - 1e-6:
            best_es, best_state, bad = es, {k: v.clone() for k, v in
                                            model.state_dict().items()}, 0
        else:
            bad += 1
            if bad >= patience:
                break
    if best_state is not None:
        model.load_state_dict(best_state)
    model.eval()
    with torch.no_grad():
        pred = model(torch.tensor(Xva, dtype=torch.float32)).numpy()
    return pred


def freq_encode(train_vals, all_vals):
    """Frequency encoding fit on TRAIN ONLY (no leakage)."""
    freq = pd.Series(train_vals).value_counts(normalize=True)
    return np.array([freq.get(v, 0.0) for v in all_vals], dtype=float)


def cv_eval(X_raw_builder, y, groups, n_splits, configs, label):
    """Run GroupKFold; X_raw_builder(train_mask) returns a fully-built,
    leakage-controlled feature matrix (freq-encoding + scaling fit on train).
    Returns dict of model -> OOF R2."""
    gkf = GroupKFold(n_splits=n_splits)
    n = len(y)
    oof = {("FT-small"): np.zeros(n), ("FT-large"): np.zeros(n),
           ("XGB"): np.zeros(n)}
    for fold, (tr, va) in enumerate(gkf.split(np.zeros(n), y, groups)):
        Xtr, Xva = X_raw_builder(tr, va)
        ytr = y[tr]
        sc = StandardScaler().fit(Xtr)
        Xtr_s, Xva_s = sc.transform(Xtr), sc.transform(Xva)
        # FT variants
        oof["FT-small"][va] = train_ft(Xtr_s, ytr, Xva_s, configs["small"])
        oof["FT-large"][va] = train_ft(Xtr_s, ytr, Xva_s, configs["large"])
        print(f"   [{label}] fold {fold+1}/{n_splits} done "
              f"(ntr={len(tr)} nva={len(va)})", flush=True)
        # XGB baseline on same fold (raw features; trees are scale-invariant)
        m = xgb.XGBRegressor(
            n_estimators=400, max_depth=4, learning_rate=0.05,
            subsample=0.8, colsample_bytree=0.8, reg_lambda=1.0,
            n_jobs=16, random_state=SEED)
        m.fit(Xtr, ytr)
        oof["XGB"][va] = m.predict(Xva)
    res = {k: r2_score(y, v) for k, v in oof.items()}
    print(f"\n[{label}] n={n}  GroupKFold k={n_splits}")
    for k in ["FT-small", "FT-large", "XGB"]:
        print(f"   {k:9s}  CV R2 = {res[k]:+.4f}")
    return res


# ----------------------------------------------------------------------------
# T1: curl-magnitude
# ----------------------------------------------------------------------------
def run_T1():
    df = pd.read_csv("./curl_clusters.csv")
    # non-commuting subset: omegaNorm2 may be a huge int stored as string
    om = pd.to_numeric(df["omegaNorm2"], errors="coerce").fillna(0.0)
    df = df[om > 0].reset_index(drop=True)
    om = pd.to_numeric(df["omegaNorm2"], errors="coerce").fillna(0.0)
    y = df["curlFrac"].astype(float).values
    groups = df["block"].values
    # PRE-STATE features only: k, actors, pool freq-enc, log flow-magnitude.
    # EXCLUDE gradFrac, rho, scalarCurlFrac, harmonicFrac, curlFrac, perms.
    k = df["k"].astype(float).values
    actors = df["actors"].astype(float).fillna(0.0).values \
        if df["actors"].isna().any() else df["actors"].astype(float).values
    actors = np.nan_to_num(actors)
    log_flow = np.log1p(om.values.astype(float))  # log flow-magnitude
    pools = df["pool"].astype(str).values

    def builder(tr, va):
        pf = freq_encode(pools[tr], pools)
        feats = np.column_stack([k, actors, log_flow, pf])
        return feats[tr], feats[va]

    configs = {
        "small": dict(d_token=16, depth=1, heads=2, ff_mult=2, dropout=0.1),
        "large": dict(d_token=64, depth=4, heads=8, ff_mult=2, dropout=0.1),
    }
    n_splits = min(5, df["block"].nunique())
    return cv_eval(builder, y, groups, n_splits, configs, "T1 curlFrac"), \
        dict(n=len(y), feats=4)


# ----------------------------------------------------------------------------
# T2: value-magnitude
# ----------------------------------------------------------------------------
def run_T2():
    df = pd.read_csv("./censorship_candidates.csv")
    y = np.log(df["V_BNBwei"].astype(float).values)
    groups = df["block"].values
    # EXCLUDE: grossBNBwei (V+gas leak), leadTimeSec (post-treatment),
    # V_USD (deterministic fn of V_BNBwei), tx, dropped (degenerate label).
    num_cols = ["token0Side", "poolReserveLog10", "hops", "gas",
                "gasFeeCapWei", "gasTipCapWei", "blockFullness", "ledgerSize"]
    num = df[num_cols].astype(float).values
    # log-scale the wide-range fee/gas columns
    for j, c in enumerate(num_cols):
        if c in ("gas", "gasFeeCapWei", "gasTipCapWei", "ledgerSize"):
            num[:, j] = np.log1p(num[:, j])
    cat_cols = ["pool", "dex", "numeraire", "validator"]
    cats = {c: df[c].astype(str).values for c in cat_cols}

    def builder(tr, va):
        enc = [freq_encode(cats[c][tr], cats[c]) for c in cat_cols]
        feats = np.column_stack([num] + enc)
        feats = np.nan_to_num(feats)
        return feats[tr], feats[va]

    configs = {
        "small": dict(d_token=16, depth=1, heads=2, ff_mult=2, dropout=0.1),
        "large": dict(d_token=64, depth=4, heads=8, ff_mult=2, dropout=0.1),
    }
    n_splits = 5
    return cv_eval(builder, y, groups, n_splits, configs, "T2 log(V_BNBwei)"), \
        dict(n=len(y), feats=len(num_cols) + len(cat_cols))


if __name__ == "__main__":
    print("=" * 64)
    print("ATTENTION-BASED TABULAR DL RUNG  (FT-Transformer, CPU)")
    print("=" * 64)
    t1, t1meta = run_T1()
    t2, t2meta = run_T2()
    print("\n" + "=" * 64)
    print("SUMMARY (cross-validated R2)")
    print("=" * 64)
    print(f"T1 curlFrac      (n={t1meta['n']}): "
          f"FT-small {t1['FT-small']:+.3f} | FT-large {t1['FT-large']:+.3f} "
          f"| XGB {t1['XGB']:+.3f}")
    print(f"T2 log(V_BNBwei) (n={t2meta['n']}): "
          f"FT-small {t2['FT-small']:+.3f} | FT-large {t2['FT-large']:+.3f} "
          f"| XGB {t2['XGB']:+.3f}")
    import json
    print("JSON_RESULT=" + json.dumps({
        "T1": {**t1, "n": t1meta["n"], "feats": t1meta["feats"]},
        "T2": {**t2, "n": t2meta["n"], "feats": t2meta["feats"]},
    }))
