//go:build windows

package hvsock

import (
	"testing"
	"unsafe"
)

func TestParseGUID(t *testing.T) {
	const s = "59145519-2c9d-4fec-8cae-4206ed93cba4"
	for _, in := range []string{s, "59145519-2C9D-4FEC-8CAE-4206ED93CBA4", "{" + s + "}"} {
		g, err := ParseGUID(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if g.String() != s {
			t.Fatalf("%s: round trip gave %s", in, g)
		}
	}
	g, _ := ParseGUID(s)
	want := GUID{0x59145519, 0x2c9d, 0x4fec, [8]byte{0x8c, 0xae, 0x42, 0x06, 0xed, 0x93, 0xcb, 0xa4}}
	if g != want {
		t.Fatalf("layout %+v, want %+v", g, want)
	}
}

func TestParseGUIDInvalid(t *testing.T) {
	for _, in := range []string{"", "not-a-guid", "59145519-2c9d-4fec-8cae-4206ed93cbaZ", "591455192c9d4fec8cae4206ed93cba4"} {
		if _, err := ParseGUID(in); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func TestVsockServiceID(t *testing.T) {
	if got := VsockServiceID(0x5779).String(); got != "00005779-facb-11e6-bd58-64006a7986d3" {
		t.Fatalf("got %s", got)
	}
}

func TestAddrIsSockaddrHV(t *testing.T) {
	if n := unsafe.Sizeof(Addr{}); n != 36 {
		t.Fatalf("SOCKADDR_HV is 36 bytes, Addr is %d", n)
	}
}
