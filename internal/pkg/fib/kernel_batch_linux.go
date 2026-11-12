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

package fib

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"time"

	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Batched route programming.
//
// One netlink request per route costs a send and a receive system call, and
// allocations, for each of 900,000 routes. The kernel does not need that: it
// handles every message in a buffer, in order, and replies per message. So we
// put many route messages into one buffer, send it with one call, and read the
// replies with a few. Two details keep the traffic down:
//
//   - Only the last message of a batch asks for an acknowledgement. The kernel
//     always answers a message that fails, with the error, and it handles messages
//     in order, so when the last acknowledgement arrives every earlier message is
//     done and the errors that came before it say which ones failed.
//   - Messages are built straight into one reused buffer, with no per-route
//     objects.
const (
	maxBatchBytes = 64 << 10 // bytes per sendto; well below the socket send buffer
	maxBatchMsgs  = 1024     // messages per sendto
	batchTimeout  = 30 * time.Second
)

var native = binary.NativeEndian

// batchSocket is a netlink socket opened in the namespace we program, used only
// for batches, plus the buffers reused between them.
type batchSocket struct {
	sock *nl.NetlinkSocket
	fd   int
	seq  uint32
	out  []byte
	in   []byte

	// What is in out right now: for each message the index of its op and
	// whether it is a delete, and where the last message starts.
	pending    []int
	deleting   []bool
	lastMsgOff int
}

func (k *kernelProgrammer) openBatchSocket() (*batchSocket, error) {
	ns := netns.None()
	if k.ns != nil {
		ns = *k.ns
	}
	s, err := nl.GetNetlinkSocketAt(ns, netns.None(), unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("open netlink batch socket: %w", err)
	}
	tv := unix.NsecToTimeval(int64(batchTimeout))
	if err := s.SetReceiveTimeout(&tv); err != nil {
		s.Close()
		return nil, err
	}
	return &batchSocket{sock: s, fd: s.GetFd(), out: make([]byte, 0, maxBatchBytes+4096), in: make([]byte, 64<<10)}, nil
}

// Apply implements BatchProgrammer.
func (k *kernelProgrammer) Apply(ops []Op) []error {
	errs := make([]error, len(ops))
	k.bmu.Lock()
	defer k.bmu.Unlock()
	if k.bsock == nil {
		s, err := k.openBatchSocket()
		if err != nil {
			for i := range errs {
				errs[i] = err
			}
			return errs
		}
		k.bsock = s
	}
	b := k.bsock
	for start := 0; start < len(ops); {
		end := k.fillBatch(b, ops, errs, start)
		if len(b.pending) > 0 {
			if err := k.sendBatch(b, errs); err != nil {
				// The socket is in an unknown state: drop it so the next call
				// starts clean, and fail what was in flight.
				for _, idx := range b.pending {
					if errs[idx] == nil {
						errs[idx] = err
					}
				}
				b.sock.Close()
				k.bsock = nil
				for i := end; i < len(ops); i++ {
					errs[i] = err
				}
				return errs
			}
		}
		start = end
	}
	return errs
}

// fillBatch encodes ops from start into the socket's buffer until it is full,
// and returns the index of the first op not encoded. An op that cannot be
// encoded gets its error and is left out of the batch.
func (k *kernelProgrammer) fillBatch(b *batchSocket, ops []Op, errs []error, start int) int {
	b.out = b.out[:0]
	b.pending = b.pending[:0]
	i := start
	for ; i < len(ops) && len(b.pending) < maxBatchMsgs && len(b.out) < maxBatchBytes; i++ {
		b.seq++
		off := len(b.out)
		var err error
		b.out, err = k.appendRoute(b.out, b.seq, ops[i])
		if err != nil {
			b.out = b.out[:off]
			b.seq--
			errs[i] = err
			continue
		}
		b.pending = append(b.pending, i)
	}
	return i
}

// sendBatch sends the encoded messages and collects the results into errs.
func (k *kernelProgrammer) sendBatch(b *batchSocket, errs []error) error {
	// Only the last message asks for an acknowledgement.
	lastOff := b.lastMsgOff
	native.PutUint16(b.out[lastOff+6:], native.Uint16(b.out[lastOff+6:])|unix.NLM_F_ACK)
	lastSeq := b.seq
	firstSeq := lastSeq - uint32(len(b.pending)) + 1
	if err := unix.Sendto(b.fd, b.out, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("send route batch: %w", err)
	}
	for {
		n, _, err := unix.Recvfrom(b.fd, b.in, 0)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("read route batch results: %w", err)
		}
		for off := 0; off+unix.NLMSG_HDRLEN <= n; {
			msgLen := int(native.Uint32(b.in[off:]))
			if msgLen < unix.NLMSG_HDRLEN || off+msgLen > n {
				return fmt.Errorf("malformed netlink reply")
			}
			typ := native.Uint16(b.in[off+4:])
			seq := native.Uint32(b.in[off+8:])
			if typ == unix.NLMSG_ERROR && msgLen >= unix.NLMSG_HDRLEN+4 {
				e := int32(native.Uint32(b.in[off+unix.NLMSG_HDRLEN:]))
				if seq >= firstSeq && seq <= lastSeq {
					idx := b.pending[seq-firstSeq]
					if e != 0 {
						errs[idx] = batchError(syscall.Errno(-e), b.deleting[seq-firstSeq])
					}
					if seq == lastSeq {
						return nil
					}
				}
			}
			off += (msgLen + 3) &^ 3
		}
	}
}

