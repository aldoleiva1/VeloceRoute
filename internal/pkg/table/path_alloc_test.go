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

package table

import (
	"bytes"
	"math"
	"math/rand"
	"net/netip"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// The functions below are the implementations that the allocation work
// replaced. They are the reference the new code must match.

func legacyCloneAsPath(asAttr *bgp.PathAttributeAsPath) *bgp.PathAttributeAsPath {
	newASparams := make([]bgp.AsPathParamInterface, len(asAttr.Value))
	for i, param := range asAttr.Value {
		asList := param.GetAS()
		as := make([]uint32, len(asList))
		copy(as, asList)
		newASparams[i] = bgp.NewAs4PathParam(param.GetType(), as)
	}
	return bgp.NewPathAttributeAsPath(newASparams)
}

func legacyPrependAsn(path *Path, asn uint32, repeat uint8, confed bool) {
	var segType uint8
	if confed {
		segType = bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SEQ
	} else {
		segType = bgp.BGP_ASPATH_ATTR_TYPE_SEQ
	}
	original := path.GetAsPath()
	asns := make([]uint32, repeat)
	for i := range asns {
		asns[i] = asn
	}
	var asPath *bgp.PathAttributeAsPath
	if original == nil {
		asPath = bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{})
	} else {
		asPath = legacyCloneAsPath(original)
	}
	if len(asPath.Value) > 0 {
		param := asPath.Value[0]
		asList := param.GetAS()
		if param.GetType() == segType {
			n := min(int(repeat), max(255-len(asList), 0))
			asPath.Value[0] = bgp.NewAs4PathParam(segType, slices.Concat(asns[:n], asList))
			asns = asns[n:]
		}
	}
	if len(asns) > 0 {
		p := bgp.NewAs4PathParam(segType, asns)
		asPath.Value = append([]bgp.AsPathParamInterface{p}, asPath.Value...)
	}
	path.setPathAttr(asPath)
}

func legacyGetPathAttrs(path *Path) []bgp.PathAttributeInterface {
	deleted := NewBitmap(math.MaxUint8)
	modified := make(map[uint]bgp.PathAttributeInterface)
	p := path
	for {
		for _, t := range p.dels {
			deleted.Flag(uint(t))
		}
		if p.parent == nil {
			list := PathAttrs(make([]bgp.PathAttributeInterface, 0, len(p.pathAttrs)))
			for _, a := range p.pathAttrs {
				typ := uint(a.GetType())
				if m, ok := modified[typ]; ok {
					list = append(list, m)
					delete(modified, typ)
				} else if !deleted.GetFlag(typ) {
					list = append(list, a)
				}
			}
			if len(modified) > 0 {
				for _, m := range modified {
					list = append(list, m)
				}
				sort.Sort(list)
			}
			return list
		} else {
			for _, a := range p.pathAttrs {
				typ := uint(a.GetType())
				if _, ok := modified[typ]; !deleted.GetFlag(typ) && !ok {
					modified[typ] = a
				}
			}
		}
		p = p.parent
	}
}

func randAsPath(rng *rand.Rand, as4 bool) *bgp.PathAttributeAsPath {
	var params []bgp.AsPathParamInterface
	for i, n := 0, rng.Intn(4); i < n; i++ {
		segTypes := []uint8{bgp.BGP_ASPATH_ATTR_TYPE_SEQ, bgp.BGP_ASPATH_ATTR_TYPE_SET,
			bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SEQ, bgp.BGP_ASPATH_ATTR_TYPE_CONFED_SET}
		typ := segTypes[rng.Intn(len(segTypes))]
		// include full and nearly full segments to exercise the 255 limit
		sizes := []int{1, 2, 5, 250, 253, 254, 255}
		l := sizes[rng.Intn(len(sizes))]
		if as4 {
			as := make([]uint32, l)
			for j := range as {
				as[j] = rng.Uint32()
			}
			params = append(params, bgp.NewAs4PathParam(typ, as))
		} else {
			as := make([]uint16, l)
			for j := range as {
				as[j] = uint16(rng.Intn(65536))
			}
			params = append(params, bgp.NewAsPathParam(typ, as))
		}
	}
	return bgp.NewPathAttributeAsPath(params)
}

func newTestPath(t testing.TB, asp *bgp.PathAttributeAsPath) *Path {
	t.Helper()
	nlri, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix("192.0.2.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	nh, _ := bgp.NewPathAttributeNextHop(netip.MustParseAddr("10.0.0.1"))
	attrs := []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(0), nh}
	if asp != nil {
		attrs = append(attrs, asp)
	}
	return NewPath(bgp.RF_IPv4_UC, nil, bgp.PathNLRI{NLRI: nlri}, false, attrs, time.Now(), false)
}

