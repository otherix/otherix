// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
)

// ImgInfo is the subset of `qemu-img info --output=json` the agent needs:
// the image's virtual size (the size the guest sees) and its format.
type ImgInfo struct {
	VirtualSize int64
	Format      string
}

// imgInfoJSON mirrors the qemu-img info JSON fields parseImgInfo reads.
type imgInfoJSON struct {
	VirtualSize int64  `json:"virtual-size"`
	Format      string `json:"format"`
}

// parseImgInfo parses the JSON document emitted by
// `qemu-img info --output=json`, returning the virtual size and format.
func parseImgInfo(raw []byte) (ImgInfo, error) {
	var j imgInfoJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return ImgInfo{}, fmt.Errorf("parse qemu-img info json: %v", err)
	}
	if j.VirtualSize <= 0 {
		return ImgInfo{}, fmt.Errorf("qemu-img info reported non-positive virtual-size %d", j.VirtualSize)
	}
	return ImgInfo{VirtualSize: j.VirtualSize, Format: j.Format}, nil
}

// ImgInfoOf runs `qemu-img info --output=json` against path and returns the
// parsed virtual size + format. Errors wrap the command failure.
func ImgInfoOf(ctx context.Context, path string) (ImgInfo, error) {
	// #nosec G204 -- path is an agent-owned pool file, not user input.
	out, err := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", path).Output()
	if err != nil {
		return ImgInfo{}, fmt.Errorf("qemu-img info %s: %v", path, err)
	}
	return parseImgInfo(out)
}

// ResizeImg grows the qcow2 at path to sizeBytes via `qemu-img resize`.
// qemu-img resize only grows here (the caller rejects shrink before calling).
func ResizeImg(ctx context.Context, path string, sizeBytes int64) error {
	// #nosec G204 -- path is an agent-owned pool file; size is an int64 literal.
	out, err := exec.CommandContext(ctx, "qemu-img", "resize", path, strconv.FormatInt(sizeBytes, 10)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img resize %s -> %d: %v (%s)", path, sizeBytes, err, out)
	}
	return nil
}