// batchError gives an errno the same meaning Replace and Delete give it.
func batchError(errno syscall.Errno, isDelete bool) error {
	switch {
	case isDelete && errors.Is(errno, unix.ESRCH):
		return nil // already gone
	case !isDelete && (errors.Is(errno, unix.ENETUNREACH) || errors.Is(errno, unix.ENODEV)):
		return fmt.Errorf("%w: %v", ErrNextHopUnreachable, errno)
	}
	return errno
}

// ---- message encoding ----

const (
	rtaDst       = unix.RTA_DST
	rtaOif       = unix.RTA_OIF
	rtaPriority  = unix.RTA_PRIORITY
	rtaGateway   = unix.RTA_GATEWAY
	rtaMultipath = unix.RTA_MULTIPATH
	rtaTable     = unix.RTA_TABLE
)

func align4(n int) int { return (n + 3) &^ 3 }

func appendU16(b []byte, v uint16) []byte { return native.AppendUint16(b, v) }
func appendU32(b []byte, v uint32) []byte { return native.AppendUint32(b, v) }

// appendAttr appends a netlink attribute with a payload of the given bytes.
func appendAttr(b []byte, typ uint16, payload []byte) []byte {
	b = appendU16(b, uint16(4+len(payload)))
	b = appendU16(b, typ)
	b = append(b, payload...)
	for pad := align4(len(payload)) - len(payload); pad > 0; pad-- {
		b = append(b, 0)
	}
	return b
}

func appendAttrU32(b []byte, typ uint16, v uint32) []byte {
	b = appendU16(b, 8)
	b = appendU16(b, typ)
	return appendU32(b, v)
}

// appendRoute appends one RTM_NEWROUTE (replace) or RTM_DELROUTE message. It
// fills the socket's bookkeeping through k.bsock via the closure below.
func (k *kernelProgrammer) appendRoute(b []byte, seq uint32, op Op) ([]byte, error) {
	bs := k.bsock
	p := op.Route.Prefix
	if !p.IsValid() {
		return b, errors.New("route has an invalid prefix")
	}
	if !op.Delete && len(op.Route.NextHops) == 0 {
		return b, errors.New("route has no next hop")
	}
	family := uint8(unix.AF_INET)
	if p.Addr().Is6() {
		family = unix.AF_INET6
	}

	off := len(b)
	// nlmsghdr: length (patched), type, flags, seq, port id.
	typ, flags := uint16(unix.RTM_NEWROUTE), uint16(unix.NLM_F_REQUEST|unix.NLM_F_CREATE|unix.NLM_F_REPLACE)
	if op.Delete {
		typ, flags = unix.RTM_DELROUTE, unix.NLM_F_REQUEST
	}
	b = appendU32(b, 0)
	b = appendU16(b, typ)
	b = appendU16(b, flags)
	b = appendU32(b, seq)
	b = appendU32(b, 0)
	// rtmsg: family, dst len, src len, tos, table, protocol, scope, type, flags.
	table := uint8(unix.RT_TABLE_UNSPEC)
	if k.table < 256 {
		table = uint8(k.table)
	}
	scope, rtype := uint8(unix.RT_SCOPE_UNIVERSE), uint8(unix.RTN_UNICAST)
	if op.Delete {
		scope, rtype = unix.RT_SCOPE_NOWHERE, 0
	}
	b = append(b, family, uint8(p.Bits()), 0, 0, table, uint8(k.proto), scope, rtype)
	b = appendU32(b, 0)

	b = appendAttr(b, rtaDst, p.Addr().AsSlice())
	if !op.Delete {
		switch hops := op.Route.NextHops; len(hops) {
		case 1:
			gw, idx, err := k.gateway(hops[0])
			if err != nil {
				return b, err
			}
			b = appendAttr(b, rtaGateway, ipBytes(gw))
			if idx != 0 {
				b = appendAttrU32(b, rtaOif, uint32(idx))
			}
		default:
			// RTA_MULTIPATH: a list of rtnexthop structures, each followed by
			// its own attributes.
			mpOff := len(b)
			b = appendU16(b, 0) // patched below
			b = appendU16(b, rtaMultipath)
			for _, h := range hops {
				gw, idx, err := k.gateway(h)
				if err != nil {
					return b, err
				}
				g := ipBytes(gw)
				nhLen := 8 + 4 + align4(len(g))
				b = appendU16(b, uint16(nhLen)) // rtnh_len
				b = append(b, 0, 0)             // rtnh_flags, rtnh_hops (weight 1)
				b = appendU32(b, uint32(idx))   // rtnh_ifindex
				b = appendAttr(b, rtaGateway, g)
			}
			native.PutUint16(b[mpOff:], uint16(len(b)-mpOff))
		}
	}
	if k.table >= 256 {
		b = appendAttrU32(b, rtaTable, uint32(k.table))
	}
	b = appendAttrU32(b, rtaPriority, uint32(k.metric))

	native.PutUint32(b[off:], uint32(len(b)-off))
	bs.lastMsgOff = off
	bs.deleting = append(bs.deleting[:len(bs.pending)], op.Delete)
	return b, nil
}

// ipBytes returns the 4 or 16 address bytes the kernel expects.
func ipBytes(ip []byte) []byte {
	if len(ip) == 16 {
		if a, ok := netip.AddrFromSlice(ip); ok && a.Is4In6() {
			x := a.As4()
			return x[:]
		}
	}
	return ip
}
