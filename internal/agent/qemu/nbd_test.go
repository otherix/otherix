// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestQemuNBDServerArgs(t *testing.T) {
	args := NBDServerArgs(NBDServerSpec{
		CredsDir:       "/run/mig/tls",
		SourceIdentity: "CN=node-src",
		BindHost:       "10.0.0.2",
		Port:           49152,
		Export:         "tok123",
		DiskPath:       "/var/lib/otherix/vms/v1/disk.qcow2",
	})
	joined := strings.Join(args, " ")

	wants := []string{
		"--object tls-creds-x509,id=migtls,endpoint=server,dir=/run/mig/tls,verify-peer=on",
		"--object authz-simple,id=migauthz,identity=CN=node-src",
		"--tls-creds migtls",
		"--tls-authz migauthz",
		"--persistent",
		"--cache=none",
		"-f qcow2",
		"-x tok123",
		"-b 10.0.0.2",
		"-p 49152",
		"/var/lib/otherix/vms/v1/disk.qcow2",
	}
	for _, w := range wants {
		if !strings.Contains(joined, w) {
			t.Errorf("NBDServerArgs missing %q in:\n%s", w, joined)
		}
	}
	// Writable: must NOT contain the read-only flag.
	if strings.Contains(joined, "--read-only") || containsArg(args, "-r") {
		t.Errorf("NBDServerArgs is read-only; need writable for a push target")
	}
	// Fail-closed: never plaintext.
	if strings.Contains(joined, "verify-peer=off") {
		t.Errorf("NBDServerArgs disables peer verification")
	}
}

func TestQemuImgPushArgs(t *testing.T) {
	args := ImgPushArgs(ImgPushSpec{
		CredsDir:       "/run/mig/tls",
		SourceDisk:     "/var/lib/otherix/vms/v1/disk.qcow2",
		TargetHost:     "10.0.0.2",
		TargetPort:     49152,
		TargetIdentity: "node-tgt.agents.otherix.local",
		Export:         "tok123",
	})
	joined := strings.Join(args, " ")

	wants := []string{
		"convert",
		"-n",
		"-f qcow2",
		"--object tls-creds-x509,id=migtls,endpoint=client,dir=/run/mig/tls,verify-peer=on",
		"--target-image-opts",
		"/var/lib/otherix/vms/v1/disk.qcow2",
		"driver=nbd",
		"server.type=inet",
		"server.host=10.0.0.2",
		"server.port=49152",
		"tls-creds=migtls",
		"tls-hostname=node-tgt.agents.otherix.local",
		"export=tok123",
	}
	for _, w := range wants {
		if !strings.Contains(joined, w) {
			t.Errorf("ImgPushArgs missing %q in:\n%s", w, joined)
		}
	}
	// Source disk (positional) must precede the target image-opts blob.
	si := indexOfArg(args, "/var/lib/otherix/vms/v1/disk.qcow2")
	ti := lastIndexContaining(args, "driver=nbd")
	if si < 0 || ti < 0 || si > ti {
		t.Errorf("source disk (%d) must come before target opts (%d)", si, ti)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func indexOfArg(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func lastIndexContaining(args []string, sub string) int {
	idx := -1
	for i, a := range args {
		if strings.Contains(a, sub) {
			idx = i
		}
	}
	return idx
}

func TestWaitNBDListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if err := WaitNBDListening(context.Background(), ln.Addr().String(), 2*time.Second); err != nil {
		t.Errorf("WaitNBDListening on a live listener = %v, want nil", err)
	}

	// A free port nobody listens on -> timeout.
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := free.Addr().String()
	free.Close()
	if err := WaitNBDListening(context.Background(), addr, 300*time.Millisecond); err == nil {
		t.Errorf("WaitNBDListening on a dead address = nil, want timeout error")
	}
}
