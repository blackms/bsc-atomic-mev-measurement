// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// sandwich_test.go unit-proves the pure-math sandwich valuator (sandwich.go)
// WITHOUT a live node: the ground-truth EVM re-execution needs deployed router
// code (exercised at runtime in SIMENGINE_DRYRUN=sandwich), so here we pin the
// closed-form V2 sandwich profit against the research worked example, prove the
// golden-section frontrun search matches/improves the closed-form seed under a
// realistic slippage cap, and check the victim decoding + dust threshold + flash
// fee.
package strategy

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var e18 = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

func wbnb(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), e18) }

// ---------------------------------------------------------------------------
// (1) V2 closed-form sandwich profit vs the research worked example.
// ---------------------------------------------------------------------------

// TestV2SandwichGrossWorkedExample pins V2SandwichGross to the EXACT wei figure
// from the verified research: pool x=2000 WBNB, y=1,200,000 USDT; victim buys
// with v=20 WBNB; the half-victim frontrun f=v/2=10 WBNB yields GROSS =
// 0.148562350649079609 WBNB.
func TestV2SandwichGrossWorkedExample(t *testing.T) {
	x := wbnb(2000)    // reserveIn (WBNB)
	y := wbnb(1200000) // reserveOut (USDT)
	v := wbnb(20)      // victim amountIn (WBNB)
	f := wbnb(10)      // frontrun = v/2

	gross := V2SandwichGross(x, y, v, f, GammaPancakeV2)
	want, _ := new(big.Int).SetString("148562350649079609", 10)
	if gross.Cmp(want) != 0 {
		t.Fatalf("V2SandwichGross = %s, want %s (research worked example)", gross, want)
	}
}

// TestV2SandwichGrossZeroFrontrunIsZero confirms a zero/negative frontrun yields
// no profit (defensive boundary).
func TestV2SandwichGrossZeroFrontrunIsZero(t *testing.T) {
	x, y, v := wbnb(2000), wbnb(1200000), wbnb(20)
	if g := V2SandwichGross(x, y, v, big.NewInt(0), GammaPancakeV2); g.Sign() != 0 {
		t.Fatalf("zero frontrun gross = %s, want 0", g)
	}
	if g := V2SandwichGross(x, y, v, big.NewInt(-5), GammaPancakeV2); g.Sign() != 0 {
		t.Fatalf("negative frontrun gross = %s, want 0", g)
	}
}

// TestHalfVictimSeed checks the small-trade optimal-frontrun rule f* = Vv/2.
func TestHalfVictimSeed(t *testing.T) {
	if got := HalfVictimSeed(wbnb(20)); got.Cmp(wbnb(10)) != 0 {
		t.Fatalf("HalfVictimSeed(20) = %s, want 10e18", got)
	}
	if got := HalfVictimSeed(big.NewInt(0)); got.Sign() != 0 {
		t.Fatalf("HalfVictimSeed(0) = %s, want 0", got)
	}
}

// TestV2CombinedSeedClose checks the combined-trade seed is a sane, positive
// frontrun near v/2 for a deep pool (research: ~9.975 WBNB for v=20 at x=2000).
func TestV2CombinedSeedClose(t *testing.T) {
	x, v := wbnb(2000), wbnb(20)
	seed := V2CombinedSeed(x, v, GammaPancakeV2)
	if seed.Sign() <= 0 {
		t.Fatalf("combined seed non-positive: %s", seed)
	}
	// Within 10% of v/2 = 10 WBNB.
	half := wbnb(10)
	diff := new(big.Int).Abs(new(big.Int).Sub(seed, half))
	tol := new(big.Int).Div(half, big.NewInt(10))
	if diff.Cmp(tol) > 0 {
		t.Fatalf("combined seed %s not within 10%% of v/2 %s (diff %s)", seed, half, diff)
	}
}

// ---------------------------------------------------------------------------
// (1b) Gamma generalization: the closed-form V2 sandwich profit must vary
// correctly with the pool's fee factor (any V2-style fork), not just Pancake.
// ---------------------------------------------------------------------------

