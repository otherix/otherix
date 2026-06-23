// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagCreateType       = "type"
	flagCreateBridgeName = "bridge-name"
	flagCreateManaged    = "managed"
	flagCreateEgress     = "egress"
	flagCreateSubnet     = "subnet"
	flagCreateGateway    = "gateway"
	flagCreateMTU        = "mtu"
	flagCreateVLAN       = "vlan"
	flagCreateDhcp       = "dhcp"
	flagCreateDns        = "dns"

	defaultNetworkType   = "bridge"
	defaultNetworkEgress = "none"
)

// newCreateCommand returns the `otherix network create` cobra command.
// One invocation registers one cluster-wide network. Admin-only
// (`network:manage`).
func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a network (admin-only).",
		Long: `Submits POST /v1/networks. Required permission:
network:manage (admin role). Networks are cluster-wide bridge
definitions with a globally-unique name.

--egress nat requires --managed and a --subnet; the gateway is derived
as the first usable host when --gateway is omitted. --egress none (the
default) forbids --subnet/--gateway. The server re-validates the
(type, managed, egress) triple and surfaces a 400 validation_failed
when the combination is invalid.

--type overlay requires --subnet and forbids --bridge-name/--mtu/--vlan/
--managed/--gateway (the server derives the bridge name from the
allocated VNI and forces the network managed). --egress nat is allowed
on overlay and enables per-node anycast-gateway SNAT, giving the
overlay's VMs internet egress.

Example:
  otherix network create net-dev --bridge-name br0

  # Managed NAT network with an egress subnet:
  otherix network create net-nat \
    --bridge-name br-nat \
    --managed \
    --egress nat \
    --subnet 10.10.0.0/24

  # Overlay (VXLAN) network:
  otherix network create my-overlay --type overlay --subnet 10.50.0.0/24

  # Overlay (VXLAN) network with internet egress:
  otherix network create my-overlay --type overlay \
    --subnet 10.50.0.0/24 --egress nat`,
		Args: cobra.ExactArgs(1),
		RunE: runCreate,
	}
	cmd.Flags().String(flagCreateType, defaultNetworkType, "network type (bridge|overlay)")
	cmd.Flags().String(flagCreateBridgeName, "", "host bridge interface name (required for --type bridge)")
	cmd.Flags().Bool(flagCreateManaged, false, "control plane manages the bridge lifecycle")
	cmd.Flags().String(flagCreateEgress, defaultNetworkEgress, "managed egress mode: none|nat")
	cmd.Flags().String(flagCreateSubnet, "", "egress subnet in CIDR form (required for --egress nat)")
	cmd.Flags().String(flagCreateGateway, "", "gateway IP inside --subnet (derived when omitted)")
	cmd.Flags().Int(flagCreateMTU, 0, "link MTU (68..9216; server defaults to 1500 when omitted)")
	cmd.Flags().Int(flagCreateVLAN, 0, "VLAN tag (1..4094; omitted leaves the network untagged)")
	cmd.Flags().Bool(flagCreateDhcp, false, "enable CP-IPAM + DHCP responder (overlay only; requires --subnet)")
	cmd.Flags().Bool(flagCreateDns, true, "advertise the overlay resolver (169.254.1.1) via DHCP option 6 (overlay only; --dns=false to suppress)")
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	cmd.Flags().Bool(flagShowIDs, false, "include the network UUID in the text output")

	return cmd
}

func runCreate(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	name := args[0]
	if name == "" {
		return errors.New("network name is required")
	}

	params, err := createParamsFromFlags(cmd, name)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}
	showIDs, _ := cmd.Flags().GetBool(flagShowIDs)

	created, err := c.CreateNetwork(cmd.Context(), params)
	if err != nil {
		if errors.Is(err, cpclient.ErrNetworkExists) {
			return fmt.Errorf("network %s already exists - use 'otherix network get %s' to inspect, or pick a different name",
				name, name)
		}
		return classifyError(err)
	}

	return renderCreateOutput(cmd, created, format, showIDs)
}

// createParamsFromFlags reads the create flags and assembles the
// CreateNetworkParams. It branches on --type to enforce type-specific
// requirements: overlay requires --subnet and forbids bridge-only flags
// (bridge-name, mtu, vlan, managed, gateway); bridge requires
// --bridge-name. Optional integer flags are translated to pointers only
// when the operator changed them, so an omitted flag lands as nil and
// the server applies its default. Split from runCreate to keep
// cyclomatic complexity in check.
func createParamsFromFlags(cmd *cobra.Command, name string) (cpclient.CreateNetworkParams, error) {
	netType, err := cmd.Flags().GetString(flagCreateType)
	if err != nil {
		return cpclient.CreateNetworkParams{}, err
	}
	if netType == "" {
		netType = defaultNetworkType
	}

	switch netType {
	case "overlay":
		return createOverlayParams(cmd, name)
	default:
		return createBridgeParams(cmd, name, netType)
	}
}

