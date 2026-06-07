// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// distribution.go is the O(1)-memory characterisation accumulator for the v4 EVM
// oracle's GROSS-POSITIVE cycles. The headline v4 result is that ~0.87 ground-
// truth gross-positive cross-venue cycles exist per sampled block but ZERO clear
// the ~$0.50 gas threshold (280k gas * 3 gwei). This file answers the natural
// follow-up — "how far below gas are they?" — by accumulating, for every cycle
// whose ground-truth gross profit is positive (even when net <= 0):
//
//   - the gross profit in USD (grossWei * liveWbnbPriceUSD), into fixed log-scale
//     buckets so percentiles (p50/p90/p99/max) are recovered with bucket-edge
//     tolerance in O(1) memory (no per-sample reservoir);
//   - the BREAK-EVEN gas price (gwei) = grossWei / gasUnits, i.e. the gas price at
//     which net would be exactly zero, into its own log-scale buckets (p50/p90/max);
//   - a GAS-SENSITIVITY SWEEP: how many gross-positive cycles WOULD be net-positive
//     at each gas price in a configurable sweep (default {0,0.1,0.3,1,3} gwei),
//     counted as breakevenGwei > sweepGwei (bid=margin=0 for the sweep);
//   - per-dexMix and per-cycle-length (#hops) counters for the structural breakdown.
//
// The type is concurrency-safe (one mutex) and pure: it takes the already-computed
// grossWei, gasUnits, USD price, dexMix label and hop count and does only integer/
// float bookkeeping. It is strictly read-only w.r.t. the chain. Memory is O(1) in
// the number of samples (bounded by the number of buckets + distinct dexMix/len
// keys, which are tiny for the 12-pool watch set).
package strategy

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// gwei is 1e9 wei.
var gweiWei = big.NewInt(1_000_000_000)

// usdBucketCount / breakevenBucketCount are the number of log-scale buckets. The
// grossUSD buckets span a wide dynamic range (sub-cent dust up to large arbs);
// the breakeven-gwei buckets span sub-milli-gwei up to far above the 3 gwei
// configured price. Both are fixed so memory is O(1).
const (
	// grossUSD buckets: bucket i covers [usdLo*10^(i*usdDecadeStep), next). With
	// usdLo=1e-6 ($0.000001) and a 0.25-decade step over 64 buckets the top edge is
	// ~1e10, comfortably above any realistic single-cycle gross.
	usdBuckets     = 64
	usdLogLo       = -6.0  // log10($1e-6) lower edge
	usdLogStep     = 0.25  // decade fraction per bucket
	usdSampleScale = 1e-12 // sub-bucket "dust below lowest edge" floor in USD

	// breakeven-gwei buckets: bucket i covers gwei in [beLo*10^(i*beStep), next).
	// beLo=1e-6 gwei lower edge, 0.25-decade step over 64 buckets -> top ~1e10 gwei.
	beBuckets = 64
	beLogLo   = -6.0
	beLogStep = 0.25
)

// defaultGasPriceSweepGwei is the default gas-price sweep (gwei). The detector's
// configured price is 3 gwei; the sweep asks how the gross-positive population's
// profitability would change at lower (and zero) gas prices.
var defaultGasPriceSweepGwei = []float64{0, 0.1, 0.3, 1, 3}

// GrossDist is the O(1)-memory distribution accumulator for gross-positive
// cycles. All fields are guarded by mu; the zero value is NOT ready — use
// NewGrossDist.
type GrossDist struct {
	mu sync.Mutex

	count uint64 // number of gross-positive samples recorded

	usdHist [usdBuckets]uint64 // log-scale histogram of grossUSD
	beHist  [beBuckets]uint64  // log-scale histogram of breakeven gas price (gwei)

	usdMax float64 // running max grossUSD (exact, not bucketed)
	beMax  float64 // running max breakeven gwei (exact, not bucketed)

	byDexMix map[string]uint64 // gross-positive count per dexMix label
	byLen    map[int]uint64    // gross-positive count per cycle length (#hops)

	// sweepGwei is the gas-price sweep in gwei; sweepCount[i] is the number of
	// gross-positive cycles whose breakeven gwei STRICTLY EXCEEDS sweepGwei[i]
	// (i.e. would be net-positive at that gas price with bid=margin=0).
	sweepGwei  []float64
	sweepCount []uint64
}

// NewGrossDist builds an accumulator with the given gas-price sweep (gwei). A nil
// or empty sweep falls back to the default {0,0.1,0.3,1,3} gwei.
func NewGrossDist(sweepGwei []float64) *GrossDist {
	sw := sweepGwei
	if len(sw) == 0 {
		sw = append([]float64(nil), defaultGasPriceSweepGwei...)
	} else {
		sw = append([]float64(nil), sw...)
	}
	return &GrossDist{
		byDexMix:   make(map[string]uint64),
		byLen:      make(map[int]uint64),
		sweepGwei:  sw,
		sweepCount: make([]uint64, len(sw)),
	}
}

