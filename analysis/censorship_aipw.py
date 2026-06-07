import warnings, json, sys
warnings.filterwarnings("ignore")
import numpy as np, pandas as pd
from sklearn.model_selection import StratifiedKFold, KFold, cross_val_predict
from sklearn.metrics import roc_auc_score, r2_score, mean_absolute_error
import xgboost as xgb
import shap

RNG = 42
np.random.seed(RNG)

df = pd.read_csv('./censorship_candidates.csv')
n = len(df)
n_drop = int((df['dropped']==1).sum()); n_inc = int((df['dropped']==0).sum())
print(f"n={n}  dropped(1)={n_drop}  included(0)={n_inc}  frac_dropped={df['dropped'].mean():.6f}")

# ---- feature engineering ----
# leadTimeSec is POST-treatment / leaky (it IS the delay that defines drop vs include).
# Keep it out of the legitimate propensity covariates; show its leakage separately.
cat_cols = ['dex','numeraire','token0Side','validator']  # 'pool' too high-cardinality (284) for n=7680, drop to avoid overfit; encode others
num_cols = ['grossBNBwei','poolReserveLog10','hops','gas','gasFeeCapWei','gasTipCapWei','blockFullness','ledgerSize']
leaky_col = 'leadTimeSec'

for c in cat_cols:
    df[c] = df[c].astype('category')

def make_X(cols):
    X = df[cols].copy()
    for c in cols:
        if str(df[c].dtype)=='category':
            X[c] = X[c].cat.codes
    return X

# ============================================================
# (a) PROPENSITY CLASSIFIER  e(X)=P(dropped|X)
# ============================================================
print("\n==== (a) PROPENSITY e(X) ====")
y = df['dropped'].values

def cv_auc(cols, label):
    X = make_X(cols).values
    # need >=2 classes per fold; with 7 positives use 5-fold stratified
    skf = StratifiedKFold(n_splits=5, shuffle=True, random_state=RNG)
    oof = np.full(n, np.nan)
    for tr, te in skf.split(X, y):
        clf = xgb.XGBClassifier(
            n_estimators=200, max_depth=3, learning_rate=0.05,
            subsample=0.8, colsample_bytree=0.8, reg_lambda=1.0,
            eval_metric='logloss', random_state=RNG, n_jobs=-1,
            scale_pos_weight=(y==1).sum()/max((y==0).sum(),1))
        clf.fit(X[tr], y[tr])
        oof[te] = clf.predict_proba(X[te])[:,1]
    try:
        auc = roc_auc_score(y, oof)
    except Exception as e:
        auc = float('nan')
    print(f"  [{label}] CV-OOF AUC = {auc:.4f}  (cols={cols})")
    return auc, oof

auc_clean, oof_clean = cv_auc(cat_cols+num_cols, "legitimate (no leadTime)")
auc_leaky, oof_leaky = cv_auc(cat_cols+num_cols+[leaky_col], "WITH leadTimeSec (leaky)")

# class-conditional separation of the leaky feature
print(f"  leadTimeSec  included mean={df.loc[df.dropped==0,leaky_col].mean():.3f}s  "
      f"dropped median={df.loc[df.dropped==1,leaky_col].median():.3f}s")

# SHAP on full-data classifier (legit features) for driver interpretation
Xc = make_X(cat_cols+num_cols)
clf_full = xgb.XGBClassifier(n_estimators=200,max_depth=3,learning_rate=0.05,
    subsample=0.8,colsample_bytree=0.8,reg_lambda=1.0,eval_metric='logloss',
    random_state=RNG,n_jobs=-1,scale_pos_weight=(y==1).sum()/max((y==0).sum(),1))
clf_full.fit(Xc.values, y)
expl = shap.TreeExplainer(clf_full)
sv = expl.shap_values(Xc.values)
shap_imp = np.abs(sv).mean(axis=0)
order = np.argsort(shap_imp)[::-1]
print("  SHAP drivers (drop-vs-include, legit features):")
clf_drivers=[]
for i in order[:8]:
    print(f"    {Xc.columns[i]:18s} mean|SHAP|={shap_imp[i]:.4f}")
    clf_drivers.append(Xc.columns[i])

# ============================================================
# (b) OUTCOME REGRESSOR  mu(X)=E[V_BNBwei|X]
# ============================================================
print("\n==== (b) OUTCOME mu(X)=E[V_BNBwei|X] ====")
# regress on log scale (V_BNBwei spans 5e13..1.7e19)
v = df['V_BNBwei'].values.astype(float)
logv = np.log(v)
Xr = make_X(cat_cols+num_cols)   # legit covariates only (no leakage, exclude grossBNBwei? it's near-identical to V)
# NOTE: grossBNBwei = V_BNBwei + gas-ish; it's almost the label -> exclude to avoid trivial leakage for mu(X)
reg_cols = cat_cols + ['poolReserveLog10','hops','gas','gasFeeCapWei','gasTipCapWei','blockFullness','ledgerSize']
Xr = make_X(reg_cols)
kf = KFold(n_splits=5, shuffle=True, random_state=RNG)
oof_log = np.full(n, np.nan)
for tr,te in kf.split(Xr.values):
    reg = xgb.XGBRegressor(n_estimators=400,max_depth=4,learning_rate=0.03,
        subsample=0.8,colsample_bytree=0.8,reg_lambda=1.0,random_state=RNG,n_jobs=-1)
    reg.fit(Xr.values[tr], logv[tr])
    oof_log[te] = reg.predict(Xr.values[te])
