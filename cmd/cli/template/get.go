// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package template

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <template>",
		Short: "Show a template's projection.",
		Long: `Fetches the template view from the CP. The positional is a template
name (UUID literals rejected by the server with 400
validation_failed). Cross-user private templates surface as 404 (no
existence leak).`,
		Args: cobra.ExactArgs(1),
		RunE: runGet,
	}
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	cmd.Flags().Bool(flagShowIDs, false, "include UUIDs (id, owner_id, firmware_id) in text output")
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	identifier := args[0]
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}
	showIDs, _ := cmd.Flags().GetBool(flagShowIDs)

	t, raw, err := c.GetTemplate(cmd.Context(), identifier)
	if err != nil {
		return classifyError(err)
	}

	if format == "json" {
		return printJSON(cmd, raw)
	}
	printTemplateText(cmd, t, showIDs)
	return nil
}

func printTemplateText(cmd *cobra.Command, t cpclient.Template, showIDs bool) {
	if showIDs {
		printf(cmd, "id: %s\n", t.ID)
		printf(cmd, "owner_id: %s\n", t.OwnerID)
	}
	printf(cmd, "name: %s\n", t.Name)
	if t.Description != "" {
		printf(cmd, "description: %s\n", t.Description)
	}
	printf(cmd, "visibility: %s\n", t.Visibility)
	printf(cmd, "architecture: %s\n", t.Architecture)
	printf(cmd, "os_family: %s\n", t.OSFamily)
	if t.OSVariant != "" {
		printf(cmd, "os_variant: %s\n", t.OSVariant)
	}
	printf(cmd, "image_url: %s\n", t.ImageURL)
	if t.ImageChecksumSHA256 != nil {
		printf(cmd, "image_checksum_sha256: %s\n", *t.ImageChecksumSHA256)
	} else {
		printf(cmd, "image_checksum_sha256: (unset — compute mode, agent computes on first import)\n")
	}
	printf(cmd, "image_format: %s\n", t.ImageFormat)
	if t.ImageSizeBytes != nil {
		printf(cmd, "image_size_bytes: %d\n", *t.ImageSizeBytes)
	}
	if t.FirmwareType != "" {
		printf(cmd, "firmware_type: %s\n", t.FirmwareType)
	}
	if showIDs && t.FirmwareID != nil {
		printf(cmd, "firmware_id: %s\n", *t.FirmwareID)
	}
	printf(cmd, "default_cpu_cores: %d\n", t.DefaultCPUCores)
	printf(cmd, "default_memory_mib: %d\n", t.DefaultMemoryMiB)
	printf(cmd, "default_disk_gib: %d\n", t.DefaultDiskGiB)
	printf(cmd, "cloud_init_supported: %t\n", t.CloudInitSupported)
	printCloudInitUserData(cmd, t.CloudInitUserData)
	printf(cmd, "qemu_guest_agent_expected: %t\n", t.QemuGuestAgentExpected)
	printf(cmd, "created_at: %s\n", t.CreatedAt)
	printf(cmd, "updated_at: %s\n", t.UpdatedAt)
}

// printCloudInitUserData renders the baked cloud-init blob with a
// terse, copy-friendly summary so the default text output doesn't
// drown the operator's terminal in a multi-line YAML dump. nil → one
// line "(unset)"; set → "set, N bytes (first line: …)" where the
// first line is trimmed to 60 chars so even a 1 KB user-data fits
// in two terminal lines. Operators wanting the full payload reach
// for `--output=json` which surfaces the raw string verbatim.
func printCloudInitUserData(cmd *cobra.Command, ud *string) {
	if ud == nil {
		printf(cmd, "cloud_init_user_data: (unset)\n")
		return
	}
	body := *ud
	firstLine := body
	if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		firstLine = body[:idx]
	}
	const maxFirstLine = 60
	if len(firstLine) > maxFirstLine {
		firstLine = firstLine[:maxFirstLine] + "…"
	}
	printf(cmd, "cloud_init_user_data: set, %d bytes (first line: %q; use --output=json for full payload)\n",
		len(body), firstLine)
}