func asPathBytes(t testing.TB, p *Path) []byte {
	t.Helper()
	a := p.GetAsPath()
	if a == nil {
		return nil
	}
	b, err := a.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// PrependAsn must produce the same AS_PATH as the deep-copy version for any
// starting path, segment type, repeat count and 2- or 4-octet segments.
func TestPrependAsnMatchesLegacy(t *testing.T) {
	rng := rand.New(rand.NewSource(21))
	for iter := 0; iter < 3000; iter++ {
		var asp *bgp.PathAttributeAsPath
		if rng.Intn(5) != 0 {
			asp = randAsPath(rng, rng.Intn(2) == 0)
		}
		asn := rng.Uint32()
		repeat := uint8(rng.Intn(6))
		if rng.Intn(10) == 0 {
			repeat = uint8(250 + rng.Intn(6)) // overflow into a second segment
		}
		confed := rng.Intn(4) == 0

		got, want := newTestPath(t, asp), newTestPath(t, asp)
		got.PrependAsn(asn, repeat, confed)
		legacyPrependAsn(want, asn, repeat, confed)

		if !bytes.Equal(asPathBytes(t, got), asPathBytes(t, want)) {
			t.Fatalf("iter %d: wire differs (repeat=%d confed=%v)\n got %x\nwant %x", iter, repeat, confed, asPathBytes(t, got), asPathBytes(t, want))
		}
		if got.GetAsPath().String() != want.GetAsPath().String() {
			t.Fatalf("iter %d: String differs", iter)
		}
		if got.GetAsPathLen() != want.GetAsPathLen() {
			t.Fatalf("iter %d: AsPathLen differs", iter)
		}
		// every segment must be a 4-octet one, as the deep copy made them
		for _, p := range got.GetAsPath().Value {
			if _, ok := p.(*bgp.As4PathParam); !ok {
				t.Fatalf("iter %d: segment %T is not As4PathParam", iter, p)
			}
		}
	}
}

// The original path must not change when a derived path prepends to its
// AS_PATH, even though unchanged segments are now shared.
func TestPrependAsnLeavesOriginalUntouched(t *testing.T) {
	rng := rand.New(rand.NewSource(22))
	for iter := 0; iter < 500; iter++ {
		asp := randAsPath(rng, true)
		orig := newTestPath(t, asp)
		before := asPathBytes(t, orig)
		derived := orig.Clone(false)
		derived.PrependAsn(65000, uint8(1+rng.Intn(4)), false)
		derived.PrependAsn(65001, 1, false)
		if !bytes.Equal(before, asPathBytes(t, orig)) {
			t.Fatalf("iter %d: original AS_PATH changed", iter)
		}
	}
}

func TestGetPathAttrsMatchesLegacy(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	for iter := 0; iter < 2000; iter++ {
		base := newTestPath(t, randAsPath(rng, true))
		p := base
		// build a chain of derived paths that add, replace and delete attributes
		for d, n := 0, rng.Intn(4); d < n; d++ {
			p = p.Clone(false)
			switch rng.Intn(4) {
			case 0:
				p.PrependAsn(65000+uint32(d), 1, false)
			case 1:
				p.setPathAttr(bgp.NewPathAttributeMultiExitDisc(rng.Uint32()))
			case 2:
				p.setPathAttr(bgp.NewPathAttributeLocalPref(rng.Uint32()))
			case 3:
				p.delPathAttr(bgp.BGP_ATTR_TYPE_ORIGIN)
			}
		}
		got, want := p.GetPathAttrs(), legacyGetPathAttrs(p)
		if len(got) != len(want) {
			t.Fatalf("iter %d: %d attrs, want %d", iter, len(got), len(want))
		}
		for i := range got {
			if got[i].GetType() != want[i].GetType() || got[i].String() != want[i].String() {
				t.Fatalf("iter %d attr %d: got %s want %s", iter, i, got[i], want[i])
			}
		}
	}
}

func BenchmarkPrependAsn(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	asns := make([]uint32, 4)
	for i := range asns {
		asns[i] = rng.Uint32()
	}
	asp := bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, asns)})
	for _, tc := range []struct {
		name string
		f    func(*Path)
	}{
		{"new", func(p *Path) { p.PrependAsn(65000, 1, false) }},
		{"legacy", func(p *Path) { legacyPrependAsn(p, 65000, 1, false) }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				p := newTestPath(b, asp).Clone(false)
				tc.f(p)
			}
		})
	}
}

func BenchmarkGetPathAttrs(b *testing.B) {
	p := newTestPath(b, bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{1, 2, 3})}))
	for _, tc := range []struct {
		name string
		f    func(*Path) []bgp.PathAttributeInterface
	}{
		{"new", (*Path).GetPathAttrs},
		{"legacy", legacyGetPathAttrs},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = tc.f(p)
			}
		})
	}
}
