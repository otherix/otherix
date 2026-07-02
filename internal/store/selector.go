// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import "encoding/json"

// SelectorMatches reports whether every key/value in selector equals a label on
// the VM. labels is the raw VM.Labels JSON object; a non-empty selector against
// empty or malformed labels is a non-match (fail toward excluding the backend).
// An empty selector never matches (a load balancer with no selector selects no
// backend, by construction of the create-time non-empty-selector validation).
func SelectorMatches(selector map[string]string, labels []byte) bool {
	if len(selector) == 0 {
		return false
	}
	var lbls map[string]string
	if err := json.Unmarshal(labels, &lbls); err != nil {
		return false
	}
	for k, v := range selector {
		if lbls[k] != v {
			return false
		}
	}
	return true
}
