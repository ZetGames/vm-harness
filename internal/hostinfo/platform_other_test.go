//go:build !amd64

package hostinfo

import "testing"

func TestBareMetalWithoutCPUID(t *testing.T) {
	if got := Detect(); got != BareMetal {
		t.Fatalf("Detect() = %d without a cpuid implementation", got)
	}
}
