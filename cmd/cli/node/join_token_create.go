// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newJoinTokenCreateCommand builds `otherix node join-token create`.
// Mints a fresh token bundle and prints it exactly once.
func newJoinTokenCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint a fresh join token (admin only).",
		Long: `Mints a new bootstrap token. Output is the token bundle —
plaintext token + active cluster CA fingerprint — returned exactly
once. Save BOTH NOW; the server stores only sha256(token) and
plaintext cannot be retrieved.

Use --node-name to bind the token to a specific node identity
(single-use only). An omitted --max-uses defaults to single-use;
pass --max-uses=N to mint a multi-use fleet-bootstrap token.

Pass token + fingerprint to the agent via:
  OTHERIX_BOOTSTRAP__TOKEN=<token>
  OTHERIX_BOOTSTRAP__CA_FINGERPRINT=<fingerprint>
(double underscore: koanf env-nesting separator)

Examples:
  otherix node join-token create --node-name node-dev --ttl 10m
  otherix node join-token create --max-uses 3 --ttl 1h
  otherix node join-token create --ttl 24h            # single-use within 24h`,
		RunE: runJoinTokenCreate,
	}
	cmd.Flags().Duration(flagTTL, defaultTokenTTL, "token validity duration (1m..24h)")
	cmd.Flags().Int32(flagMaxUses, 0, "consumption cap (0 = server default of 1)")
	cmd.Flags().String(flagNodeName, "", "bind token to a specific node identity (forces single-use)")
	cmd.Flags().String(flagKind, "node", "join kind: node|gateway (cluster tokens use `otherix cluster join-token create`)")
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json")
	return cmd
}

func runJoinTokenCreate(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	ttl, _ := cmd.Flags().GetDuration(flagTTL)
	maxUses, _ := cmd.Flags().GetInt32(flagMaxUses)
	nodeName, _ := cmd.Flags().GetString(flagNodeName)
	kind, _ := cmd.Flags().GetString(flagKind)
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	if ttl < time.Minute || ttl > 24*time.Hour {
		return errors.New("validation_failed: --ttl must be in [1m, 24h]")
	}

	req := cpclient.CreateJoinTokenRequest{}
	ttlSeconds := int(ttl.Seconds())
	req.TTLSeconds = &ttlSeconds

	// --kind selects node (default) or gateway. Cluster tokens are minted by
	// `otherix cluster join-token create`, so they are rejected here. The
	// default omits the field entirely so the server applies its node default.
	switch kind {
	case "node", "":
		// Leave req.Kind nil; the server defaults to node.
	case "gateway":
		k := kind
		req.Kind = &k
	default:
		return errors.New(`validation_failed: --kind must be "node" or "gateway"`)
	}

	if maxUses != 0 {
		if maxUses < 0 {
			return errors.New("validation_failed: --max-uses must be >= 1")
		}
		v := maxUses
		req.MaxUses = &v
	}
	if nodeName != "" {
		req.IntendedNodeName = &nodeName
		// Pre-bound tokens are single-use by construction. CLI sets
		// max_uses=1 explicitly so the request body is self-describing
		// and the server-side validator accepts it.
		one := int32(1)
		req.MaxUses = &one
	}

	resp, err := c.CreateJoinToken(cmd.Context(), req)
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printTokenBundle(cmd, resp, ttl)
	}
	return nil
}

// printTokenBundle renders the bundle in the block-style layout:
// prominent plaintext + fingerprint + Save-BOTH-NOW warning + expiry
// / max-uses metadata.
func printTokenBundle(cmd *cobra.Command, resp cpclient.CreateJoinTokenResponse, ttl time.Duration) {
	printf(cmd, "Token created (id: %s):\n\n", resp.ID)
	printf(cmd, "  %s\n\n", resp.Token)
	printf(cmd, "CA fingerprint:\n\n")
	printf(cmd, "  sha256:%s\n\n", resp.CAFingerprintSHA256)
	printf(cmd, "Save BOTH NOW — server stores only hash, plaintext cannot be retrieved.\n")
	printf(cmd, "Pass to agent via OTHERIX_BOOTSTRAP__TOKEN + OTHERIX_BOOTSTRAP__CA_FINGERPRINT (koanf double-underscore nesting).\n")
	printf(cmd, "Expires: %s (TTL: %s).\n", resp.ExpiresAt, ttl)
	printf(cmd, "Max uses: %s.\n", maxUsesLabel(resp.MaxUses))
	if resp.IntendedNodeName != nil {
		printf(cmd, "Bound to node-name: %s (single-use).\n", *resp.IntendedNodeName)
	}
}
