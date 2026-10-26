package snapshot

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"
)

// Appendix-B boundary bits plus deterministic samples are checked against
// ECMAScript JSON.stringify (Node 24.20.0); published JCS vectors remain separate.
func TestECMAScriptNumbers(t *testing.T) {
	path := "testdata/number-vectors.json"
	if full := os.Getenv("QUERYDOCK_NUMBER_PARITY_FIXTURE"); full != "" {
		path = full
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct{ Bits, Canonical string }
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		bits, err := strconv.ParseUint(v.Bits, 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		if got := number(math.Float64frombits(bits)); got != v.Canonical {
			t.Fatalf("binary64 %s: got %s want %s", v.Bits, got, v.Canonical)
		}
	}
}
