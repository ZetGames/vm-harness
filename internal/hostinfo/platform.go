package hostinfo

import "encoding/binary"

type Platform int

const (
	BareMetal Platform = iota
	HyperVRoot
	Guest
)

const (
	hypervisorPresent = 1 << 31
	vendorLeaf        = 0x40000000
	privilegesLeaf    = 0x40000003
	createPartitions  = 1
	hyperVVendor      = "Microsoft Hv"
)

func detect(cpuid func(leaf uint32) (eax, ebx, ecx, edx uint32)) Platform {
	if _, _, ecx, _ := cpuid(1); ecx&hypervisorPresent == 0 {
		return BareMetal
	}
	maxLeaf, ebx, ecx, edx := cpuid(vendorLeaf)
	if maxLeaf < privilegesLeaf || vendor(ebx, ecx, edx) != hyperVVendor {
		return Guest
	}
	if _, privileges, _, _ := cpuid(privilegesLeaf); privileges&createPartitions == 0 {
		return Guest
	}
	return HyperVRoot
}

func vendor(regs ...uint32) string {
	b := make([]byte, 0, 4*len(regs))
	for _, r := range regs {
		b = binary.LittleEndian.AppendUint32(b, r)
	}
	return string(b)
}
