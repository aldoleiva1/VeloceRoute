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
	"bytes"
	"encoding/binary"
	"math/rand"
	"net/netip"
	"sync"
	"testing"
)

// The functions below are the allocating implementations the append-style
// writer replaced. They are kept here as the reference the new code must
// match byte for byte.

func legacyToSlice(p *PathNLRI, isAddPath bool) ([]byte, error) {
	var buf []byte
	if isAddPath {
		var tmp [4]byte
		binary.BigEndian.PutUint32(tmp[:], p.ID)
		buf = append(buf, tmp[:]...)
	}
	nbuf, err := p.NLRI.Serialize()
	if err != nil {
		return nil, err
	}
	return append(buf, nbuf...), nil
}

func legacyUpdateSerialize(msg *BGPUpdate, options ...*MarshallingOption) ([]byte, error) {
	wbuf := make([]byte, 2)
	v4AddPath := IsAddPathEnabled(false, RF_IPv4_UC, options)
	for _, w := range msg.WithdrawnRoutes {
		onewbuf, err := legacyToSlice(&w, v4AddPath)
		if err != nil {
			return nil, err
		}
		wbuf = append(wbuf, onewbuf...)
	}
	binary.BigEndian.PutUint16(wbuf, uint16(len(wbuf)-2))

	o := MarshallingOption{attributes: getBGPUpdateAttributesFromMsg(msg)}
	options = append(options, &o)
	pbuf := make([]byte, 2)
	for _, p := range msg.PathAttributes {
		onepbuf, err := p.Serialize(options...)
		if err != nil {
			return nil, err
		}
		pbuf = append(pbuf, onepbuf...)
	}
	binary.BigEndian.PutUint16(pbuf, uint16(len(pbuf)-2))

	buf := append(wbuf, pbuf...)
	for _, n := range msg.NLRI {
		nbuf, err := legacyToSlice(&n, v4AddPath)
		if err != nil {
			return nil, err
		}
		buf = append(buf, nbuf...)
	}
	return buf, nil
}

func addPathOpt(on bool) []*MarshallingOption {
	if !on {
		return nil
	}
	return []*MarshallingOption{{AddPath: map[Family]BGPAddPathMode{
		RF_IPv4_UC: BGP_ADD_PATH_BOTH,
		RF_IPv6_UC: BGP_ADD_PATH_BOTH,
	}}}
}

