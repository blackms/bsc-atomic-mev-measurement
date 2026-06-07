// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_curl_test.go unit-proves the math core of the ordering-curl engine
// (SIMENGINE_DRYRUN=curl) WITHOUT a full block replay: it builds synthetic
// antisymmetric Omega 2-forms with KNOWN structure and asserts the Hodge
// decomposition's invariants — the three GATE-1/GATE-2 properties the live
// experiment relies on:
//
//   - a PURE-GRADIENT Omega (built as grad of a known potential = a COMMUTING /
//     separable value where C(i,j)=psi_j-psi_i) decomposes with rho == 1 and
//     curlFrac == 0 (the near-potential / commuting limit);
//   - a PURE-CURL Omega (a balanced 3-cycle = a NON-commuting value) decomposes
//     with rho == 0 and curlFrac == 1 (the discretionary / path-dependent limit);
//   - the ORTHOGONALITY IDENTITY ||Omega||^2 == ||grad psi||^2 + ||curl||^2 holds
//     (residual < 1e-9 relative) on RANDOM antisymmetric Omega of varying size,
//     and the harmonic fraction is ~0 on the filled clique.
//
// These exercise antisym + hodgeDecompose directly, so they are independent of the
// EVM/state machinery (which SimulateOnState already validates 5/5). They are the
// load-bearing GATE-1 correctness proof: if the decomposition were wrong these
// would fail and the live rho would be untrustworthy.
package simengine

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
)

// buildGradOmega builds the antisymmetric 2-form Omega_ij = psi_j - psi_i from a
// potential. This is a PURE GRADIENT (curl-free) flow: the canonical COMMUTING /
// separable value where the realized value of an ordering is a sum of per-tx
// potentials, so V(..i.j..)-V(..j.i..) depends only on the potentials.
func buildGradOmega(psi []float64) *antisym {
	k := len(psi)
	a := newAntisym(k)
	for i := 0; i < k; i++ {
		for j := i + 1; j < k; j++ {
			// Omega_ij = psi_j - psi_i (scaled to wei-like integers so set() round-trips
			// the same float we assert on).
			v := big.NewInt(int64(math.Round((psi[j] - psi[i]) * 1e6)))
			a.set(i, j, v)
		}
	}
	return a
}

// TestHodgePureGradientIsPotential proves: a pure-gradient Omega decomposes with
// rho == 1 and curlFrac == 0 (the COMMUTING / near-potential limit). This is the
// "curl=0 => rho=1" synthetic cluster the spec requires.
func TestHodgePureGradientIsPotential(t *testing.T) {
	for _, psi := range [][]float64{
		{1, 2, 3},          // k=3
		{0, 5, -2, 7},      // k=4
		{3, -1, 4, 1, -5},  // k=5
		{2, 7, 1, 8, 2, 8}, // k=6
	} {
		omega := buildGradOmega(psi)
		dec := hodgeDecompose(omega)
		rho := ratioF(dec.gradNorm2f, dec.omegaNorm2f)
		curlFrac := ratioF(dec.curlNorm2f, dec.omegaNorm2f)
		if math.Abs(rho-1.0) > 1e-9 {
			t.Fatalf("k=%d pure-gradient: rho=%.12f, want 1.0", len(psi), rho)
		}
		if curlFrac > 1e-9 {
			t.Fatalf("k=%d pure-gradient: curlFrac=%.12f, want 0", len(psi), curlFrac)
		}
		// Orthogonality identity.
		if got := dec.orthoResidualFrac(dec.omegaNorm2); got > 1e-9 {
			t.Fatalf("k=%d pure-gradient: ortho residual=%.3e > 1e-9", len(psi), got)
		}
	}
}

// build3Cycle builds a balanced antisymmetric 3-cycle: Omega_01 = Omega_12 =
// Omega_20 = w (and antisymmetric). This is a PURE CURL (divergence-free) flow:
// every node's divergence is 0, so the least-squares potential is 0 and the whole
// energy is curl. It is the canonical NON-commuting value (a rock-paper-scissors
// ordering preference) where reordering strictly changes realized value cyclically.
func build3Cycle(w int64) *antisym {
	a := newAntisym(3)
	a.set(0, 1, big.NewInt(w))
	a.set(1, 2, big.NewInt(w))
	a.set(2, 0, big.NewInt(w)) // i.e. Omega_02 = -w
	return a
}

