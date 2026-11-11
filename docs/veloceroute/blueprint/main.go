// Blueprint: embed a WireGuard tunnel and the GoBGP v4 control plane in one process.
// Reference only. It compiles against this repository (see ../blueprint.md) but has no
// error recovery, kernel FIB sync, or policy. Run on Linux with CAP_NET_ADMIN.
//
// Required environment (nothing secret is hard-coded):
//
//	WG_PRIVATE_KEY_HEX   local WireGuard private key, hex encoded
//	WG_PEER_PUBLIC_HEX   remote WireGuard public key, hex encoded
//	WG_PEER_ENDPOINT     remote endpoint, e.g. 203.0.113.10:51820
//	RS_ENDPOINTS         optional, comma separated Route Server endpoint ENI addresses
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	api "github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/server"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// STEP 1: WireGuard (userspace). The interface address (192.168.10.1/30) and
	// link up are done outside this sample, e.g. with netlink or `ip`.
	tunDev, err := tun.CreateTUN("wg0", 1420)
	if err != nil {
		log.Fatalf("create TUN: %v", err)
	}
	wg := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "[wg] "))
	defer wg.Close()

	ipc := fmt.Sprintf("private_key=%s\nlisten_port=51820\npublic_key=%s\nendpoint=%s\npersistent_keepalive_interval=25\nallowed_ip=192.168.10.2/32\n",
		mustEnv("WG_PRIVATE_KEY_HEX"), mustEnv("WG_PEER_PUBLIC_HEX"), mustEnv("WG_PEER_ENDPOINT"))
	if err := wg.IpcSet(ipc); err != nil {
		log.Fatalf("configure WireGuard: %v", err)
	}
	if err := wg.Up(); err != nil {
		log.Fatalf("bring WireGuard up: %v", err)
	}

	// STEP 2: GoBGP v4 control plane.
	bgp := server.NewBgpServer()
	go bgp.Serve()
	if err := bgp.StartBgp(ctx, &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        64512,
			RouterId:   "172.16.0.1",
			ListenPort: 179,
			// Optional: BindToDevice restricts the listener to one interface.
		},
	}); err != nil {
		log.Fatalf("start BGP: %v", err)
	}

	// STEP 3: peer across the tunnel. v4 uses AddPeer, not AddNeighbor.
	if err := bgp.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "192.168.10.2", PeerAsn: 65001},
		Transport: &api.Transport{LocalAddress: "192.168.10.1"},
	}}); err != nil {
		log.Fatalf("add WireGuard peer: %v", err)
	}

	// STEP 4 (Pattern A): peer with the VPC Route Server endpoints. The device must
	// initiate the session (the endpoint does not), so passive mode stays off.
	// Create one Route Server peer in AWS per endpoint. Advertise at most 100 routes.
	if eps := os.Getenv("RS_ENDPOINTS"); eps != "" {
		for _, ep := range strings.Split(eps, ",") {
			if err := bgp.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
				Conf: &api.PeerConf{NeighborAddress: strings.TrimSpace(ep), PeerAsn: 65000, Description: "aws-route-server"},
			}}); err != nil {
				log.Fatalf("add Route Server peer %s: %v", ep, err)
			}
		}
	}

	log.Println("WireGuard and GoBGP running; Ctrl-C to stop")
	<-ctx.Done()
	_ = bgp.StopBgp(context.Background(), &api.StopBgpRequest{})
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing environment variable %s", k)
	}
	return v
}
