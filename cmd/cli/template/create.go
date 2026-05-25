// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package template

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cloudinit"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// Local-only flags. `--pool` here is the materialisation target; the
// root command group's --output is shared via flagOutput.
//
// Naming note: flagCloudInitSupported used to be plain `--cloud-init`
// (boolean) but the operator UX iteration repurposed `--cloud-init`
// for the YAML file/stdin path (flagCloudInitPath). The boolean was
// renamed to keep both useful and unambiguous; tests in
// create_test.go that exercise the boolean were updated in the same
// landing.
const (
	flagDescription        = "description"
	flagOSVariant          = "os-variant"
	flagImageURL           = "image-url"
	flagImageFormat        = "image-format"
	flagExpectedSHA256     = "expected-sha256"
	flagFirmwareType       = "firmware-type"
	flagCPUCores           = "cpu-cores"
	flagMemoryMiB          = "memory-mib"
	flagDiskGiB            = "disk-gib"
	flagCloudInitSupported = "cloud-init-supported"
	flagCloudInitPath      = "cloud-init"
	flagQemuGuestAgent     = "qemu-guest-agent"
	flagCreatePool         = "pool"
)

// newCreateCommand returns the `otherix template create` cobra
// command. The implementation is composite — see runCreate's comment
// for the registration + optional materialisation split.
func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a template (and optionally materialise its image to a pool).",
		Long: `Creates a template by registering its desired-state metadata in the
Control Plane.

This command performs up to two operations:
  1. Registers the template (POST /v1/templates).
  2. If --pool is provided, triggers an async image-import task
     (POST /v1/templates/{name}/images) that asks the pool's agent
     to fetch, verify (or compute), and stage the image. Use --wait to
     block until the task reaches its terminal state.

The command is idempotent across the registration step: a re-run that
hits an already-registered template (HTTP 409) falls through to the
materialisation step instead of failing. Operators can re-run the same
command after a partial failure (e.g. register OK, materialise rejected)
to retry the materialise step.

To register without materialising, omit --pool. To materialise later,
re-run the same command with --pool when ready. To publish a private
template (admin / operator only), use the dedicated set-visibility
endpoint — visibility cannot be set at creation time.

Two checksum trust modes:

  - **Verify mode** — pass --expected-sha256. The agent verifies the
    streamed bytes during download and fails fast on mismatch. Use when
    the operator has a trusted upstream checksum (SHA256SUMS file,
    signed release, compliance requirement).
  - **Compute mode** — omit --expected-sha256. The agent computes the
    sha256 during streaming download and returns it; the Control Plane
    back-propagates the value onto the template row so future
    operations behave as if verify mode had been used. Use when the
    source URL integrity is sufficient (TLS + reputable mirror).

Example (verify mode):
  otherix template create ubuntu-jammy \
    --arch arm64 \
    --os-family linux \
    --image-url https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-arm64.img \
    --expected-sha256 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
    --pool pool-mvp \
    --wait

Example (compute mode):
  otherix template create ubuntu-jammy \
    --arch arm64 \
    --os-family linux \
    --image-url https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-arm64.img \
    --pool pool-mvp \
    --wait`,
		Args: cobra.ExactArgs(1),
		RunE: runCreate,
	}

	// Required template-identity fields.
	cmd.Flags().String(flagArch, "", "architecture (amd64|arm64) — required")
	cmd.Flags().String(flagOSFamily, "", "OS family (linux|windows|bsd|other) — required")
	cmd.Flags().String(flagImageURL, "", "image source URL (https://…) — required")

	// Optional — when set, runs in verify mode (agent fails fast on
	// checksum mismatch). When absent or empty, runs in compute mode
	// (agent computes during download, CP back-propagates).
	cmd.Flags().String(flagExpectedSHA256, "", "expected image SHA-256 (64 lowercase hex chars) — optional; absence triggers compute mode")

	// Optional template fields. Empty / zero values omit the field
	// from the request body so the server applies its spec defaults
	// (image_format=qcow2, firmware_type=uefi, cpu/memory/disk = 2/2048/20).
	cmd.Flags().String(flagDescription, "", "human description")
	cmd.Flags().String(flagOSVariant, "", "OS variant hint (e.g. ubuntu22.04)")
	cmd.Flags().String(flagImageFormat, "", "image format (qcow2|raw) — server defaults to qcow2")
	cmd.Flags().String(flagFirmwareType, "", "firmware type (bios|uefi) — server defaults to uefi")
	cmd.Flags().Int32(flagCPUCores, 0, "default VM vCPUs (server default 2)")
	cmd.Flags().Int32(flagMemoryMiB, 0, "default VM memory MiB (server default 2048)")
	cmd.Flags().Int32(flagDiskGiB, 0, "default VM disk GiB (server default 20)")
	cmd.Flags().String(flagCloudInitSupported, "", "cloud-init supported (true|false) — server defaults to true")
	cmd.Flags().String(flagCloudInitPath, "", "path to a `#cloud-config` YAML file baked into the template; use '-' to read stdin")
	cmd.Flags().String(flagQemuGuestAgent, "", "qemu guest agent expected (true|false) — server defaults to false")

	// Materialisation knobs.
	cmd.Flags().String(flagCreatePool, "", "if set, materialise the image to this pool (name or UUID)")
	cmd.Flags().Bool(flagWait, false, "block until the materialisation task reaches terminal status (requires --pool)")
	cmd.Flags().Duration(flagWaitTimeout, defaultWaitTO, "max time to wait when --wait is set")
	cmd.Flags().String(flagOutput, "text", "output format: text|json")

	_ = cmd.MarkFlagRequired(flagArch)
	_ = cmd.MarkFlagRequired(flagOSFamily)
	_ = cmd.MarkFlagRequired(flagImageURL)

	return cmd
}

