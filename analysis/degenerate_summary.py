#!/usr/bin/env python
"""Combine demo + diagnostic into one honest degenerate-demo summary."""
import json
demo = json.load(open("./demo_out.json"))
diag = json.load(open("./diag_out.json"))
out = {
    "rung": "degenerate-demo",
    "model_class": "degenerate-demo (largest MLP + XGBoost on 0-positive and 7-positive extraction targets)",
    "no_gpu": True,
    "targetA_captured": demo["targetA_captured"],
    "targetB_realCensored": demo["targetB_realCensored"],
    "targetB_diagnostic": diag,
    "headline": (
        "Capacity is irrelevant: (A) realized 'captured' has 0 positives -> no "
        "classifier can be fit, AUC undefined, oracle value 0 BNB. (B) 'real-censored' "
        "has 7/7680 positives (0.091%); a 577-param net ties XGBoost (AUC~0.96) and the "
        "0.8M-param net is worse (0.76) -> bigger does not help. The AUC is statistically "
        "above the block-permutation null (p=0) but is descriptive separation of a 7-row "
        "delayed-inclusion artifact (AUPR~0.22, bootstrap CI [0.92,1.00], 1-3 positives "
        "per fold) with 0 BNB realizable value; leadTimeSec leak inflates AUC to ~0.997."),
}
json.dump(out, open("./degenerate_summary.json", "w"), indent=2)
print(json.dumps(out, indent=2)[:1200])
print("\n... written to ./degenerate_summary.json")
