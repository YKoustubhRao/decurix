package census

import "strings"

// cpuid runs the CPUID instruction (cpuid_amd64.s).
func cpuid(eaxArg, ecxArg uint32) (eax, ebx, ecx, edx uint32)

// xgetbv reads XCR0, which says which register states the OS saves.
func xgetbv() (eax, edx uint32)

// bit reports whether bit n of v is set.
func bit(v uint32, n uint) bool { return v&(1<<n) != 0 }

// x86Flags reads the instruction sets inference runtimes care about. AVX and
// AVX-512 count only when the OS saves their registers (XCR0), as a runtime
// would crash using them otherwise.
func x86Flags() []string {
	maxLeaf, _, _, _ := cpuid(0, 0)
	if maxLeaf < 1 {
		return nil
	}
	var flags []string
	_, _, ecx1, _ := cpuid(1, 0)
	if bit(ecx1, 20) {
		flags = append(flags, "sse4_2")
	}

	osAVX, osAVX512, osAMX := false, false, false
	if bit(ecx1, 27) { // OSXSAVE
		xcr0, _ := xgetbv()
		osAVX = xcr0&0x6 == 0x6
		osAVX512 = osAVX && xcr0&0xE0 == 0xE0
		osAMX = xcr0&(3<<17) == 3<<17
	}
	if osAVX && bit(ecx1, 28) {
		flags = append(flags, "avx")
		if bit(ecx1, 12) {
			flags = append(flags, "fma")
		}
		if bit(ecx1, 29) {
			flags = append(flags, "f16c")
		}
	}

	if maxLeaf < 7 {
		return flags
	}
	maxSub, ebx7, ecx7, edx7 := cpuid(7, 0)
	if osAVX && bit(ebx7, 5) {
		flags = append(flags, "avx2")
	}
	if osAVX512 && bit(ebx7, 16) {
		flags = append(flags, "avx512f")
		if bit(ebx7, 30) {
			flags = append(flags, "avx512bw")
		}
		if bit(ebx7, 31) {
			flags = append(flags, "avx512vl")
		}
		if bit(ecx7, 11) {
			flags = append(flags, "avx512_vnni")
		}
	}
	if osAMX {
		if bit(edx7, 24) {
			flags = append(flags, "amx_tile")
		}
		if bit(edx7, 22) {
			flags = append(flags, "amx_bf16")
		}
		if bit(edx7, 25) {
			flags = append(flags, "amx_int8")
		}
	}
	if maxSub >= 1 {
		eax71, _, _, _ := cpuid(7, 1)
		if osAVX && bit(eax71, 4) {
			flags = append(flags, "avx_vnni")
		}
		if osAVX512 && bit(eax71, 5) {
			flags = append(flags, "avx512_bf16")
		}
	}
	return flags
}

// x86Brand reads the processor's brand string.
func x86Brand() string {
	maxExt, _, _, _ := cpuid(0x80000000, 0)
	if maxExt < 0x80000004 {
		return ""
	}
	var b strings.Builder
	for leaf := uint32(0x80000002); leaf <= 0x80000004; leaf++ {
		a, bb, c, d := cpuid(leaf, 0)
		for _, r := range []uint32{a, bb, c, d} {
			for i := 0; i < 4; i++ {
				ch := byte(r >> (8 * i))
				if ch != 0 {
					b.WriteByte(ch)
				}
			}
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
