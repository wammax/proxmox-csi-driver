# Plan: templated disk names (`diskName`)

Status: **planning done, ready to implement** — all open questions decided, live
verification and e2e baseline done.

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
- **LVM device-mapper limit** (`lvm`/`lvmthin` only): `len(vg) + 1 + len(name) +
  hyphens(vg) + hyphens(name)` must be at most 127 (device-mapper doubles every `-`).
  The VG name comes from the storage config (`vgname`), which `CreateVolume` already
  fetches. Too long gives `InvalidArgument`, for example `LVM device name would be 182
  characters (hyphens count double), max 127`. Verified live, see open question 7.
- **Volume ID limit:** in addition, the full volume ID (`region/zone/storage/disk`, the
  longer of the two ID forms) may be at most **128 bytes**, the CSI spec limit. Too long
  gives `InvalidArgument` with an error naming both lengths (disk name and volume ID).
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

**The parameter:** StorageClass parameter `diskNameEnforceNamespace`, default `"true"` (a `*bool`, see [Unchanged behaviour without `diskName`](#unchanged-behaviour-without-diskname)).

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

## Unchanged behaviour without `diskName`

Without `diskName`, the driver must behave exactly as before: same Proxmox calls, same
volume IDs, same PV attributes.

- **Scoped to `diskName`:** template expansion, all name checks (characters, 120/128,
  LVM), the PV check, the attach check, the all-nodes search, growing a reused disk,
  **reporting the actual size of a larger disk**, the reuse log line, blocking
  `replicate`, the PVC lookup for annotations.
- **`diskNameEnforceNamespace` is a `*bool`** (json tag `diskNameEnforceNamespace`),
  with the `true` default applied in code. `ToMap()` skips nil pointers, so the key
  never appears in `volumeAttributes` unless it was set. A plain `bool` would add
  `diskNameEnforceNamespace: "1"` to every new PV, because plain bools are always
  written (that's why every PV today has `backup: "0"`, `iothread: "1"`). The
  parameter is ignored when `diskName` isn't set.
- **`diskName` itself** is a string with `omitempty`, so it only appears in
  `volumeAttributes` when set. That's also how `ControllerPublishVolume` knows a
  volume is a `diskName` volume.
- **Global but without effect** (verified): `--extra-create-metadata` (keys ignored, not
  copied into the PV), `features.k8sClusterName` (only checked when set).
- **Two bug fixes stay global** (needed fixes #1 `isVolumeAttached` and #5
  `CopyVolume`). For names the driver creates itself (`vm-9999-pvc-<uuid>`,
  `9999/vm-9999-pvc-<uuid>.<fmt>`), they give exactly the same results as today: a UUID
  can't be part of another one, and these names never contain a `.` except before
  the format. The results only differ for manually imported static PVs with unusual
  names, where the old behaviour was the bug.
- **Proof:** see the "no `diskName`" tests under [Tests](#tests), plus the same e2e
  scenarios compared with the baseline.

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
   - **Larger than requested (only with `diskName`):** report the disk's **actual** size as
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

- **No `diskName` (regression):**
  - `CreateVolume` and `ControllerPublishVolume` without `diskName` produce exactly the
    same Proxmox requests, volume ID and `volumeAttributes` as before;
  - `ToMap()` without `diskName`/`diskNameEnforceNamespace` has no new keys;
  - `diskNameEnforceNamespace` alone (without `diskName`) is ignored;
  - `isVolumeAttached` and `CopyVolume` give the same results as before for
    `vm-<id>-pvc-<uuid>` and `<id>/vm-<id>-pvc-<uuid>.<fmt>` names;
  - the extra `csi.storage.k8s.io/*` parameters are ignored.
- **LVM device-mapper check:** a name with many hyphens on `lvmthin` is rejected; the
  same name on ZFS is accepted; the error states the computed length.
- `pkg/csi/parameters_test.go`: parsing `diskName` and `diskNameEnforceNamespace`
  (`*bool`: unset, `"true"`, `"false"`).
- New unit tests for expanding templates: every variable, annotation keys with
  `.` and `/`, missing and empty values, invalid characters (rejected, not replaced),
  the 120-character limit, the 128-byte volume ID limit (long region/zone/storage
  names with a disk name under 120).
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

## Test environment

A dedicated Proxmox VE test host is set up (2026-09-26). Go and Docker are **not**
installed locally, so all building and testing happens there.

- **Access:** `ssh -F ~/.ssh/claude-pve.config <pve|builder|k8s-a|k8s-b>` (key
  `~/.ssh/claude-pve`; guests are reached through `pve`). Host `10.20.30.10`, Proxmox
  node name `dev`, standalone PVE 9.2, 2 vCPU, 15 GiB RAM.
- **Guests** on an internal NAT network `10.99.0.0/24`:
  - `builder` (LXC 200, `10.99.0.20`): Go 1.27.1, golangci-lint v2.13.2 (the CI
    version), Docker with buildx, helm, kubectl, image registry `10.99.0.20:5000`.
  - `k8s-a` (VM 101, `10.99.0.11`) and `k8s-b` (VM 102, `10.99.0.12`): single-node k3s
    v1.36.4, node labels `region=dev`, `zone=dev`, driver installed as Helm release
    `proxmox-csi` in `csi-proxmox`, VolumeSnapshot CRDs and controller v8.6.0. Both
    have a Proxmox snapshot `clean` for rollback. **k8s-b is kept stopped**; start it
    (`qm start 102`) for the multi-cluster `${k8sClusterName}` tests.
- **Storages and StorageClasses** (all smoke-tested: create, attach, write, delete):

  | StorageClass | Proxmox storage | Type |
  |---|---|---|
  | `proxmox-lvm` (default) | `local-lvm` | LVM-thin (block) |
  | `proxmox-zfs` | `local-zfs` (pool `tank/data`) | ZFS (block) |
  | `proxmox-dir` | `local`, `storageFormat: qcow2` | directory (file-based) |

  No Ceph/RBD, so RBD naming can't be tested here.
- **Proxmox API:** user `kubernetes-csi@pve`, token `csi`, role `CSI` with the extended
  (replication) privileges from `docs/install.md`.
- **Dev loop:**
  1. `rsync -az --delete -e "ssh -F $HOME/.ssh/claude-pve.config" ./ builder:/src/`
     (include `.git`; the Makefile needs it for the image tag).
  2. On builder: `source /etc/profile.d/go.sh; cd /src && make unit` / `make lint`.
  3. On builder: `/root/dev/deploy.sh [k8s-a|k8s-b]` builds the images, pushes them
     with a unique tag, runs `helm upgrade` with `/root/dev/values-<cluster>.yaml` and
     waits for the rollout. Clusters that aren't running are skipped.
  - Kubeconfigs: `/root/dev/kube/<cluster>` on builder. Values files contain the
    token secret; don't copy them into the repo.
- **Disk space is tight** (60 GB host disk + 10 GB ZFS disk). Keep test PVCs at
  1–2 Gi and delete them afterwards. The builder caps its Docker build cache at 3 GB.
- **Baseline (commit 82ce560):** `make lint` 0 issues, `make unit` passes.
- **Before implementing:**
  1. ~~Check the assumptions listed under "Not found in Context7" against the live
     clusters.~~ Done, see
     [Verification (live test env)](#verification-live-test-env-2026-09-26).
  2. Run a baseline e2e pass. **Result on `91e8f1f` (unchanged driver code), k8s-a,
     `E2E_STORAGECLASS=proxmox-lvm`:**

     | Scenario | Result | Why |
     |---|---|---|
     | `capacity` | pass | |
     | `attributes` | pass | Proxmox-side check skipped (`E2E_PROXMOX_CONFIG` not set) |
     | `snapshot-zones` | skip | needs two zones |
     | `lifecycle` | **fail (env)** | the test StatefulSet has *required* pod anti-affinity on `kubernetes.io/hostname`; the 2nd replica can't be scheduled on a single-node cluster |
     | `snapshot` | **fail (env)** | 1st run: the chart's `csi-snapshotter` sidecar is off by default (`controller.snapshotter.enabled`), so nothing handled the snapshot. **Fixed:** enabled in `/root/dev/values-k8s-{a,b}.yaml` (backups `*.bak-snapshotter`) and on k8s-a via `helm upgrade --reuse-values`; the snapshotter then cleaned up the stuck leftovers. 2nd run: `403 Permission check failed (user != root@pam)`. Snapshots need a **root@pam** API token (`docs/volumesnapshot.md`), and the test env uses `kubernetes-csi@pve`. Cleaned up properly. |
     | `ephemeral`, `shared`, `replication` | not run | need an encrypted SC, shared storage or two zones |

     Both failures come from the environment, not the driver. To compare after
     implementing: `lifecycle` can't pass on single-node clusters (or run only its
     single-replica steps); `snapshot` needs a root@pam token. **Decided: skip the
     snapshot e2e tests for now** (snapshots are experimental; no root@pam token is set
     up). The snapshot-related parts of the plan (`CopyVolume` fix #5) are covered by
     unit tests only. The e2e framework defaults to some StorageClass names
     that don't exist here (`proxmox`, `proxmox-secret`, `proxmox-ceph`, `proxmox-rbd`;
     `proxmox-zfs` for replication does match); set
     `E2E_STORAGECLASS=proxmox-lvm`, `E2E_STORAGECLASSES=proxmox-lvm,proxmox-zfs,proxmox-dir`
     and friends (see `test/e2e/framework/config.go`). Shared-storage (Ceph) tests don't
     apply.

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
  - **Size on reuse:** a reused disk grows to the requested size but never shrinks
    (Proxmox: "Shrinking disk size is not supported"); the PV shows the actual size.
  - **Destroying a Kubernetes node VM in Proxmox:** `qm destroy` removes the VM "and all
    used/owned volumes", so PV disks still attached to it are deleted too, even though
    they are named after the controller VMID. Drain the node (so disks are detached)
    before destroying its VM.
  - **Never create a VM with the controller VMID** (default 9999). Destroying it removes
    all volumes it owns, and with `--destroy-unreferenced-disks` every `vm-9999-*` disk
    on all storages, which is every PV disk of the driver.
  - **Changing `fstype`** (for example ext4 → xfs): reusing an old disk then fails at
    mount time. The driver never formats a disk that already has a filesystem, so no
    data is lost, but the PVC won't start.
  - **Recipes:** single cluster; several clusters sharing one storage
    (`${k8sClusterName}.${pvc.metadata.namespace}.${pvc.metadata.name}`); per-node disks.
- `docs/config.md`: `k8sClusterName`. Note that `controllerVmID` becomes part of templated
  disk names, so changing it later means existing disks aren't found for reuse.
- New example StorageClass under `docs/`.

## Verification (Context7, 2026-09-26)

**Confirmed:**
- **nfs-csi `subDir`** uses exactly `${pvc.metadata.name}`, `${pvc.metadata.namespace}`,
  `${pv.metadata.name}`, and has `onDelete: delete|retain|archive`. Our naming follows it.
- **`csi.storage.k8s.io/pvc/namespace`** is a real CreateVolume parameter key; another CSI
  driver (democratic-csi) builds volume IDs from it.
- **Keys with the `csi.storage.k8s.io/` prefix are reserved** by the external-provisioner,
  so our own parameter names (`diskName`, `diskNameEnforceNamespace`) must not use it.
  They don't.
- **`Retain`:** deleting the claim leaves the PV `Released` with the data intact, and an
  admin must clean up by hand. A StorageClass without `reclaimPolicy` defaults to
  `Delete`.
- **Proxmox dir storage naming:** `vm-<VMID>-<NAME>.<FORMAT>`, where `<NAME>` is
  "arbitrary name (ascii) without white space" and `<FORMAT>` is `raw|qcow2|vmdk`. This
  matches our character set and the `CopyVolume` fix (#5).
- **Proxmox volume ownership:** image volumes are owned by the VMID in their name. `qm
  destroy` removes "all used/owned volumes"; `--destroy-unreferenced-disks` also removes
  unattached disks with that VMID. Added to the docs caveats.
- **Proxmox cannot shrink disks.** This matches the "grow only" decision.
- **gopkg.in/yaml.v3:** unknown keys are ignored unless `KnownFields(true)` is set; keys
  are matched against the `yaml:` tag.

**Not found in Context7 (still from knowledge):**
- the CSI spec itself: `FailedPrecondition` for "published to another node",
  `OutOfRange` for `limit_bytes`, and the 128-byte size limit (see open question 6);
- RBD naming and length limits (no Ceph in the test env).

## Verification (live test env, 2026-09-26)

Checked on k8s-a / the `dev` Proxmox host. The test objects were removed and the Helm
release rolled back afterwards.

- **`--extra-create-metadata`** exists in csi-provisioner v6.3.0 ("add pv/pvc metadata
  to plugin create requests as parameters"). With it enabled, `CreateVolume` received:
  `csi.storage.k8s.io/pv/name: pvc-<uuid>`, `csi.storage.k8s.io/pvc/name: data`,
  `csi.storage.k8s.io/pvc/namespace: dn-verify`.
  - These keys did **not** end up in the PV's `volumeAttributes` (only known fields
    are copied), so turning the flag on changes nothing for existing volumes.
  - **PVC annotations are not passed**, which confirms the extra PVC lookup for
    `${pvc.metadata.annotations.*}`.
- **`volume.kubernetes.io/selected-node: k8s-a`** is set on the PVC with
  `WaitForFirstConsumer`, and stays there after binding.
- **Request details seen:** `capacity_range` had only `required_bytes` (no
  `limit_bytes`), so the `OutOfRange` case will rarely trigger with Kubernetes. A
  `ReadWriteOnce` PVC arrives as access mode `SINGLE_NODE_MULTI_WRITER`.
  `accessibility_requirements` had requisite and preferred both set to the selected
  node's zone.
- **StorageClass immutability:** a server-side dry-run patch of `parameters` and of
  `reclaimPolicy` both failed with "field is immutable".
- **Proxmox volume names** (`pvesm alloc`, 4 MiB, freed afterwards) on `local-lvm`
  (LVM-thin), `local-zfs` (ZFS) and `local` (dir, raw): names with `.`, `_`,
  uppercase, and a 120-character name were **all accepted** on all three.
- **LVM device-mapper limit is real:** a 120-character name with 56 hyphens failed on
  LVM-thin with "Failed to set device name" (nothing left behind). Device-mapper names
  are `<vg>-<lv>` with every `-` doubled and must stay under 128 characters. With VG
  `pve`, a 120-character name can contain only about 3 hyphens. Typical names are far
  below this (`vm-9999-myns.data.prod-k8s`: 26 characters, 4 hyphens → 34). See open
  question 7.

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
6. ~~Volume ID length.~~ Decided: keep the 120-character disk name limit **and** check
   that the full volume ID is at most 128 bytes (CSI spec, from knowledge; a
   third-party driver's docs also cite it). Today's IDs are about 75 bytes.
7. **LVM device-mapper name length (new, from live verification).** On `lvm`/`lvmthin`
   storages, `len(vg) + 1 + len(name) + hyphens(vg) + hyphens(name)` must stay under
   128, otherwise Proxmox fails with "Failed to set device name" (tested). Options:
   - **Check in the driver:** `CreateVolume` already fetches the storage config;
     `vgname` is returned by the API (`local-lvm` → `pve`) and the client's `Storage`
     struct has a `VGName` field. Give `InvalidArgument` with a clear message
     ("LVM device name would be N characters (hyphens count double), max 127").
   - **Leave it to Proxmox:** the error appears in the PVC events, but it's cryptic.

   **Decided: check in the driver** (only for `diskName` volumes on `lvm`/`lvmthin`).
8. ~~Does the driver behave exactly as before without `diskName`?~~ Decided, see
   [Unchanged behaviour without `diskName`](#unchanged-behaviour-without-diskname).

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
