// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_curl_hodge.go is the PURE-MATH core of the ordering-curl engine: the
// antisymmetric 2-form container and the discrete Hodge decomposition on the
// complete graph K_k with all triangles filled (the clique 2-complex, on which
// H^1 = 0 so there is NO harmonic term). It has no node/EVM dependency and is
// exercised directly by dryrun_curl_test.go.
//
// THE DECOMPOSITION. Given an antisymmetric edge flow Omega_ij on the complete
// graph, we want Omega = grad(psi) + curl with:
//
//	grad(psi)_ij = psi_j - psi_i          (a gradient / potential flow)
//	curl = Omega - grad(psi)              (the divergence-free residual)
//
// The least-squares potential psi solves the graph-Laplacian normal equation
//
//	L0 psi = -div(Omega),    div(Omega)_i = sum_j Omega_ij
//
// (the -div / co-boundary sign that matches grad_ij = psi_j - psi_i: for a pure
// gradient, div_i = (sum psi) - k*psi_i, so L0 psi = -div recovers psi exactly).
// On the connected complete graph K_k, L0 = k*I - J (J the all-ones matrix). psi
// is defined up to an additive constant; we fix sum(psi) = 0. On K_k the solution
// is CLOSED-FORM: with sum(psi)=0, (k*I - J)psi = k*psi, so psi = -div(Omega)/k.
// We nonetheless verify the normal equation rather than assume the form, and the
// orthogonality identity ||Omega||^2 = ||grad psi||^2 + ||curl||^2 is the GATE-1
// correctness check.
//
// All arithmetic is float64 here (the Omega entries arrive as exact wei big.Ints
// but the decomposition is a least-squares projection — a real-valued operation).
// The orthogonality residual is reported relative to ||Omega||^2 so GATE-1 can
// kill the engine if the projection is wrong (residual > 1e-6).
package simengine

import (
	"math"
	"math/big"
)

// antisym is a dense antisymmetric k-by-k 2-form. Only the strict upper triangle
// is stored as float64 (the exact big.Int entries are converted on set); the
// lower triangle is -upper and the diagonal is 0 by construction, so the form is
// EXACTLY antisymmetric regardless of any rounding in a single entry.
type antisym struct {
	k int
	u [][]float64 // u[i][j] for j>i holds Omega_ij
}

// newAntisym allocates a k-by-k antisymmetric form (all zero).
func newAntisym(k int) *antisym {
	u := make([][]float64, k)
	for i := range u {
		u[i] = make([]float64, k)
	}
	return &antisym{k: k, u: u}
}

// set stores Omega_ij = v (and implicitly Omega_ji = -v). v is the exact integer
// curl entry; it is converted to float64 for the projection. Only i<j is stored;
// callers pass i<j (the engine builds the upper triangle).
func (a *antisym) set(i, j int, v *big.Int) {
	if i == j {
		return
	}
	f := bigToFloat(v)
	if i < j {
		a.u[i][j] = f
	} else {
		a.u[j][i] = -f
	}
}

// get returns Omega_ij (antisymmetric: get(i,j) == -get(j,i), get(i,i)==0).
func (a *antisym) get(i, j int) float64 {
	if i == j {
		return 0
	}
	if i < j {
		return a.u[i][j]
	}
	return -a.u[j][i]
}

// norm2 returns the squared Frobenius norm over the strict upper triangle (the
// energy of the 1-form on undirected edges): sum_{i<j} Omega_ij^2. Returned as a
// big.Float-free big.Int-compatible value is not exact (floats), so we return it
// as a *big.Int of the rounded value for the LOG line AND keep a float for the
// ratios; the ratios use the float path (ratioBig converts). To keep the public
// log integer-looking we round.
func (a *antisym) norm2() *big.Int {
	return floatToBig(a.norm2f())
}

// norm2f returns the squared upper-triangle Frobenius norm as a float64.
func (a *antisym) norm2f() float64 {
	var s float64
	for i := 0; i < a.k; i++ {
		for j := i + 1; j < a.k; j++ {
			s += a.u[i][j] * a.u[i][j]
		}
	}
	return s
}

// div returns the divergence vector div_i = sum_j Omega_ij (j != i).
func (a *antisym) div() []float64 {
	d := make([]float64, a.k)
	for i := 0; i < a.k; i++ {
		var s float64
		for j := 0; j < a.k; j++ {
			if i == j {
				continue
			}
			s += a.get(i, j)
		}
		d[i] = s
	}
	return d
}

// hodgeResult carries the decomposition energies (all squared, float64-derived
// and rounded into big.Ints for the log) plus the float energies for the ratios.
type hodgeResult struct {
	psi        []float64 // potential (sum psi = 0)
	gradNorm2  *big.Int  // ||grad psi||^2 (rounded)
	curlNorm2  *big.Int  // ||Omega - grad psi||^2 (rounded)
	omegaNorm2 *big.Int  // ||Omega||^2 (rounded; set by caller for scalar path)

	gradNorm2f  float64
	curlNorm2f  float64
	omegaNorm2f float64
}

