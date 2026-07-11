// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildCapabilitiesJSONIncludesCompressedSwap(t *testing.T) {
	c := nodeCapabilitiesReport{
		KvmAvailable: true,
		CompressedSwap: &compressedSwapReport{
			Kind: "zram", SizeMib: 768, MemLimitMib: 256, Algorithm: "zstd",
		},
	}
	blob, err := buildCapabilitiesJSON(c)
	if err != nil {
		t.Fatalf("buildCapabilitiesJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cs, ok := got["compressed_swap"].(map[string]any)
	if !ok {
		t.Fatalf("compressed_swap missing from stored capabilities blob: %s", blob)
	}
	if cs["algorithm"] != "zstd" || cs["mem_limit_mib"].(float64) != 256 {
		t.Errorf("compressed_swap = %v, want algorithm zstd / mem_limit_mib 256", cs)
	}
}

func TestBuildCapabilitiesJSONOmitsCompressedSwapWhenNil(t *testing.T) {
	blob, err := buildCapabilitiesJSON(nodeCapabilitiesReport{KvmAvailable: true})
	if err != nil {
		t.Fatalf("buildCapabilitiesJSON: %v", err)
	}
	if strings.Contains(string(blob), "compressed_swap") {
		t.Errorf("nil compressed_swap should be omitted, got %s", blob)
	}
}
