// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package netfabric wraps Linux host networking (bridges, taps, NAT)
// behind a small testable interface so the agent's VM manager can attach
// VMs to Linux bridges without depending on netlink directly. The
// concrete implementation is Linux-only; on other platforms New returns
// an unsupported stub so the agent still cross-compiles on developer
// machines.
package netfabric

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// tapPrefix is the host interface-name prefix every Otherix-managed tap
// device carries. NIC.TapName builds names from it, and ListTaps filters
// host tuntap links by it.
const tapPrefix = "ot"

// WireGuardMTU is the otwg0 link MTU. The physical underlay is 1500; WireGuard's
// own encapsulation costs 60 bytes (20 IP + 8 UDP + 32 WG), leaving 1440. The
// overlay inner MTU (store.OverlayMTU, 1390) is in turn 1440 minus the 50-byte
// VXLAN encap, so a VM frame fits end to end without fragmentation.
const WireGuardMTU = 1440

// allowedModels is the set of QEMU NIC models the fabric recognises.
var allowedModels = map[string]struct{}{
	"virtio":  {},
	"e1000":   {},
	"e1000e":  {},
	"rtl8139": {},
}

// Fabric materialises and tears down host networking primitives for VM
// interfaces. The Linux implementation serialises every mutating method
// with an internal mutex, so it is safe for concurrent use: one instance
// is shared between the network reconciler goroutine and the VM-manager
// handler goroutines.
type Fabric interface {
	// EnsureBridge creates the named Linux bridge if absent, sets its MTU
	// when mtu is positive, and brings it up. It is idempotent.
	EnsureBridge(name string, mtu int) error
	// RemoveBridge deletes the named bridge. It returns nil if the bridge
	// is already absent.
	RemoveBridge(name string) error
	// BridgeExists reports whether a bridge with the given name exists.
	BridgeExists(name string) (bool, error)

	// CreateTap creates a tap device with the given name and MTU.
	CreateTap(name string, mtu int) error
	// AttachTap enslaves the tap device to the named bridge.
	AttachTap(tap, bridge string) error
	// DeleteTap removes the named tap device.
	DeleteTap(name string) error
	// ListTaps returns the names of every Otherix-managed tap device on
	// the host, that is every tuntap link whose name carries the "ot"
	// prefix used by NIC.TapName. The names are returned sorted.
	ListTaps() ([]string, error)

	// EnsureGatewayAddr assigns addr to the named bridge, idempotently.
	EnsureGatewayAddr(bridge string, addr netip.Prefix) error
	// RemoveGatewayAddr removes addr from the named bridge.
	RemoveGatewayAddr(bridge string, addr netip.Prefix) error
	// EnsureMasquerade installs a masquerade rule for traffic that entered via
	// bridge with a source address in subnet, egressing via egressIface,
	// idempotently. Matching the input bridge keeps the rule scoped to the
	// one network: managed-bridge subnets may overlap across networks, so a
	// rule matched on source subnet alone could SNAT another network's traffic.
	EnsureMasquerade(subnet netip.Prefix, bridge, egressIface string) error
	// RemoveMasquerade removes every masquerade rule for subnet on bridge.
	RemoveMasquerade(subnet netip.Prefix, bridge string) error
	// EnableIPForwarding turns on IPv4 forwarding in the agent's network
	// namespace (net.ipv4.ip_forward=1). Idempotent. Required before a node
	// can route overlay VM egress out its uplink.
	EnableIPForwarding() error

	// EnsureAnycastGateway pins mac as the bridge link's hardware address and
	// assigns addr as a /32 to it, idempotently. The bridge becomes the
	// VM-facing anycast L3 gateway: uniform addr+mac on every node so a VM's
	// cached next-hop survives live migration and is always answered locally.
	EnsureAnycastGateway(bridge string, addr netip.Addr, mac net.HardwareAddr) error
	// RemoveAnycastGateway removes addr (/32) from the bridge. nil when the
	// bridge or the address is already absent.
	RemoveAnycastGateway(bridge string, addr netip.Addr) error

	// EnsureVeth idempotently materialises an ingress gateway's veth pair for one
	// overlay membership (host end = tenant IP + unicast MAC, peer end enslaved to
	// the bridge) and applies the host-end sysctls. RemoveVeth deletes the pair by
	// its host-end name, idempotently.
	EnsureVeth(cfg VethConfig) error
	RemoveVeth(host string) error

	// EnsureMasqueradeIface installs a masquerade rule for traffic entering via
	// inIface and leaving via egressIface (empty = host default route),
	// idempotently. CIDR-independent; used for overlay egress.
	EnsureMasqueradeIface(inIface, egressIface string) error
	// RemoveMasqueradeIface removes every masquerade rule tagged for inIface.
	RemoveMasqueradeIface(inIface string) error

	// EnsureBridgeRoute installs a link-scoped connected route for subnet via the
	// named bridge, idempotently, so the node routes return traffic to VMs on the
	// overlay back to the bridge (the anycast gateway is a link-local /32 and
	// gives the node no route to the overlay subnet otherwise).
	EnsureBridgeRoute(subnet netip.Prefix, bridge string) error
	// RemoveBridgeRoute removes the link-scoped connected route for subnet on the
	// named bridge. Idempotent: a no-op (nil) when the route or bridge is already
	// absent, so reaping a stale route after a subnet change is safe to retry.
	RemoveBridgeRoute(subnet netip.Prefix, bridge string) error

	// EnsureVXLAN creates the otvx<vni> VXLAN VTEP if absent, sets its MTU and
	// brings it up. Learning is off (the FDB is controller-authoritative);
	// remotes are supplied only through FDBAppend. It is idempotent.
	EnsureVXLAN(cfg VXLANConfig) error
	// RemoveVXLAN deletes the otvx<vni> VTEP. It returns nil if absent.
	RemoveVXLAN(vni uint32) error
	// VXLANExists reports whether the otvx<vni> VTEP exists.
	VXLANExists(vni uint32) (bool, error)

	// FDBAppend installs a MAC -> dst VTEP entry in the otvx<vni> VTEP's
	// kernel FDB, idempotently.
	FDBAppend(vni uint32, e FDBEntry) error
	// FDBDelete removes a MAC -> dst VTEP entry from the otvx<vni> FDB. It
	// returns nil when the entry is already absent.
	FDBDelete(vni uint32, e FDBEntry) error
	// FDBList returns the MAC -> dst VTEP entries in the otvx<vni> FDB.
	FDBList(vni uint32) ([]FDBEntry, error)

	// LinkState returns the observed kernel state of the named link: whether it
	// is administratively up, its MTU, and the unicast addresses assigned to it.
	// An absent link reports a zero LinkState (Up=false) with a nil error,
	// mirroring BridgeExists / VXLANExists, so callers gate on Up rather than
	// distinguishing "absent" from "down".
	LinkState(name string) (LinkState, error)

	// EnsureWireGuard creates the named WireGuard link if absent, configures
	// its private key and listen port, assigns Address, sets MTU and brings it
	// up. It is idempotent. Peers are managed separately via SetWireGuardPeers.
	EnsureWireGuard(cfg WGConfig) error
	// RemoveWireGuard deletes the named WireGuard link. It returns nil if the
	// link is already absent.
	RemoveWireGuard(name string) error
	// WireGuardExists reports whether a WireGuard link of the given name
	// exists. A link of the same name but a different type reports false.
	WireGuardExists(name string) (bool, error)
	// SetWireGuardPeers atomically replaces the full peer set on the named
	// interface (wgctrl ReplacePeers). PersistentKeepalive is applied per peer.
	// The CP recomputes the whole peer list each heartbeat, so the declarative
	// replace fits the reconciler better than per-peer add/delete.
	SetWireGuardPeers(name string, peers []WGPeer) error
	// WireGuardPeers returns the configured peers on the named interface,
	// sorted by public key, for observed-state reporting.
	WireGuardPeers(name string) ([]WGPeer, error)
	// WireGuardPeerHandshakes returns the observed per-peer handshake state on
	// the named interface, for observed-state reporting. Peers with no
	// completed handshake report a zero LastHandshake. Distinct from
	// WireGuardPeers, which returns the configured peer set (no handshake).
	WireGuardPeerHandshakes(name string) ([]WGPeerHandshake, error)

	// SendGARP broadcasts a gratuitous ARP for mac/ip on the named bridge,
	// announcing the VM's location after a live-migration cutover so the L2
	// segment (physical switch / learning bridge) and neighbor ARP caches
	// relearn the port. ip must be IPv4; callers skip NICs without one.
	SendGARP(bridge string, mac string, ip netip.Addr) error

	// NeighborMAC resolves ip to its link-layer address in the kernel neighbor
	// (ARP/ND) table on the named device (a bridge for a hypervisor node, a gateway
	// veth host end for ingress). It returns (mac, true, nil) for a resolved entry
	// (REACHABLE / STALE / DELAY / PROBE / PERMANENT), (nil, false, nil) when ip has
	// no resolved neighbor, and (nil, false, err) on a lookup error. The
	// implementation may send a single probe datagram to provoke resolution and
	// re-read once when the first lookup finds nothing. The ingress gateway uses it
	// to bind a session credential's dial to the credential's NIC MAC: it refuses
	// the dial unless the neighbor the guest IP resolves to equals the credential
	// MAC, so a stale credential whose IP has been reassigned to a different NIC
	// fails closed.
	NeighborMAC(device string, ip netip.Addr) (net.HardwareAddr, bool, error)
}

