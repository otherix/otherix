// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"github.com/google/uuid"
)

// heartbeatRequest mirrors HeartbeatRequest in
// api/openapi/control-plane.yaml. It is declared locally because the
// CP-side spec is not codegen'd alongside agent.yaml.
type heartbeatRequest struct {
	AgentVersion string                 `json:"agent_version"`
	Architecture string                 `json:"architecture"`
	Migration    *heartbeatMigrationCap `json:"migration,omitempty"`
	Capabilities heartbeatCapabilities  `json:"capabilities"`
	Resources    heartbeatResources     `json:"resources"`
	Vms          []heartbeatVMReport    `json:"vms"`
}

type heartbeatCapabilities struct {
	CPUModel           string            `json:"cpu_model"`
	CPUFlags           []string          `json:"cpu_flags"`
	CPUCoresTotal      int               `json:"cpu_cores_total"`
	MemoryTotalMib     int64             `json:"memory_total_mib"`
	Hugepages2MibTotal *int              `json:"hugepages_2mib_total,omitempty"`
	Hugepages1GibTotal *int              `json:"hugepages_1gib_total,omitempty"`
	KernelVersion      string            `json:"kernel_version"`
	QEMUVersion        string            `json:"qemu_version"`
	KvmAvailable       bool              `json:"kvm_available"`
	NestedVirt         bool              `json:"nested_virt"`
	QEMUBinaries       map[string]string `json:"qemu_binaries"`
}

type heartbeatResources struct {
	CPUCoresAvailable  int   `json:"cpu_cores_available"`
	MemoryAvailableMib int64 `json:"memory_available_mib"`
}

type heartbeatVMReport struct {
	VMUUID uuid.UUID `json:"vm_uuid"`
	Phase  string    `json:"phase"`
}

type heartbeatMigrationCap struct {
	Host           string `json:"host"`
	PortRangeStart int    `json:"port_range_start"`
	PortRangeEnd   int    `json:"port_range_end"`
	PortsInUse     int    `json:"ports_in_use,omitempty"`
	PortsAvailable int    `json:"ports_available,omitempty"`
}

// SendHeartbeatNow synchronously builds a heartbeat from current
// state, posts it to ControlPlaneURL, and returns the CP's HTTP
// status. Independent of the goroutine schedule.
func (m *Mock) SendHeartbeatNow(ctx context.Context) (int, error) {
	if m.cpURL == "" {
		return 0, fmt.Errorf("agentmock: ControlPlaneURL is empty; SendHeartbeatNow needs Options.ControlPlaneURL")
	}
	body, err := m.buildHeartbeatBody()
	if err != nil {
		return 0, err
	}
	return m.pushHeartbeat(ctx, body)
}

// SuppressHeartbeats pauses the heartbeat goroutine. No-op when
// HeartbeatInterval is zero or the goroutine is already paused.
func (m *Mock) SuppressHeartbeats() {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	m.state.heartbeatSuppressed = true
}

// ResumeHeartbeats undoes SuppressHeartbeats. The next tick fires at
// most HeartbeatInterval later — no immediate catch-up.
func (m *Mock) ResumeHeartbeats() {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	m.state.heartbeatSuppressed = false
}

// buildHeartbeatBody constructs a heartbeatRequest from current state
// under the lock, releases the lock, and returns the marshalled body.
// The optional migration field is included only when the value
// differs from the value carried in the previous heartbeat.
func (m *Mock) buildHeartbeatBody() ([]byte, error) {
	m.state.mu.Lock()
	s := m.state
	caps := heartbeatCapabilities{
		CPUModel:           s.cpuModel,
		CPUFlags:           append([]string(nil), s.cpuFeatures...),
		CPUCoresTotal:      s.cpuCoresTotal,
		MemoryTotalMib:     s.memoryTotalMib,
		Hugepages2MibTotal: s.hugepages2MiBTotal,
		Hugepages1GibTotal: s.hugepages1GiBTotal,
		KernelVersion:      s.kernelVersion,
		QEMUVersion:        s.qemuVersion,
		KvmAvailable:       s.kvmAvailable,
		NestedVirt:         s.nestedVirt,
		QEMUBinaries:       cloneStringMap(s.qemuBinaries),
	}
	resources := heartbeatResources{
		CPUCoresAvailable:  s.cpuCoresAvailable,
		MemoryAvailableMib: s.memoryAvailableMib,
	}
	req := heartbeatRequest{
		AgentVersion: s.agentVersion,
		Architecture: s.architecture,
		Capabilities: caps,
		Resources:    resources,
		Vms:          []heartbeatVMReport{},
	}
	migrationCopy := s.migration
	if !reflect.DeepEqual(s.lastHeartbeatMigration, &migrationCopy) {
		req.Migration = &heartbeatMigrationCap{
			Host:           migrationCopy.Host,
			PortRangeStart: migrationCopy.PortRangeStart,
			PortRangeEnd:   migrationCopy.PortRangeEnd,
			PortsInUse:     migrationCopy.PortsInUse,
			PortsAvailable: migrationCopy.PortsAvailable,
		}
		s.lastHeartbeatMigration = &migrationCopy
	}
	m.state.mu.Unlock()

	return json.Marshal(req)
}

// pushHeartbeat sends body to the CP heartbeat endpoint outside the
// state lock. 4xx and 5xx responses are returned to the caller; the
// goroutine logs them and continues.
func (m *Mock) pushHeartbeat(ctx context.Context, body []byte) (int, error) {
	endpoint := fmt.Sprintf("%s/v1/nodes/%s/heartbeat", m.cpURL, m.nodeName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("agentmock: build heartbeat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.heartbeatHTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("agentmock: post heartbeat: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

// heartbeatLoop drives the periodic push. It exits when ctx is
// cancelled (Stop() owns the cancel func).
func (m *Mock) heartbeatLoop(ctx context.Context) {
	defer close(m.heartbeatDone)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.state.mu.Lock()
			suppressed := m.state.heartbeatSuppressed
			m.state.mu.Unlock()
			if suppressed {
				continue
			}
			body, err := m.buildHeartbeatBody()
			if err != nil {
				m.logger.Warn("agentmock: heartbeat build failed", slog.String("error", err.Error()))
				continue
			}
			status, err := m.pushHeartbeat(ctx, body)
			if err != nil {
				if ctx.Err() == nil {
					m.logger.Warn("agentmock: heartbeat push failed", slog.String("error", err.Error()))
				}
				continue
			}
			if status >= 400 {
				m.logger.Warn("agentmock: heartbeat received non-2xx",
					slog.Int("status", status))
			}
		}
	}
}

// newHeartbeatClient returns the *http.Client used by the heartbeat
// goroutine. In TLS-disabled mode it is a default client; in
// TLS-enabled mode it carries the node client cert and pins ca.crt
// as the trust root for the CP server cert.
func (m *Mock) newHeartbeatClient() (*http.Client, error) {
	if m.tlsMode == TLSDisabled {
		return &http.Client{Timeout: 30 * time.Second}, nil
	}
	cfg, err := clientTLSConfig()
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}, nil
}
