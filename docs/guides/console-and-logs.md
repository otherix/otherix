# Console and logs

Reach a VM's serial console interactively, or stream its serial output
non-interactively, through the `otherix vm` CLI. Both reach the agent
owning the VM and require the VM to be running.

## Interactive console

```bash
otherix vm console web-1
```

`vm console` attaches you to the VM's serial console over a WebSocket
that the control plane bridges to the QEMU serial socket on the owning
node. The CLI issues a single-use console token, dials the stream, and
flips your terminal into raw mode for the duration:

```text
connected to web-1 (serial console). Press Ctrl+] to detach.
```

The Ubuntu cloud image boots through to a login prompt; press Enter to
wake it.

### Detaching

Press **Ctrl+]** (0x1D, telnet-style) to bring up a local prompt:

```text
close console? (y/N):
```

Answer `y` to detach; any other key resumes the session. The CLI never
forwards Ctrl+] to the guest - to send that byte literally you need a
different tool (the same trade-off `virsh console` makes).

### Constraints and errors

- **Serial only.** Only the serial protocol is implemented; vnc / spice
  are reserved for future iterations.
- **VM must be running.** Against a stopped VM the dial fails with
  `vm_not_running` - start it first.
- **Single active console.** The agent enforces one console session per
  VM. A second attach returns `console_in_use`; close the other session
  (Ctrl+] in that terminal) and retry.
- **Interactive terminal required.** `vm console` refuses to run when
  stdin is not a TTY.

Other dial errors are surfaced in operator language:
`protocol_not_available`, `unauthenticated` (token expired or already
consumed - just retry), `permission_denied`, `vm_not_found`, and
`agent_unreachable` (the CP could not reach the owning node's agent).

## Streaming logs

```bash
otherix vm logs web-1
```

`vm logs` streams the VM's serial console output to stdout
(kubectl-style). By default the full retained history is shown, then
the command exits.

| Flag | Default | Notes |
|---|---|---|
| `--tail` | `-1` | Trailing history lines. `-1` = all, `0` = none. |
| `--follow` / `-f` | `false` | Keep streaming live bytes after the initial history flushes. |

```bash
otherix vm logs web-1 --tail=100
otherix vm logs web-1 --follow
otherix vm logs web-1 --tail=50 --follow
```

With `--follow`, press **Ctrl+C** to stop streaming - the cancelled
context is treated as a clean exit, not an error.

Unlike `vm console`, `vm logs` does **not** contend for the single
console-session slot: you can tail logs while another operator (or you)
holds an interactive console on the same VM.

## See also

- [Create and manage VMs](create-and-manage-vms.md)
- [Join a node](join-a-node.md)
