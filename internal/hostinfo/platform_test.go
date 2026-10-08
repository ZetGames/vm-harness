package hostinfo

import (
	"encoding/binary"
	"testing"
)

type fakeCPU map[uint32][4]uint32

func (c fakeCPU) cpuid(leaf uint32) (eax, ebx, ecx, edx uint32) {
	r := c[leaf]
	return r[0], r[1], r[2], r[3]
}

func hypervisorLeaf(maxLeaf uint32, vendor string) [4]uint32 {
	b := []byte(vendor + "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")
	le := binary.LittleEndian
	return [4]uint32{maxLeaf, le.Uint32(b[0:]), le.Uint32(b[4:]), le.Uint32(b[8:])}
}

func TestDetect(t *testing.T) {
	const flagged = hypervisorPresent | 1<<25
	cases := []struct {
		name string
		cpu  fakeCPU
		want Platform
	}{
		{"bare metal", fakeCPU{1: {0, 0, 1 << 25, 0}}, BareMetal},
		{"bare metal ignores hypervisor leaves", fakeCPU{
			1:              {0, 0, 1 << 25, 0},
			vendorLeaf:     hypervisorLeaf(0x4000000b, hyperVVendor),
			privilegesLeaf: {0, createPartitions, 0, 0},
		}, BareMetal},
		{"hyper-v root partition", fakeCPU{
			1:              {0, 0, flagged, 0},
			vendorLeaf:     hypervisorLeaf(0x4000000b, hyperVVendor),
			privilegesLeaf: {0x2fff, 0x3bfff, 0, 0},
		}, HyperVRoot},
		{"hyper-v child partition", fakeCPU{
			1:              {0, 0, flagged, 0},
			vendorLeaf:     hypervisorLeaf(0x4000000b, hyperVVendor),
			privilegesLeaf: {0x2e7f, 0x3b8030, 0, 0},
		}, Guest},
		{"kvm with hyper-v enlightenments", fakeCPU{
			1:              {0, 0, flagged, 0},
			vendorLeaf:     hypervisorLeaf(0x40000005, hyperVVendor),
			privilegesLeaf: {0x2e7f, 0, 0, 0},
		}, Guest},
		{"hyper-v vendor without privilege leaf", fakeCPU{
			1:          {0, 0, flagged, 0},
			vendorLeaf: hypervisorLeaf(0x40000001, hyperVVendor),
		}, Guest},
		{"vmware", fakeCPU{1: {0, 0, flagged, 0}, vendorLeaf: hypervisorLeaf(0x40000010, "VMwareVMware")}, Guest},
		{"kvm", fakeCPU{1: {0, 0, flagged, 0}, vendorLeaf: hypervisorLeaf(0x40000001, "KVMKVMKVM")}, Guest},
		{"no vendor leaf", fakeCPU{1: {0, 0, flagged, 0}}, Guest},
	}
	for _, c := range cases {
		if got := detect(c.cpu.cpuid); got != c.want {
			t.Errorf("%s: detect = %d, want %d", c.name, got, c.want)
		}
	}
}
