// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

const (
	flagEnabled = "enabled"
	flagSuffix  = "suffix"
)

func newGetSSHIngressCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get-ssh-ingress",
		Short: "Show the cluster SSH-ingress configuration.",
		Long: `Fetches /v1/cluster/ssh-ingress. Prints the cluster-wide SSH-ingress
master switch and the DNS suffix VM hostnames are addressed under. An
unconfigured cluster reports enabled=false with an empty suffix.`,
		RunE: runGetSSHIngress,
	}
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}

func runGetSSHIngress(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	got, err := c.GetClusterSSHIngress(cmd.Context())
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}
	printf(cmd, "ssh-ingress: enabled=%t cluster-suffix=%q\n", got.Enabled, got.ClusterSuffix)
	return nil
}

func newSetSSHIngressCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-ssh-ingress",
		Short: "Set the cluster SSH-ingress configuration (admin only).",
		Long: `PUT /v1/cluster/ssh-ingress with the supplied master switch and DNS
suffix. Enabling requires --suffix to be a non-empty, valid DNS domain
(the connector bundle and cert-mint address a VM under it); the server
rejects a missing or malformed suffix with 400 validation_failed.
Requires admin (cluster:manage); other roles receive 403
permission_denied.

Enable:  otherix cluster set-ssh-ingress --enabled --suffix ssh.otherix.local
Disable: otherix cluster set-ssh-ingress --enabled=false`,
		RunE: runSetSSHIngress,
	}
	cmd.Flags().Bool(flagEnabled, true, "enable the cluster SSH-ingress master switch (pass --enabled=false to disable)")
	cmd.Flags().String(flagSuffix, "", "DNS suffix VM hostnames are addressed under (required when enabling)")
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}

func runSetSSHIngress(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}
	enabled, err := cmd.Flags().GetBool(flagEnabled)
	if err != nil {
		return err
	}
	suffix, err := cmd.Flags().GetString(flagSuffix)
	if err != nil {
		return err
	}

	got, err := c.SetClusterSSHIngress(cmd.Context(), enabled, suffix)
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
		return nil
	}
	printf(cmd, "ssh-ingress set: enabled=%t cluster-suffix=%q\n", got.Enabled, got.ClusterSuffix)
	return nil
}
