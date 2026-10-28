package inventory

import (
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
)

func TestQuantityCanonicalDecimalProperties(t *testing.T) {
	t.Parallel()
	property := func(whole uint64, fraction uint32) bool {
		whole %= 1_000_000_000_000_000_000
		fraction %= 1_000_000
		if whole == 0 && fraction == 0 {
			fraction = 1
		}
		canonical := fmt.Sprintf("%d.%06d", whole, fraction)
		value, ok := new(big.Rat).SetString(canonical)
		if !ok || value.Sign() <= 0 || ValidateQuantity(canonical) != nil {
			return false
		}
		for _, invalid := range []string{"-" + canonical, "+" + canonical, "0" + canonical, canonical + "0", canonical + "e0", " " + canonical, canonical + "\n", strings.Replace(canonical, ".", ",", 1)} {
			if ValidateQuantity(invalid) == nil {
				t.Logf("accepted noncanonical decimal %q", invalid)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 256, Rand: rand.New(rand.NewSource(2026102801))}); err != nil {
		t.Fatal(err)
	}
	for _, zero := range []string{"0", "0.0", "0.000000"} {
		if ValidateQuantity(zero) == nil {
			t.Fatalf("accepted zero %q", zero)
		}
	}
}

func TestIdentifierNormalizationProperties(t *testing.T) {
	t.Parallel()
	property := func(a, b uint64) bool {
		if a == 0 && b == 0 {
			b = 1
		}
		hex := fmt.Sprintf("%016x%016x", a, b)
		canonical := hex[:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:]
		for _, input := range []string{canonical, strings.ToUpper(canonical)} {
			got, err := NormalizeID(input)
			if err != nil || got != canonical {
				return false
			}
		}
		for _, invalid := range []string{canonical + " ", " " + canonical, hex, canonical + "\n", "00000000-0000-0000-0000-000000000000"} {
			if _, err := NormalizeID(invalid); err == nil {
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 256, Rand: rand.New(rand.NewSource(2026102802))}); err != nil {
		t.Fatal(err)
	}
}
