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
	"encoding/binary"
	"math/rand"
	"net"
	"net/netip"
	"testing"
)

// legacyDecodeNLRIs is the per-prefix-allocation decode loop that the slab
// decode replaced, for the NLRI field (everything after the attributes).
func legacyDecodeNLRIs(data []byte, addpathLen int) ([]PathNLRI, error) {
	eCode := uint8(BGP_ERROR_UPDATE_MESSAGE_ERROR)
	out := make([]PathNLRI, 0)
	for restlen := len(data); restlen > 0; {
		id := uint32(0)
		if addpathLen != 0 {
			if len(data) < 4 {
				return out, NewMessageError(eCode, BGP_ERROR_SUB_MALFORMED_ATTRIBUTE_LIST, nil, "NLRI length is short")
			}
			id = binary.BigEndian.Uint32(data[:4])
			data = data[4:]
		}
		n := &IPAddrPrefix{}
		if err := n.decodeFromBytes(data, net.IPv4len); err != nil {
			return out, err
		}
		restlen -= n.Len() + addpathLen
		if len(data) < n.Len() {
			return out, NewMessageError(eCode, BGP_ERROR_SUB_INVALID_NETWORK_FIELD, nil, "NLRI length is short")
		}
		if n.Len() > 32 {
			return out, NewMessageError(eCode, BGP_ERROR_SUB_INVALID_NETWORK_FIELD, nil, "NLRI length is too long")
		}
		data = data[n.Len():]
		out = append(out, PathNLRI{NLRI: n, ID: id})
	}
	return out, nil
}

// updateBody assembles withdrawn + attributes + nlri raw bytes.
func updateBody(t testing.TB, withdrawn, nlri []byte) []byte {
	t.Helper()
	nh, _ := NewPathAttributeNextHop(netip.MustParseAddr("10.0.12.1"))
	attrs := []PathAttributeInterface{
		NewPathAttributeOrigin(0),
		NewPathAttributeAsPath([]AsPathParamInterface{NewAs4PathParam(BGP_ASPATH_ATTR_TYPE_SEQ, []uint32{65001, 65002})}),
		nh,
	}
	var ab []byte
	for _, a := range attrs {
		b, err := a.Serialize()
		if err != nil {
			t.Fatal(err)
		}
		ab = append(ab, b...)
	}
	body := binary.BigEndian.AppendUint16(nil, uint16(len(withdrawn)))
	body = append(body, withdrawn...)
	body = binary.BigEndian.AppendUint16(body, uint16(len(ab)))
	body = append(body, ab...)
	return append(body, nlri...)
}

func samePathNLRIs(a, b []PathNLRI) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].NLRI.String() != b[i].NLRI.String() {
			return false
		}
	}
	return true
}

// Valid and corrupted NLRI/withdrawn fields must decode exactly as the old
// loop did: same prefixes, same ids, same success or failure.
func TestUpdateDecodeSlabMatchesLegacy(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for iter := 0; iter < 4000; iter++ {
		addPath := iter%2 == 1
		apLen := 0
		var opts []*MarshallingOption
		if addPath {
			apLen = 4
			opts = addPathOptDecode()
		}
		raw := func() []byte {
			var b []byte
			for i, n := 0, rng.Intn(40); i < n; i++ {
				if addPath {
					b = binary.BigEndian.AppendUint32(b, rng.Uint32())
				}
				bits := rng.Intn(33)
				b = append(b, byte(bits))
				for j := 0; j < (bits+7)/8; j++ {
					b = append(b, byte(rng.Intn(256)))
				}
			}
			// corrupt about a third of the inputs: flip, truncate, or append junk
			switch rng.Intn(6) {
			case 0:
				if len(b) > 0 {
					b[rng.Intn(len(b))] = byte(rng.Intn(256))
				}
			case 1:
				if len(b) > 0 {
					b = b[:rng.Intn(len(b))]
				}
			case 2:
				b = append(b, byte(rng.Intn(256)), byte(rng.Intn(256)))
			}
			return b
		}
		wd, nl := raw(), raw()
		body := updateBody(t, wd, nl)

		var got BGPUpdate
		gerr := got.DecodeFromBytes(body, opts...)

		wantW, werrW := legacyDecodeNLRIs(wd, apLen)
		wantN, werrN := legacyDecodeNLRIs(nl, apLen)
		wantErr := werrW != nil || werrN != nil
		if (gerr != nil) != wantErr {
			t.Fatalf("iter %d: error mismatch got=%v wantW=%v wantN=%v\nwd=%x nl=%x", iter, gerr, werrW, werrN, wd, nl)
		}
		if gerr == nil {
			if !samePathNLRIs(got.WithdrawnRoutes, wantW) || !samePathNLRIs(got.NLRI, wantN) {
				t.Fatalf("iter %d: decoded prefixes differ\nwd=%x nl=%x", iter, wd, nl)
			}
			if got.WithdrawnRoutes == nil || got.NLRI == nil {
				t.Fatalf("iter %d: empty fields must stay non-nil", iter)
			}
		}
	}
}

func addPathOptDecode() []*MarshallingOption {
	return []*MarshallingOption{{AddPath: map[Family]BGPAddPathMode{RF_IPv4_UC: BGP_ADD_PATH_BOTH}}}
}

// Prefixes of one UPDATE share a backing array but must not alias: changing
// one must leave the others untouched.
func TestUpdateDecodeSlabNoAliasing(t *testing.T) {
	var nl []byte
	for _, p := range []string{"10.0.0.0/8", "10.1.0.0/16", "10.1.2.0/24", "192.0.2.0/24"} {
		pf := netip.MustParsePrefix(p)
		nl = append(nl, byte(pf.Bits()))
		a := pf.Addr().As4()
		nl = append(nl, a[:(pf.Bits()+7)/8]...)
	}
	var u BGPUpdate
	if err := u.DecodeFromBytes(updateBody(t, nil, nl)); err != nil {
		t.Fatal(err)
	}
	if len(u.NLRI) != 4 {
		t.Fatalf("got %d prefixes", len(u.NLRI))
	}
	u.NLRI[0].NLRI.(*IPAddrPrefix).Prefix = netip.MustParsePrefix("203.0.113.0/24")
	want := []string{"203.0.113.0/24", "10.1.0.0/16", "10.1.2.0/24", "192.0.2.0/24"}
	for i, w := range want {
		if got := u.NLRI[i].NLRI.String(); got != w {
			t.Errorf("prefix %d = %s, want %s", i, got, w)
		}
	}
	seen := map[*IPAddrPrefix]bool{}
	for _, n := range u.NLRI {
		p := n.NLRI.(*IPAddrPrefix)
		if seen[p] {
			t.Fatal("two NLRI entries share one pointer")
		}
		seen[p] = true
	}
}