// createFlags collects the parsed CLI inputs. Pointer fields mirror
// CreateTemplateRequest's "nil → server default" semantics: an unset
// CLI flag stays nil and is omitted by the JSON encoder.
type createFlags struct {
	name                string
	architecture        string
	osFamily            string
	imageURL            string
	imageChecksumSHA256 string

	description    *string
	osVariant      *string
	imageFormat    *string
	firmwareType   *string
	cpuCores       *int32
	memoryMiB      *int32
	diskGiB        *int32
	cloudInit      *bool
	qemuGuestAgent *bool

	// cloudInitUserData carries the resolved `#cloud-config` YAML
	// content read from the --cloud-init flag (file path or stdin).
	// Nil when the flag was not set; pointer-to-empty-string allowed
	// and forwarded to the server so the operator can explicitly clear
	// a previously-set pre-bake on PATCH paths (create uses non-empty
	// only — empty file is a warning, not a bake).
	cloudInitUserData *string

	pool      string
	wait      bool
	timeout   time.Duration
	outputFmt string
}

// runCreate orchestrates the composite create flow:
//
//  1. Always: POST /v1/templates (registration). 409 → fall through to
//     step 2 (idempotent retry).
//  2. If --pool is set: POST /v1/templates/{name}/images
//     (materialisation).
//  3. If --wait is set: poll the materialisation task to terminal.
//
// Partial failure (register OK, materialise rejected) surfaces a
// non-zero exit with a retry hint; re-running the same command resumes
// from the materialise step thanks to 409-fall-through.
func runCreate(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	flags, err := parseCreateFlags(cmd, args[0])
	if err != nil {
		return err
	}
	if flags.wait && flags.pool == "" {
		return errors.New("--wait requires --pool (nothing to wait for without materialisation)")
	}

	ctx := cmd.Context()

	template, registered, err := createTemplateIdempotent(ctx, c, flags)
	if err != nil {
		return classifyError(err)
	}
	if registered {
		printf(cmd, "template %s created\n", flags.name)
	} else {
		printf(cmd, "template %s already exists; proceeding to materialisation\n", flags.name)
	}

	if flags.pool == "" {
		return renderCreateResult(cmd, template, flags.outputFmt)
	}

	accepted, err := c.ImportTemplateImage(ctx, flags.name, cpclient.ImportTemplateImageRequest{
		Pool: flags.pool,
	})
	if err != nil {
		// Registration is durable; materialisation failed. Surface a
		// retry-friendly message — the same command re-run will
		// fall-through registration and re-attempt materialise.
		return fmt.Errorf("materialise: %w (template %s remains registered; re-run the same command to retry)",
			classifyError(err), flags.name)
	}

	printf(cmd, "materialise accepted task=%s status=%s\n", accepted.TaskID, accepted.Status)

	if !flags.wait {
		return nil
	}

	taskID, err := parseTaskID(accepted.TaskID)
	if err != nil {
		return fmt.Errorf("request_failed: %v", err)
	}
	if err := waitForTask(ctx, cmd, c, taskID, flags.timeout); err != nil {
		return classifyError(err)
	}
	printf(cmd, "image materialised template=%s pool=%s\n", flags.name, flags.pool)
	return nil
}

