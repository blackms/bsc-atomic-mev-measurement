// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// dryrun_backrun_any_test.go pins the DETECTOR-FACING marginal-attribution
// primitives the backrun-any detector (dryrun_backrun_any.go) relies on, directly
// and hermetically: backrunNetFloor (best realizable NET per state, floored at 0)
// and the SHARED strategy.MarginalNet rule the detector and MarginalBackrunGrossV2
// both route through. Without these, a detector regression in the marginal rule
// (lost pre-floor, swapped operands, missing difference-floor) would go uncaught.
package simengine

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/strategy"
)

// TestBackrunNetFloor pins the per-state baseline: a not-found / nil / net-negative
// best cycle contributes 0; a net-positive one contributes its exact net.
func TestBackrunNetFloor(t *testing.T) {
	if got := backrunNetFloor(strategy.SandwichEval{}, false); got.Sign() != 0 {
		t.Fatalf("not-found must floor to 0, got %s", got)
	}
	if got := backrunNetFloor(strategy.SandwichEval{NetProfit: nil}, true); got.Sign() != 0 {
		t.Fatalf("nil net must floor to 0, got %s", got)
	}
	if got := backrunNetFloor(strategy.SandwichEval{NetProfit: big.NewInt(-5)}, true); got.Sign() != 0 {
		t.Fatalf("net-negative best cycle must floor to 0 (would not be executed), got %s", got)
	}
	if got := backrunNetFloor(strategy.SandwichEval{NetProfit: big.NewInt(7)}, true); got.Cmp(big.NewInt(7)) != 0 {
		t.Fatalf("net-positive best cycle must pass through, got %s want 7", got)
	}
}

// TestMarginalNetRule pins the SHARED marginal rule (strategy.MarginalNet) used by
// both the detector and the pure graph helper. The four named cases plus the two
// regression guards (lost pre-floor, swapped operands) lock the exact semantics.
func TestMarginalNetRule(t *testing.T) {
	cases := []struct {
		name            string
		preNet, postNet *big.Int
		want            *big.Int
	}{
		// STANDING: identical pre/post profitable value -> nothing victim-created.
		{"standing", big.NewInt(100), big.NewInt(100), big.NewInt(0)},
		// VICTIM-CREATED: no pre value, profitable post -> full post is attributable.
		{"created", big.NewInt(0), big.NewInt(100), big.NewInt(100)},
		// PARTIAL-CLOSE: victim shrinks a pre-existing gap (post < pre) -> 0, never < 0.
		{"partial-close", big.NewInt(100), big.NewInt(40), big.NewInt(0)},
		// WIDENED: victim enlarges a pre-existing gap -> only the increment counts.
		{"widened", big.NewInt(40), big.NewInt(100), big.NewInt(60)},
		// PRE-FLOOR guard: a negative pre net must be treated as 0 (NOT added back).
		// Without the pre-floor this would be 10-(-5)=15; the floor pins it to 10.
		{"negative-pre-floored", big.NewInt(-5), big.NewInt(10), big.NewInt(10)},
		// NIL inputs are treated as 0.
		{"nil-pre", nil, big.NewInt(10), big.NewInt(10)},
		{"nil-post", big.NewInt(10), nil, big.NewInt(0)},
	}
	for _, c := range cases {
		got := strategy.MarginalNet(c.preNet, c.postNet)
		if got.Cmp(c.want) != 0 {
			t.Fatalf("MarginalNet(%v, %v) [%s] = %s, want %s", c.preNet, c.postNet, c.name, got, c.want)
		}
	}

	// OPERAND-ORDER guard: the rule is NOT symmetric. Swapping pre/post on the
	// created case must change the result (post-as-pre would wrongly report 0), so a
	// future operand swap in the detector or helper is caught.
	createdOrder := strategy.MarginalNet(big.NewInt(0), big.NewInt(100))
	swappedOrder := strategy.MarginalNet(big.NewInt(100), big.NewInt(0))
	if createdOrder.Cmp(swappedOrder) == 0 {
		t.Fatalf("MarginalNet must be operand-order sensitive (pre,post); both gave %s", createdOrder)
	}
}

// TestSanityRejectBackrunOpp pins the catch-it-don't-bake-it cap predicate the
// backrun-any detector uses to refuse decimal-mismatch / degenerate-pool math
// outliers (mirrors the prior sandwich units-bug catch). The cap is forensic
// and an order of magnitude above any realistic independent-searcher single-tx
// backrun, so a real opp can never trip it; only math glitches do.
func TestSanityRejectBackrunOpp(t *testing.T) {
	// Default cap (the production defaults: $100k USD, 1000 BNB).
	capUSD := 100_000.0
	bnb := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	capNetWei := new(big.Int).Mul(big.NewInt(1000), bnb)

	cases := []struct {
		name      string
		grossUSD  float64
		netWei    *big.Int
		capUSD    float64
		capNetWei *big.Int
		want      bool
	}{
		// NORMAL: the live SANE observations ($0.55 - $3.60 gross, 1e14 - 4e15 wei net).
		{"sane-small", 0.55, big.NewInt(1e14), capUSD, capNetWei, false},
		{"sane-large", 3.60, big.NewInt(4e15), capUSD, capNetWei, false},
		// NORMAL: a beefy but realistic backrun ($50k, 500 BNB) must NOT trip the cap.
		{"realistic-cap-edge", 50_000.0, new(big.Int).Mul(big.NewInt(500), bnb), capUSD, capNetWei, false},
		// OUTLIER: the live degenerate observation (grossUSD 3.597e15, netWei 2.95e30).
		// This is the units-bug-style outlier the cap exists to catch.
		{"degenerate-units-bug", 3.597e15, mustBig("2950000000000000000000000000000"), capUSD, capNetWei, true},
		// USD-side-only trip: gross > cap but net under cap.
		{"usd-only-trips", 1e9, big.NewInt(1), capUSD, capNetWei, true},
		// Net-side-only trip: USD under cap but net wei > cap.
		{"net-only-trips", 1.0, new(big.Int).Mul(big.NewInt(10_000), bnb), capUSD, capNetWei, true},
		// EXACT BOUNDARY: == cap does NOT trip (predicate is strictly greater-than).
		{"boundary-usd-eq", 100_000.0, big.NewInt(1), capUSD, capNetWei, false},
		{"boundary-net-eq", 1.0, new(big.Int).Set(capNetWei), capUSD, capNetWei, false},
		// JUST-OVER: 1 unit above cap trips.
		{"boundary-net-just-over", 1.0, new(big.Int).Add(new(big.Int).Set(capNetWei), big.NewInt(1)), capUSD, capNetWei, true},
		// NIL-NET / ZERO-CAP behavior: nil net is treated as not-set (no net trip);
		// zero USD cap disables the USD side of the predicate.
		{"nil-net-no-trip", 1.0, nil, capUSD, capNetWei, false},
		{"zero-usd-cap-disables-usd-side", 1e18, big.NewInt(1), 0, capNetWei, false},
	}
	for _, c := range cases {
		got := sanityRejectBackrunOpp(c.grossUSD, c.netWei, c.capUSD, c.capNetWei)
		if got != c.want {
			t.Fatalf("sanityRejectBackrunOpp(%g, %v, capUSD=%g, capNet=%v) [%s] = %v, want %v",
				c.grossUSD, c.netWei, c.capUSD, c.capNetWei, c.name, got, c.want)
		}
	}
}

// mustBig parses a base-10 big-int literal or panics. Used only in the sanity-cap
// table to spell the live degenerate observation (2.95e30 wei) exactly.
func mustBig(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("mustBig: bad literal " + s)
	}
	return n
}
