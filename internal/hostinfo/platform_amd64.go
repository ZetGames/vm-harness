package hostinfo

func cpuid(leaf uint32) (eax, ebx, ecx, edx uint32)

func Detect() Platform {
	return detect(cpuid)
}