r2_log = r2_score(logv, oof_log)
# back-transform metric
oof_v = np.exp(oof_log)
r2_lvl = r2_score(v, oof_v)
mae_log = mean_absolute_error(logv, oof_log)
print(f"  CV-OOF R2 (log V_BNBwei) = {r2_log:.4f}")
print(f"  CV-OOF R2 (level V_BNBwei)= {r2_lvl:.4f}")
print(f"  CV-OOF MAE (log)         = {mae_log:.4f}")

# also report including grossBNBwei to show it's a trivial proxy
Xr2 = make_X(reg_cols+['grossBNBwei'])
oof_log2 = cross_val_predict(
    xgb.XGBRegressor(n_estimators=400,max_depth=4,learning_rate=0.03,subsample=0.8,
        colsample_bytree=0.8,reg_lambda=1.0,random_state=RNG,n_jobs=-1),
    Xr2.values, logv, cv=kf)
print(f"  (with grossBNBwei, near-label) CV R2 (log)= {r2_score(logv,oof_log2):.4f}  <- trivial proxy")

# ============================================================
# (c) AIPW doubly-robust D  (censorship differential)
# ============================================================
print("\n==== (c) AIPW doubly-robust D ====")
# Estimand framing: D = E[V | included] - E[V | dropped] style censorship-differential,
# using EXACT V (Invariant E). Treatment T = dropped (1) ; control = included (0).
# AIPW (ATE-style) influence-function estimate using OOF nuisances.
# mu1/mu0 fit per-arm would need both arms; with n0=7 we CANNOT fit a control outcome model.
# We therefore use a single mu(X) (pooled) and OOF propensity, and report POSITIVITY failure.

e = np.clip(oof_clean, 1e-6, 1-1e-6)   # P(dropped)
T = y.astype(float)                     # 1=dropped
mu = oof_v                              # pooled outcome model E[V|X] on EXACT V scale (BNB wei)

# Naive / plug-in difference of group means (exact V):
naive_D = df.loc[df.dropped==0,'V_BNBwei'].mean() - df.loc[df.dropped==1,'V_BNBwei'].mean()

# AIPW for ATE = E[Y(drop)-Y(include)].  With one pooled mu we approximate the
# IPW-corrected differential.  Honest caveat: overlap fails (min e for included rows):
e_inc = e[T==0]; e_drop = e[T==1]
print(f"  propensity overlap: included rows e(drop) min={e_inc.min():.4f} max={e_inc.max():.4f}")
print(f"                      dropped  rows e(drop) min={e_drop.min():.4f} median={np.median(e_drop):.4f}")
# AIPW influence values (Y exact)
Y = v
psi1 = mu + T*(Y-mu)/e
psi0 = mu + (1-T)*(Y-mu)/(1-e)
if_D = psi1 - psi0            # AIPW pointwise contribution to D = E[Y|drop]-E[Y|inc]
aipw_D = if_D.mean()
se = if_D.std(ddof=1)/np.sqrt(n)
ci_lo, ci_hi = aipw_D-1.96*se, aipw_D+1.96*se
# convert wei->BNB for readability
W=1e18
print(f"  naive plug-in D (E[V|drop]-E[V|inc]) = {(-naive_D)/W:.4f} BNB   (group-mean diff)")
print(f"  AIPW D = {aipw_D/W:.4f} BNB   95% CI [{ci_lo/W:.4f}, {ci_hi/W:.4f}] BNB")
print(f"  AIPW D = {aipw_D:.4e} wei   SE={se:.4e} wei")

# Defensive: the realized-capture / censorship effect D-hat is ~0 by project finding.
# Report whether CI straddles 0 relative to scale.
straddles = (ci_lo<0<ci_hi)
print(f"  CI straddles 0: {straddles}")

result = dict(
  n=n, n_drop=n_drop, n_inc=n_inc,
  auc_clean=round(auc_clean,4), auc_leaky=round(auc_leaky,4),
  r2_log=round(r2_log,4), r2_level=round(r2_lvl,4),
  naive_D_BNB=round(-naive_D/W,4),
  aipw_D_BNB=round(aipw_D/W,4), ci_lo_BNB=round(ci_lo/W,4), ci_hi_BNB=round(ci_hi/W,4),
  clf_drivers=clf_drivers[:5], straddles_zero=bool(straddles)
)
print("\nJSON_RESULT="+json.dumps(result))
PY
