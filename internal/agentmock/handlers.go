// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/console"
	"github.com/otherix/otherix/internal/agentapi"
)

const maxCapturedBodyBytes = 64 << 10

// bodyCaptureKey is the request-context key under which the
// body-capture middleware stores up-to-64 KiB of the request body for
// later inclusion in RequestRecord.Body.
type bodyCaptureKey struct{}

// capturedBody is the value the body-capture middleware stores.
type capturedBody struct {
	bytes     []byte
	truncated bool
}

// captureBodyMiddleware reads the request body up to
// maxCapturedBodyBytes, attaches the captured bytes to the request
// context, and replaces r.Body with a fresh reader so the downstream
// handler can still consume the body.
func captureBodyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		buf, err := io.ReadAll(io.LimitReader(r.Body, maxCapturedBodyBytes+1))
		if err != nil {
			_ = r.Body.Close()
			http.Error(w, "agentmock: read body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		captured := capturedBody{bytes: buf, truncated: false}
		if len(buf) > maxCapturedBodyBytes {
			captured.bytes = buf[:maxCapturedBodyBytes]
			captured.truncated = true
		}
		ctx := context.WithValue(r.Context(), bodyCaptureKey{}, captured)
		r = r.WithContext(ctx)
		r.Body = io.NopCloser(bytes.NewReader(buf))
		next.ServeHTTP(w, r)
	})
}

func capturedFromContext(ctx context.Context) capturedBody {
	v, _ := ctx.Value(bodyCaptureKey{}).(capturedBody)
	return v
}

// peerCN extracts the verified client's CommonName from r.TLS, or "" when
// TLS is not in use.
func peerCN(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return r.TLS.PeerCertificates[0].Subject.CommonName
}

// preDispatch checks the OnRequest callback and the per-operationID
// injection registry. If either short-circuits the request, the
// response is written, the request is recorded, and the function
// returns true so the calling handler returns immediately.
func (m *Mock) preDispatch(w http.ResponseWriter, r *http.Request, operationID string) bool {
	m.state.mu.Lock()
	onReq := m.state.inject.onRequest
	m.state.mu.Unlock()

	if onReq != nil {
		if action := onReq(r, operationID); action != nil {
			m.applyAction(w, r, operationID, action)
			return true
		}
	}

	m.state.mu.Lock()
	inj, hasInj := m.state.takeInjectionLocked(operationID)
	m.state.mu.Unlock()
	if hasInj {
		m.applyInjectedError(w, r, operationID, inj)
		return true
	}
	return false
}

func (m *Mock) applyAction(w http.ResponseWriter, r *http.Request, operationID string, action *RequestAction) {
	for k, vs := range action.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	status := action.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if action.Body != nil {
		_ = json.NewEncoder(w).Encode(action.Body)
	}
	m.recordRequest(operationID, r, status)
}

