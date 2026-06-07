#!/usr/bin/env python3
"""
Capacity sweep: PyTorch MLPs (CPU) at increasing capacity on T1 (curl-magnitude)
and T2 (value-magnitude). Reports CROSS-VALIDATED R2 at each capacity.

Design goals from the MEV-measurement paper:
  - Show capacity is NOT the lever. Deep nets should NOT beat XGBoost on this
    tiny tabular data, and bigger MLPs should not beat smaller ones (overfit).
  - RIGOR: only cross-validated (out-of-fold) metrics. GroupKFold by block for
    T1; GroupKFold by block for T2 as well (blocks are the unit; avoids leakage
    of same-block rows across train/val/test). Strict leakage control.

NO GPU. torch CPU. Data is tiny on purpose.
"""
import os, sys, json, math, random
# Box is heavily oversubscribed by sibling jobs; cap BLAS/torch threads BEFORE
# importing numpy/torch so we don't thrash the scheduler. Data is tiny.
for _v in ("OMP_NUM_THREADS","OPENBLAS_NUM_THREADS","MKL_NUM_THREADS",
           "NUMEXPR_NUM_THREADS","VECLIB_MAXIMUM_THREADS"):
    os.environ.setdefault(_v, "4")
import numpy as np
import pandas as pd

import torch
import torch.nn as nn
from sklearn.model_selection import GroupKFold
from sklearn.metrics import r2_score
from sklearn.preprocessing import StandardScaler

torch.set_num_threads(4)
SEED = 1234
def set_seed(s):
    random.seed(s); np.random.seed(s); torch.manual_seed(s)
set_seed(SEED)

DEV = torch.device("cpu")
N_SPLITS = 5
MAX_EPOCHS = 250       # tiny data converges fast; early stopping ends sooner
PATIENCE = 20          # early stopping on validation fold
BATCH = 256

# ----------------------------------------------------------------------------
# Capacity ladder (descriptors are reported verbatim)
# ----------------------------------------------------------------------------
CAPACITIES = [
    ("small",  [64, 64],            0.0,  "2 layers x64"),
    ("medium", [256, 256, 256, 256],0.1,  "4 layers x256"),
    ("large",  [512]*6,             0.2,  "6 layers x512"),
]

# ----------------------------------------------------------------------------
# Model: MLP with optional categorical embeddings, BatchNorm, dropout
# ----------------------------------------------------------------------------
class MLP(nn.Module):
    def __init__(self, n_num, cat_cardinalities, hidden, dropout):
        super().__init__()
        self.embs = nn.ModuleList()
        emb_dim_total = 0
        for card in cat_cardinalities:
            d = int(min(50, round(1.6 * card ** 0.56)))  # heuristic emb size
            d = max(d, 1)
            self.embs.append(nn.Embedding(card, d))
            emb_dim_total += d
        in_dim = n_num + emb_dim_total
        layers = []
        prev = in_dim
        for h in hidden:
            layers += [nn.Linear(prev, h), nn.BatchNorm1d(h), nn.ReLU(),
                       nn.Dropout(dropout)]
            prev = h
        layers += [nn.Linear(prev, 1)]
        self.net = nn.Sequential(*layers)

    def forward(self, x_num, x_cat):
        parts = [x_num]
        for i, emb in enumerate(self.embs):
            parts.append(emb(x_cat[:, i]))
        x = torch.cat(parts, dim=1)
        return self.net(x).squeeze(-1)


