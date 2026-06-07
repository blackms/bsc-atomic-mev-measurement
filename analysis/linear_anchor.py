#!/usr/bin/env python
"""
LOW-CAPACITY ANCHOR rung of the capacity ladder.

Fits standardized linear regression (Ridge / Lasso) with CV-tuned alpha on:
  T1 curl-magnitude:  target = curlFrac on NON-COMMUTING subset (omegaNorm2>0)
  T2 value-magnitude: target = log(V_BNBwei) on censorship_candidates

Reports GroupKFold-by-block CROSS-VALIDATED R2 (never train metrics).
Strict leakage control: PRE-STATE features only; Hodge outputs / near-label
columns / post-treatment columns excluded.

CPU only. Data is tiny by design (that is the point: capacity is not the lever).
"""
import numpy as np
import pandas as pd
from sklearn.linear_model import Ridge, Lasso
from sklearn.preprocessing import StandardScaler
from sklearn.pipeline import Pipeline
from sklearn.model_selection import GroupKFold, cross_val_score, GridSearchCV
from sklearn.metrics import r2_score

RNG = 0
np.random.seed(RNG)

# Frequency-encode a categorical column using ONLY the training-fold rows, to
# avoid target/test leakage. Implemented as a fold-safe transform: we compute a
# per-fold frequency map. For the linear anchor we instead fold-encode inside CV.

def freq_encode_cv(series, train_idx):
    """Return a full-length array; frequencies learned ONLY from train_idx."""
    counts = series.iloc[train_idx].value_counts()
    n = len(train_idx)
    # unseen categories -> very low freq (1/(n+1))
    mapped = series.map(counts).fillna(0.0)
    return (mapped.values + 0.0) / float(n)


def grouped_cv_r2(X_raw, y, groups, freq_cols, model_factory, alphas, n_splits=5):
    """
    Manual GroupKFold CV that fits the StandardScaler, frequency-encoding and the
    inner alpha selection ENTIRELY within each training fold. Returns per-fold
    out-of-fold R2 list and the pooled out-of-fold R2.
    freq_cols: list of (col_name, source_series) computed per-fold.
    X_raw: DataFrame of already-numeric (non-freq) features.
    """
    gkf = GroupKFold(n_splits=n_splits)
    fold_r2 = []
    oof_pred = np.full(len(y), np.nan)
    for tr, te in gkf.split(X_raw, y, groups):
        # build design matrices fold-locally
        Xtr = X_raw.iloc[tr].copy()
        Xte = X_raw.iloc[te].copy()
        for col, src in freq_cols:
            enc = freq_encode_cv(src, tr)  # learned on train rows only
            Xtr[col + "_freq"] = enc[tr]
            Xte[col + "_freq"] = enc[te]
        # inner alpha selection via nested GroupKFold on the train fold
        inner_groups = groups[tr]
        best_alpha, best_score = None, -np.inf
        n_inner = min(5, np.unique(inner_groups).size)
        if n_inner < 2:
            n_inner = 2
        for a in alphas:
            pipe = Pipeline([("sc", StandardScaler()),
                             ("mdl", model_factory(a))])
            try:
                sc = cross_val_score(pipe, Xtr.values, y[tr],
                                     groups=inner_groups,
                                     cv=GroupKFold(n_splits=n_inner),
                                     scoring="r2").mean()
            except Exception:
                sc = -np.inf
            if sc > best_score:
                best_score, best_alpha = sc, a
        pipe = Pipeline([("sc", StandardScaler()),
                         ("mdl", model_factory(best_alpha))])
        pipe.fit(Xtr.values, y[tr])
        pred = pipe.predict(Xte.values)
        oof_pred[te] = pred
        fold_r2.append(r2_score(y[te], pred))
    pooled = r2_score(y, oof_pred)
    return np.array(fold_r2), pooled


