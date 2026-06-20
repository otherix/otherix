# Images

A VM in Otherix boots directly from an **image URL**. There is no template
object, no image registry to run, and no import step: you point `vm create` at a
qcow2 (or raw) image, and the agent on the node that runs the VM fetches that
image, caches it, and materializes the VM's root disk from it. This page explains
how that cache works, how content is pinned, and how a cached image moves between
nodes. For the commands, see the
[Create and manage VMs guide](../guides/create-and-manage-vms.md).

## Boot from a URL, not a template

```
otherix vm create web-1 --image-url https://example.com/ubuntu-24.04.qcow2 --arch arm64
```

The VM row is self-describing: it records the image URL, the optional expected
digest, the format, the architecture, and the firmware. There is no separate
"image" resource to manage and no cluster-side import worker - the image is just
content the agent fetches on first use, exactly like a container runtime pulling
an image the first time a pod lands on a node.

## The node image cache is content-addressed

Each node keeps an image cache keyed by the image's **sha256 content digest**.
The first VM that needs a given image on a node downloads it once; every later
VM on that node that needs the same content clones its disk from the cached copy
instead of downloading again. Cloning is a full byte copy, so a running VM is
fully decoupled from the cache - the cache entry can later be evicted or replaced
without touching any disk already in use.

Because the cache key is the content digest, two different URLs that serve the
*same bytes* share one cache entry, and one URL whose bytes change over time
produces two distinct entries.

## Pinned vs unpinned images

How the cache is keyed on a `create` depends on whether you pin the content:

| Mode | Flag | Behavior |
|---|---|---|
| **Unpinned** | `--image-url U` (no digest) | The URL is resolved to whatever content it currently serves, cached by that content's digest. |
| **Pinned** | `--image-url U --image-sha256 S` | The download is verified to hash to `S`. If a cached copy disagrees with `S` it is re-fetched and overwritten; if the URL serves something other than `S` the create fails `checksum_mismatch`. |

Pin an image (`--image-sha256`) whenever you need every VM from that line to be
**exactly** the same bits - reproducible fleets, golden images, anything you'd
otherwise verify by hand.

## Pull policy: mutable URLs and `imagePullPolicy`

Otherix borrows Kubernetes' `imagePullPolicy` model. The `--pull-policy` flag
(or `spec.imagePullPolicy` in a manifest) controls whether a cached image is
trusted for a URL:

| Policy | Meaning |
|---|---|
| `if-not-present` (default) | If the URL already resolves to a cached image, reuse it - do not re-fetch from the origin. Mirrors Kubernetes `IfNotPresent`. |
| `always` | Ignore the cache and the cluster's URL knowledge; re-fetch from the URL every time, then update what the cluster remembers. |

!!! warning "A mutable URL is not re-fetched under the default policy"
    URLs are mutable: a "latest" or rolling tag can serve new content under the
    same address. Under the default `if-not-present` a node that already cached
    that URL keeps booting the **old** content, the same way Kubernetes keeps an
    old image under `IfNotPresent`. For a URL whose content changes, either pin
    the exact bytes with `--image-sha256`, or use `--pull-policy always` to force
    a fresh fetch.

Under the hood the control plane keeps a small **URL-to-digest registry** so that
an `if-not-present` create can resolve the URL to the digest the cluster has seen
before and reuse a cached copy (including one on another node - see below) rather
than re-downloading. `always` deliberately bypasses that registry and refreshes
it from the freshly fetched bytes.

## Cached images move between nodes (peer pull)

A cache entry is not stranded on the node that first downloaded it. When a VM
lands on a node that does not yet have the image, the control plane brokers a
**peer pull**: the node fetches the cached image directly from a peer node that
already holds it, over mutual TLS, instead of re-downloading from the origin URL.
The control plane orchestrates the handoff (it discovers a holder and mints a
one-time token) but is never in the data path - the bytes flow node to node.

The practical effect: the slow origin download happens roughly once per cluster
for a given image, not once per node. Subsequent placements are a fast
intra-cluster copy.

## The image cache is bounded

The node image cache is not allowed to grow without limit. A background sweeper
evicts the **least-recently-used** images (an image's "last used" time is bumped
on every cache hit) once the cache exceeds its byte budget or the node drops
below a free-space floor. Eviction is safe by construction:

- a VM's disk is a full copy of the image, so evicting a cache entry never
  affects a running VM;
- an image that is mid-clone is locked and skipped, so eviction never races a
  create;
- on any error the sweeper does nothing (it never deletes on uncertainty).

If an evicted image is needed again later, it is simply re-fetched - from a peer
that still holds it if possible, otherwise from the origin URL.

## How this relates to snapshots

Images and [snapshots](snapshots.md) share the same content-addressed,
peer-pullable blob model: an image blob and a snapshot disk blob are both
immutable, digest-named artifacts that a node can serve to a peer. The two are
kept in separate stores and tracked independently - the durability and
reference-counted reclamation described on the snapshots page apply to snapshot
blobs, while images use the bounded LRU cache described above.