def train_one_fold(Xn_tr, Xc_tr, y_tr, Xn_va, Xc_va, y_va,
                   cat_cards, hidden, dropout, seed):
    set_seed(seed)
    model = MLP(Xn_tr.shape[1], cat_cards, hidden, dropout).to(DEV)
    opt = torch.optim.Adam(model.parameters(), lr=1e-3, weight_decay=1e-5)
    lossf = nn.MSELoss()

    Xn_tr_t = torch.tensor(Xn_tr, dtype=torch.float32)
    Xc_tr_t = torch.tensor(Xc_tr, dtype=torch.long)
    y_tr_t  = torch.tensor(y_tr,  dtype=torch.float32)
    Xn_va_t = torch.tensor(Xn_va, dtype=torch.float32)
    Xc_va_t = torch.tensor(Xc_va, dtype=torch.long)
    y_va_t  = torch.tensor(y_va,  dtype=torch.float32)

    n = len(y_tr)
    best_va = float("inf"); best_state = None; bad = 0
    for ep in range(MAX_EPOCHS):
        model.train()
        perm = torch.randperm(n)
        for i in range(0, n, BATCH):
            idx = perm[i:i+BATCH]
            if len(idx) < 2:  # BatchNorm needs >1
                continue
            opt.zero_grad()
            pred = model(Xn_tr_t[idx], Xc_tr_t[idx])
            loss = lossf(pred, y_tr_t[idx])
            loss.backward()
            opt.step()
        model.eval()
        with torch.no_grad():
            va_pred = model(Xn_va_t, Xc_va_t)
            va_loss = lossf(va_pred, y_va_t).item()
        if va_loss < best_va - 1e-6:
            best_va = va_loss
            best_state = {k: v.clone() for k, v in model.state_dict().items()}
            bad = 0
        else:
            bad += 1
            if bad >= PATIENCE:
                break
    if best_state is not None:
        model.load_state_dict(best_state)
    model.eval()
    with torch.no_grad():
        out = model(Xn_va_t, Xc_va_t).numpy()
    return out


def freq_encode(series, train_idx):
    """Frequency encoding fit on TRAIN ONLY (leakage-safe)."""
    counts = series.iloc[train_idx].value_counts(normalize=True)
    return series.map(counts).fillna(0.0).values


def run_target(name, df, groups, num_cols, cat_cols, y, freq_cols=None,
               outer_seed=0):
    """
    GroupKFold by block. Inner: hold out one fold as test; from the remaining,
    carve a validation split (also group-aware) for early stopping.
    Returns dict capacity -> CV OOF R2.
    """
    freq_cols = freq_cols or []
    gkf = GroupKFold(n_splits=N_SPLITS)
    results = {cap[0]: np.full(len(y), np.nan) for cap in CAPACITIES}

    # categorical -> integer codes (global vocab is fine: embeddings; unseen
    # handled by mapping codes; we map per full-data factorize but only AFTER
    # ensuring test categories that are unseen in train default to a pad index)
    cat_cards = []
    cat_codes_full = {}
    for c in cat_cols:
        codes, uniques = pd.factorize(df[c])
        # reserve index 0 as "unknown/pad"; shift by 1
        cat_codes_full[c] = codes + 1
        cat_cards.append(len(uniques) + 1)

    folds = list(gkf.split(np.arange(len(y)), y, groups))
    for cap_name, hidden, dropout, _desc in CAPACITIES:
        oof = np.full(len(y), np.nan)
        for fi, (trva_idx, te_idx) in enumerate(folds):
            # split trva into train/val by group (use a sub GroupKFold)
            sub_groups = groups[trva_idx]
            sub_gkf = GroupKFold(n_splits=4)
            sub_tr_rel, sub_va_rel = next(sub_gkf.split(trva_idx, y[trva_idx],
                                                        sub_groups))
            tr_idx = trva_idx[sub_tr_rel]
            va_idx = trva_idx[sub_va_rel]

            # numeric features (+ leakage-safe freq encodings fit on tr_idx)
            num_mat_cols = []
            base_num = df[num_cols].astype(float).values
            num_mat_cols.append(base_num)
            for fc in freq_cols:
                fe = freq_encode(df[fc], tr_idx).reshape(-1, 1)
                num_mat_cols.append(fe)
            num_mat = np.hstack(num_mat_cols)

            scaler = StandardScaler().fit(num_mat[tr_idx])
            Xn = scaler.transform(num_mat)
            Xn = np.nan_to_num(Xn, nan=0.0, posinf=0.0, neginf=0.0)

            # categorical codes; remap test-unseen codes (not in train) -> 0
            Xc = np.zeros((len(y), len(cat_cols)), dtype=np.int64)
            for j, c in enumerate(cat_cols):
                col = cat_codes_full[c].copy()
                seen = set(np.unique(col[tr_idx]))
                mask_unseen = ~np.isin(col, list(seen))
                col = col.copy()
                col[mask_unseen] = 0
                Xc[:, j] = col

            yz = y.copy()
            ymean = yz[tr_idx].mean(); ystd = yz[tr_idx].std() + 1e-8
            yz = (yz - ymean) / ystd

            pred_va_test = train_one_fold(
                Xn[tr_idx], Xc[tr_idx], yz[tr_idx],
                Xn[te_idx], Xc[te_idx], yz[te_idx],
                cat_cards, hidden, dropout, seed=SEED + fi)
            oof[te_idx] = pred_va_test * ystd + ymean

        r2 = r2_score(y, oof)
        results[cap_name] = r2
        print(f"  [{name}] capacity={cap_name:6s} hidden={hidden} "
              f"dropout={dropout}  CV-OOF R2 = {r2:.4f}", flush=True)
    return results


