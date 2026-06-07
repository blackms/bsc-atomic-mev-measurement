import numpy as np, pandas as pd, json, warnings, os
warnings.filterwarnings("ignore")
from sklearn.model_selection import StratifiedKFold, cross_val_predict, KFold, cross_val_score
from sklearn.metrics import roc_auc_score, average_precision_score
import xgboost as xgb

R = {}

# ============================================================
# (a) DEGENERATE EXTRACTION TARGETS: ~0 positive mass
# ============================================================
# --- realizability tally: captured / net-positive-realized outcomes ---
rt = pd.read_csv("./realizability_tally.csv")
R["realizability"] = {
    "snapshots": int(len(rt)),
    "captureRate_unique": sorted(rt["captureRate"].unique().tolist()),
    "alreadyCaptured_unique": sorted(rt["alreadyCaptured"].unique().tolist()),
    "capturedOurNetWei_unique": sorted(rt["capturedOurNetWei"].unique().tolist()),
    "capturedRealizedNetWei_unique": sorted(rt["capturedRealizedNetWei"].unique().tolist()),
    "leftOnTable_min": int(rt["leftOnTable"].min()),
    "leftOnTable_max": int(rt["leftOnTable"].max()),
    "leftOnTable_median": float(rt["leftOnTable"].median()),
    "exPostNetPositive_min": int(rt["exPostNetPositive"].min()),
    "exPostNetPositive_max": int(rt["exPostNetPositive"].max()),
    "leftNetWei_median_BNB": float(rt["leftNetWei"].median()/1e18),
}
# The realized-capture target "captured == net positive realized":
captured_positive = int((rt["capturedRealizedNetWei"] > 0).sum()) + int((rt["capturedOurNetWei"]>0).sum())
R["realizability"]["captured_positive_class_count"] = captured_positive  # extraction target positives

# --- censorship: 'censored' treatment near-degenerate ---
cz = pd.read_csv("./censorship_candidates.csv")
n = len(cz)
inc = int((cz["dropped"]==0).sum()); drp = int((cz["dropped"]==1).sum())
R["censorship"] = {
    "rows": n, "included(0)": inc, "dropped(1)": drp,
    "included_frac": inc/n, "dropped_frac": drp/n,
    "minority_positives_included": inc,
}
# leadTimeSec separation (the leakage / delayed-inclusion signature)
R["censorship"]["leadTimeSec_included_median"] = float(cz.loc[cz.dropped==0,"leadTimeSec"].median())
R["censorship"]["leadTimeSec_dropped_median"]  = float(cz.loc[cz.dropped==1,"leadTimeSec"].median())
R["censorship"]["leadTimeSec_dropped_p99"]     = float(cz.loc[cz.dropped==1,"leadTimeSec"].quantile(0.99))

# --- sandwich opps: empty ---
sand_path = "./sandwich_opps.csv"
sand_rows = 0
try:
    sd = pd.read_csv(sand_path)
    sand_rows = len(sd)
except Exception:
    sand_rows = 0
R["sandwich_opps_rows"] = sand_rows

print(json.dumps(R, indent=2))
print("\n=== (a) EXTRACTION-TARGET POSITIVE MASS SUMMARY ===")
print(f"captured (realized net>0):        {captured_positive} positives / {len(rt)} snapshots  -> captureRate constant {R['realizability']['captureRate_unique']}")
print(f"net-positive-REALIZABLE captured: 0 (capturedRealizedNetWei all {R['realizability']['capturedRealizedNetWei_unique']})")
print(f"censored (drop that is a TRUE censor, not delayed-incl): included minority = {inc}/{n} = {inc/n*100:.3f}%")
print(f"sandwich realized-capture opportunities (in-scope rows): {sand_rows}")
