# Plan: templated disk names (`diskName`)

Status: **planning done, ready to implement** — all open questions decided.

## Goal

Let a StorageClass define the Proxmox disk name through a template, similar to
`subDir` in csi-driver-nfs. With a predictable name, deleting and recreating a PVC
(same namespace and name) reattaches the existing Proxmox disk and its data,
instead of creating a new empty `vm-<id>-pvc-<uuid>` disk.

Example:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: proxmox-zfs-named
provisioner: csi.proxmox.sinextra.dev
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
parameters:
  storage: local-zfs
  diskName: "${pvc.metadata.namespace}.${pvc.metadata.name}.${k8sClusterName}"
```

Result: `local-zfs:vm-9999-myns.data.prod-k8s`

## Why this is mostly a naming change

`CreateVolume` (`pkg/csi/controller.go:338`) builds the name as
`fmt.Sprintf("vm-%d-%s", id, pvc)`, where `pvc` is the generated PV name (`pvc-<uuid>`).
Right after that it calls `getVolumeSize` and **only creates the disk if it doesn't
exist yet**, so reusing an existing disk already works once the name is predictable.

## Template variables

| Variable | Source | Controlled by |
|---|---|---|
| `${pvc.metadata.namespace}` | `csi.storage.k8s.io/pvc/namespace` (`--extra-create-metadata`) | cluster admin |
| `${pvc.metadata.name}` | `csi.storage.k8s.io/pvc/name` (`--extra-create-metadata`) | PVC creator |
| `${pv.metadata.name}` | `csi.storage.k8s.io/pv/name` (`--extra-create-metadata`) | provisioner (`pvc-<uuid>`) |
| `${pvc.metadata.annotations.<key>}` | PVC fetched through `kclient` | PVC creator |
| `${k8sClusterName}` | `features.k8sClusterName` in the driver config (Kubernetes cluster name) | admin |
| `${region}` | Proxmox cluster (`clusters[].region`), already in `CreateVolume` | admin |
| `${zone}` | Proxmox node where the disk is created, already in `CreateVolume` | admin / scheduler |

Notes:
- **Annotation keys:** the key is everything after `${pvc.metadata.annotations.` up to
  `}`, so keys with `.` and `/` work, for example
  `${pvc.metadata.annotations.volume.kubernetes.io/selected-node}`.
- **When the PVC is fetched:** only when the template uses an annotation (one extra
  API call). The controller ClusterRole already allows `get` on
  `persistentvolumeclaims`.
- **Missing annotation or metadata:** return an error so the provisioner retries.
  Never fall back to an empty string or to the old name.
- **Selected node:** the `selected-node` annotation (set with `WaitForFirstConsumer`)
  gives the Kubernetes node, but if the pod moves to another node the name changes
  and the old disk isn't reused. Document this.
- **No `${host}` variable:** use the annotation instead.

## Naming rules

- **Final name:** `vm-<controllerVmID>-<expanded template>`. The `vm-<vmid>-` prefix is
  required by the Proxmox LVM, ZFS and RBD plugins and by the driver's own `vmidre`.
  The template only controls the suffix.
- **Allowed characters:** `[A-Za-z0-9._-]` in the expanded suffix. These characters
  are valid in every storage type:

  | Where | Allowed | Length |
  |---|---|---|
  | Proxmox plugins (LVM, ZFS, RBD, dir) | `vm-<vmid>-` prefix, then no whitespace (dir: no `/`) | – |
  | LVM / LVM-thin | `A-Za-z0-9+_.-` | 127; device-mapper name (`vg-lv`, `-` doubled) about 128 |
  | ZFS | `A-Za-z0-9_-:.` | 255 for the full dataset path |
  | RBD | almost anything | large |
  | dir / nfs / cephfs / btrfs | no `/` | 255 bytes including `.raw`/`.qcow2` |
  | QEMU drive string | no `,` | – |

  - Uppercase is allowed: `${region}`, `${k8sClusterName}` and annotations can contain it.
  - Invalid characters are **rejected** (`InvalidArgument`), never replaced or
    lowercased. Changing them could make different inputs produce the same name
    (`Data` and `data`).
- **Empty values:** a variable that expands to `""` (for example an annotation set to
  an empty string) is an error, just like a missing one.
- **Length limit:** the whole name `vm-<vmid>-<suffix>` may be at most **120
  characters**.
  - Too long is an error, never cut short (cutting could also create identical names).
  - Edge cases (device-mapper `-` doubling) are left to Proxmox/LVM, which return
    clear errors.
- **All of these rules always apply**, whether `diskNameEnforceNamespace` is on or off.
- **File-based storages** (dir, nfs, cephfs, btrfs): the existing `<vmid>/…<.format>`
  handling in `volume.NewVolume` still applies.

## Namespace isolation: `diskNameEnforceNamespace`

**Problem.** `${pvc.metadata.name}` and annotations are chosen by whoever creates the
PVC. Without the namespace in the name, a PVC in namespace A can get namespace B's
disk.

**Why a simple "contains the namespace" check isn't enough.** Namespaces may contain
`-`: `team-a` + `db` and `team` + `a-db` both give `team-a-db`.

**What makes it safe.** Namespaces are DNS labels (`[a-z0-9-]`) and can't contain `.`
or `_`. If the namespace is followed by `.` or `_`, and everything before it has a
known end, the namespace can always be found without ambiguity. Nothing a user
controls can forge another namespace.

**The parameter:** StorageClass parameter `diskNameEnforceNamespace`, default `"true"`.

- **`"true"`: the template is checked; if it breaks a rule, `InvalidArgument`:**
  1. It must contain `${pvc.metadata.namespace}`, directly followed by `.` or `_`.
  2. Before the namespace, only these are allowed:
     - fixed literal text (for example `k8s.`);
     - `${k8sClusterName}` and `${zone}`, each **directly followed by `.` or `_`**.
       Neither can contain `.` or `_`: Proxmox node names are single hostname labels,
       and `features.k8sClusterName` is checked at config load (see
       [`${k8sClusterName}`](#k8sclustername)).
  3. Not allowed before the namespace:
     - `${pvc.metadata.name}`, `${pvc.metadata.annotations.*}` and `${pv.metadata.name}`
       (user- or provisioner-controlled);
     - `${region}`: it comes from the existing `clusters[].region` config and is also
       a topology label, so it may contain `.`. Restricting it would break existing
       configs.
  4. After the namespace separator, anything goes, including `-` and any variable.
- **`"false"`:** the template's structure is free (any order, any separators,
  namespace optional). This is an explicit opt-in to possible name conflicts across
  namespaces. The general [naming rules](#naming-rules) still apply.
- The template is checked rather than the namespace added automatically, so the
  StorageClass shows exactly the name that ends up in Proxmox.

**Why `-` is not allowed as a separator after the namespace (or `${k8sClusterName}` /
`${zone}`):** namespaces may contain `-`, so `team-a` + `db` and `team` + `a-db` would
both give `team-a-db`. Everywhere else, `-` is fine.

| Template (enforcement on) | Result | Allowed? |
|---|---|---|
| `${pvc.metadata.namespace}.${pvc.metadata.name}` | `myns.data` | yes |
| `${pvc.metadata.namespace}_${pvc.metadata.name}-${zone}` | `myns_data-pve-1` | yes |
| `${pvc.metadata.namespace}.${pvc.metadata.name}.${k8sClusterName}` | `myns.data.prod-k8s` | yes |
| `${k8sClusterName}.${pvc.metadata.namespace}.${pvc.metadata.name}` | `prod-k8s.myns.data` | yes |
| `k8s_${k8sClusterName}_${pvc.metadata.namespace}.${pvc.metadata.name}` | `k8s_prod-k8s_myns.data` | yes |
| `${pvc.metadata.namespace}-${pvc.metadata.name}` | `myns-data` | no: `-` after namespace |
| `${k8sClusterName}-${pvc.metadata.namespace}.${pvc.metadata.name}` | `prod-k8s-myns.data` | no: `-` after `${k8sClusterName}` |
| `${pvc.metadata.name}.${pvc.metadata.namespace}` | `data.myns` | no: user value before namespace |
| `${region}.${pvc.metadata.namespace}.${pvc.metadata.name}` | – | no: `${region}` before namespace |

**Error messages** must say which rule was broken and suggest a fix, for example:
`diskName: "-" after ${pvc.metadata.namespace} is not allowed; use "." or "_", or set
diskNameEnforceNamespace: "false"`.

## `${k8sClusterName}`

- **New config option:** `features.k8sClusterName` in `pkg/config/config.go`,
  next to `controllerVmID`. Also exposed through the chart `config.features`.
- **Use case:** several Kubernetes clusters (for example several single-node clusters)
  sharing one Proxmox storage, where all controllers default to VMID 9999 and would
  otherwise produce the same disk names.
- **Allowed value:** a DNS label (`[a-z0-9-]`, max 63), checked when the config is
  loaded. It can then safely appear before the namespace (see namespace isolation).
- **Why this name:** in this driver "cluster" already means the Proxmox cluster
  (`clusters:` list, `vol.Cluster()` returns the region, and the docs call `region`
  the "cluster name"). `k8s` in the key makes the meaning clear.
- **The template variable uses the same name,** `${k8sClusterName}`, so config key and
  variable match one-to-one. `diskName` is written once per StorageClass, so the
  longer name costs nothing. The docs still point out: for the Proxmox cluster, use
  `${region}`.
- **No default.** A shared default (like the cloud-controller-manager's `kubernetes`)
  would be identical across clusters and defeat the purpose.
- **Not set but used in a template:** return `InvalidArgument`, for example
  `diskName uses ${k8sClusterName} but features.k8sClusterName is not set in the driver
  config`.

## Needed fixes in existing code

1. **`isVolumeAttached` (`pkg/csi/utils.go:284`)** uses
   `strings.Contains(disk.File, pvc)`. With readable names, `vm-9999-ns.data` matches
   `vm-9999-ns.data2`, so attach and detach could act on the wrong disk. Change it to
   an exact match on the volume part of `disk.File` (strip `storage:` and drive
   options).
2. **`Volume.PV()` (`pkg/utils/volume/volume.go:149`)** assumes `vm-<id>-<pv>[.<fmt>]`
   and cuts at the first `.`. With templated names it returns wrong values. It is used
   by `deleteReplication`. Fix it, or keep templated names out of that path (see
   replication below).
3. **Size of a reused disk.** Today, without a source volume, `CreateVolume` only logs
   "volume has been created with different capacity" and reports the *requested*
   size, whatever the disk's actual size is. Proxmox disks can only grow. Decided:
   - **Too small:** grow automatically, **only when `diskName` is set**. Reuse the
     existing clone/snapshot path: set `ResizeRequired` / `ResizeSizeBytes`, so
     `ControllerPublishVolume` grows the disk after attaching (`controller.go:603`)
     and `NodeStageVolume` grows the filesystem (`node.go:210`). Classic
     `pvc-<uuid>` volumes keep today's behaviour.
   - **Larger than requested:** report the disk's **actual** size as
     `CapacityBytes`. If it exceeds the request's `limit_bytes`, return `OutOfRange`
     (CSI spec).
   - **Logging:** log clearly when an existing disk is reused, with old and new size.
4. **Handing out a disk that is still in use.** Today nothing prevents it. See
   [Protecting reused disks](#protecting-reused-disks).
5. **`Volume.CopyVolume` (`pkg/utils/volume/volume.go:64`)**, used for snapshots
   (`controller.go:863`), treats any `.` in the disk name as a file extension. On
   ZFS or LVM, `vm-9999-myns.data` would give the snapshot name
   `9999/vm-9999-snapshot-….data`, which is invalid there. PVC names may contain dots
   too, so this is needed anyway. Fix: only treat the end as an extension if it's a
   real format (`raw`, `qcow2`, `vmdk`) or the disk uses the file-based `<vmid>/…`
   layout.

## Protecting reused disks

### Current behaviour (why this is needed)

- **Lifecycle with `Retain`:**
  1. When the pod stops, `ControllerUnpublishVolume` detaches the disk.
  2. Deleting the PVC leaves the PV `Released` and the disk on the storage,
     unattached.
  3. A new PVC with the same name gets a **new PV with the same volume handle**.
     Kubernetes allows that.
  4. `NodeStageVolume` never formats a disk that already has a filesystem, so the
     data is kept.
- **`ControllerPublishVolume` → `attachVolume` (`utils.go:591`) only looks at the
  target VM.** A disk attached to **another** VM is attached additionally.
  `getVMByAttachedVolume` exists but is only used for resize and modify.
- **`CreateVolume` only checks whether the disk exists.** It never looks at PVs.
- **Local storage on several Proxmox nodes:** `CreateVolume` only searches the
  required node. If the old disk is on another node, a second, empty disk with the
  same name is created.
- **Consequence without the checks below:** two VMs writing to the same block device
  (same node with local storage, or any node with shared storage): data corruption.
- **Where Kubernetes helps, and where it doesn't:** it identifies CSI volumes by
  driver + volume handle, so its multi-attach protection may catch the "old detach
  not finished" race. It can't help when two bound PVCs map to the same disk, or
  when a VM outside Kubernetes uses it.

### Two moments, two checks (only for `diskName` volumes)

- **Provisioning (`CreateVolume`)** runs once per PVC; the target VM isn't known yet.
- **Attaching (`ControllerPublishVolume`)** runs each time a pod starts; now the target
  VM is known.
- **Why the PV check lives at provisioning:** when attaching, "the disk is attached to
  the target VM" looks the same whether it's this PV's own earlier attach or another
  PV's disk. So protection against two PVs sharing one disk must happen at
  provisioning, and it applies whether or not the disk is attached.

**`CreateVolume`:**

1. **PV check (always, whether or not the disk is attached):** list PVs and look for
   another PV whose `spec.csi.volumeHandle` equals the new volume ID.
   - `Bound`, `Available`, `Pending` → error naming that PV. The disk still belongs
     to someone. Example: a Deployment scaled to 0 keeps its PV `Bound` while the
     disk is detached.
   - `Released`, `Failed` → ignored. `Released` is the intended reuse case; a `Failed`
     PV can't be used by any pod.
   - The new PV doesn't exist yet at this point, so it can't match itself.
   - RBAC already allows `list` on `persistentvolumes`.
2. **Find the disk:**
   - Shared storage: as today.
   - Local storage: search **all** nodes that have the storage.
     - Found on another node than required: if the topology allows that node
       (`Immediate` binding), use it and return its topology. If not
       (`WaitForFirstConsumer` already picked a node), **fail with a clear error**
       instead of creating a duplicate.
3. **Found → reuse** (grow if too small, see needed fixes #3). **Not found → create.**

**`ControllerPublishVolume`:**

1. Disk attached to the **target VM** → success (normal CSI retry behaviour).
2. Disk attached to **any other VM** (another node of this cluster, another Kubernetes
   cluster, a VM attached by hand) → `FailedPrecondition` (what the CSI spec
   prescribes for "volume published to another node"), naming the VM ID. The pod
   stays in `ContainerCreating` with a clear event.
3. Not attached → attach.

Notes:
- **Cost of the attach check:** `getVMByAttachedVolume` reads the config of every VM
  on the storage's nodes, so it only runs for `diskName` volumes. The VolumeContext
  records `diskName`, so `ControllerPublishVolume` can tell.
- **Depends on needed fixes #1** (exact match in `isVolumeAttached`); otherwise
  `ns.data` would also match `ns.data2`.
- **The owner VMID (`vm-<vmid>-`) is already skipped** by `getVMByAttachedVolume`.

### Scenario overview

| Situation | Checked at | Result |
|---|---|---|
| Disk attached to a VM outside this Kubernetes cluster | attach | `FailedPrecondition`, event names the VM |
| Disk attached to another node VM of this cluster | attach | same |
| Disk not attached, no active PV refers to it | provisioning + attach | reused and attached |
| Disk not attached, but a `Bound`/`Available`/`Pending` PV refers to it | provisioning | error naming the PV |
| Disk already attached to the target VM | attach | success |
| No disk with that name | provisioning | created (local storage: all nodes searched first) |
| Disk on another local node, topology doesn't allow it | provisioning | error, no duplicate created |

## Replication

`diskName` together with `replicate: "true"` is rejected (`InvalidArgument`) in the
first version. Replication names the replication VM after the PV and finds it again
via `Volume.PV()`.

## Deleting and reclaiming

- **First version:** document `reclaimPolicy: Retain` (a top-level StorageClass field)
  as the way to keep disks for reuse. This needs no code.
  - Downside: `Released` PV objects are left over, and disks are never removed
    automatically.
- `onDelete` and the `lifecycle: keep` fix are postponed; see
  [Later / out of scope](#later--out-of-scope).
- The `diskName` docs point to `reclaimPolicy: Retain` and don't mention the
  `lifecycle: keep` annotation.

## Chart and deployment

- **`--extra-create-metadata`: always on.** Hard-code it in the `csi-provisioner` args in
  `charts/proxmox-csi-plugin/templates/controller-deployment.yaml`, next to
  `--leader-election`.
  - Harmless: unknown parameters are ignored, and only known fields go into the
    VolumeContext.
  - Driver side: if `diskName` is set but the metadata is missing, return an error
    that names the flag, for users with their own manifests.
- **`diskName`: through the existing `extraParameters` passthrough.** No new
  `storageClass[]` keys; add a commented example to `values.yaml`:
  ```yaml
  storageClass:
    - name: proxmox-zfs-named
      storage: local-zfs
      reclaimPolicy: Retain
      extraParameters:
        diskName: "${pvc.metadata.namespace}.${pvc.metadata.name}.${k8sClusterName}"
        # diskNameEnforceNamespace: "false"
  ```
  - Dedicated keys can be added later if the feature becomes popular.
- Add `features.k8sClusterName` to `values.yaml` `config.features`.
- Chart version bump (`hack/bump-chart-version.sh`); regenerate the chart README if
  `values.yaml` comments change it.
- **Docs note:** StorageClass `parameters` can't be changed after creation. Adding or
  changing `diskName` means creating a new StorageClass (or deleting and recreating
  the old one); existing PVs are not affected. `${…}` is not interpreted by Helm.

## Tests

- `pkg/csi/parameters_test.go`: parsing `diskName` and `diskNameEnforceNamespace`.
- New unit tests for expanding templates: every variable, annotation keys with
  `.` and `/`, missing and empty values, invalid characters (rejected, not replaced),
  the 120-character limit.
- Namespace-check tests: every row of the example table in
  [Namespace isolation](#namespace-isolation-disknameenforcenamespace), the
  `team-a`/`team` + `a-db` case, literal prefixes, and enforcement off.
- `pkg/utils/volume/volume_test.go`: `CopyVolume` with dots in block-storage names
  and with file-based names.
- `pkg/csi/utils_test.go`: `isVolumeAttached` exact-match cases (`ns.data` vs
  `ns.data2`).
- `pkg/utils/volume/volume_test.go`: `PV()` with templated names.
- `pkg/csi/controller_test.go`: `CreateVolume` with `diskName`:
  - a new disk is created;
  - an existing disk is reused (no create call);
  - a smaller reused disk gets `ResizeRequired` / `ResizeSizeBytes`; a larger one
    reports its actual size; larger than `limit_bytes` gives `OutOfRange`;
  - without `diskName`, the size behaviour is unchanged;
  - PV check: another `Bound`/`Available`/`Pending` PV with the same volume handle
    gives an error; `Released`/`Failed` PVs are ignored;
  - local storage: disk found on another node, both with a topology that allows it
    and with one that doesn't (error, no duplicate created);
  - missing metadata or annotation gives an error;
  - `replicate` is rejected.
- `pkg/csi/controller_test.go`: `ControllerPublishVolume` with `diskName`:
  - disk attached to the target VM gives success;
  - disk attached to another VM gives `FailedPrecondition` with the VM ID;
  - unattached disk is attached;
  - without `diskName`, the other-VM check is not run.
- `pkg/config/config_test.go`: `k8sClusterName`, including rejecting `.`/`_`/uppercase.
- e2e (later): delete and recreate a PVC and check the data is still there. Confirm
  that `.` in disk names works on LVM, ZFS, RBD and dir storages.

## Docs

The naming rules are not intuitive, so they need a dedicated, well-explained docs
section, not just a parameter list entry.

- `docs/options.md`, or a new `docs/disk-name.md` linked from `options.md`:
  - **What it's for:** reusing disks by name, and the flow of deleting and
    recreating a PVC.
  - **Reference:** all variables with source, who controls them, and examples.
  - **General naming rules:** characters, length, `vm-<vmid>-` prefix,
    empty/missing values.
  - **Namespace isolation, explained with the "why":** the `team-a-db` example, why
    `.`/`_` work and `-` doesn't, what may come before the namespace and why
    `${region}` may not.
  - **The allowed/rejected example table** from this plan.
  - **What `diskNameEnforceNamespace: "false"` means:** free structure, general
    rules still apply, conflicts across namespaces are the admin's responsibility.
  - **Reuse needs `reclaimPolicy: Retain`;** Released PVs must be cleaned up by hand.
  - **Caveats:** `selected-node` changes if a pod moves; changing `controllerVmID` or
    `k8sClusterName` later means existing disks aren't found; StorageClass parameters
    can't be changed after creation.
  - **Protection against sharing a disk:** what's checked when (see
    [Protecting reused disks](#protecting-reused-disks)), including the scenario
    table and the errors users will see (PVC error naming the PV, pod event naming
    the VM).
  - **Local storage on multi-node Proxmox clusters:** use `Immediate` binding or
    shared storage with `diskName`; otherwise a pod scheduled on another node gets
    an error instead of its old disk.
  - **Size on reuse:** a reused disk grows to the requested size but never shrinks;
    the PV shows the actual size.
  - **Changing `fstype`** (for example ext4 → xfs): reusing an old disk then fails at
    mount time. The driver never formats a disk that already has a filesystem, so no
    data is lost, but the PVC won't start.
  - **Recipes:** single cluster; several clusters sharing one storage
    (`${k8sClusterName}.${pvc.metadata.namespace}.${pvc.metadata.name}`); per-node disks.
- `docs/config.md`: `k8sClusterName`. Note that `controllerVmID` becomes part of templated
  disk names, so changing it later means existing disks aren't found for reuse.
- New example StorageClass under `docs/`.

## Open questions

1. ~~Name of the config option.~~ Decided: `features.k8sClusterName`, no default.
2. ~~Chart: `--extra-create-metadata` always on? Separate `diskName` values or
   `extraParameters`?~~ Decided: always on; `extraParameters` with an example.
3. ~~`lifecycle: keep`: restore, replace with `onDelete`, or remove the docs?~~
   Postponed; doesn't affect `diskName` (see below).
4. ~~Exact allowed character set and maximum length.~~ Decided: `[A-Za-z0-9._-]`,
   rejected not replaced, 120 characters total; `.`/`_` as namespace separator,
   relaxed prefix rule (see naming rules and namespace isolation).
5. ~~Resizing a reused disk that's too small: resize it, or reject the request?~~
   Decided: grow automatically (only with `diskName`), report actual size, log reuse
   (see needed fixes #3).

## Later / out of scope

Neither item affects `diskName`. With `reclaimPolicy: Retain`, `DeleteVolume` is never
called. The current `DeleteVolume` doesn't look up the PV by name, and
`deleteReplication` returns early for disks that use the controller VMID.

### `lifecycle: keep` annotation is broken (separate bug-fix PR)

- **What the docs say:** `docs/options.md` ("Persistent Storage") documents
  `csi.proxmox.sinextra.dev/lifecycle: "keep"` on a PV to keep the Proxmox disk.
- **What changed:** added in `0f7cd72`, removed in `be54c6b` with the claim that the
  check was unreachable. That's wrong, because the external-provisioner doesn't know
  the annotation. The constant is gone too, so the documented feature does nothing.
- **The real bug:** the old code looked up the PV by `Volume.PV()` (a name read
  from the disk name). For other disk names this either failed on every retry or
  skipped the delete without any message.
- **Fix:** in `DeleteVolume`, list PVs and match `spec.csi.volumeHandle` against the
  volume ID (RBAC already allows `list`). If no PV is found, delete as usual.

### `onDelete: delete|retain|archive` StorageClass parameter (follow-up)

- Builds on the same PV lookup.
- Store `onDelete` in `StorageParameters`, so it ends up in the PV's
  `spec.csi.volumeAttributes`. No change to the volume ID format, and no StorageClass
  lookup.
- Order of precedence: PV annotation `keep` > `onDelete` > default `delete`.
- Allows `reclaimPolicy: Delete` together with reuse: no leftover `Released` PVs.
- `archive` renames the disk, so a new PVC with the same name starts empty.