def run_T1():
    c = pd.read_csv('./curl_clusters.csv')
    c['omegaNorm2'] = pd.to_numeric(c['omegaNorm2'], errors='coerce')
    nc = c[c['omegaNorm2'] > 0].copy().reset_index(drop=True)
    y = nc['curlFrac'].astype(float).values
    # PRE-STATE features only:
    #   k, actors, log flow-magnitude (log10 omegaNorm2), pool frequency-encoding
    # EXCLUDE: gradFrac, rho, scalarCurlFrac, harmonicFrac (Hodge outputs),
    #          curlFrac (label), perms/vspreadWei (mostly missing & post).
    nc['logFlow'] = np.log10(nc['omegaNorm2'].clip(lower=1.0))
    base = nc[['k', 'actors', 'logFlow']].astype(float).copy()
    groups = nc['block'].values
    freq_cols = [('pool', nc['pool'])]
    n_splits = min(5, nc['block'].nunique())

    results = {}
    for name, factory in [
        ('Ridge', lambda a: Ridge(alpha=a, random_state=RNG)),
        ('Lasso', lambda a: Lasso(alpha=a, random_state=RNG, max_iter=20000)),
    ]:
        alphas = (np.logspace(-3, 3, 25) if name == 'Ridge'
                  else np.logspace(-4, 1, 25))
        fr, pooled = grouped_cv_r2(base, y, groups, freq_cols, factory,
                                   alphas, n_splits=n_splits)
        results[name] = dict(fold_mean=float(fr.mean()), fold_std=float(fr.std()),
                             pooled=float(pooled))
        print(f"[T1 {name}] foldR2 mean={fr.mean():.4f} std={fr.std():.4f} "
              f"pooledOOF={pooled:.4f}  folds={np.round(fr,3).tolist()}")
    return results, dict(n=len(y), blocks=int(nc['block'].nunique()),
                         features=list(base.columns) + ['pool_freq'])


def grouped_cv_r2_onehot(X_raw, y, groups, oh_cols, model_factory, alphas,
                         n_splits=5):
    """
    GroupKFold CV with fold-safe ONE-HOT of high-cardinality categoricals.
    Categories unseen in a train fold map to all-zero columns (no leakage).
    Scaler/alpha-selection all fit within train fold.
    oh_cols: list of (name, source_series).
    """
    gkf = GroupKFold(n_splits=n_splits)
    fold_r2 = []
    oof_pred = np.full(len(y), np.nan)
    for tr, te in gkf.split(X_raw, y, groups):
        Xtr = X_raw.iloc[tr].copy()
        Xte = X_raw.iloc[te].copy()
        for col, src in oh_cols:
            cats = pd.Index(src.iloc[tr].unique())  # train-only vocabulary
            tr_oh = pd.get_dummies(src.iloc[tr], prefix=col, dtype=float)
            tr_oh = tr_oh.reindex(columns=[f"{col}_{c}" for c in cats],
                                  fill_value=0.0)
            te_oh = pd.get_dummies(src.iloc[te], prefix=col, dtype=float)
            te_oh = te_oh.reindex(columns=tr_oh.columns, fill_value=0.0)
            Xtr = pd.concat([Xtr.reset_index(drop=True),
                             tr_oh.reset_index(drop=True)], axis=1)
            Xte = pd.concat([Xte.reset_index(drop=True),
                             te_oh.reset_index(drop=True)], axis=1)
        inner_groups = groups[tr]
        best_alpha, best_score = None, -np.inf
        n_inner = max(2, min(5, np.unique(inner_groups).size))
        for a in alphas:
            pipe = Pipeline([("sc", StandardScaler(with_mean=False)),
                             ("mdl", model_factory(a))])
            try:
                sc = cross_val_score(pipe, Xtr.values, y[tr],
                                     groups=inner_groups,
                                     cv=GroupKFold(n_splits=n_inner),
                                     scoring="r2").mean()
            except Exception:
                sc = -np.inf
            if sc > best_score:
                best_score, best_alpha = sc, a
        pipe = Pipeline([("sc", StandardScaler(with_mean=False)),
                         ("mdl", model_factory(best_alpha))])
        pipe.fit(Xtr.values, y[tr])
        pred = pipe.predict(Xte.values)
        oof_pred[te] = pred
        fold_r2.append(r2_score(y[te], pred))
    pooled = r2_score(y, oof_pred)
    return np.array(fold_r2), pooled