// createTemplateIdempotent invokes CreateTemplate, returning the
// registered template plus a boolean indicating whether this call
// performed the insert. On 409 (ErrTemplateExists) it falls through to
// a GET so the caller still gets a fully-populated Template to render.
func createTemplateIdempotent(ctx context.Context, c *cpclient.Client, f createFlags) (cpclient.Template, bool, error) {
	t, err := c.CreateTemplate(ctx, cpclient.CreateTemplateRequest{
		Name:                   f.name,
		Description:            f.description,
		Architecture:           f.architecture,
		OSFamily:               f.osFamily,
		OSVariant:              f.osVariant,
		ImageURL:               f.imageURL,
		ImageChecksumSHA256:    f.imageChecksumSHA256,
		ImageFormat:            f.imageFormat,
		FirmwareType:           f.firmwareType,
		DefaultCPUCores:        f.cpuCores,
		DefaultMemoryMiB:       f.memoryMiB,
		DefaultDiskGiB:         f.diskGiB,
		CloudInitSupported:     f.cloudInit,
		CloudInitUserData:      f.cloudInitUserData,
		QemuGuestAgentExpected: f.qemuGuestAgent,
	})
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, cpclient.ErrTemplateExists) {
		return cpclient.Template{}, false, err
	}
	existing, getErr := c.GetTemplate(ctx, f.name)
	if getErr != nil {
		return cpclient.Template{}, false, getErr
	}
	return existing, false, nil
}

// parseCreateFlags lifts cobra-level flag values into a createFlags
// struct. Pointer fields stay nil when the operator did not set the
// flag, matching CreateTemplateRequest's optional-field semantics.
func parseCreateFlags(cmd *cobra.Command, name string) (createFlags, error) {
	f := createFlags{name: name}
	var err error

	if f.architecture, err = cmd.Flags().GetString(flagArch); err != nil {
		return f, err
	}
	if f.osFamily, err = cmd.Flags().GetString(flagOSFamily); err != nil {
		return f, err
	}
	if f.imageURL, err = cmd.Flags().GetString(flagImageURL); err != nil {
		return f, err
	}
	if f.imageChecksumSHA256, err = cmd.Flags().GetString(flagExpectedSHA256); err != nil {
		return f, err
	}

	f.description = optionalString(cmd, flagDescription)
	f.osVariant = optionalString(cmd, flagOSVariant)
	f.imageFormat = optionalString(cmd, flagImageFormat)
	f.firmwareType = optionalString(cmd, flagFirmwareType)
	if f.cpuCores, err = optionalInt32(cmd, flagCPUCores); err != nil {
		return f, err
	}
	if f.memoryMiB, err = optionalInt32(cmd, flagMemoryMiB); err != nil {
		return f, err
	}
	if f.diskGiB, err = optionalInt32(cmd, flagDiskGiB); err != nil {
		return f, err
	}
	if f.cloudInit, err = optionalBool(cmd, flagCloudInitSupported); err != nil {
		return f, err
	}
	if f.cloudInitUserData, err = readCloudInitFlag(cmd, flagCloudInitPath); err != nil {
		return f, err
	}
	if f.qemuGuestAgent, err = optionalBool(cmd, flagQemuGuestAgent); err != nil {
		return f, err
	}

	if f.pool, err = cmd.Flags().GetString(flagCreatePool); err != nil {
		return f, err
	}
	if f.wait, err = cmd.Flags().GetBool(flagWait); err != nil {
		return f, err
	}
	if f.timeout, err = cmd.Flags().GetDuration(flagWaitTimeout); err != nil {
		return f, err
	}
	if f.outputFmt, err = outputFormat(cmd, "text"); err != nil {
		return f, err
	}
	return f, nil
}