// createOverlayParams assembles CreateNetworkParams for --type overlay.
// Overlay networks require --subnet and reject bridge-only flags
// (bridge-name, mtu, vlan, managed, gateway) because those are
// server-derived for overlay. --egress nat is allowed: it enables
// per-node anycast-gateway SNAT for the overlay's VMs.
func createOverlayParams(cmd *cobra.Command, name string) (cpclient.CreateNetworkParams, error) {
	subnet, err := cmd.Flags().GetString(flagCreateSubnet)
	if err != nil {
		return cpclient.CreateNetworkParams{}, err
	}
	if subnet == "" {
		return cpclient.CreateNetworkParams{}, errors.New("--type overlay requires --subnet")
	}

	// Reject flags that are bridge-only / server-derived for overlay.
	var forbidden []string
	if cmd.Flags().Changed(flagCreateBridgeName) {
		forbidden = append(forbidden, "--bridge-name")
	}
	if cmd.Flags().Changed(flagCreateMTU) {
		forbidden = append(forbidden, "--mtu")
	}
	if cmd.Flags().Changed(flagCreateVLAN) {
		forbidden = append(forbidden, "--vlan")
	}
	if cmd.Flags().Changed(flagCreateManaged) {
		forbidden = append(forbidden, "--managed")
	}
	if cmd.Flags().Changed(flagCreateGateway) {
		forbidden = append(forbidden, "--gateway")
	}
	if len(forbidden) > 0 {
		return cpclient.CreateNetworkParams{}, fmt.Errorf(
			"--type overlay forbids %s (server-derived)",
			strings.Join(forbidden, "/"),
		)
	}

	egress, _ := cmd.Flags().GetString(flagCreateEgress)
	if egress == defaultNetworkEgress {
		egress = ""
	}
	dhcp, _ := cmd.Flags().GetBool(flagCreateDhcp)
	dns, _ := cmd.Flags().GetBool(flagCreateDns)
	return cpclient.CreateNetworkParams{
		Name:   name,
		Type:   "overlay",
		Egress: egress,
		Subnet: subnet,
		Dhcp:   dhcp,
		DNS:    &dns,
	}, nil
}

// createBridgeParams assembles CreateNetworkParams for --type bridge
// (and any future non-overlay types that share the bridge flag set).
// --bridge-name is required for bridge.
func createBridgeParams(cmd *cobra.Command, name, netType string) (cpclient.CreateNetworkParams, error) {
	bridgeName, err := cmd.Flags().GetString(flagCreateBridgeName)
	if err != nil {
		return cpclient.CreateNetworkParams{}, err
	}
	if bridgeName == "" {
		return cpclient.CreateNetworkParams{}, fmt.Errorf("--bridge-name is required for --type %s", netType)
	}

	managed, _ := cmd.Flags().GetBool(flagCreateManaged)
	egress, _ := cmd.Flags().GetString(flagCreateEgress)
	subnet, _ := cmd.Flags().GetString(flagCreateSubnet)
	gateway, _ := cmd.Flags().GetString(flagCreateGateway)
	dhcp, _ := cmd.Flags().GetBool(flagCreateDhcp)

	// The default egress is "none", which the server applies when the
	// field is omitted; drop it so the request body carries egress only
	// when the operator picked a non-default mode.
	if egress == defaultNetworkEgress {
		egress = ""
	}

	params := cpclient.CreateNetworkParams{
		Name:       name,
		Type:       netType,
		BridgeName: bridgeName,
		Managed:    managed,
		Egress:     egress,
		Subnet:     subnet,
		Gateway:    gateway,
		Dhcp:       dhcp,
	}
	if cmd.Flags().Changed(flagCreateMTU) {
		mtu, _ := cmd.Flags().GetInt(flagCreateMTU)
		params.Mtu = &mtu
	}
	if cmd.Flags().Changed(flagCreateVLAN) {
		vlan, _ := cmd.Flags().GetInt(flagCreateVLAN)
		params.VlanTag = &vlan
	}
	return params, nil
}

// renderCreateOutput writes the JSON or text representation of the
// created network.
func renderCreateOutput(cmd *cobra.Command, n cpclient.Network, format string, showIDs bool) error {
	if format == "json" {
		raw, err := json.MarshalIndent(n, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}
	printf(cmd, "network %s created\n", n.Name)
	if showIDs {
		printf(cmd, "id: %s\n", n.ID)
	}
	printf(cmd, "type: %s\n", n.Type)
	if n.BridgeName != "" {
		printf(cmd, "bridge_name: %s\n", n.BridgeName)
	}
	if n.VNI != nil {
		printf(cmd, "vni: %d\n", *n.VNI)
	}
	printf(cmd, "managed: %t\n", n.Managed)
	printf(cmd, "egress: %s\n", n.Egress)
	printf(cmd, "subnet: %s\n", orDash(n.Subnet))
	printf(cmd, "gateway: %s\n", orDash(n.Gateway))
	printf(cmd, "dhcp: %t\n", n.Dhcp != nil && *n.Dhcp)
	printf(cmd, "mtu: %d\n", n.MTU)
	return nil
}