func randPrefix(t testing.TB, rng *rand.Rand, v6 bool) *IPAddrPrefix {
	t.Helper()
	var addr netip.Addr
	bits := 0
	if v6 {
		var b [16]byte
		rng.Read(b[:])
		addr, bits = netip.AddrFrom16(b), rng.Intn(129)
	} else {
		var b [4]byte
		rng.Read(b[:])
		addr, bits = netip.AddrFrom4(b), rng.Intn(33)
	}
	p, err := NewIPAddrPrefix(netip.PrefixFrom(addr, bits).Masked())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Every prefix length, IPv4 and IPv6, with and without add-path, plus NLRI
// types that do not implement the fast path (they go through Serialize).
func TestPathNLRIAppendMatchesLegacy(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var nlris []NLRI
	for i := 0; i < 2000; i++ {
		nlris = append(nlris, randPrefix(t, rng, i%2 == 1))
	}
	for bits := 0; bits <= 32; bits++ {
		p, _ := NewIPAddrPrefix(netip.PrefixFrom(netip.MustParseAddr("203.0.113.255"), bits).Masked())
		nlris = append(nlris, p)
	}
	lp, err := NewLabeledIPAddrPrefix(netip.MustParsePrefix("192.0.2.0/24"), *NewMPLSLabelStack(16000))
	if err != nil {
		t.Fatal(err)
	}
	vp, err := NewLabeledVPNIPAddrPrefix(netip.MustParsePrefix("10.1.0.0/16"), *NewMPLSLabelStack(100), NewRouteDistinguisherTwoOctetAS(65000, 1))
	if err != nil {
		t.Fatal(err)
	}
	nlris = append(nlris, lp, vp)

	for _, addPath := range []bool{false, true} {
		for i, n := range nlris {
			pn := &PathNLRI{NLRI: n, ID: uint32(i * 977)}
			want, werr := legacyToSlice(pn, addPath)
			got, gerr := pn.appendTo(nil, addPath)
			if (werr == nil) != (gerr == nil) || !bytes.Equal(want, got) {
				t.Fatalf("addPath=%v nlri %d (%s): got %x want %x (errs %v / %v)", addPath, i, n.String(), got, want, gerr, werr)
			}
			// Appending must keep the existing contents of dst.
			pre := []byte{0xde, 0xad}
			got2, _ := pn.appendTo(append([]byte(nil), pre...), addPath)
			if !bytes.Equal(got2[:2], pre) || !bytes.Equal(got2[2:], want) {
				t.Fatalf("addPath=%v nlri %d: append clobbered dst", addPath, i)
			}
		}
	}
}

func TestIPAddrPrefixAppendMatchesSerialize(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 4000; i++ {
		p := randPrefix(t, rng, i%2 == 0)
		want, _ := p.Serialize()
		if got := p.appendNLRI(nil); !bytes.Equal(got, want) {
			t.Fatalf("%s: got %x want %x", p, got, want)
		}
	}
}

// Whole UPDATEs: withdrawn routes, attributes (including MP_REACH/MP_UNREACH
// carrying prefixes) and NLRI, with and without add-path.
func TestUpdateSerializeMatchesLegacy(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	nh, _ := NewPathAttributeNextHop(netip.MustParseAddr("10.0.12.1"))
	for iter := 0; iter < 300; iter++ {
		var wd, nl []PathNLRI
		for i, n := 0, rng.Intn(20); i < n; i++ {
			wd = append(wd, PathNLRI{NLRI: randPrefix(t, rng, false), ID: rng.Uint32()})
		}
		for i, n := 0, rng.Intn(60); i < n; i++ {
			nl = append(nl, PathNLRI{NLRI: randPrefix(t, rng, false), ID: rng.Uint32()})
		}
		var v6 []PathNLRI
		for i, n := 0, rng.Intn(10); i < n; i++ {
			v6 = append(v6, PathNLRI{NLRI: randPrefix(t, rng, true), ID: rng.Uint32()})
		}
		attrs := []PathAttributeInterface{
			NewPathAttributeOrigin(uint8(rng.Intn(3))),
			NewPathAttributeAsPath([]AsPathParamInterface{NewAs4PathParam(BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{65001, rng.Uint32()})}),
			nh,
		}
		if len(v6) > 0 {
			mp, err := NewPathAttributeMpReachNLRI(RF_IPv6_UC, v6, netip.MustParseAddr("2001:db8::1"))
			if err != nil {
				t.Fatal(err)
			}
			un, err := NewPathAttributeMpUnreachNLRI(RF_IPv6_UC, v6)
			if err != nil {
				t.Fatal(err)
			}
			attrs = append(attrs, mp, un)
		}
		for _, addPath := range []bool{false, true} {
			opts := addPathOpt(addPath)
			upd := NewBGPUpdateMessage(wd, attrs, nl).Body.(*BGPUpdate)
			want, werr := legacyUpdateSerialize(upd, opts...)
			got, gerr := upd.Serialize(opts...)
			if (werr == nil) != (gerr == nil) || !bytes.Equal(want, got) {
				t.Fatalf("iter %d addPath=%v: wire bytes differ\n got %x\nwant %x", iter, addPath, got, want)
			}
			wantLen := binary.BigEndian.Uint16(want[2+int(binary.BigEndian.Uint16(want)):])
			if upd.TotalPathAttributeLen != wantLen {
				t.Fatalf("iter %d: TotalPathAttributeLen %d, want %d", iter, upd.TotalPathAttributeLen, wantLen)
			}
		}
	}
}

// legacyMessageSerialize is the pre-change BGPMessage.Serialize for UPDATEs:
// header prepended to a separately built body.
func legacyMessageSerialize(msg *BGPMessage, options ...*MarshallingOption) ([]byte, error) {
	b, err := legacyUpdateSerialize(msg.Body.(*BGPUpdate), options...)
	if err != nil {
		return nil, err
	}
	length := msg.Header.Len
	if length == 0 {
		maxLen := BGP_MAX_MESSAGE_LENGTH
		if IsExtendedMessageSerialization(options) {
			maxLen = BGP_MAX_EXTENDED_MESSAGE_LENGTH
		}
		if BGP_HEADER_LENGTH+len(b) > maxLen {
			return nil, NewMessageError(0, 0, nil, "too long")
		}
		length = BGP_HEADER_LENGTH + uint16(len(b))
	}
	return append(msg.Header.serialize(length), b...), nil
}

func TestMessageSerializeMatchesLegacy(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	nh, _ := NewPathAttributeNextHop(netip.MustParseAddr("10.0.12.1"))
	for iter := 0; iter < 300; iter++ {
		var nl []PathNLRI
		// Vary size so some messages cross the 4096 limit.
		for i, n := 0, rng.Intn(900); i < n; i++ {
			nl = append(nl, PathNLRI{NLRI: randPrefix(t, rng, false)})
		}
		asns := make([]uint32, rng.Intn(100)) // long AS paths exercise extended-length attrs
		for i := range asns {
			asns[i] = rng.Uint32()
		}
		attrs := []PathAttributeInterface{
			NewPathAttributeOrigin(0),
			NewPathAttributeAsPath([]AsPathParamInterface{NewAs4PathParam(BGP_ASPATH_ATTR_TYPE_SEQ, asns)}),
			nh,
		}
		for _, ext := range []bool{false, true} {
			opts := []*MarshallingOption{{ExtendedMessage: ext}}
			m := NewBGPUpdateMessage(nil, attrs, nl)
			want, werr := legacyMessageSerialize(m, opts...)
			got, gerr := m.Serialize(opts...)
			if (werr == nil) != (gerr == nil) || !bytes.Equal(want, got) {
				t.Fatalf("iter %d ext=%v: differ (err got=%v want=%v)", iter, ext, gerr, werr)
			}
		}
	}
}

// Concurrent serialization of one shared message must be race-free and
// stable now that scratch buffers come from a pool.
func TestSerializeConcurrentPool(t *testing.T) {
	nh, _ := NewPathAttributeNextHop(netip.MustParseAddr("10.0.12.1"))
	attrs := []PathAttributeInterface{
		NewPathAttributeOrigin(0),
		NewPathAttributeAsPath([]AsPathParamInterface{NewAs4PathParam(BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{65001, 65002, 65003})}),
		nh,
	}
	pfx, err := NewIPAddrPrefix(netip.MustParsePrefix("192.0.2.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewBGPUpdateMessage(nil, attrs, []PathNLRI{{NLRI: pfx}})
	want, err := m.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	// The first call fills the cached length fields; later calls only read.
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				got, err := m.Serialize()
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("concurrent serialize mismatch: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