// optionalString returns a pointer to the flag's value when the operator
// supplied a non-empty string, nil otherwise. Lets the JSON encoder omit
// the field so the server applies its default.
func optionalString(cmd *cobra.Command, name string) *string {
	raw, _ := cmd.Flags().GetString(name)
	if raw == "" {
		return nil
	}
	return &raw
}

// optionalInt32 returns a pointer to the int32 flag's value when the
// operator supplied a positive number, nil otherwise. Zero is treated
// as "unset" — the server's defaults (cpu=2, memory=2048, disk=20) are
// all > 0 so this is a safe sentinel.
func optionalInt32(cmd *cobra.Command, name string) (*int32, error) {
	raw, err := cmd.Flags().GetInt32(name)
	if err != nil {
		return nil, err
	}
	if raw <= 0 {
		return nil, nil
	}
	return &raw, nil
}

// readCloudInitFlag turns the `--cloud-init=<path|->` flag into the
// resolved YAML content. Empty flag returns nil (operator opted out);
// non-empty reads the source (file or stdin) and runs the best-effort
// validator. Validation warnings land on cmd's stderr but do not block
// dispatch (locked decision: warn, not gate). Validation errors —
// malformed YAML — bubble up so cobra exits non-zero before the HTTP
// call leaves the box.
func readCloudInitFlag(cmd *cobra.Command, name string) (*string, error) {
	raw, err := cmd.Flags().GetString(name)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	data, err := cloudinit.ReadSource(raw)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", name, err)
	}
	warnings, err := cloudinit.Validate(data)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", name, err)
	}
	stderr := cmd.ErrOrStderr()
	if stderr == nil {
		stderr = os.Stderr
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(stderr, "warning: --%s: %s\n", name, w)
	}
	out := string(data)
	return &out, nil
}

// optionalBool reads a string flag and converts true/false to a *bool.
// Empty string means "unset" so server defaults apply. Other strings
// surface as an error so the operator gets a clear validation message
// rather than a silent fall-through.
func optionalBool(cmd *cobra.Command, name string) (*bool, error) {
	raw, err := cmd.Flags().GetString(name)
	if err != nil {
		return nil, err
	}
	switch raw {
	case "":
		return nil, nil
	case "true":
		v := true
		return &v, nil
	case "false":
		v := false
		return &v, nil
	default:
		return nil, fmt.Errorf("--%s: must be true or false, got %q", name, raw)
	}
}

// renderCreateResult prints the registered template in the requested
// format. Mirrors `template get`'s output shape so operators can pipe
// the create output into the same downstream tooling.
func renderCreateResult(cmd *cobra.Command, t cpclient.Template, format string) error {
	switch format {
	case "json":
		raw, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	default:
		printTemplateText(cmd, t, false)
	}
	return nil
}
