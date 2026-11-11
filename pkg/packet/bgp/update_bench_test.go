// Copyright (C) 2026 VeloceRoute contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package bgp

import (
	"fmt"
	"net/netip"
	"testing"
)

// benchUpdate builds a typical full-feed UPDATE: ORIGIN, a 4-hop AS_PATH,
// NEXT_HOP and COMMUNITIES carrying nlriCount /24 prefixes.
func benchUpdate(tb testing.TB, nlriCount int) *BGPMessage {
	tb.Helper()
	nh, err := NewPathAttributeNextHop(netip.MustParseAddr("10.0.12.1"))
	if err != nil {
		tb.Fatal(err)
	}
	attrs := []PathAttributeInterface{
		NewPathAttributeOrigin(0),
		NewPathAttributeAsPath([]AsPathParamInterface{
			NewAs4PathParam(BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{65001, 3356, 1299, 15169}),
		}),
		nh,
		NewPathAttributeCommunities([]uint32{65001<<16 | 100, 65001<<16 | 200}),
	}
	nlri := make([]PathNLRI, 0, nlriCount)
	for i := 0; i < nlriCount; i++ {
		p, err := NewIPAddrPrefix(netip.PrefixFrom(netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)}), 24))
		if err != nil {
			tb.Fatal(err)
		}
		nlri = append(nlri, PathNLRI{NLRI: p})
	}
	return NewBGPUpdateMessage(nil, attrs, nlri)
}

func BenchmarkUpdateSerialize(b *testing.B) {
	for _, n := range []int{1, 40} {
		b.Run(fmt.Sprintf("nlri=%d", n), func(b *testing.B) {
			msg := benchUpdate(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := msg.Serialize(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkUpdateDecode(b *testing.B) {
	for _, n := range []int{1, 40} {
		b.Run(fmt.Sprintf("nlri=%d", n), func(b *testing.B) {
			wire, err := benchUpdate(b, n).Serialize()
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				hdr := &BGPHeader{}
				if err := hdr.DecodeFromBytes(wire[:BGP_HEADER_LENGTH]); err != nil {
					b.Fatal(err)
				}
				if _, err := ParseBGPBody(hdr, wire[BGP_HEADER_LENGTH:]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestUpdateRoundTripBench keeps the benchmark inputs honest: what the
// benchmark serializes must decode back to the same wire bytes.
func TestUpdateRoundTripBench(t *testing.T) {
	for _, n := range []int{1, 40} {
		wire, err := benchUpdate(t, n).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		hdr := &BGPHeader{}
		if err := hdr.DecodeFromBytes(wire[:BGP_HEADER_LENGTH]); err != nil {
			t.Fatal(err)
		}
		m, err := ParseBGPBody(hdr, wire[BGP_HEADER_LENGTH:])
		if err != nil {
			t.Fatal(err)
		}
		again, err := m.Serialize()
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(wire) {
			t.Fatalf("nlri=%d: round trip changed the wire bytes", n)
		}
	}
}
