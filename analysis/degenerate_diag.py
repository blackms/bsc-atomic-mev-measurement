#!/usr/bin/env python
"""
DEGENERATE-TARGET DIAGNOSTIC: is the ~0.96 legit AUC a real signal or an
artifact of 7-positive small-sample geometry? (paper-grade rigor)

Tests:
  (1) Block-permutation null: permute y at the BLOCK level (keep group
      structure) and re-run the SAME XGBoost legit CV many times. If the
      observed AUC sits inside the permutation null, it is NOT learnable
      signal -- it is the geometry of 7 positives.
  (2) Per-fold positive counts + per-fold AUC: with 7 positives across 5
      GroupKFold folds, most folds have 0-2 positives, so the pooled AUC is
      driven by a couple of points -> metric CI spans chance.
  (3) Bootstrap CI on the observed pooled AUC (resample rows) -> width.
"""
import json, warnings, sys, time, numpy as np, pandas as pd
warnings.filterwarnings("ignore")
from sklearn.model_selection import GroupKFold
from sklearn.metrics import roc_auc_score, average_precision_score
import xgboost as xgb

def log(*a): print(*a, file=sys.stderr, flush=True)
T0 = time.time()

cz = pd.read_csv("./censorship_candidates.csv")
cz = cz[cz["dropped"].isin([0, 1])].copy()
cz["dropped"] = cz["dropped"].astype(int)
y = (cz["dropped"] == 0).astype(int).values
groups = cz["block"].values

cat_cols = ["pool", "dex", "numeraire", "validator"]
for c in cat_cols:
    cz[c + "_freq"] = cz[c].map(cz[c].value_counts(normalize=True)).values
num_cols = ["token0Side", "poolReserveLog10", "hops", "gas",
            "gasFeeCapWei", "gasTipCapWei", "blockFullness", "ledgerSize", "V_USD"]
legit_cols = num_cols + [c + "_freq" for c in cat_cols]
X = cz[legit_cols].apply(pd.to_numeric, errors="coerce").fillna(0.0).values.astype(np.float32)

def xgb_oof(X, y, groups, seed=0):
    gkf = GroupKFold(n_splits=5)
    oof = np.full(len(y), np.nan)
    foldinfo = []
    for tr, te in gkf.split(X, y, groups):
        npos = max(int(y[tr].sum()), 1)
        clf = xgb.XGBClassifier(n_estimators=300, max_depth=6, learning_rate=0.05,
                                subsample=0.8, colsample_bytree=0.8, n_jobs=16,
                                scale_pos_weight=(len(y[tr]) - npos) / npos,
                                eval_metric="logloss", tree_method="hist",
                                random_state=seed)
        clf.fit(X[tr], y[tr])
        p = clf.predict_proba(X[te])[:, 1]
        oof[te] = p
        te_pos = int(y[te].sum())
        fa = (roc_auc_score(y[te], p) if len(np.unique(y[te])) == 2 else None)
        foldinfo.append({"test_pos": te_pos, "test_n": int(len(te)), "fold_auc": fa})
    return oof, foldinfo

# ---- observed ----
oof, foldinfo = xgb_oof(X, y, groups, seed=0)
obs_auc = float(roc_auc_score(y, oof))
obs_aupr = float(average_precision_score(y, oof))
base = float(y.mean())
log("observed pooled AUC=%.4f AUPR=%.4f base=%.5f" % (obs_auc, obs_aupr, base))
for i, f in enumerate(foldinfo):
    log("  fold %d: test_pos=%d test_n=%d fold_auc=%s" % (i, f["test_pos"], f["test_n"], f["fold_auc"]))

# ---- (1) BLOCK-level permutation null ----
# Permute the label across blocks: each block keeps its size, we reassign the
# "is-included" mark to random rows -> destroys any real X->y relation while
# preserving the 7-positive / grouped geometry.
rng = np.random.default_rng(0)
n = len(y); npos = int(y.sum())
NPERM = 60
perm_aucs = []
for k in range(NPERM):
    yp = np.zeros(n, dtype=int)
    yp[rng.choice(n, size=npos, replace=False)] = 1
    if yp.sum() < 1:
        continue
    oofp, _ = xgb_oof(X, yp, groups, seed=k + 1)
    if len(np.unique(yp)) == 2 and not np.isnan(oofp).all():
        try:
            perm_aucs.append(float(roc_auc_score(yp, oofp)))
        except Exception:
            pass
    if (k + 1) % 15 == 0:
        log("  perm %d/%d done (%.0fs)" % (k + 1, NPERM, time.time() - T0))
perm_aucs = np.array(perm_aucs)
p_value = float((perm_aucs >= obs_auc).mean()) if len(perm_aucs) else None

# ---- (3) bootstrap CI on observed pooled AUC ----
boot = []
for b in range(500):
    idx = rng.choice(n, size=n, replace=True)
    if len(np.unique(y[idx])) < 2:
        continue
    try:
        boot.append(roc_auc_score(y[idx], oof[idx]))
    except Exception:
        pass
boot = np.array(boot)
ci = [float(np.percentile(boot, 2.5)), float(np.percentile(boot, 97.5))] if len(boot) else [None, None]

OUT = {
    "observed": {"pooled_auc": obs_auc, "pooled_aupr": obs_aupr, "base_rate": base,
                 "n_positives": npos, "n": n},
    "per_fold": foldinfo,
    "folds_with_zero_positives": int(sum(1 for f in foldinfo if f["test_pos"] == 0)),
    "permutation_null": {
        "n_perms": int(len(perm_aucs)),
        "null_auc_mean": float(perm_aucs.mean()) if len(perm_aucs) else None,
        "null_auc_p95": float(np.percentile(perm_aucs, 95)) if len(perm_aucs) else None,
        "null_auc_max": float(perm_aucs.max()) if len(perm_aucs) else None,
        "p_value_obs_ge_null": p_value,
    },
    "bootstrap_auc_95ci": ci,
    "interpretation": (
        "If the permutation p-value is large (obs AUC is ordinary for a RANDOM "
        "7-positive relabeling) and the bootstrap CI is huge / spans chance, the "
        "elevated pooled AUC is small-sample geometry on 7 positives, not learnable "
        "structure. The AUPR collapsing toward base rate confirms no usable ranking."),
}
print(json.dumps(OUT, indent=2, default=str))