// VXLANConfig parametrises a VXLAN VTEP. For the single-agent N1b scaffold
// the VTEP binds to loopback (Local 127.0.0.1); N2 rebinds it to otwg0.
type VXLANConfig struct {
	VNI    uint32     // device name otvx<vni> per the overlay naming convention
	Local  netip.Addr // local VTEP source IP (otwg0 overlay IP once rebound)
	Port   uint16     // UDP dstport (IANA VXLAN 4789)
	MTU    int        // inner MTU (1390 for overlay)
	Master string     // bridge to enslave the VTEP into ("" leaves it unenslaved)
}

// VethConfig describes one ingress-gateway veth pair. The root-namespace host end
// carries the membership's tenant IP (Addr, with the overlay subnet prefix so the
// kernel installs an on-link route to the whole subnet) and its unicast MAC; the
// peer end is enslaved to Bridge as an ordinary bridge port. HostName / PeerName
// are the VNI-derived otvg<vni> / otvgp<vni> so teardown and adopt need no stored
// state.
type VethConfig struct {
	HostName string
	PeerName string
	Bridge   string
	Addr     netip.Prefix
	MAC      net.HardwareAddr
	MTU      int
}

// LinkState is the observed kernel state of a network link, returned by
// Fabric.LinkState. It is observed output: the reconciler compares it against
// desired state (e.g. otwg0 up and carrying the node's overlay IP) rather than
// the fabric encoding any readiness policy.
type LinkState struct {
	Up    bool           // link is administratively up (IFF_UP)
	MTU   int            // link MTU
	Addrs []netip.Prefix // unicast addresses assigned to the link
}