// hodgeDecompose performs the discrete Hodge decomposition of the antisymmetric
// 2-form on the filled clique. It solves L0 psi = div(Omega) with sum(psi)=0 (the
// closed-form psi = div/k on K_k, which we ALSO verify against the explicit
// Laplacian solve), forms grad(psi)_ij = psi_j - psi_i, and returns the gradient
// and curl (residual) energies.
func hodgeDecompose(omega *antisym) hodgeResult {
	k := omega.k
	res := hodgeResult{
		gradNorm2:  big.NewInt(0),
		curlNorm2:  big.NewInt(0),
		omegaNorm2: floatToBig(omega.norm2f()),
	}
	res.omegaNorm2f = omega.norm2f()
	if k < 2 {
		res.psi = make([]float64, k)
		return res
	}

	// Divergence with the SIGN CONVENTION matching grad(psi)_ij = psi_j - psi_i.
	// For a pure gradient, Omega_ij = psi_j - psi_i, so
	//   div_i := sum_j Omega_ij = sum_j(psi_j - psi_i) = (sum psi) - k*psi_i.
	// The graph-Laplacian normal equation for the least-squares potential is
	//   L0 psi = -div(Omega),   L0 = k*I - J,
	// (the standard "div as a co-boundary" sign). Under the gauge sum(psi)=0,
	// J psi = 0 so L0 psi = k*psi, giving psi = -div/k. (We verify the explicit
	// normal equation in dryrun_curl_test.go rather than rely on the closed form.)
	d := omega.div()
	var dmean float64
	for _, v := range d {
		dmean += v
	}
	dmean /= float64(k)
	psi := make([]float64, k)
	for i := range psi {
		psi[i] = -(d[i] - dmean) / float64(k)
	}
	res.psi = psi

	// grad(psi)_ij = psi_j - psi_i; curl_ij = Omega_ij - grad_ij. Sum energies over
	// the strict upper triangle.
	var gradN2, curlN2 float64
	for i := 0; i < k; i++ {
		for j := i + 1; j < k; j++ {
			g := psi[j] - psi[i]
			c := omega.u[i][j] - g
			gradN2 += g * g
			curlN2 += c * c
		}
	}
	res.gradNorm2f = gradN2
	res.curlNorm2f = curlN2
	res.gradNorm2 = floatToBig(gradN2)
	res.curlNorm2 = floatToBig(curlN2)
	return res
}

// orthoResidualFrac returns the RELATIVE orthogonality residual,
// |omega^2 - grad^2 - curl^2| / omega^2, the GATE-1 correctness number. On a
// correct Hodge decomposition of a filled clique (H^1=0) this is ~0 (the harmonic
// energy is identically 0 and the cross term vanishes by orthogonality). A residual
// above ~1e-6 means the decomposition code is wrong (GATE-1 KILL).
func (h hodgeResult) orthoResidualFrac(omegaNorm2 *big.Int) float64 {
	on2 := h.omegaNorm2f
	if on2 <= 0 {
		on2 = bigToFloat(omegaNorm2)
	}
	if on2 <= 0 {
		return 0
	}
	resid := math.Abs(on2 - h.gradNorm2f - h.curlNorm2f)
	return resid / on2
}

// ratioBig returns num/den as a float64 from two big.Ints (used for the LOG only;
// the precise ratios come from the float energies). Returns 0 for a non-positive
// denominator.
func ratioBig(num, den *big.Int) float64 {
	if num == nil || den == nil || den.Sign() <= 0 {
		return 0
	}
	r := new(big.Float).Quo(new(big.Float).SetInt(num), new(big.Float).SetInt(den))
	f, _ := r.Float64()
	return f
}

// bigToFloat converts a (possibly huge, wei-scale) big.Int to float64. The Omega
// entries are wei differences; float64 has 53 bits of mantissa so the LEAST
// significant wei may be lost, but the DECOMPOSITION RATIOS (grad/total) are scale
// invariant and unaffected by a uniform mantissa truncation. The orthogonality
// residual is computed in the SAME float domain, so GATE-1 measures the projection
// error, not the big->float rounding (which cancels in the identity).
func bigToFloat(v *big.Int) float64 {
	if v == nil {
		return 0
	}
	f := new(big.Float).SetInt(v)
	out, _ := f.Float64()
	return out
}

// floatToBig rounds a non-negative float64 energy to a big.Int for the log line.
// Energies are squared wei (can overflow float exactness) — this is for display
// only; the ratios use the float path.
func floatToBig(f float64) *big.Int {
	if f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return big.NewInt(0)
	}
	bf := big.NewFloat(f)
	out, _ := bf.Int(nil)
	if out == nil {
		return big.NewInt(0)
	}
	return out
}
