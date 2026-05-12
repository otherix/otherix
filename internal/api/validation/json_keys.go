// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"bytes"
	"encoding/json"
)

// ScanForbiddenKeys returns the subset of keys that appear in the
// top-level JSON object of body. A non-object body or unparseable
// payload returns nil — strict-decode upstream surfaces a clearer
// 400 a moment later. Used by handlers that need to reject
// system-managed fields (e.g. `id`, `created_at`) sent by clients
// before the body is bound to a struct.
func ScanForbiddenKeys(body []byte, keys []string) []string {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var seen map[string]json.RawMessage
	if err := json.Unmarshal(body, &seen); err != nil || seen == nil {
		return nil
	}
	var out []string
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			out = append(out, k)
		}
	}
	return out
}
