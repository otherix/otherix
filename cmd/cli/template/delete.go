// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package template

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a template by name.",
		Long: `Submits DELETE /v1/templates/{name}. The command fails with 409 conflict
when the template still has dependent resources:

  - storage_images materialised in pools;
  - virtual machines still referencing the template.

The failure output lists the blocking resources and their counts so
operators know what to clean up. Storage images are deleted via
storage-images.delete; VMs via vms.delete. No confirmation prompt is
emitted — the cascade-refusal contract guards against accidental
blast radius.

Path argument is the template name; UUID literals are rejected with 400
validation_failed per the name-only narrowing of Template identifiers.

Example:
  otherix template delete ubuntu-jammy
  otherix template delete ubuntu-jammy --output json`,
		Args: cobra.ExactArgs(1),
		RunE: runDelete,
	}
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}

func runDelete(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	name := args[0]
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	err = c.DeleteTemplate(cmd.Context(), name)
	if err != nil {
		var blocked *cpclient.ErrTemplateBlocked
		if errors.As(err, &blocked) {
			return renderBlockedDelete(cmd, name, blocked, format)
		}
		return classifyError(err)
	}

	switch format {
	case "json":
		printf(cmd, "{\"deleted\":true,\"name\":%q}\n", name)
	default:
		printf(cmd, "template %s deleted\n", name)
	}
	return nil
}

// renderBlockedDelete prints the blocking-resources envelope in the
// requested format and returns a non-zero-exit error. The text mode
// formats each resource type → count pair so operators can scan the
// list and decide what to clean up first.
func renderBlockedDelete(cmd *cobra.Command, name string, blocked *cpclient.ErrTemplateBlocked, format string) error {
	switch format {
	case "json":
		payload := map[string]any{
			"deleted":            false,
			"name":               name,
			"code":               blocked.Code,
			"blocking_resources": blocked.Resources,
		}
		raw, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printf(cmd, "cannot delete template %s — blocked by:\n", name)
		keys := make([]string, 0, len(blocked.Resources))
		for k := range blocked.Resources {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			printf(cmd, "  - %s: %d\n", k, blocked.Resources[k])
		}
		printf(cmd, "\nclean up the listed dependents before retrying delete.\n")
	}
	return fmt.Errorf("template %s blocked: %s", name, blocked.Code)
}
