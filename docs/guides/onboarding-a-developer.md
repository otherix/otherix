# Onboard a developer

This walkthrough connects three things that are otherwise documented
separately: an **admin creating a user**, that **user authenticating their
CLI**, and that user **launching their first VM** - then what the `developer`
role can and cannot do. It is the multi-user counterpart of the
[Quickstart](../get-started/quickstart.md), which runs entirely as the bootstrap
admin.

Two people are involved:

- the **admin** (you already have an admin CLI cluster configured - see
  [Quickstart](../get-started/quickstart.md#2-point-the-cli-at-the-cluster));
- the **developer** being onboarded, who will end up with their own CLI access.

## 1. Admin: create the developer account

A username is the developer's unique identity and login credential (lowercase
letters, digits, interior hyphens, 3..32). Create the account with the
`developer` role:

```bash
# As the admin. Password is read from a no-echo prompt; pipe it with
# --password-stdin in scripts.
otherix user create dev-user --role developer
# or: otherix user create dev-user --role developer --password-stdin <<<"$DEV_PASSWORD"

otherix user get dev-user
# username    role       email   display_name
# dev-user    developer  -       -
```

Hand the username and the initial password to the developer (over a secure
channel). They can change their own password later with
`otherix user set-password dev-user`. See [Users and RBAC](users-and-rbac.md)
for the full role matrix.

## 2. Developer: authenticate the CLI

The developer points their own `otherix` CLI at the cluster. `config add cluster`
logs in with their **username** and password, mints a long-lived `otx_*` API
token scoped to their account, and stores it under a named cluster profile:

```bash
# As the developer, on their own machine.
otherix config add cluster \
  --name mycluster \
  --server https://cp.example.com \
  --login dev-user
# password prompted interactively (or --password / OTHERIX_PASSWORD)
```

For a TLS control plane the developer pins the cluster CA the same way the admin
did; see [Securing the API](securing-the-api.md) for `--ca-file` /
`--ca-fingerprint`. They can confirm who they are:

```bash
otherix user whoami
# username=dev-user role=developer
```

## 3. Developer: launch the first VM

A `developer` can create VMs and owns the ones they create. Booting a VM from a
cloud image is identical to the [Quickstart](../get-started/quickstart.md#4-create-a-vm):

```bash
otherix vm create my-first-vm \
  --image-url https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img \
  --arch amd64 \
  --vcpus 2 \
  --memory-mb 2048 \
  --wait

otherix vm list
otherix vm console my-first-vm   # or: otherix vm logs my-first-vm -f
```

The scheduler places the VM on an eligible node; the developer does not choose
the node (an admin/operator can pin one with `--node`).

## 4. What a developer can and cannot do

Ownership is the dividing line. A `developer` holds their VM and snapshot
permissions at **own** scope:

- **Can**: create VMs and snapshots; start/stop/console/resize/migrate/delete
  **their own** VMs; read cluster metadata (nodes, networks, pools) needed to
  place work.
- **Can see, but not mutate, others' VMs**: listing shows every VM's metadata,
  but operating on a VM they do not own returns `403 permission_denied` (for an
  action their role allows on their own resources) or `404 not_found` (existence
  is never leaked across owners).
- **Cannot**: manage users, nodes, networks, storage pools, or firmwares -
  those are admin/operator surfaces.

```bash
# Allowed - the developer's own VM:
otherix vm delete my-first-vm

# Refused - an admin-only surface:
otherix user list
# error: permission_denied
```

A user cannot be deleted while they still own resources, so before offboarding a
developer, reassign or delete their VMs and snapshots first.

## Next steps

- [Users and RBAC](users-and-rbac.md) - the full role/permission matrix and the
  `otherix user` commands.
- [Create and manage VMs](create-and-manage-vms.md) - placement, lifecycle,
  resize, and snapshots.
- [Securing the API](securing-the-api.md) - CA pinning and token handling for
  the developer's `config add`.
