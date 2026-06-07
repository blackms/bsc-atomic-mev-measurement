// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// distribution_test.go unit-proves the GrossDist accumulator used by the
// intra-block detector to characterise the gross-positive population. It feeds
// KNOWN gross/gasUnits samples and asserts: (a) the breakeven gas-price
// computation (grossWei/gasUnits in gwei) is exact; (b) the gas-sensitivity sweep
// counts (breakevenGwei > g) are correct; (c) the grossUSD and breakeven-gwei
// percentiles fall within their log-scale bucket tolerance; (d) the per-dexMix /
// per-cycle-length breakdowns are right; (e) the CSV gas-price sweep parser.
package strategy

import (
	"math"
	"math/big"
	"testing"
)

// gweiToWei converts a gwei value to a *big.Int wei amount. All test breakevens
// are multiples of 0.01 gwei = 1e7 wei, so this is exact with no float rounding.
func gweiToWei(gwei float64) *big.Int {
	hundredths := int64(math.Round(gwei * 100)) // 0.01 gwei units
	return new(big.Int).Mul(big.NewInt(hundredths), big.NewInt(10_000_000))
}

// approx asserts a and b agree within a relative tolerance (or absolute when both
// are tiny).
func approx(a, b, relTol float64) bool {
	if a == b {
		return true
	}
	d := math.Abs(a - b)
	m := math.Max(math.Abs(a), math.Abs(b))
	if m < 1e-12 {
		return d < 1e-12
	}
	return d/m <= relTol
}

// TestBreakevenGwei pins the breakeven gas-price computation: gross/gasUnits in
// gwei. With gasUnits=200k, a gross of (200k*3) gwei-wei must yield exactly 3 gwei.
func TestBreakevenGwei(t *testing.T) {
	const gasUnits = 200_000

	cases := []struct {
		gross *big.Int
		want  float64 // gwei
	}{
		{new(big.Int).Mul(big.NewInt(gasUnits), gweiToWei(3)), 3.0},
		{new(big.Int).Mul(big.NewInt(gasUnits), gweiToWei(0.3)), 0.3},
		{new(big.Int).Mul(big.NewInt(gasUnits), gweiToWei(1)), 1.0},
		{big.NewInt(0), 0.0},
		{big.NewInt(-5), 0.0},
	}
	for i, c := range cases {
		got := BreakevenGwei(c.gross, gasUnits)
		if !approx(got, c.want, 1e-9) {
			t.Fatalf("case %d: BreakevenGwei=%g want %g", i, got, c.want)
		}
	}
	// gasUnits=0 must be guarded (no division by zero).
	if got := BreakevenGwei(big.NewInt(1), 0); got != 0 {
		t.Fatalf("gasUnits=0 must yield 0, got %g", got)
	}
}

// TestGrossDistSweepAndBreakeven feeds a known mix of cycles and asserts the
// gas-sensitivity sweep counts. Each cycle has gasUnits=200k; we set its gross so
// its breakeven gwei is an exact known value, then check how many exceed each
// sweep threshold (strict >).
func TestGrossDistSweepAndBreakeven(t *testing.T) {
	const gasUnits = 200_000
	sweep := []float64{0, 0.1, 0.3, 1, 3}
	d := NewGrossDist(sweep)

	// breakeven gwei values for the samples (USD price 1:1 with wei-as-USD here is
	// irrelevant to the sweep; we pass arbitrary grossUSD).
	breakevens := []float64{0.05, 0.2, 0.5, 2, 5}
	for _, be := range breakevens {
		gross := new(big.Int).Mul(big.NewInt(gasUnits), gweiToWei(be))
		d.Add(gross, gasUnits, 0 /*usd*/, "pancake_v3", 3)
	}

	s := d.Snapshot()
	if s.Count != uint64(len(breakevens)) {
		t.Fatalf("count=%d want %d", s.Count, len(breakevens))
	}

	// Expected counts of breakeven > g:
	//   g=0   : all 5 (0.05,0.2,0.5,2,5 all > 0)
	//   g=0.1 : 4     (0.2,0.5,2,5)
	//   g=0.3 : 3     (0.5,2,5)
	//   g=1   : 2     (2,5)
	//   g=3   : 1     (5)
	wantCount := []uint64{5, 4, 3, 2, 1}
	for i := range sweep {
		if s.SweepCount[i] != wantCount[i] {
			t.Fatalf("sweep g=%g count=%d want %d", sweep[i], s.SweepCount[i], wantCount[i])
		}
		wantPct := 100 * float64(wantCount[i]) / float64(len(breakevens))
		if !approx(s.SweepPct[i], wantPct, 1e-9) {
			t.Fatalf("sweep g=%g pct=%g want %g", sweep[i], s.SweepPct[i], wantPct)
		}
	}

	// Breakeven max must be the exact running max (5 gwei).
	if !approx(s.BreakevenGweiMax, 5.0, 1e-9) {
		t.Fatalf("breakeven max=%g want 5", s.BreakevenGweiMax)
	}
	// Breakeven p50 (median of {0.05,0.2,0.5,2,5}) is 0.5; the reported bucket low
	// edge must bracket it: lowEdge <= 0.5 < lowEdge*10^step.
	assertBracketsValue(t, "breakevenGwei p50", s.BreakevenGweiP50, 0.5, beLogStep)
	// p90 (nearest-rank ceil(0.9*5)=5th) is the largest sample, 5.
	assertBracketsValue(t, "breakevenGwei p90", s.BreakevenGweiP90, 5.0, beLogStep)
}