// ParseGasPriceSweepGwei parses a CSV of gwei values (e.g. "0,0.1,0.3,1,3") into
// a float slice, dropping malformed/negative entries. An empty/blank string
// returns the default sweep. Used to honour env SIMENGINE_DRYRUN_GASPRICES.
func ParseGasPriceSweepGwei(csv string) []float64 {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return append([]float64(nil), defaultGasPriceSweepGwei...)
	}
	var out []float64
	for _, tok := range strings.Split(csv, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		v, err := strconv.ParseFloat(tok, 64)
		if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return append([]float64(nil), defaultGasPriceSweepGwei...)
	}
	return out
}

// BreakevenGwei returns the break-even gas price in gwei for a gross-positive
// cycle: breakevenWei = grossWei / gasUnits, expressed in gwei (1 gwei = 1e9 wei).
// It returns 0 when gasUnits is 0 or grossWei is non-positive. The computation is
// done as a big.Rat so very small/large values keep precision before the final
// float conversion.
func BreakevenGwei(grossWei *big.Int, gasUnits uint64) float64 {
	if grossWei == nil || grossWei.Sign() <= 0 || gasUnits == 0 {
		return 0
	}
	// breakevenWei = grossWei / gasUnits ; gwei = breakevenWei / 1e9.
	// gwei = grossWei / (gasUnits * 1e9).
	den := new(big.Int).Mul(new(big.Int).SetUint64(gasUnits), gweiWei)
	r := new(big.Rat).SetFrac(grossWei, den)
	f, _ := r.Float64()
	return f
}

// Add records one gross-positive cycle. grossWei is the (positive) gross profit
// in the start token (WBNB wei); gasUnits is CycleGasUnits(cycle); grossUSD is
// grossWei*liveWbnbPriceUSD already computed by the caller; dexMix and cycleLen
// are the structural labels. Non-positive grossWei is ignored (defensive). This
// is the single mutation point, called from the intra-block valuation path right
// where gross>0 is determined.
func (d *GrossDist) Add(grossWei *big.Int, gasUnits uint64, grossUSD float64, dexMix string, cycleLen int) {
	if d == nil || grossWei == nil || grossWei.Sign() <= 0 {
		return
	}
	be := BreakevenGwei(grossWei, gasUnits)

	d.mu.Lock()
	defer d.mu.Unlock()

	d.count++
	d.usdHist[usdBucketIndex(grossUSD)]++
	d.beHist[beBucketIndex(be)]++
	if grossUSD > d.usdMax {
		d.usdMax = grossUSD
	}
	if be > d.beMax {
		d.beMax = be
	}
	d.byDexMix[dexMix]++
	d.byLen[cycleLen]++

	// Gas-sensitivity sweep: a cycle is net-positive at sweep price g (bid=margin=0)
	// iff gross > gasUnits*g, i.e. breakevenGwei > g. Use strict > so the threshold
	// gas price (where net == 0) is NOT counted as profitable.
	for i, g := range d.sweepGwei {
		if be > g {
			d.sweepCount[i]++
		}
	}
}

// usdBucketIndex maps a grossUSD value to its log-scale bucket index, clamped to
// [0, usdBuckets-1]. Non-positive values fall in bucket 0 (the dust floor).
func usdBucketIndex(usd float64) int {
	if usd <= 0 || math.IsNaN(usd) {
		return 0
	}
	idx := int(math.Floor((math.Log10(usd) - usdLogLo) / usdLogStep))
	if idx < 0 {
		return 0
	}
	if idx >= usdBuckets {
		return usdBuckets - 1
	}
	return idx
}

// usdBucketLowEdge returns the lower edge (USD) of bucket i.
func usdBucketLowEdge(i int) float64 {
	return math.Pow(10, usdLogLo+float64(i)*usdLogStep)
}

// beBucketIndex maps a breakeven-gwei value to its log-scale bucket index.
func beBucketIndex(gwei float64) int {
	if gwei <= 0 || math.IsNaN(gwei) {
		return 0
	}
	idx := int(math.Floor((math.Log10(gwei) - beLogLo) / beLogStep))
	if idx < 0 {
		return 0
	}
	if idx >= beBuckets {
		return beBuckets - 1
	}
	return idx
}

// beBucketLowEdge returns the lower edge (gwei) of bucket i.
func beBucketLowEdge(i int) float64 {
	return math.Pow(10, beLogLo+float64(i)*beLogStep)
}