// TestV2SandwichGrossGammaGeneralization confirms V2SandwichGross is properly
// gamma-parameterized: a LOWER fee (gamma closer to 1) leaves more value in the
// round-trip, so for the same pool/victim/frontrun the gross is monotonically
// higher as the fee falls. We compare the generic 0.30% upper bound (997/1000),
// Pancake 0.25% (9975/10000), and Biswap 0.20% (998/1000).
func TestV2SandwichGrossGammaGeneralization(t *testing.T) {
	x, y, v, f := wbnb(2000), wbnb(1200000), wbnb(20), wbnb(10)

	gross030 := V2SandwichGross(x, y, v, f, GammaUniswapV2) // 0.30% (997/1000)
	gross025 := V2SandwichGross(x, y, v, f, GammaPancakeV2) // 0.25% (9975/10000)
	gross020 := V2SandwichGross(x, y, v, f, GammaBiswapV2)  // 0.20% (998/1000)

	if !(gross020.Cmp(gross025) > 0 && gross025.Cmp(gross030) > 0) {
		t.Fatalf("gross should rise as fee falls: 0.20%%=%s 0.25%%=%s 0.30%%=%s (want 0.20>0.25>0.30)",
			gross020, gross025, gross030)
	}
	// All three must be positive for this deep-pool worked example.
	if gross030.Sign() <= 0 {
		t.Fatalf("even the 0.30%% gross should be positive here, got %s", gross030)
	}
}

// TestGammaForFeeTierFactor pins the V3 fee-tier -> gamma conversion used by the
// any-pool valuator's seed math: gamma = (1e6 - feeTier)/1e6.
func TestGammaForFeeTierFactor(t *testing.T) {
	g := gammaForFeeTier(2500) // 0.25%
	if g.Num.Cmp(big.NewInt(997500)) != 0 || g.Den.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("gammaForFeeTier(2500) = %s/%s, want 997500/1000000", g.Num, g.Den)
	}
	g2 := gammaForFeeTier(100) // 0.01%
	if g2.Num.Cmp(big.NewInt(999900)) != 0 {
		t.Fatalf("gammaForFeeTier(100) Num = %s, want 999900", g2.Num)
	}
}

// ---------------------------------------------------------------------------
// (2) Golden-section frontrun search vs the closed form, under a slippage cap.
// ---------------------------------------------------------------------------

// TestOptimalFrontrunRespectsSlippageCap is the load-bearing test: WITHOUT a
// slippage cap the unconstrained gross-maximising frontrun runs away above the
// victim (an artifact — the real victim would revert). The search must therefore
// converge to the feasibility cap when the cap binds, exactly as the ground-truth
// EVM re-execution does via the victim's amountOutMin revert. We model the cap by
// making the probe return !ok above Vf_max, and assert the search lands AT the
// cap and never below the half-victim seed's gross.
func TestOptimalFrontrunRespectsSlippageCap(t *testing.T) {
	x, y, v := wbnb(2000), wbnb(1200000), wbnb(20)
	cap := wbnb(12) // victim's slippage tolerance binds the frontrun at 12 WBNB.

	probe := func(f *big.Int) (*big.Int, bool) {
		if f.Sign() <= 0 || f.Cmp(cap) > 0 {
			return big.NewInt(0), false // victim would revert: infeasible.
		}
		return V2SandwichGross(x, y, v, f, GammaPancakeV2), true
	}

	seeds := []*big.Int{HalfVictimSeed(v), V2CombinedSeed(x, v, GammaPancakeV2)}
	front, gross := OptimalFrontrun(probe, seeds, wbnb(160)) // hi = 8x victim.

	if gross.Sign() <= 0 {
		t.Fatalf("search found no profit")
	}
	// Gross at the cap is the constrained maximum (profit is increasing up to the
	// cap in this regime — confirmed by the brute scan in the research).
	grossAtCap := V2SandwichGross(x, y, v, cap, GammaPancakeV2)
	// The search must reach (within the integer grid) the cap's gross and never
	// regress below the half-victim seed's gross.
	seedGross := V2SandwichGross(x, y, v, HalfVictimSeed(v), GammaPancakeV2)
	if gross.Cmp(seedGross) < 0 {
		t.Fatalf("search gross %s < half-victim seed gross %s", gross, seedGross)
	}
	// Allow a small slack: the grid may land a few wei from the exact cap.
	diff := new(big.Int).Abs(new(big.Int).Sub(gross, grossAtCap))
	slack := new(big.Int).Div(grossAtCap, big.NewInt(1000)) // 0.1%
	if slack.Sign() == 0 {
		slack = big.NewInt(1000)
	}
	if diff.Cmp(slack) > 0 {
		t.Fatalf("constrained search gross %s far from cap gross %s (diff %s, front %s)", gross, grossAtCap, diff, front)
	}
	// The chosen frontrun must not exceed the feasibility cap.
	if front.Cmp(cap) > 0 {
		t.Fatalf("search frontrun %s exceeds feasibility cap %s", front, cap)
	}
}

