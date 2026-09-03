package address

import (
	"strings"
	"testing"
)

// FuzzDecode drives Decode with arbitrary strings. It must never panic, and
// whatever it accepts must be exactly a v1 address with a 32-byte program
// that re-encodes to the same string. Run with:
//
//	go test ./address -run '^$' -fuzz FuzzDecode -fuzztime 60s
func FuzzDecode(f *testing.F) {
	prog := make([]byte, 32)
	for i := range prog {
		prog[i] = byte(i)
	}
	for _, hrp := range []string{"sq", "ssq"} {
		a, _ := Encode(hrp, 1, prog)
		f.Add(hrp, a)
		f.Add(hrp, strings.ToUpper(a))
		f.Add(hrp, a[:len(a)-1]+"q") // checksum broken
	}
	f.Add("sq", "sq1p4w46h2at4w46h2at4w46h2at4w46h2atl52wv6") // v1, 20-byte program
	f.Add("sq", "")
	f.Add("sq", "sq1")
	f.Add("sq", strings.Repeat("q", 200))

	f.Fuzz(func(t *testing.T, hrp, addr string) {
		ver, prog, err := Decode(hrp, addr)
		if err != nil {
			return
		}
		if ver != 1 || len(prog) != WitnessProgramSize {
			t.Fatalf("Decode accepted v%d with a %d-byte program: %q", ver, len(prog), addr)
		}
		re, err := Encode(strings.ToLower(hrp), ver, prog)
		if err != nil {
			t.Fatalf("re-encode of an accepted address failed: %v", err)
		}
		if re != strings.ToLower(addr) {
			t.Fatalf("round trip changed the address: %q -> %q", addr, re)
		}
		if spk := WitnessProgram(ver, prog); len(spk) != 34 || spk[0] != 0x51 || spk[1] != 32 {
			t.Fatalf("script for an accepted address is malformed: %x", spk)
		}
	})
}

// FuzzEncode: any version and program must either encode to something Decode
// accepts back identically, or be refused; it must never panic.
func FuzzEncode(f *testing.F) {
	f.Add("sq", byte(1), make([]byte, 32))
	f.Add("ssq", byte(1), make([]byte, 32))
	f.Add("sq", byte(0), make([]byte, 20))
	f.Add("sq", byte(17), make([]byte, 32))
	f.Add("SQ", byte(1), make([]byte, 32))
	f.Fuzz(func(t *testing.T, hrp string, ver byte, prog []byte) {
		a, err := Encode(hrp, ver, prog)
		if err != nil {
			return
		}
		if len(a) == 0 {
			t.Fatal("Encode returned an empty address without error")
		}
		v2, p2, err := Decode(strings.ToLower(hrp), a)
		if err != nil {
			return // e.g. unsupported version or length: Encode is generic, Decode is the gate
		}
		if v2 != ver || string(p2) != string(prog) {
			t.Fatalf("round trip changed version/program: %d/%x -> %d/%x", ver, prog, v2, p2)
		}
	})
}