// percentileFromHist returns the lower edge of the bucket containing the p-th
// percentile (p in [0,1]) of a log-scale histogram, given a per-bucket low-edge
// function. The reported value is the bucket's low edge, so the true percentile
// lies in [returned, returned*10^step). Returns 0 for an empty histogram. The
// "nearest-rank" rule selects the smallest bucket whose cumulative count reaches
// ceil(p*N).
func percentileFromHist(hist []uint64, total uint64, p float64, lowEdge func(int) float64) float64 {
	if total == 0 {
		return 0
	}
	if p <= 0 {
		// First non-empty bucket.
		for i := range hist {
			if hist[i] > 0 {
				return lowEdge(i)
			}
		}
		return 0
	}
	if p > 1 {
		p = 1
	}
	rank := uint64(math.Ceil(p * float64(total)))
	if rank == 0 {
		rank = 1
	}
	var cum uint64
	for i := range hist {
		cum += hist[i]
		if cum >= rank {
			return lowEdge(i)
		}
	}
	// Fallback: last non-empty bucket.
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i] > 0 {
			return lowEdge(i)
		}
	}
	return 0
}

// Snapshot is an immutable, marshalled view of the accumulator for logging.
type Snapshot struct {
	Count uint64

	GrossUSDp50 float64
	GrossUSDp90 float64
	GrossUSDp99 float64
	GrossUSDMax float64

	BreakevenGweiP50 float64
	BreakevenGweiP90 float64
	BreakevenGweiMax float64

	// Sweep[i] corresponds to SweepGwei[i]: count (and Pct) of gross-positive
	// cycles that would be net-positive at SweepGwei[i] gwei (breakevenGwei > g).
	SweepGwei  []float64
	SweepCount []uint64
	SweepPct   []float64

	ByDexMix map[string]uint64
	ByLen    map[int]uint64
}

// Snapshot atomically reads the accumulator and computes the reportable
// percentiles, sweep counts/percentages, and breakdowns. Cheap and lock-held
// only for the copy; the percentile math runs on the local copy.
func (d *GrossDist) Snapshot() Snapshot {
	d.mu.Lock()
	count := d.count
	usdHist := d.usdHist
	beHist := d.beHist
	usdMax := d.usdMax
	beMax := d.beMax
	sweepGwei := append([]float64(nil), d.sweepGwei...)
	sweepCount := append([]uint64(nil), d.sweepCount...)
	byDexMix := make(map[string]uint64, len(d.byDexMix))
	for k, v := range d.byDexMix {
		byDexMix[k] = v
	}
	byLen := make(map[int]uint64, len(d.byLen))
	for k, v := range d.byLen {
		byLen[k] = v
	}
	d.mu.Unlock()

	pct := make([]float64, len(sweepCount))
	for i, c := range sweepCount {
		if count > 0 {
			pct[i] = 100 * float64(c) / float64(count)
		}
	}

	return Snapshot{
		Count:            count,
		GrossUSDp50:      percentileFromHist(usdHist[:], count, 0.50, usdBucketLowEdge),
		GrossUSDp90:      percentileFromHist(usdHist[:], count, 0.90, usdBucketLowEdge),
		GrossUSDp99:      percentileFromHist(usdHist[:], count, 0.99, usdBucketLowEdge),
		GrossUSDMax:      usdMax,
		BreakevenGweiP50: percentileFromHist(beHist[:], count, 0.50, beBucketLowEdge),
		BreakevenGweiP90: percentileFromHist(beHist[:], count, 0.90, beBucketLowEdge),
		BreakevenGweiMax: beMax,
		SweepGwei:        sweepGwei,
		SweepCount:       sweepCount,
		SweepPct:         pct,
		ByDexMix:         byDexMix,
		ByLen:            byLen,
	}
}

// SweepString renders the gas-sensitivity sweep as a compact, deterministic
// "g=<gwei>:<count>(<pct>%)" list, e.g. "g=0:42(100.0%) g=0.1:5(11.9%) ...".
func (s Snapshot) SweepString() string {
	var b strings.Builder
	for i := range s.SweepGwei {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "g=%s:%d(%.1f%%)", trimFloat(s.SweepGwei[i]), s.SweepCount[i], s.SweepPct[i])
	}
	return b.String()
}

// DexMixString renders the per-dexMix breakdown sorted by label for determinism.
func (s Snapshot) DexMixString() string {
	keys := make([]string, 0, len(s.ByDexMix))
	for k := range s.ByDexMix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s:%d", k, s.ByDexMix[k])
	}
	return b.String()
}

// LenString renders the per-cycle-length breakdown sorted by length.
func (s Snapshot) LenString() string {
	keys := make([]int, 0, len(s.ByLen))
	for k := range s.ByLen {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%dhop:%d", k, s.ByLen[k])
	}
	return b.String()
}

// trimFloat formats a float without trailing zeros (e.g. 0.1, 3, 0.3).
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
