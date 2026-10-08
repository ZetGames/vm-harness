package hostinfo

import (
	"strings"
	"testing"

	"golang.org/x/sys/cpu"
)

func TestCPUIDRegisters(t *testing.T) {
	maxLeaf, ebx0, ecx0, edx0 := cpuid(0)
	if maxLeaf < 7 {
		t.Skipf("cpuid(0) reports max leaf %d, the register checks need leaf 7", maxLeaf)
	}
	if v := vendor(ebx0, edx0, ecx0); strings.Trim(v, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz ") != "" {
		t.Errorf("cpu vendor %q is not plain text", v)
	}
	_, _, ecx1, edx1 := cpuid(1)
	_, ebx7, _, _ := cpuid(7)
	features := []struct {
		name string
		reg  uint32
		bit  uint
		has  bool
	}{
		{"SSE3", ecx1, 0, cpu.X86.HasSSE3},
		{"PCLMULQDQ", ecx1, 1, cpu.X86.HasPCLMULQDQ},
		{"SSSE3", ecx1, 9, cpu.X86.HasSSSE3},
		{"CX16", ecx1, 13, cpu.X86.HasCX16},
		{"SSE4.1", ecx1, 19, cpu.X86.HasSSE41},
		{"SSE4.2", ecx1, 20, cpu.X86.HasSSE42},
		{"POPCNT", ecx1, 23, cpu.X86.HasPOPCNT},
		{"AES", ecx1, 25, cpu.X86.HasAES},
		{"OSXSAVE", ecx1, 27, cpu.X86.HasOSXSAVE},
		{"RDRAND", ecx1, 30, cpu.X86.HasRDRAND},
		{"SSE2", edx1, 26, cpu.X86.HasSSE2},
		{"BMI1", ebx7, 3, cpu.X86.HasBMI1},
		{"BMI2", ebx7, 8, cpu.X86.HasBMI2},
		{"ERMS", ebx7, 9, cpu.X86.HasERMS},
		{"RDSEED", ebx7, 18, cpu.X86.HasRDSEED},
		{"ADX", ebx7, 19, cpu.X86.HasADX},
	}
	for _, f := range features {
		if got := f.reg&(1<<f.bit) != 0; got != f.has {
			t.Errorf("%s: cpuid bit %d = %v, x/sys/cpu reports %v", f.name, f.bit, got, f.has)
		}
	}
	if got, want := Detect() == BareMetal, ecx1&hypervisorPresent == 0; got != want {
		t.Errorf("Detect() = %d with cpuid(1) ecx %#x", Detect(), ecx1)
	}
}
