// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import "github.com/otherix/otherix/internal/store"

// selectorMatches reports whether every key/value in selector equals a label on
// the VM. It delegates to store.SelectorMatches so the connect eligibility path
// and the store-side backend resolution share one matcher.
func selectorMatches(selector map[string]string, labels []byte) bool {
	return store.SelectorMatches(selector, labels)
}
