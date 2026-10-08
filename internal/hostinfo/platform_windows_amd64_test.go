package hostinfo

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestIntegrationMatchesWindows(t *testing.T) {
	if os.Getenv("VMH_INTEGRATION") != "1" {
		t.Skip("set VMH_INTEGRATION=1 to compare with what Windows reports")
	}
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-CimInstance Win32_ComputerSystem).HypervisorPresent").Output()
	if err != nil {
		t.Fatal(err)
	}
	windows := strings.EqualFold(strings.TrimSpace(string(out)), "True")
	platform := Detect()
	if got := platform != BareMetal; got != windows {
		t.Fatalf("Detect() = %d, Win32_ComputerSystem.HypervisorPresent = %q", platform, strings.TrimSpace(string(out)))
	}
	_, ebx, ecx, edx := cpuid(vendorLeaf)
	t.Logf("hypervisor present: %v, platform %d, vendor %q", windows, platform, vendor(ebx, ecx, edx))
}
