// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package response

// AsyncTaskAccepted mirrors components/schemas/AsyncTaskAccepted in
// api/openapi/control-plane.yaml. Returned from every 202 mutating
// endpoint that hands work off to a worker. Carries the freshly
// minted task id, its initial status (pending/running), and a
// self-link the caller polls for completion.
type AsyncTaskAccepted struct {
	TaskID string         `json:"task_id"`
	Status string         `json:"status"`
	Links  AsyncTaskLinks `json:"links"`
}

// AsyncTaskLinks carries the navigational links surfaced inside an
// AsyncTaskAccepted envelope. Currently only `self` is populated; the
// shape is reserved for future link relations.
type AsyncTaskLinks struct {
	Self string `json:"self"`
}