// FDBEntry is one MAC -> remote VTEP mapping in a VXLAN VTEP's kernel FDB.
type FDBEntry struct {
	MAC net.HardwareAddr
	Dst netip.Addr // remote VTEP IP (127.0.0.1 for the N1b loopback scaffold)
}

// WGConfig parametrises the otwg0 WireGuard interface. netfabric is a pure
// applier: PrivateKey is generated at bootstrap (N2c) and supplied here - the
// fabric never generates keys. Address is assigned to the link and becomes the
// VXLAN VTEP source address when the VTEP rebinds onto otwg0 in N2c.
type WGConfig struct {
	Name       string       // "otwg0" - the single WireGuard interface, all peers
	PrivateKey wgtypes.Key  // agent's Curve25519 private key
	ListenPort int          // UDP listen port (51820)
	Address    netip.Prefix // agent overlay IP on the link (e.g. 10.42.0.<n>/16)
	MTU        int          // link MTU (1440 for otwg0; see WireGuardMTU)
}

// WGPeer is one remote agent in the WireGuard mesh.
type WGPeer struct {
	PublicKey           wgtypes.Key
	Endpoint            *net.UDPAddr   // advertised host:port; nil leaves the endpoint unset (roaming)
	AllowedIPs          []netip.Prefix // remote overlay IPs routed into the tunnel
	PersistentKeepalive time.Duration  // keepalive interval (25s); zero leaves it unset
}

// WGPeerHandshake is one peer's observed handshake state on a WireGuard
// interface. It is observed output, distinct from the WGPeer config input:
// LastHandshake is the kernel's last successful handshake time (zero when no
// handshake has completed yet).
type WGPeerHandshake struct {
	PublicKey     wgtypes.Key
	LastHandshake time.Time
}

// NIC is one VM network interface to materialise. It is shared by the
// later VM-wiring tasks and carries everything the fabric needs to
// create the host-side tap and the guest-side device.
type NIC struct {
	ID          uuid.UUID
	Bridge      string
	MAC         string
	Model       string
	MTU         int
	DeviceOrder int

	// IPv4 is the NIC's configured IPv4 address, used only to source a
	// post-migration gratuitous ARP. Zero (invalid) when the NIC has no static
	// address; GARP is then skipped. Not persisted.
	IPv4 netip.Addr
}

// TapName returns the deterministic host tap interface name for the NIC:
// "ot" followed by the first 12 hex digits of the NIC id (14 chars
// total, within the Linux IFNAMSIZ-1 limit of 15).
func (n NIC) TapName() string {
	hex := n.ID.String()[:8] + n.ID.String()[9:13]
	return tapPrefix + hex
}

// ValidateMAC reports whether s is a valid 6-octet (EUI-48) MAC address.
// Empty strings and longer forms such as EUI-64 are rejected.
func ValidateMAC(s string) error {
	hw, err := net.ParseMAC(s)
	if err != nil {
		return fmt.Errorf("netfabric: invalid MAC %q: %v", s, err)
	}
	if len(hw) != 6 {
		return fmt.Errorf("netfabric: MAC %q is not a 6-octet address", s)
	}
	return nil
}

// ValidateModel reports whether s names a supported QEMU NIC model. An
// empty string is accepted; a default is applied later by the caller.
func ValidateModel(s string) error {
	if s == "" {
		return nil
	}
	if _, ok := allowedModels[s]; !ok {
		return fmt.Errorf("netfabric: unsupported NIC model %q", s)
	}
	return nil
}
