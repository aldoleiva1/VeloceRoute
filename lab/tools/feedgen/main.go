// feedgen injects a synthetic full-table IPv4 feed into a gobgpd over gRPC.
// Usage: feedgen -addr 127.0.0.1:50051 -n 900000 -batch 1000 [-share 40]
//
// -share K makes K consecutive prefixes carry one identical attribute set (as in
// real tables, where many prefixes share an AS path). The default, 1, gives every
// prefix its own attributes; that feed is unchanged so earlier results stay comparable.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net/netip"
	"os"
	"time"

	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "gobgpd gRPC address")
	n := flag.Int("n", 900000, "number of /24 prefixes")
	batch := flag.Int("batch", 1000, "paths per stream message")
	nh := flag.String("nexthop", "10.0.12.1", "next hop")
	share := flag.Int("share", 1, "consecutive prefixes that share one attribute set (1 = every prefix distinct)")
	withdraw := flag.Bool("withdraw", false, "withdraw the same feed instead of adding")
	flag.Parse()
	if *share < 1 {
		fatal(fmt.Errorf("-share must be >= 1"))
	}

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal(err)
	}
	defer conn.Close()
	client := api.NewGoBgpServiceClient(conn)
	ctx := context.Background()
	stream, err := client.AddPathStream(ctx)
	if err != nil {
		fatal(err)
	}

	rng := rand.New(rand.NewSource(1)) // deterministic feed across runs
	asPool := make([]uint32, 5000)
	for i := range asPool {
		asPool[i] = uint32(1000 + rng.Intn(60000))
	}
	nhAddr := netip.MustParseAddr(*nh)

	start := time.Now()
	paths := make([]*api.Path, 0, *batch)
	sent := 0
	flush := func() {
		if len(paths) == 0 {
			return
		}
		if err := stream.Send(&api.AddPathStreamRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Paths: paths}); err != nil {
			fatal(err)
		}
		sent += len(paths)
		paths = make([]*api.Path, 0, *batch)
	}

	var attrs []bgp.PathAttributeInterface
	addrU := uint32(1 << 24)
	for count := 0; count < *n; {
		first := addrU >> 24
		if first == 10 || first == 127 || first >= 224 {
			addrU += 1 << 24
			if first >= 224 {
				addrU = 1 << 24
			}
			continue
		}
		b := [4]byte{byte(addrU >> 24), byte(addrU >> 16), byte(addrU >> 8), 0}
		addrU += 256
		nlri, err := bgp.NewIPAddrPrefix(netip.PrefixFrom(netip.AddrFrom4(b), 24))
		if err != nil {
			fatal(err)
		}
		if count%*share == 0 {
			plen := 2 + rng.Intn(5)
			seq := make([]uint32, plen)
			for i := range seq {
				seq[i] = asPool[rng.Intn(len(asPool))]
			}
			nhAttr, _ := bgp.NewPathAttributeNextHop(nhAddr)
			attrs = []bgp.PathAttributeInterface{
				bgp.NewPathAttributeOrigin(0),
				bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, seq)}),
				nhAttr,
			}
			if nc := rng.Intn(4); nc > 0 {
				comms := make([]uint32, nc)
				for i := range comms {
					comms[i] = uint32(seq[0])<<16 | uint32(rng.Intn(1000))
				}
				attrs = append(attrs, bgp.NewPathAttributeCommunities(comms))
			}
		}
		p, err := apiutil.NewPath(bgp.RF_IPv4_UC, nlri, *withdraw, attrs, time.Now())
		if err != nil {
			fatal(err)
		}
		paths = append(paths, p)
		count++
		if len(paths) == *batch {
			flush()
		}
	}
	flush()
	if _, err := stream.CloseAndRecv(); err != nil {
		fatal(err)
	}
	el := time.Since(start)
	fmt.Printf("sent=%d withdraw=%v elapsed=%s rate=%.0f paths/s\n", sent, *withdraw, el.Round(time.Millisecond), float64(sent)/el.Seconds())
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "feedgen:", err); os.Exit(1) }