// TestGrossDistUSDPercentiles asserts the grossUSD percentiles fall in the bucket
// containing the true value, and that max is exact.
func TestGrossDistUSDPercentiles(t *testing.T) {
	d := NewGrossDist(nil) // default sweep

	// 100 samples with grossUSD = 0.01 * i for i in 1..100 -> [$0.01 .. $1.00].
	// p50 ~= $0.50, p90 ~= $0.90, p99 ~= $0.99, max = $1.00.
	for i := 1; i <= 100; i++ {
		usd := 0.01 * float64(i)
		// gross wei here is arbitrary-positive (the USD value is passed explicitly).
		d.Add(big.NewInt(int64(i)), 200_000, usd, "pancake_v2+pancake_v3", 2)
	}
	s := d.Snapshot()
	if s.Count != 100 {
		t.Fatalf("count=%d want 100", s.Count)
	}
	if !approx(s.GrossUSDMax, 1.0, 1e-9) {
		t.Fatalf("grossUSD max=%g want 1.0", s.GrossUSDMax)
	}
	assertBracketsValue(t, "grossUSD p50", s.GrossUSDp50, 0.50, usdLogStep)
	assertBracketsValue(t, "grossUSD p90", s.GrossUSDp90, 0.90, usdLogStep)
	assertBracketsValue(t, "grossUSD p99", s.GrossUSDp99, 0.99, usdLogStep)

	// Breakdown checks.
	if s.ByDexMix["pancake_v2+pancake_v3"] != 100 {
		t.Fatalf("byDexMix V2xV3=%d want 100", s.ByDexMix["pancake_v2+pancake_v3"])
	}
	if s.ByLen[2] != 100 {
		t.Fatalf("byLen[2]=%d want 100", s.ByLen[2])
	}
}

// assertBracketsValue asserts a reported log-scale percentile (a bucket LOW edge)
// brackets the true value v: lowEdge <= v < lowEdge * 10^step. This is the bucket
// tolerance the histogram guarantees.
func assertBracketsValue(t *testing.T, name string, reported, v, logStep float64) {
	t.Helper()
	hi := reported * math.Pow(10, logStep)
	// Allow a tiny epsilon at the edges for float rounding.
	if reported > v*(1+1e-9) || v >= hi*(1+1e-9) {
		t.Fatalf("%s: reported bucket low=%g does not bracket true value %g (bucket hi=%g)",
			name, reported, v, hi)
	}
}

// TestGrossDistBreakdownMix checks per-dexMix and per-len counters accumulate
// independently across heterogeneous samples.
func TestGrossDistBreakdownMix(t *testing.T) {
	d := NewGrossDist([]float64{0})
	g := big.NewInt(1_000_000)
	d.Add(g, 200_000, 1, "pancake_v3+pancake_v3", 2)
	d.Add(g, 200_000, 1, "pancake_v3+pancake_v3", 2)
	d.Add(g, 300_000, 1, "biswap_v2+pancake_v2", 3)
	d.Add(g, 400_000, 1, "biswap_v2+pancake_v2+pancake_v3", 4)

	s := d.Snapshot()
	if s.ByDexMix["pancake_v3+pancake_v3"] != 2 {
		t.Fatalf("V3xV3=%d want 2", s.ByDexMix["pancake_v3+pancake_v3"])
	}
	if s.ByDexMix["biswap_v2+pancake_v2"] != 1 || s.ByDexMix["biswap_v2+pancake_v2+pancake_v3"] != 1 {
		t.Fatalf("dexmix breakdown wrong: %v", s.ByDexMix)
	}
	if s.ByLen[2] != 2 || s.ByLen[3] != 1 || s.ByLen[4] != 1 {
		t.Fatalf("byLen breakdown wrong: %v", s.ByLen)
	}
	// Deterministic rendering must be sorted.
	if got := s.LenString(); got != "2hop:2 3hop:1 4hop:1" {
		t.Fatalf("LenString=%q", got)
	}
}

// TestParseGasPriceSweepGwei pins the env CSV parser.
func TestParseGasPriceSweepGwei(t *testing.T) {
	// Default on blank.
	def := ParseGasPriceSweepGwei("")
	if len(def) != 5 || def[0] != 0 || def[4] != 3 {
		t.Fatalf("default sweep wrong: %v", def)
	}
	// Custom CSV, with whitespace and a malformed token dropped.
	got := ParseGasPriceSweepGwei(" 0, 0.5 , 2, junk, -1 ,7 ")
	want := []float64{0, 0.5, 2, 7}
	if len(got) != len(want) {
		t.Fatalf("parsed %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parsed[%d]=%g want %g", i, got[i], want[i])
		}
	}
	// All-malformed falls back to default.
	if fb := ParseGasPriceSweepGwei("junk,,nan"); len(fb) != 5 {
		t.Fatalf("all-malformed must fall back to default, got %v", fb)
	}
}

// TestGrossDistIgnoresNonPositive guards the single mutation point against
// non-positive gross (defensive; the caller only adds gross>0 but the type must
// not corrupt its counters).
func TestGrossDistIgnoresNonPositive(t *testing.T) {
	d := NewGrossDist(nil)
	d.Add(big.NewInt(0), 200_000, 5, "x", 2)
	d.Add(big.NewInt(-1), 200_000, 5, "x", 2)
	d.Add(nil, 200_000, 5, "x", 2)
	if s := d.Snapshot(); s.Count != 0 {
		t.Fatalf("non-positive gross must be ignored, count=%d", s.Count)
	}
}