func (m *Mock) applyInjectedError(w http.ResponseWriter, r *http.Request, operationID string, inj InjectedError) {
	status := inj.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	body := errorEnvelope{
		Error: errorEnvelopeBody{
			Code:    inj.Code,
			Message: inj.Message,
			Details: inj.Details,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
	m.recordRequest(operationID, r, status)
}

func (m *Mock) respondJSON(w http.ResponseWriter, r *http.Request, operationID string, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
	m.recordRequest(operationID, r, status)
}

func (m *Mock) respondNotFound(w http.ResponseWriter, r *http.Request, operationID, message string) {
	body := errorEnvelope{
		Error: errorEnvelopeBody{Code: "not_found", Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(body)
	m.recordRequest(operationID, r, http.StatusNotFound)
}

// respondError writes a custom error envelope with the given status,
// code, message, and optional details. Centralises the boilerplate
// every new mock handler would otherwise duplicate.
func (m *Mock) respondError(w http.ResponseWriter, r *http.Request, operationID string, status int, code, message string, details ...map[string]any) {
	body := errorEnvelope{
		Error: errorEnvelopeBody{
			Code:    code,
			Message: message,
		},
	}
	if len(details) > 0 {
		body.Error.Details = details[0]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
	m.recordRequest(operationID, r, status)
}

func (m *Mock) respondNotImplemented(w http.ResponseWriter, r *http.Request, operationID string) {
	body := errorEnvelope{
		Error: errorEnvelopeBody{
			Code:    "internal",
			Message: "not implemented in mock-agent",
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(body)
	m.recordRequest(operationID, r, http.StatusNotImplemented)
}

func (m *Mock) recordRequest(operationID string, r *http.Request, status int) {
	captured := capturedFromContext(r.Context())
	rec := RequestRecord{
		OperationID: operationID,
		Method:      r.Method,
		Path:        r.URL.Path,
		Status:      status,
		ReceivedAt:  time.Now().UTC(),
		Body:        append([]byte(nil), captured.bytes...),
		Truncated:   captured.truncated,
		PeerCN:      peerCN(r),
	}
	m.state.mu.Lock()
	m.state.recordRequestLocked(rec)
	m.state.mu.Unlock()
}

// errorEnvelope mirrors the JSON shape the mock writes for every
// envelope-bearing response (injected errors, 404, 501). It is
// declared locally rather than re-using agentapi.Error so the wire
// format is fully controlled by this package — agentapi's struct
// uses an inline anonymous struct that JSON-encodes the same way but
// is awkward to construct directly.
type errorEnvelope struct {
	Error errorEnvelopeBody `json:"error"`
}

type errorEnvelopeBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// ---------------------------------------------------------------------
// Functional handlers (6).
// ---------------------------------------------------------------------

// HealthCheck implements GET /v1/health.
func (m *Mock) HealthCheck(w http.ResponseWriter, r *http.Request) {
	const opID = "health.check"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondJSON(w, r, opID, http.StatusOK, agentapi.HealthResponse{
		Status: agentapi.HealthResponseStatus("ok"),
	})
}

// InfoGet implements GET /v1/info.
func (m *Mock) InfoGet(w http.ResponseWriter, r *http.Request) {
	const opID = "info.get"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.state.mu.Lock()
	body := m.buildNodeInfoLocked()
	m.state.mu.Unlock()
	m.respondJSON(w, r, opID, http.StatusOK, body)
}

func (m *Mock) buildNodeInfoLocked() agentapi.NodeInfo {
	s := m.state
	firmwares := make([]agentapi.NodeInfoFirmware, 0, len(s.firmwares))
	for _, f := range s.firmwares {
		firmwares = append(firmwares, firmwareToAPI(f))
	}
	uptime := max(int64(time.Since(s.startedAt).Seconds()), 0)
	info := agentapi.NodeInfo{
		NodeID:             s.nodeID,
		Hostname:           s.hostname,
		AgentVersion:       s.agentVersion,
		Architecture:       agentapi.NodeInfoArchitecture(s.architecture),
		KvmAvailable:       s.kvmAvailable,
		CPUModel:           s.cpuModel,
		CPUFeatures:        append([]string(nil), s.cpuFeatures...),
		CPUCoresTotal:      s.cpuCoresTotal,
		CPUCoresAvailable:  s.cpuCoresAvailable,
		MemoryTotalMib:     s.memoryTotalMib,
		MemoryAvailableMib: s.memoryAvailableMib,
		QEMUVersion:        s.qemuVersion,
		KernelVersion:      s.kernelVersion,
		QEMUBinaries:       cloneStringMap(s.qemuBinaries),
		Firmwares:          firmwares,
		Migration:          migrationToAPI(s.migration),
		StartedAt:          s.startedAt,
		UptimeSeconds:      uptime,
	}
	if s.nestedVirt {
		v := true
		info.NestedVirt = &v
	}
	if s.hugepages2MiBTotal != nil {
		v := *s.hugepages2MiBTotal
		info.Hugepages2MibTotal = &v
	}
	if s.hugepages1GiBTotal != nil {
		v := *s.hugepages1GiBTotal
		info.Hugepages1GibTotal = &v
	}
	if len(s.labels) > 0 {
		labels := cloneStringMap(s.labels)
		info.Labels = &labels
	}
	return info
}

// StoragePoolsList implements GET /v1/storage-pools.
func (m *Mock) StoragePoolsList(w http.ResponseWriter, r *http.Request, _ agentapi.StoragePoolsListParams) {
	const opID = "storagePools.list"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.state.mu.Lock()
	pools := make([]agentapi.StoragePoolReport, 0, len(m.state.pools))
	for _, p := range m.state.pools {
		pools = append(pools, poolToAPI(p))
	}
	m.state.mu.Unlock()
	body := agentapi.StoragePoolList{
		Data: pools,
		Meta: agentapi.PaginationMeta{NextCursor: nil},
	}
	m.respondJSON(w, r, opID, http.StatusOK, body)
}

// StoragePoolsGet implements GET /v1/storage-pools/{pool_name}.
func (m *Mock) StoragePoolsGet(w http.ResponseWriter, r *http.Request, poolName agentapi.PoolName) {
	const opID = "storagePools.get"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.state.mu.Lock()
	p, ok := m.state.pools[poolName]
	m.state.mu.Unlock()
	if !ok {
		m.respondNotFound(w, r, opID, "storage pool not found on this agent")
		return
	}
	m.respondJSON(w, r, opID, http.StatusOK, poolToAPI(p))
}

// StorageImagesList implements GET /v1/storage-pools/{pool_name}/images.
func (m *Mock) StorageImagesList(w http.ResponseWriter, r *http.Request, poolName agentapi.PoolName, _ agentapi.StorageImagesListParams) {
	const opID = "storageImages.list"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.state.mu.Lock()
	if _, ok := m.state.pools[poolName]; !ok {
		m.state.mu.Unlock()
		m.respondNotFound(w, r, opID, "storage pool not found on this agent")
		return
	}
	bucket := m.state.images[poolName]
	images := make([]agentapi.CachedImage, 0, len(bucket))
	for _, img := range bucket {
		images = append(images, imageToAPI(img))
	}
	m.state.mu.Unlock()
	body := agentapi.CachedImageList{
		Data: images,
		Meta: agentapi.PaginationMeta{NextCursor: nil},
	}
	m.respondJSON(w, r, opID, http.StatusOK, body)
}

// TasksGet implements GET /v1/tasks/{id}. Looks up the agent-task
// inserted by StoragePoolsScan, StorageImagesImport, or any future
// functional initiator and returns the OpenAPI Task projection at
// the current instant. Unknown ids return the standard not_found
// envelope.
//
// For storage_image.import tasks that have reached terminal-success,
// the handler materialises the imported file in state.images on the
// way out — the side effect mirrors a real agent's "file is on disk
// once the task is done" semantics, observable to subsequent
// StorageImagesList calls and the Test API's StoredImage helper.
func (m *Mock) TasksGet(w http.ResponseWriter, r *http.Request, taskID uuid.UUID) {
	const opID = "tasks.get"
	if m.preDispatch(w, r, opID) {
		return
	}
	now := time.Now().UTC()

	m.state.mu.Lock()
	task, ok := m.state.tasks[taskID]
	var snapshot agentTask
	if ok {
		snapshot = *task
		m.materializeImportLocked(&snapshot, now)
		m.materializeVMCreateLocked(&snapshot, now)
		m.materializeVMDeleteLocked(&snapshot, now)
		m.materializeVMLifecycleLocked(&snapshot, now)
	}
	m.state.mu.Unlock()

	if !ok {
		m.respondNotFound(w, r, opID, "task not found on this agent")
		return
	}
	m.respondJSON(w, r, opID, http.StatusOK, projectAgentTask(&snapshot, now))
}

// ---------------------------------------------------------------------
// Stub-501 handlers (38). Each delegates to respondNotImplemented.
// ---------------------------------------------------------------------

func (m *Mock) NetworksList(w http.ResponseWriter, r *http.Request, _ agentapi.NetworksListParams) {
	const opID = "networks.list"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) NetworksConfigure(w http.ResponseWriter, r *http.Request, _ agentapi.NetworksConfigureParams) {
	const opID = "networks.configure"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) NetworksDelete(w http.ResponseWriter, r *http.Request, _ agentapi.NetworkID, _ agentapi.NetworksDeleteParams) {
	const opID = "networks.delete"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) NetworksGet(w http.ResponseWriter, r *http.Request, _ agentapi.NetworkID) {
	const opID = "networks.get"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

// StoragePoolsScan implements POST /v1/storage-pools/{pool_name}/scan.
// Pops the next queued PoolScanResult for poolName (default success
// when the queue is empty), mints a fresh agent-task uuid, and
// returns 202 + AsyncTaskAccepted. The terminal projection
// materialises lazily on later GET /v1/tasks/{id} calls — see
// projectAgentTask.
func (m *Mock) StoragePoolsScan(w http.ResponseWriter, r *http.Request, poolName agentapi.PoolName, _ agentapi.StoragePoolsScanParams) {
	const opID = "storagePools.scan"
	if m.preDispatch(w, r, opID) {
		return
	}

	agentTaskID := uuid.New()
	m.state.mu.Lock()
	scan := m.state.takePoolScanResultLocked(poolName)
	// resourceID carries the per-instance row uuid if the pool was
	// registered through AddStoragePool; otherwise zero. Test code
	// asserts by taskType + result content, not on resourceID.
	var resourceID uuid.UUID
	if p, ok := m.state.pools[poolName]; ok {
		resourceID = p.ID
	}
	m.state.tasks[agentTaskID] = &agentTask{
		id:           agentTaskID,
		taskType:     "storage_pool.scan",
		resourceType: "storage_pool",
		resourceID:   resourceID,
		createdAt:    time.Now().UTC(),
		delay:        scan.Delay,
		result:       scan,
	}
	m.state.mu.Unlock()

	body := agentapi.AsyncTaskAccepted{
		TaskID: agentTaskID,
		Status: agentapi.AsyncTaskAcceptedStatus("pending"),
		Links: struct {
			Self string `json:"self"`
		}{Self: "/v1/tasks/" + agentTaskID.String()},
	}
	m.respondJSON(w, r, opID, http.StatusAccepted, body)
}

// VmsList implements GET /v1/vms — Phase A vertical slice. Delegates
// to vmList for the inventory snapshot.
func (m *Mock) VmsList(w http.ResponseWriter, r *http.Request, _ agentapi.VmsListParams) {
	const opID = "vms.list"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.vmList(w, r, opID)
}

// VmsCreate implements POST /v1/vms — Phase A vertical slice.
func (m *Mock) VmsCreate(w http.ResponseWriter, r *http.Request, _ agentapi.VmsCreateParams) {
	const opID = "vms.create"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.vmCreate(w, r, opID)
}

// VmsDelete implements DELETE /v1/vms/{vm_name} — Phase A vertical slice.
func (m *Mock) VmsDelete(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VmsDeleteParams) {
	const opID = "vms.delete"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.vmDelete(w, r, vmName, opID)
}

// VmsGet implements GET /v1/vms/{vm_name} — Phase A vertical slice.
func (m *Mock) VmsGet(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName) {
	const opID = "vms.get"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.vmGet(w, r, vmName, opID)
}

// VmsStart implements POST /v1/vms/{vm_name}/start — L2 async vertical
// slice. Returns 202 + AsyncTaskAccepted; the inventory transition
// (stopped/failed → running) lands lazily on later TasksGet calls.
func (m *Mock) VmsStart(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VmsStartParams) {
	const opID = "vms.start"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.vmLifecycleAsync(w, r, opID, vmName, "start")
}

// VmsStop implements POST /v1/vms/{vm_name}/stop — async graceful
// ACPI shutdown. Default success transitions running → stopped; tests
// stage stop-timeout failure via AddVMLifecycleResult with Status="failed"
// and a stop_timeout error envelope.
func (m *Mock) VmsStop(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VmsStopParams) {
	const opID = "vms.stop"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.vmLifecycleAsync(w, r, opID, vmName, "stop")
}

// VmsPoweroff implements POST /v1/vms/{vm_name}/poweroff — async
// hard shutdown. Default success transitions any → stopped.
func (m *Mock) VmsPoweroff(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VmsPoweroffParams) {
	const opID = "vms.poweroff"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.vmLifecycleAsync(w, r, opID, vmName, "poweroff")
}

// VmsReboot implements POST /v1/vms/{vm_name}/reboot — async stop+
// start cycle. Default success transitions running → running (a
// reboot cycle viewed on the inventory edge looks like a no-op since
// the wire status converges back to running; the real-agent
// distinction from reset — PID change — is not modelled at the mock
// layer because the wire shape carries no PID).
func (m *Mock) VmsReboot(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VmsRebootParams) {
	const opID = "vms.reboot"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.vmLifecycleAsync(w, r, opID, vmName, "reboot")
}

// VmsPause implements POST /v1/vms/{vm_name}/pause — L1 sync vertical
// slice. Looks up the stored AgentVM by name, transitions its status
// to paused, and returns the refreshed entry. 404 when the inventory
// has no entry for `vmName`; 409 when the current status is not
// `running`. Both error envelopes match the agent contract — the CP
// handler maps them straight via to the operator surface.
func (m *Mock) VmsPause(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VmsPauseParams) {
	const opID = "vms.pause"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.vmLifecycle(w, r, opID, vmName, "running", "paused")
}

// VmsResume implements POST /v1/vms/{vm_name}/resume — mirror of
// VmsPause, paused → running.
func (m *Mock) VmsResume(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VmsResumeParams) {
	const opID = "vms.resume"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.vmLifecycle(w, r, opID, vmName, "paused", "running")
}

// VmsReset implements POST /v1/vms/{vm_name}/reset — sync per the
// Pre-L1 spec amendment. Requires `running` and leaves the status
// `running` (reset preserves runtime identity; the QEMU process
// keeps going, only the guest CPU is reset).
func (m *Mock) VmsReset(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VmsResetParams) {
	const opID = "vms.reset"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.vmLifecycle(w, r, opID, vmName, "running", "running")
}

func (m *Mock) VmsResize(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.VmsResizeParams) {
	const opID = "vms.resize"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VmsRevert(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.VmsRevertParams) {
	const opID = "vms.revert"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMDisksList(w http.ResponseWriter, r *http.Request, _ agentapi.VMName) {
	const opID = "vmDisks.list"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMDisksAttach(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.VMDisksAttachParams) {
	const opID = "vmDisks.attach"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMDisksDetach(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.DeviceOrder, _ agentapi.VMDisksDetachParams) {
	const opID = "vmDisks.detach"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMDisksUpdate(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.DeviceOrder, _ agentapi.VMDisksUpdateParams) {
	const opID = "vmDisks.update"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMNicsList(w http.ResponseWriter, r *http.Request, _ agentapi.VMName) {
	const opID = "vmNics.list"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMNicsAttach(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.VMNicsAttachParams) {
	const opID = "vmNics.attach"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMNicsDetach(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.DeviceOrder, _ agentapi.VMNicsDetachParams) {
	const opID = "vmNics.detach"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMNicsUpdate(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.DeviceOrder, _ agentapi.VMNicsUpdateParams) {
	const opID = "vmNics.update"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMSnapshotsList(w http.ResponseWriter, r *http.Request, _ agentapi.VMName) {
	const opID = "vmSnapshots.list"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMSnapshotsCreate(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.VMSnapshotsCreateParams) {
	const opID = "vmSnapshots.create"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMSnapshotsGet(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.SnapshotName) {
	const opID = "vmSnapshots.get"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMSnapshotsDelete(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.SnapshotName, _ agentapi.VMSnapshotsDeleteParams) {
	const opID = "vmSnapshots.delete"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMMigrationsStartIncoming(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.VMMigrationsStartIncomingParams) {
	const opID = "vmMigrations.startIncoming"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMMigrationsStartOutgoing(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.VMMigrationsStartOutgoingParams) {
	const opID = "vmMigrations.startOutgoing"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMMigrationsGet(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.MigrationID) {
	const opID = "vmMigrations.get"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

func (m *Mock) VMMigrationsCancel(w http.ResponseWriter, r *http.Request, _ agentapi.VMName, _ agentapi.MigrationID, _ agentapi.VMMigrationsCancelParams) {
	const opID = "vmMigrations.cancel"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.respondNotImplemented(w, r, opID)
}

// VMConsoleIssueToken implements `POST /v1/vms/{vm_name}/console-token`
// for mock-driven integration tests. Mirrors the real-agent contract:
// looks up the stored VM, rejects non-running phase with 409, mints a
// single-use token via the shared TokenStore, and returns the
// agent-side wire shape (`ConsoleTokenResponse`).
func (m *Mock) VMConsoleIssueToken(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VMConsoleIssueTokenParams) {
	const opID = "vmConsole.issueToken"
	if m.preDispatch(w, r, opID) {
		return
	}
	name := vmName
	vm, ok := m.StoredVM(name)
	if !ok {
		m.respondError(w, r, opID, http.StatusNotFound, "not_found", "vm not found")
		return
	}
	if vm.Status != "running" {
		m.respondError(w, r, opID, http.StatusConflict, "vm_not_running",
			"vm is not running",
			map[string]any{"current_status": vm.Status})
		return
	}

	protocol := console.ProtocolSerial
	if r.Body != nil && r.ContentLength != 0 {
		var body struct {
			Protocol string `json:"protocol"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Protocol != "" {
			protocol = console.Protocol(body.Protocol)
		}
	}
	if !protocol.Valid() {
		m.respondError(w, r, opID, http.StatusBadRequest, "validation_failed",
			"unsupported protocol",
			map[string]any{"protocol": string(protocol)})
		return
	}

	raw, expiresAt, err := m.consoleTokens.Issue(name, protocol)
	if err != nil {
		m.respondError(w, r, opID, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	resp := map[string]any{
		"token":          raw,
		"expires_at":     expiresAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"websocket_path": "/v1/vms/" + name + "/console-stream",
		"protocol":       string(protocol),
	}
	m.respondJSON(w, r, opID, http.StatusOK, resp)
}

// VMConsoleStream implements `GET /v1/vms/{vm_name}/console-stream`
// for mock-driven integration tests. Validates the token, enforces
// the per-VM single-session lock, performs the WebSocket upgrade,
// and enters a minimal pump loop that echoes operator input back
// (enough so integration tests can prove bidirectional plumbing).
func (m *Mock) VMConsoleStream(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, params agentapi.VMConsoleStreamParams) {
	const opID = "vmConsole.stream"
	if m.preDispatch(w, r, opID) {
		return
	}
	name := vmName
	if params.Token == "" {
		m.respondError(w, r, opID, http.StatusUnauthorized, "unauthenticated", "missing token")
		return
	}
	if _, err := m.consoleTokens.Consume(params.Token, name); err != nil {
		m.respondError(w, r, opID, http.StatusUnauthorized, "unauthenticated", "invalid or expired token")
		return
	}
	if _, ok := m.StoredVM(name); !ok {
		m.respondError(w, r, opID, http.StatusNotFound, "not_found", "vm not found")
		return
	}
	if _, loaded := m.consoleLocks.LoadOrStore(name, struct{}{}); loaded {
		m.respondError(w, r, opID, http.StatusConflict, "console_in_use",
			"another console session is already open for this vm")
		return
	}
	defer m.consoleLocks.Delete(name)

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer func() { _ = wsConn.Close(websocket.StatusInternalError, "") }()

	// Send a banner so integration tests have something concrete to
	// observe arrive on the WebSocket. Operator-facing real agent
	// emits whatever QEMU's serial chardev produces (kernel boot
	// messages, login prompt); the mock substitutes a deterministic
	// fixture so assertions stay stable.
	if werr := wsConn.Write(r.Context(), websocket.MessageBinary, []byte("MOCK_AGENT_SERIAL_READY\r\n")); werr != nil {
		return
	}
	// Echo loop: every binary frame the operator sends comes back
	// uppercased so tests can detect round-trip plumbing with a distinct
	// signal from the banner. Exit when either side closes.
	ctx := r.Context()
	for {
		_, data, err := wsConn.Read(ctx)
		if err != nil {
			return
		}
		echo := make([]byte, len(data))
		for i, b := range data {
			if b >= 'a' && b <= 'z' {
				echo[i] = b - ('a' - 'A')
			} else {
				echo[i] = b
			}
		}
		if werr := wsConn.Write(ctx, websocket.MessageBinary, echo); werr != nil {
			return
		}
	}
}

// VmsLogs implements `GET /v1/vms/{vm_name}/logs`. The mock substitutes
// a deterministic banner so integration tests have stable bytes to
// assert against. With `follow=true` the handler also emits a steady
// drip of `MOCK_AGENT_LIVE_LOG_TICK\n` lines once per 200 ms until
// the client disconnects; with `follow=false` it ends after the
// banner.
//
// VM resolution mirrors the real-agent handler: 404 when the VM is
// not in StoredVMs. Tail / follow validation is intentionally
// permissive - the codegen layer pre-parses them so we accept
// whatever the wire produced.
func (m *Mock) VmsLogs(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, params agentapi.VmsLogsParams) {
	const opID = "vms.logs"
	if m.preDispatch(w, r, opID) {
		return
	}
	if _, ok := m.StoredVM(vmName); !ok {
		m.respondError(w, r, opID, http.StatusNotFound, "vm_not_found",
			"vm not found on mock")
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, "MOCK_AGENT_VM_LOGS_READY vm="+vmName+"\n"); err != nil {
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	follow := params.Follow != nil && *params.Follow
	if !follow {
		return
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := io.WriteString(w, "MOCK_AGENT_LIVE_LOG_TICK\n"); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}