// TestHodgePureCurlIsResidual proves: a balanced 3-cycle decomposes with rho == 0
// and curlFrac == 1 (the NON-commuting / path-dependent limit). This is the
// "curl>0" synthetic cluster the spec requires.
func TestHodgePureCurlIsResidual(t *testing.T) {
	omega := build3Cycle(1_000_000)
	dec := hodgeDecompose(omega)
	rho := ratioF(dec.gradNorm2f, dec.omegaNorm2f)
	curlFrac := ratioF(dec.curlNorm2f, dec.omegaNorm2f)
	if rho > 1e-9 {
		t.Fatalf("pure 3-cycle curl: rho=%.12f, want 0", rho)
	}
	if math.Abs(curlFrac-1.0) > 1e-9 {
		t.Fatalf("pure 3-cycle curl: curlFrac=%.12f, want 1.0", curlFrac)
	}
	// div should be ~0 at every node (divergence-free).
	for i, d := range omega.div() {
		if math.Abs(d) > 1e-6 {
			t.Fatalf("3-cycle div[%d]=%.6f, want 0 (divergence-free)", i, d)
		}
	}
	if got := dec.orthoResidualFrac(dec.omegaNorm2); got > 1e-9 {
		t.Fatalf("pure 3-cycle curl: ortho residual=%.3e > 1e-9", got)
	}
}

// TestHodgeGradPlusCurlMix proves a mixed flow (gradient + 3-cycle) splits with
// 0 < rho < 1 and rho + curlFrac == 1 exactly (energy conservation). Confirms the
// engine reports a meaningful intermediate rho, not just the two limits.
func TestHodgeGradPlusCurlMix(t *testing.T) {
	grad := buildGradOmega([]float64{1, 4, 2}) // k=3 pure gradient
	cyc := build3Cycle(500_000)                // k=3 pure curl
	mix := newAntisym(3)
	for i := 0; i < 3; i++ {
		for j := i + 1; j < 3; j++ {
			mix.u[i][j] = grad.u[i][j] + cyc.u[i][j]
		}
	}
	dec := hodgeDecompose(mix)
	rho := ratioF(dec.gradNorm2f, dec.omegaNorm2f)
	curlFrac := ratioF(dec.curlNorm2f, dec.omegaNorm2f)
	if rho <= 1e-6 || rho >= 1-1e-6 {
		t.Fatalf("mixed flow: rho=%.6f, want strictly in (0,1)", rho)
	}
	if math.Abs(rho+curlFrac-1.0) > 1e-9 {
		t.Fatalf("mixed flow: rho+curlFrac=%.12f, want 1.0 (energy conservation)", rho+curlFrac)
	}
}

// TestHodgeOrthogonalityRandom proves the orthogonality identity ||Omega||^2 ==
// ||grad psi||^2 + ||curl||^2 (residual < 1e-9 relative) on RANDOM antisymmetric
// Omega across k=2..8. This is the core GATE-1 correctness assertion: if it fails
// the decomposition is wrong. It also confirms the harmonic fraction is ~0 on the
// filled clique 2-complex (H^1=0) — implicit in the residual being ~0 (any harmonic
// component would show up as an unaccounted residual).
func TestHodgeOrthogonalityRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for k := 2; k <= 8; k++ {
		for trial := 0; trial < 200; trial++ {
			a := newAntisym(k)
			for i := 0; i < k; i++ {
				for j := i + 1; j < k; j++ {
					// Wide range incl. negatives, integer wei-scale.
					v := big.NewInt(int64(rng.Intn(20_000_001) - 10_000_000))
					a.set(i, j, v)
				}
			}
			dec := hodgeDecompose(a)
			on2 := a.norm2f()
			if on2 == 0 {
				continue
			}
			resid := math.Abs(on2-dec.gradNorm2f-dec.curlNorm2f) / on2
			if resid > 1e-9 {
				t.Fatalf("k=%d trial=%d: orthogonality residual=%.3e > 1e-9 (decomposition wrong)", k, trial, resid)
			}
			// rho must be a valid fraction in [0,1].
			rho := ratioF(dec.gradNorm2f, on2)
			if rho < -1e-9 || rho > 1+1e-9 {
				t.Fatalf("k=%d trial=%d: rho=%.6f out of [0,1]", k, trial, rho)
			}
			// gauge: sum psi == 0.
			var s float64
			for _, p := range dec.psi {
				s += p
			}
			if math.Abs(s) > 1e-6*(1+math.Abs(on2)) {
				t.Fatalf("k=%d trial=%d: sum psi=%.6e, want 0 (gauge)", k, trial, s)
			}
		}
	}
}

// TestHodgeNormalEquation proves the solved psi satisfies the graph-Laplacian
// normal equation L0 psi = -div(Omega) with L0 = k*I - J under the sum-zero gauge
// (the co-boundary sign matching grad_ij = psi_j - psi_i), confirming the
// closed-form psi = -div/k is the genuine least-squares potential and not an
// ad-hoc shortcut. Checked as (k*psi - sum(psi)) == -div within tolerance.
func TestHodgeNormalEquation(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for k := 3; k <= 7; k++ {
		a := newAntisym(k)
		for i := 0; i < k; i++ {
			for j := i + 1; j < k; j++ {
				a.set(i, j, big.NewInt(int64(rng.Intn(2_000_001)-1_000_000)))
			}
		}
		dec := hodgeDecompose(a)
		d := a.div()
		var sumPsi float64
		for _, p := range dec.psi {
			sumPsi += p
		}
		for i := 0; i < k; i++ {
			// (L0 psi)_i = k*psi_i - sum(psi); the normal equation is L0 psi = -div.
			lhs := float64(k)*dec.psi[i] - sumPsi
			if math.Abs(lhs-(-d[i])) > 1e-3 {
				t.Fatalf("k=%d node %d: (L0 psi)_i=%.6f != -div_i=%.6f", k, i, lhs, -d[i])
			}
		}
	}
}

