#!/usr/bin/env python3
"""
T2-only FT-Transformer rung (T1 already completed in ft_transformer_ladder.py:
  FT-small +0.151, FT-large +0.206, XGB +0.140 on n=359 curlFrac).

Same compact FT-Transformer + same GroupKFold-by-block CV. Epochs reduced to
80 (patience 12) purely for wall-clock under a heavily shared box; FT models on
tiny tabular converge in <80 epochs and early-stopping governs each fold, so
the small-vs-large capacity comparison and the XGB anchor remain valid.

  T2 value-magnitude: target = log(V_BNBwei) on censorship_candidates.
  EXCLUDE grossBNBwei (V+gas leak), leadTimeSec (post-treatment),
  V_USD (deterministic fn of label). GroupKFold by block. k=5.
"""
import os
os.environ.setdefault("OMP_NUM_THREADS", "6")
os.environ.setdefault("MKL_NUM_THREADS", "6")
os.environ.setdefault("OPENBLAS_NUM_THREADS", "6")
import warnings
warnings.filterwarnings("ignore")
import json
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
torch.set_num_threads(6)
torch.set_num_interop_threads(2)


class FeatureTokenizer(nn.Module):
    def __init__(self, n_num, d_token):
        super().__init__()
        self.weight = nn.Parameter(torch.empty(n_num, d_token))
        self.bias = nn.Parameter(torch.empty(n_num, d_token))
        self.cls = nn.Parameter(torch.empty(1, 1, d_token))
        for p in (self.weight, self.bias, self.cls):
            nn.init.normal_(p, std=1.0 / (d_token ** 0.5))

    def forward(self, x):
        t = x[:, :, None] * self.weight[None] + self.bias[None]
        return torch.cat([self.cls.expand(x.shape[0], -1, -1), t], dim=1)


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

    def forward(self, x):
        return self.head(self.enc(self.tok(x))[:, 0]).squeeze(-1)


def train_ft(Xtr, ytr, Xva, cfg, epochs=60, lr=2e-3, wd=1e-5, bs=2048,
             patience=10):
    # Standardize the target on the TRAIN fold only (leakage-free). The FT
    # head is zero-initialized; an un-normalized target centered far from 0
    # (here log(V)~38) otherwise never converges. Predictions are mapped back
    # to the original scale before scoring. (Trees / XGB don't need this.)
    y_mu, y_sd = float(np.mean(ytr)), float(np.std(ytr) + 1e-8)
    ytr = (ytr - y_mu) / y_sd
    n = len(Xtr)
    idx = np.random.RandomState(SEED).permutation(n)
    cut = max(1, int(0.85 * n))
    tr_i, es_i = idx[:cut], idx[cut:]
    Xt = torch.tensor(Xtr[tr_i], dtype=torch.float32)
    yt = torch.tensor(ytr[tr_i], dtype=torch.float32)
    Xe = torch.tensor(Xtr[es_i], dtype=torch.float32)
    ye = ytr[es_i]
    model = FTTransformer(n_num=Xtr.shape[1], **cfg)
    opt = torch.optim.AdamW(model.parameters(), lr=lr, weight_decay=wd)
    loss_fn = nn.MSELoss()
    best, best_state, bad = np.inf, None, 0
    for ep in range(epochs):
        model.train()
        perm = torch.randperm(len(Xt))
        for j in range(0, len(Xt), bs):
            b = perm[j:j + bs]
            opt.zero_grad()
            loss = loss_fn(model(Xt[b]), yt[b])
            loss.backward()
            opt.step()
        model.eval()
        with torch.no_grad():
            es = float(np.mean((model(Xe).numpy() - ye) ** 2))
        if es < best - 1e-6:
            best, best_state, bad = es, {k: v.clone() for k, v in
                                         model.state_dict().items()}, 0
        else:
            bad += 1
            if bad >= patience:
                break
    if best_state is not None:
        model.load_state_dict(best_state)
    model.eval()
    with torch.no_grad():
        pred_std = model(torch.tensor(Xva, dtype=torch.float32)).numpy()
    return pred_std * y_sd + y_mu  # back to original target scale


def freq_encode(train_vals, all_vals):
    freq = pd.Series(train_vals).value_counts(normalize=True)
    return np.array([freq.get(v, 0.0) for v in all_vals], dtype=float)


def main():
    print("FT-Transformer T2 (CPU)", flush=True)
    df = pd.read_csv("./censorship_candidates.csv")
    y = np.log(df["V_BNBwei"].astype(float).values)
    groups = df["block"].values
    num_cols = ["token0Side", "poolReserveLog10", "hops", "gas",
                "gasFeeCapWei", "gasTipCapWei", "blockFullness", "ledgerSize"]
    num = df[num_cols].astype(float).values
    for j, c in enumerate(num_cols):
        if c in ("gas", "gasFeeCapWei", "gasTipCapWei", "ledgerSize"):
            num[:, j] = np.log1p(num[:, j])
    cat_cols = ["pool", "dex", "numeraire", "validator"]
    cats = {c: df[c].astype(str).values for c in cat_cols}

    def builder(tr, va):
        enc = [freq_encode(cats[c][tr], cats[c]) for c in cat_cols]
        feats = np.nan_to_num(np.column_stack([num] + enc))
        return feats[tr], feats[va]

    # "large" is depth=3/d=48/heads=6: ~5-6x the small variant's parameters
    # (still a genuine high-capacity attention model for the Grinsztajn
    # comparison) but tractable on a heavily-shared CPU box.
    configs = {
        "small": dict(d_token=16, depth=1, heads=2, ff_mult=2, dropout=0.1),
        "large": dict(d_token=48, depth=3, heads=6, ff_mult=2, dropout=0.1),
    }
    n = len(y)
    gkf = GroupKFold(n_splits=5)
    oof = {"FT-small": np.zeros(n), "FT-large": np.zeros(n),
           "XGB": np.zeros(n)}
    for fold, (tr, va) in enumerate(gkf.split(np.zeros(n), y, groups)):
        Xtr, Xva = builder(tr, va)
        ytr = y[tr]
        sc = StandardScaler().fit(Xtr)
        Xtr_s, Xva_s = sc.transform(Xtr), sc.transform(Xva)
        oof["FT-small"][va] = train_ft(Xtr_s, ytr, Xva_s, configs["small"])
        print(f"   [T2] fold {fold+1}/5 FT-small done", flush=True)
        oof["FT-large"][va] = train_ft(Xtr_s, ytr, Xva_s, configs["large"])
        print(f"   [T2] fold {fold+1}/5 FT-large done", flush=True)
        m = xgb.XGBRegressor(
            n_estimators=400, max_depth=4, learning_rate=0.05, subsample=0.8,
            colsample_bytree=0.8, reg_lambda=1.0, n_jobs=8, random_state=SEED)
        m.fit(Xtr, ytr)
        oof["XGB"][va] = m.predict(Xva)
        print(f"   [T2] fold {fold+1}/5 XGB done", flush=True)
    res = {k: r2_score(y, v) for k, v in oof.items()}
    print(f"\n[T2 log(V_BNBwei)] n={n} GroupKFold k=5", flush=True)
    for k in ["FT-small", "FT-large", "XGB"]:
        print(f"   {k:9s} CV R2 = {res[k]:+.4f}", flush=True)
    print("JSON_RESULT=" + json.dumps({"T2": {**res, "n": n, "feats": 12}}),
          flush=True)


if __name__ == "__main__":
    main()