// TestOptimalFrontrunInteriorPeak confirms that when the cap does NOT bind (it is
// far above the interior optimum), the search finds an interior peak whose gross
// is >= the seed gross and is a genuine local maximum (neighbours do not beat it
// by more than grid slack). We use a SHALLOW pool so the unconstrained optimum is
// interior rather than runaway.
func TestOptimalFrontrunInteriorPeak(t *testing.T) {
	// Shallow pool: x=100 WBNB, y=60,000 USDT, victim v=50 WBNB (large vs depth).
	x, y, v := wbnb(100), wbnb(60000), wbnb(50)

	probe := func(f *big.Int) (*big.Int, bool) {
		if f.Sign() <= 0 {
			return big.NewInt(0), false
		}
		// Feasible up to a very large cap (effectively unconstrained interior peak,
		// because price impact dominates well before any cap).
		return V2SandwichGross(x, y, v, f, GammaPancakeV2), true
	}

	front, gross := OptimalFrontrun(probe, []*big.Int{HalfVictimSeed(v)}, wbnb(2000))
	if gross.Sign() <= 0 {
		t.Fatalf("search found no profit on shallow pool")
	}
	// Local-maximum check: probe a band around the found frontrun.
	step := new(big.Int).Div(front, big.NewInt(20)) // 5% steps
	if step.Sign() == 0 {
		step = big.NewInt(1)
	}
	for _, d := range []*big.Int{new(big.Int).Neg(step), step} {
		nb := new(big.Int).Add(front, d)
		if nb.Sign() <= 0 {
			continue
		}
		g := V2SandwichGross(x, y, v, nb, GammaPancakeV2)
		if g.Cmp(gross) > 0 {
			// neighbour beats the peak by more than grid slack -> not a maximum.
			slack := new(big.Int).Div(gross, big.NewInt(1000))
			if new(big.Int).Sub(g, gross).Cmp(slack) > 0 {
				t.Fatalf("neighbour %s gross %s beats peak %s gross %s", nb, g, front, gross)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// (3) Victim detection / decoding.
// ---------------------------------------------------------------------------

// pancakeV2WbnbUsdtPair is a VERIFIED watched V2 pool (token0=USDT, token1=WBNB).
var pancakeV2WbnbUsdtPair = common.HexToAddress("0x16b9a82891338f9bA80E2D6970FddA79D1eb0daE")

// pancakeV3WbnbUsdtPair is a VERIFIED watched V3 pool (token0=USDT, token1=WBNB).
var pancakeV3WbnbUsdtPair = common.HexToAddress("0x172fcd41e0913e95784454622d1c3724f546f849")

// word32 left-pads a big.Int to a 32-byte big-endian word.
func word32(v *big.Int) []byte {
	b := v.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// signedWord32 encodes a (possibly negative) big.Int as a two's-complement 32-byte
// word.
func signedWord32(v *big.Int) []byte {
	if v.Sign() >= 0 {
		return word32(v)
	}
	mod := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 256), v) // 2^256 + v
	return word32(mod)
}

// TestDecodeV2Victim decodes a V2 Swap log: victim spends token1 (WBNB) buying
// token0 (USDT). amount1In is nonzero.
func TestDecodeV2Victim(t *testing.T) {
	amount1In := wbnb(20)
	data := make([]byte, 0, 128)
	data = append(data, word32(big.NewInt(0))...) // amount0In
	data = append(data, word32(amount1In)...)     // amount1In (victim spends WBNB)
	data = append(data, word32(wbnb(11800))...)   // amount0Out (USDT received)
	data = append(data, word32(big.NewInt(0))...) // amount1Out

	l := &types.Log{Address: pancakeV2WbnbUsdtPair, Topics: []common.Hash{SwapTopic0}, Data: data}
	victim, ok := DecodeVictim(l)
	if !ok {
		t.Fatalf("DecodeVictim failed on a valid V2 Swap log")
	}
	if victim.TokenIn != WBNB {
		t.Fatalf("victim TokenIn = %s, want WBNB", victim.TokenIn.Hex())
	}
	if victim.AmountIn.Cmp(amount1In) != 0 {
		t.Fatalf("victim AmountIn = %s, want %s", victim.AmountIn, amount1In)
	}
	if victim.Pool.IsV3 {
		t.Fatalf("V2 victim decoded as V3")
	}
}

// TestDecodeV3Victim decodes a V3 Swap log: victim sells token1 (WBNB) into the
// pool (amount1 positive = into pool), receiving token0 (USDT, amount0 negative).
func TestDecodeV3Victim(t *testing.T) {
	amount1 := wbnb(5) // +5 WBNB INTO the pool (victim input)
	amount0 := new(big.Int).Neg(wbnb(2950))
	data := make([]byte, 0, 64)
	data = append(data, signedWord32(amount0)...) // amount0 (negative: out)
	data = append(data, signedWord32(amount1)...) // amount1 (positive: in)
	// (sqrtPriceX96, liquidity, tick words may follow; decoder reads only the first two.)

	l := &types.Log{Address: pancakeV3WbnbUsdtPair, Topics: []common.Hash{V3SwapTopic0}, Data: data}
	victim, ok := DecodeVictim(l)
	if !ok {
		t.Fatalf("DecodeVictim failed on a valid V3 Swap log")
	}
	if !victim.Pool.IsV3 {
		t.Fatalf("V3 victim not flagged IsV3")
	}
	if victim.TokenIn != WBNB {
		t.Fatalf("V3 victim TokenIn = %s, want WBNB (positive amount side)", victim.TokenIn.Hex())
	}
	if victim.AmountIn.Cmp(amount1) != 0 {
		t.Fatalf("V3 victim AmountIn = %s, want %s", victim.AmountIn, amount1)
	}
}

// TestDecodeVictimRejectsNonWatched rejects a Swap on an unknown pool and a Sync
// log (no directional amounts).
func TestDecodeVictimRejectsNonWatched(t *testing.T) {
	other := common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	data := make([]byte, 128)
	copy(data[32:64], word32(wbnb(1))) // amount1In
	if _, ok := DecodeVictim(&types.Log{Address: other, Topics: []common.Hash{SwapTopic0}, Data: data}); ok {
		t.Fatalf("DecodeVictim accepted a non-watched pool")
	}
	// Sync log on a watched pool: not a directional swap.
	if _, ok := DecodeVictim(&types.Log{Address: pancakeV2WbnbUsdtPair, Topics: []common.Hash{SyncTopic0}, Data: make([]byte, 64)}); ok {
		t.Fatalf("DecodeVictim accepted a Sync log as a victim")
	}
}

// ---------------------------------------------------------------------------
// (4) Dust threshold + flash fee + net gate.
// ---------------------------------------------------------------------------

// TestVictimAboveThreshold checks the USD dust filter.
func TestVictimAboveThreshold(t *testing.T) {
	// WBNB at $600: 1 WBNB = $600 > $100 floor -> above.
	if !VictimAboveThreshold(wbnb(1), 600, 100) {
		t.Fatalf("1 WBNB @ $600 should clear the $100 floor")
	}
	// 0.1 WBNB = $60 < $100 -> below.
	tenth := new(big.Int).Div(e18, big.NewInt(10))
	if VictimAboveThreshold(tenth, 600, 100) {
		t.Fatalf("0.1 WBNB @ $600 ($60) should be below the $100 floor")
	}
	// Disabled gate (minUSD <= 0) -> always true.
	if !VictimAboveThreshold(big.NewInt(1), 600, 0) {
		t.Fatalf("zero floor should disable the gate")
	}
}

// TestFlashFee checks the basis-point flash-loan fee (ceil division).
func TestFlashFee(t *testing.T) {
	// 9 bps of 10 WBNB = 0.009 WBNB = 9e15 wei.
	got := FlashFee(wbnb(10), 9)
	want, _ := new(big.Int).SetString("9000000000000000", 10)
	if got.Cmp(want) != 0 {
		t.Fatalf("FlashFee(10 WBNB, 9bps) = %s, want %s", got, want)
	}
	// Zero bps -> 0.
	if got := FlashFee(wbnb(10), 0); got.Sign() != 0 {
		t.Fatalf("FlashFee with 0 bps = %s, want 0", got)
	}
	// Ceiling: 1 bps of 1 wei = ceil(1/10000) = 1 (never rounds a real fee to free).
	if got := FlashFee(big.NewInt(1), 1); got.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("FlashFee ceiling = %s, want 1", got)
	}
}

// TestSandwichNetGate assembles the net gate from the research worked example and
// checks net = gross - gas - flashFee - bid stays positive.
func TestSandwichNetGate(t *testing.T) {
	frontrun := wbnb(10)
	gross, _ := new(big.Int).SetString("148562350649079609", 10) // 0.148562 WBNB
	gasPrice := big.NewInt(3_000_000_000)                        // 3 gwei
	gasUnits := SandwichGasUnits(false)                          // 2 V2 router legs

	eval := SandwichNet(frontrun, gross, gasPrice, gasUnits, 9, big.NewInt(0))

	// gas = 2*(60k+90k)=300k * 3gwei = 9e14 wei.
	wantGas := new(big.Int).Mul(new(big.Int).SetUint64(gasUnits), gasPrice)
	if eval.GasCost.Cmp(wantGas) != 0 {
		t.Fatalf("gas cost = %s, want %s", eval.GasCost, wantGas)
	}
	// flashFee = 9 bps of 10 WBNB = 9e15.
	wantFlash, _ := new(big.Int).SetString("9000000000000000", 10)
	if eval.FlashFee.Cmp(wantFlash) != 0 {
		t.Fatalf("flash fee = %s, want %s", eval.FlashFee, wantFlash)
	}
	// net = gross - gas - flash = 148562350649079609 - 9e14 - 9e15 > 0.
	wantNet := new(big.Int).Sub(gross, wantGas)
	wantNet.Sub(wantNet, wantFlash)
	if eval.NetProfit.Cmp(wantNet) != 0 {
		t.Fatalf("net = %s, want %s", eval.NetProfit, wantNet)
	}
	if !eval.Profitable {
		t.Fatalf("worked example should be net-positive, got net %s", eval.NetProfit)
	}
}

// TestSandwichGasUnits checks the two-leg gas budgets differ for V2 vs V3.
func TestSandwichGasUnits(t *testing.T) {
	v2 := SandwichGasUnits(false)
	v3 := SandwichGasUnits(true)
	if v2 != 2*(gasBaseUnits+gasPerV2Hop) {
		t.Fatalf("V2 sandwich gas = %d, want %d", v2, 2*(gasBaseUnits+gasPerV2Hop))
	}
	if v3 != 2*(gasBaseUnits+gasPerV3Hop) {
		t.Fatalf("V3 sandwich gas = %d, want %d", v3, 2*(gasBaseUnits+gasPerV3Hop))
	}
	if v3 <= v2 {
		t.Fatalf("V3 gas (%d) should exceed V2 gas (%d)", v3, v2)
	}
}