// ratioF is a local test helper (the engine uses ratioBig over big.Ints for the
// log; the tests assert on the float energies directly).
func ratioF(num, den float64) float64 {
	if den <= 0 {
		return 0
	}
	return num / den
}

// TestAntisymExactlyAntisymmetric confirms the container enforces exact
// antisymmetry: get(i,j) == -get(j,i) and get(i,i) == 0, regardless of set order.
func TestAntisymExactlyAntisymmetric(t *testing.T) {
	a := newAntisym(4)
	a.set(0, 2, big.NewInt(123))
	a.set(3, 1, big.NewInt(-77)) // set with i>j: stores Omega_13 = +77
	if a.get(0, 2) != 123 || a.get(2, 0) != -123 {
		t.Fatalf("antisym (0,2): got %.0f / %.0f, want 123 / -123", a.get(0, 2), a.get(2, 0))
	}
	if a.get(1, 3) != 77 || a.get(3, 1) != -77 {
		t.Fatalf("antisym (1,3): got %.0f / %.0f, want 77 / -77", a.get(1, 3), a.get(3, 1))
	}
	for i := 0; i < 4; i++ {
		if a.get(i, i) != 0 {
			t.Fatalf("diagonal get(%d,%d)=%.0f, want 0", i, i, a.get(i, i))
		}
	}
}

// TestCurlEngineHelpers exercises the small ordering helpers the live engine relies
// on: orderKey uniqueness, rotate, scalarRankToOrder round-trip, and the
// exhaustive permutation count.
func TestCurlEngineHelpers(t *testing.T) {
	// orderKey: distinct orderings -> distinct keys; same -> same.
	if orderKey([]int{0, 1, 2}) == orderKey([]int{0, 2, 1}) {
		t.Fatal("orderKey collided on distinct orderings")
	}
	if orderKey([]int{2, 0, 1}) != orderKey([]int{2, 0, 1}) {
		t.Fatal("orderKey unstable on identical orderings")
	}
	// rotate.
	got := rotate([]int{0, 1, 2, 3}, 1)
	want := []int{1, 2, 3, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rotate: got %v, want %v", got, want)
		}
	}
	// scalarRankToOrder inverts a rank permutation.
	rank := []int{2, 0, 1} // pos0->rank2, pos1->rank0, pos2->rank1
	order := scalarRankToOrder(rank)
	wantOrder := []int{1, 2, 0} // rank0=pos1, rank1=pos2, rank2=pos0
	for i := range wantOrder {
		if order[i] != wantOrder[i] {
			t.Fatalf("scalarRankToOrder: got %v, want %v", order, wantOrder)
		}
	}
	// exhaustiveValueSpread: a commuting (constant) V => zero spread, k! perms.
	constV := func(_ []int) *big.Int { return big.NewInt(42) }
	vmin, vmax, perms := exhaustiveValueSpread(constV, 4)
	if perms != 24 {
		t.Fatalf("exhaustive perms=%d, want 24 (4!)", perms)
	}
	if vmin.Cmp(vmax) != 0 || vmin.Int64() != 42 {
		t.Fatalf("constant V spread: min=%s max=%s, want both 42", vmin, vmax)
	}
	// A V that depends on the last element => non-zero spread.
	lastV := func(o []int) *big.Int { return big.NewInt(int64(o[len(o)-1]) * 1000) }
	vmin2, vmax2, _ := exhaustiveValueSpread(lastV, 3)
	if vmin2.Int64() != 0 || vmax2.Int64() != 2000 {
		t.Fatalf("last-element V spread: min=%s max=%s, want 0 / 2000", vmin2, vmax2)
	}
}

// TestFracHistQuantiles confirms the exact-quantile accumulator's median/p10/p90.
func TestFracHistQuantiles(t *testing.T) {
	h := newFracHist()
	for i := 0; i <= 100; i++ {
		h.add(float64(i) / 100.0) // 0.00, 0.01, ..., 1.00
	}
	med, p10, p90, n := h.summary()
	if n != 101 {
		t.Fatalf("n=%d, want 101", n)
	}
	if math.Abs(med-0.50) > 0.011 {
		t.Fatalf("median=%.4f, want ~0.50", med)
	}
	if math.Abs(p10-0.10) > 0.011 {
		t.Fatalf("p10=%.4f, want ~0.10", p10)
	}
	if math.Abs(p90-0.90) > 0.011 {
		t.Fatalf("p90=%.4f, want ~0.90", p90)
	}
}