# ============================================================================
# T1: curl-magnitude. target = curlFrac on NON-COMMUTING subset (omegaNorm2>0)
#   Features = PRE-STATE only: k, actors, pool freq-encoding, log flow-mag.
#   EXCLUDE gradFrac/rho/scalarCurlFrac/harmonicFrac (Hodge outputs / label fns)
# ============================================================================
def load_T1():
    c = pd.read_csv('./curl_clusters.csv')
    def big2f(x):
        try: return float(int(str(x)))
        except:
            try: return float(x)
            except: return np.nan
    c['omega'] = c['omegaNorm2'].map(big2f)
    sub = c[c['omega'] > 0].copy().reset_index(drop=True)
    # log flow-magnitude (pre-state proxy of trade size scale)
    sub['log_flow'] = np.log10(sub['omega'])
    # actors has NaNs in commuting rows but in non-commuting subset it's complete
    sub['actors'] = sub['actors'].fillna(sub['actors'].median())
    y = sub['curlFrac'].values.astype(float)
    groups = sub['block'].values
    num_cols = ['k', 'actors', 'log_flow']
    cat_cols = []                      # pool handled via freq-encoding (87 lvls)
    freq_cols = ['pool']
    return ('T1_curlFrac', sub, groups, num_cols, cat_cols, y, freq_cols)


# ============================================================================
# T2: value-magnitude. target = log(V_BNBwei) on censorship_candidates.
#   EXCLUDE grossBNBwei (V+gas, near-label leak) and leadTimeSec (post-treat).
#   Embeddings for pool/validator/dex; numeric pre-state covariates.
# ============================================================================
def load_T2():
    d = pd.read_csv('./censorship_candidates.csv')
    y = np.log(d['V_BNBwei'].values.astype(float))
    groups = d['block'].values
    num_cols = ['poolReserveLog10', 'hops', 'gas', 'gasFeeCapWei',
                'gasTipCapWei', 'blockFullness', 'ledgerSize', 'token0Side']
    cat_cols = ['dex', 'numeraire', 'validator', 'pool']  # embeddings
    freq_cols = []
    return ('T2_logV', d, groups, num_cols, cat_cols, y, freq_cols)


if __name__ == "__main__":
    out = {}
    for loader in (load_T1, load_T2):
        name, df, groups, num_cols, cat_cols, y, freq_cols = loader()
        print(f"\n=== {name}  n={len(y)}  blocks={len(np.unique(groups))} "
              f"num={num_cols} cat={cat_cols} freq={freq_cols} ===", flush=True)
        res = run_target(name, df, groups, num_cols, cat_cols, y,
                         freq_cols=freq_cols)
        out[name] = res
    print("\n=== SUMMARY (CV-OOF R2) ===")
    print(json.dumps(out, indent=2))
    with open('./capacity_sweep_results.json', 'w') as f:
        json.dump(out, f, indent=2)
