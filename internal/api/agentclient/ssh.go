// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"fmt"
	"net/url"
)

// BuildSSHPipeURL composes the wss:// URL of the owning agent's ssh-pipe
// endpoint that the CP-side relay dials. The VM name is path-escaped: a VM
// name is only length-validated today (1..255 chars, no charset), so a name
// carrying URL-special characters (e.g. a space) is creatable and its pipe
// URL would be malformed without PathEscape. The agent serves ssh-pipe over
// HTTPS via the node mTLS listener, so the scheme is always wss://; the
// caller dials it with the agentclient's mTLS-configured HTTP client.
func BuildSSHPipeURL(agentHost, vmName string) string {
	return fmt.Sprintf("wss://%s/v1/vms/%s/ssh-pipe", agentHost, url.PathEscape(vmName))
}
