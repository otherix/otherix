// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/otherix/otherix/internal/api/agentclient"
)

func TestStorageImageImportArgs_Kind(t *testing.T) {
	t.Parallel()
	if got, want := (StorageImageImportArgs{}).Kind(), "storage_image.import"; got != want {
		t.Errorf("Kind() = %q, want %q", got, want)
	}
}

func TestMarshalImportResult(t *testing.T) {
	t.Parallel()

	raw, err := marshalImportResult(ImportResult{
		ChecksumSHA256: "deadbeef",
		SizeBytes:      1 << 20,
		Format:         "qcow2",
	})
	if err != nil {
		t.Fatalf("marshalImportResult: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"checksum_sha256", "size_bytes", "format"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %q in marshalled result: %v", k, got)
		}
	}
	if got["checksum_sha256"] != "deadbeef" {
		t.Errorf("checksum_sha256 = %v, want deadbeef", got["checksum_sha256"])
	}
}

func TestClassifyImportError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "agent envelope passthrough",
			err: &agentclient.AgentError{
				Status: 409, Code: "qcow2_header_invalid", Message: "bad magic",
			},
			want: "qcow2_header_invalid",
		},
		{
			name: "agent envelope without code",
			err:  &agentclient.AgentError{Status: 502, Message: "upstream gone"},
			want: "agent_unreachable",
		},
		{
			name: "timeout",
			err:  &agentclient.TimeoutError{AgentTaskID: "x", Budget: "5s"},
			want: "import_timeout",
		},
		{
			name: "wrapped agent error",
			err: errors.Join(
				errors.New("post import"),
				&agentclient.AgentError{Status: 409, Code: "checksum_mismatch", Message: ""},
			),
			want: "checksum_mismatch",
		},
		{
			name: "generic",
			err:  errors.New("disk explosion"),
			want: "internal",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyImportError(tc.err); got != tc.want {
				t.Errorf("classifyImportError = %q, want %q", got, tc.want)
			}
		})
	}
}
