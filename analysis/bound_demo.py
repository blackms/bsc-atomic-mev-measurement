import numpy as np, pandas as pd, json, warnings
warnings.filterwarnings("ignore")
from sklearn.model_selection import StratifiedKFold, cross_val_predict, KFold
from sklearn.metrics import roc_auc_score, average_precision_score, r2_score
import xgboost as xgb

out = {}

# ============================================================
# (b) BOUND ARGUMENT - demonstrate with our numbers
# ============================================================
# The counterfactual SimEngine already grants PERFECT ex-post knowledge
# (the strongest predictor possible). Its realizable extraction value:
#   capturedRealizedNetWei = 0   (constant, by construction of the MEV-edge~0 finding)
# An oracle predictor = the SimEngine itself. realizable = 0.
# Therefore ANY learned predictor f(X) has extraction value <= oracle = ~0.

rt = pd.read_csv("./realizability_tally.csv")
oracle_realized_BNB = float(rt["capturedRealizedNetWei"].max()/1e18)  # best snapshot
oracle_ournet_BNB   = float(rt["capturedOurNetWei"].max()/1e18)
expost_gross_BNB    = float(rt["leftNetWei"].median()/1e18)  # ex-post (pre-competition) net visible
out["bound"] = {
  "oracle_realized_capture_BNB": oracle_realized_BNB,   # 0.0
  "oracle_ournet_capture_BNB": oracle_ournet_BNB,       # 0.0
  "expost_visible_net_BNB_median": expost_gross_BNB,    # ~199 BNB visible ex-post but unrealizable
  "leftOnTable_count_median": float(rt["leftOnTable"].median()),
  "argument": "oracle(perfect ex-post)=0 realized => sup_f value(f) <= 0",
}

# ----- Censorship 'dropped' classifier (the only classification target) -----
# Honest CV: 7 minority positives -> can't reliably train. Show two things:
#   (1) leadTimeSec ALONE trivially separates (leakage/tautology, not predictive edge)
#   (2) without the leaky lead-time feature, near-degenerate; AUPRC ~ base rate
cz = pd.read_csv("./censorship_candidates.csv")
y = (cz["dropped"]==0).astype(int).values  # predict the RARE 'included' class (positive=minority)
base_rate = y.mean()

cat_cols = ["pool","dex","numeraire","validator"]
num_cols = ["token0Side","grossBNBwei","poolReserveLog10","hops","gas",
            "gasFeeCapWei","gasTipCapWei","blockFullness","ledgerSize","leadTimeSec"]
Xall = cz[num_cols].copy()
for c in cat_cols:
    Xall[c] = cz[c].astype("category").cat.codes

def cv_auc_aupr(X, y, leaky_note=""):
    # Need >=2 per class per fold; with 7 positives use 5-fold stratified but guard
    skf = StratifiedKFold(n_splits=5, shuffle=True, random_state=0)
    clf = xgb.XGBClassifier(n_estimators=200, max_depth=4, learning_rate=0.05,
                            subsample=0.8, colsample_bytree=0.8, n_jobs=8,
                            scale_pos_weight=(len(y)-y.sum())/max(y.sum(),1),
                            eval_metric="logloss", tree_method="hist")
    try:
        proba = cross_val_predict(clf, X, y, cv=skf, method="predict_proba")[:,1]
        return float(roc_auc_score(y, proba)), float(average_precision_score(y, proba))
    except Exception as e:
        return None, None

# WITH leaky leadTimeSec
auc_l, aupr_l = cv_auc_aupr(Xall, y)
# WITHOUT leaky leadTimeSec (drop the tautological delayed-inclusion signal)
auc_n, aupr_n = cv_auc_aupr(Xall.drop(columns=["leadTimeSec"]), y)

out["censorship_clf"] = {
  "positives_included": int(y.sum()), "rows": int(len(y)), "base_rate": float(base_rate),
  "cv_auc_with_leadTime(leaky)": auc_l, "cv_aupr_with_leadTime(leaky)": aupr_l,
  "cv_auc_without_leadTime": auc_n, "cv_aupr_without_leadTime": aupr_n,
  "note": "7 positives => any metric is statistically meaningless (CI spans chance). leadTimeSec is delayed-inclusion tautology, not an extraction signal.",
}

# ----- Censorship V_BNBwei outcome regression (mu(X)) - is value PREDICTABLE? -----
# This is the strongest legit regression target. Honest 5-fold CV R^2.
yv = np.log10(cz["V_BNBwei"].values.astype(float))  # log scale (range spans 6 orders)
Xv = Xall.drop(columns=["leadTimeSec"])  # exclude leaky outcome-correlated timing
kf = KFold(n_splits=5, shuffle=True, random_state=0)
reg = xgb.XGBRegressor(n_estimators=400, max_depth=5, learning_rate=0.05,
                       subsample=0.8, colsample_bytree=0.8, n_jobs=8, tree_method="hist")
pred = cross_val_predict(reg, Xv, yv, cv=kf)
out["censorship_value_reg"] = {
  "target": "log10(V_BNBwei)", "cv_r2": float(r2_score(yv, pred)),
  "note": "Even if value magnitude is predictable, realizable extraction stays 0 (bound).",
}

# ============================================================
# (c) CURL CLUSTERS regression - the Hodge rho/curlFrac targets
# (not an extraction target; structural/interpretability only)
# ============================================================
cc = pd.read_csv("./curl_clusters.csv")
feats = ["k","actors","omegaNorm2","harmonicFrac","scalarCurlFrac","perms","vspreadWei"]
# harmonicFrac degenerate (constant 0); perms/vspreadWei mostly NaN. drop fully-empty.
feats = [f for f in feats if cc[f].notna().any() and cc[f].nunique()>1]
Xc = cc[feats].copy()
for f in feats:
    Xc[f] = pd.to_numeric(Xc[f], errors="coerce")
Xc = Xc.fillna(-1)
def cv_r2(X, ytar):
    kf = KFold(n_splits=5, shuffle=True, random_state=0)
    reg = xgb.XGBRegressor(n_estimators=300, max_depth=4, learning_rate=0.05,
                           subsample=0.8, colsample_bytree=0.8, n_jobs=8, tree_method="hist")
    pred = cross_val_predict(reg, X, ytar, cv=kf)
    return float(r2_score(ytar, pred))
curlFrac_t = pd.to_numeric(cc["curlFrac"], errors="coerce")
rho_t = pd.to_numeric(cc["rho"], errors="coerce")  # 'n/a(commuting)' -> NaN (no-curl rows)
m_cf = curlFrac_t.notna().values
m_rho = rho_t.notna().values
out["curl_clusters"] = {
  "rows": int(len(cc)), "usable_feats": feats,
  "curlFrac_nonnull": int(m_cf.sum()), "rho_nonnull": int(m_rho.sum()),
  "rho_commuting_sentinel_rows": int((~m_rho).sum()),
  "cv_r2_curlFrac": cv_r2(Xc.loc[m_cf], curlFrac_t[m_cf].values),
  "cv_r2_rho_gradFrac": cv_r2(Xc.loc[m_rho], rho_t[m_rho].values),
  "note": "structural decomposition; NOT an extraction target. curlFrac/rho mechanically tied to omegaNorm2 components. 142 rho rows are 'n/a(commuting)'=no-curl.",
}

print(json.dumps(out, indent=2, default=str))