def run_T2():
    d = pd.read_csv('./censorship_candidates.csv')
    y = np.log(d['V_BNBwei'].astype('float64').values)
    # EXCLUDE: grossBNBwei (=V+gas, near-label leak), leadTimeSec (post-treatment),
    #          V_USD (= V scaled, leak), V_BNBwei (label), tx (id), dropped (degen).
    num_cols = ['token0Side', 'poolReserveLog10', 'hops', 'gas',
                'gasFeeCapWei', 'gasTipCapWei', 'blockFullness', 'ledgerSize']
    base = d[num_cols].astype(float).copy()
    # one-hot small categoricals
    base['is_v3'] = (d['dex'] == 'pancake_v3').astype(float)
    base['num_wbnb'] = (d['numeraire'] == 'wbnb').astype(float)
    groups = d['block'].values
    n_splits = 5

    # The value-level signal lives in pool IDENTITY (per-pool typical trade size),
    # which is a level effect frequency-encoding cannot represent. The honest
    # linear anchor therefore uses fold-safe pool one-hot. We report BOTH the
    # frequency-encoded variant (no level info) and the one-hot variant.
    freq_cols = [('pool', d['pool']), ('validator', d['validator'])]
    oh_cols = [('pool', d['pool']), ('validator', d['validator'])]

    results = {}
    for name, factory in [
        ('Ridge', lambda a: Ridge(alpha=a, random_state=RNG)),
        ('Lasso', lambda a: Lasso(alpha=a, random_state=RNG, max_iter=20000)),
    ]:
        alphas = (np.logspace(-3, 3, 25) if name == 'Ridge'
                  else np.logspace(-4, 1, 25))
        fr_f, pooled_f = grouped_cv_r2(base, y, groups, freq_cols, factory,
                                       alphas, n_splits=n_splits)
        fr_o, pooled_o = grouped_cv_r2_onehot(base, y, groups, oh_cols, factory,
                                              alphas, n_splits=n_splits)
        results[name] = dict(
            freqenc=dict(fold_mean=float(fr_f.mean()), fold_std=float(fr_f.std()),
                         pooled=float(pooled_f)),
            onehot=dict(fold_mean=float(fr_o.mean()), fold_std=float(fr_o.std()),
                        pooled=float(pooled_o)))
        print(f"[T2 {name} freq] foldR2 mean={fr_f.mean():.4f} std={fr_f.std():.4f} "
              f"pooledOOF={pooled_f:.4f}")
        print(f"[T2 {name} 1hot] foldR2 mean={fr_o.mean():.4f} std={fr_o.std():.4f} "
              f"pooledOOF={pooled_o:.4f}  folds={np.round(fr_o,3).tolist()}")
    return results, dict(n=len(y), blocks=int(d['block'].nunique()),
                         features_freq=list(base.columns) + ['pool_freq', 'validator_freq'],
                         features_onehot=list(base.columns) + ['pool_onehot', 'validator_onehot'])


if __name__ == '__main__':
    print("=== LOW-CAPACITY ANCHOR: standardized linear (Ridge/Lasso), GroupKFold by block ===")
    print("\n--- T1 curl-magnitude (curlFrac | omegaNorm2>0) ---")
    t1, t1meta = run_T1()
    print("T1 meta:", t1meta)
    print("\n--- T2 value-magnitude (log V_BNBwei) ---")
    t2, t2meta = run_T2()
    print("T2 meta:", t2meta)

    import json
    out = dict(T1=dict(results=t1, meta=t1meta),
               T2=dict(results=t2, meta=t2meta))
    with open('./linear_anchor_results.json', 'w') as f:
        json.dump(out, f, indent=2)
    print("\nSaved -> ./linear_anchor_results.json")
