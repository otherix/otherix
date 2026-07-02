# SSH access to VMs

Otherix lets you SSH into a VM **by name** even though the VM has **no public
IP** and there is **no bastion for you to run**. The control plane brokers a
short-lived tunnel: you run `otherix ssh <vm>`, and Otherix mints a short-lived
credential and pipes an ordinary `ssh` session through the control plane to the
guest. There is nothing to expose on the node and no guest IP to reach directly.

For how the brokering works under the hood - the gateway and relay transports,
the short-lived credentials, the anti-SSRF pinning - see
[Ingress concepts](../concepts/ingress.md).

## Prerequisites

- **The VM must trust the cluster SSH user-CA.** Otherix holds a cluster-wide
  SSH user certificate authority. Only a VM created to trust that CA can accept
  a brokered SSH login. Opt a VM in at create time with `--ssh-ingress`:

  ```bash
  otherix vm create web01 \
    --image-url https://example.com/noble.img \
    --arch arm64 \
    --network net-app \
    --ssh-ingress
  ```

  In a [manifest](declarative-manifests.md) this is the `sshIngressEnabled`
  field:

  ```yaml
  apiVersion: otherix/v1
  kind: VM
  metadata:
    name: web01
  spec:
    imageURL: https://example.com/noble.img
    arch: arm64
    network: net-app
    sshIngressEnabled: true
  ```

    !!! warning "Create-time only, no retrofit"
        SSH ingress is provisioned when the VM is created. A VM created without
        `--ssh-ingress` cannot gain it later - re-create the VM to opt it in.

- **The cluster SSH-ingress switch must be on.** SSH ingress is a cluster-level
  capability that an admin controls. If it is disabled, requesting
  `--ssh-ingress` at create time is rejected rather than silently producing a VM
  that cannot accept a brokered login.
- **A logged-in Otherix user with reach to the VM.** Your `otherix` CLI must be
  pointed at the cluster with a valid session, and your role must hold
  `vm:ssh` / `vm:connect` on the target VM. See
  [Users and RBAC](users-and-rbac.md).
- **A login that exists in the guest.** Otherix gates *reach* to the VM, not
  *login* - the guest `sshd` is the login authority. Use the cloud image's
  default account or a user you created in the VM's `user-data`, and pass it
  with `--login`.

## SSH into a VM

With the prerequisites met, one command opens a session:

```bash
otherix ssh web01                  # log in as root (default)
otherix ssh web01 --login deploy   # log in as a specific guest user
```

`otherix ssh` resolves your cluster credential, mints a **short-lived guest SSH
certificate**, and execs your system `ssh` client with a `ProxyCommand` that
tunnels through the control plane. It re-homes the connection to the guest's
port 22 and manages a TOFU (trust-on-first-use) known-hosts entry
(`StrictHostKeyChecking=accept-new`). `--login` selects the guest user and
defaults to `root`.

Because it is the real `ssh` client underneath, everything you would normally
pass to `ssh` still applies - the tunnel is transparent.

## How it tunnels

`otherix ssh` uses the **same broker** as `otherix forward`. The transport is
chosen automatically by the VM's NIC type:

- **Overlay VMs** are reached through the cluster **gateway** (the control plane
  stays out of the data path).
- **Bridge VMs** are reached through the **control plane relay**.

You do not choose or configure either path - Otherix picks it for you. See
[Ingress concepts](../concepts/ingress.md) for the detail.

    !!! note "`otherix ssh proxy` is internal"
        `otherix ssh proxy <vm> <port>` is the `ProxyCommand` primitive that
        `ssh` invokes under the hood. It is not a command you run by hand.

## Copying files and running scripted commands

`otherix ssh <vm-name>` opens an **interactive** session - it takes only the VM
name, so a trailing remote command (`otherix ssh web01 'systemctl status nginx'`)
is not accepted, and file-copy tools like `scp`/`rsync` cannot target the
brokered name directly (`scp web01:/tmp/file .` does **not** resolve, because
`web01` is a name Otherix brokers, not a routable host).

For a one-off remote command, a script, or a file copy, expose the guest's
SSH port on your loopback with [`otherix forward`](port-forwarding.md) and point
the standard tools at that local port:

```bash
# in one terminal: expose the guest's sshd on local port 2222
otherix forward web01 22 -L 127.0.0.1:2222

# in another: run a command, or copy files, through the local listener
ssh -p 2222 deploy@127.0.0.1 'systemctl status nginx'
scp -P 2222 file.tar.gz deploy@127.0.0.1:/tmp/
rsync -e 'ssh -p 2222' -av ./dist/ deploy@127.0.0.1:/srv/app/
```

See [Port forwarding](port-forwarding.md) for the full `otherix forward`
workflow.

## External / third-party SSH

To give SSH access to someone who has **no Otherix account** - a contractor, an
on-call engineer outside your team - do not share your own credential. Instead
mint a scoped **ingress-grant** and hand them the thin `otherix-ssh` connector.
That flow is covered in [Grant external access](external-access.md).

## Security notes

- **Short-lived certificates.** Each `otherix ssh` invocation mints a fresh
  guest certificate with a brief lifetime; there is no long-lived key sitting on
  the guest's `authorized_keys`.
- **Least privilege.** Otherix gates *reach* to the VM (your role's `vm:ssh` /
  `vm:connect` on that VM); the guest `sshd` remains the sole login authority.
  Removing or locking a guest account denies access regardless of your cluster
  role.
- **Brokered, no inbound path.** The VM has no public IP and no inbound port
  open on the node. All traffic flows out through the control plane broker to
  the gateway or relay - there is no route an attacker can dial directly.
- **TOFU host keys.** The client pins the guest's host key on first connect
  (`accept-new`) and warns on any later change, so a swapped guest key is
  visible.
