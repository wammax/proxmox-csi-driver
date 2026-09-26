# Disk names (`diskName`)

By default every PersistentVolume gets a new Proxmox disk named after the PV, for example
`local-zfs:vm-9999-pvc-6c1d…`. The name is random, so a PVC that is deleted and created
again always gets a new, empty disk.

With the StorageClass parameter `diskName` you define the disk name with a template
instead, similar to `subDir` in [csi-driver-nfs](https://github.com/kubernetes-csi/csi-driver-nfs).
The same PVC then always maps to the same disk:

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
  diskName: "${pvc.metadata.namespace}.${pvc.metadata.name}"
```

A PVC `data` in namespace `myns` gets the disk `local-zfs:vm-9999-myns.data`.

A complete example is in [proxmox-disk-name.yaml](proxmox-disk-name.yaml).

## Reusing a disk

1. The PVC `myns/data` is created. The driver creates the disk `vm-9999-myns.data`.
2. The PVC is deleted. With `reclaimPolicy: Retain` the PersistentVolume becomes
   `Released` and the disk stays on the Proxmox storage.
3. A new PVC `myns/data` is created. The driver finds the existing disk and uses it
   again, with all its data. No new disk is created.

Things to know:

* **Reuse needs `reclaimPolicy: Retain`.** With `Delete`, the disk is deleted together
  with the PVC. `Released` PersistentVolumes are not cleaned up automatically, delete
  them by hand when you no longer need them (this doesn't delete the disk).
* **Size:** a reused disk is never resized when it's reused.
  * **Larger than the request:** it keeps its size (Proxmox can't shrink disks), and the
    PV shows the real size.
  * **Smaller than the request:** provisioning fails with an error like `existing disk
    … is 1Gi, but the PVC requests 2Gi; request at most 1Gi and expand the PVC
    afterwards`. The disk and its data are not touched. Create the PVC with at most the
    disk's size, then grow it by editing the PVC (the StorageClass needs
    `allowVolumeExpansion: true`).
  * Why not grow it automatically: Kubernetes identifies a volume by its volume handle,
    and a reused disk has the same handle as before. If the old disk is still attached
    when the new pod starts, Kubernetes keeps that attachment and the driver never gets
    a chance to grow the disk. Normal PVC expansion always works.
* **File system:** an existing file system is never formatted again. If you change
  `csi.storage.k8s.io/fstype` (for example from `ext4` to `xfs`), mounting a reused disk
  fails, but no data is lost.
* **Snapshots and clones:** if a disk with the name already exists, it's reused as it
  is, and the snapshot or source volume is **not** copied into it.
* **StorageClass parameters can't be changed.** Kubernetes rejects changes to
  `parameters` and `reclaimPolicy` of an existing StorageClass. To start using `diskName`,
  create a new StorageClass (or delete and recreate the old one). Existing PVs are not
  affected.
* **Replication** (`replicate: "true"`) can't be combined with `diskName`.

## Template variables

| Variable | Value | Chosen by |
|---|---|---|
| `${pvc.metadata.namespace}` | namespace of the PVC | cluster admin |
| `${pvc.metadata.name}` | name of the PVC | whoever creates the PVC |
| `${pv.metadata.name}` | name of the PV (`pvc-<uuid>`) | Kubernetes |
| `${pvc.metadata.annotations.<key>}` | value of the PVC annotation `<key>` | whoever creates the PVC |
| `${k8sClusterName}` | `features.k8sClusterName` from the [driver config](config.md) | driver admin |
| `${region}` | Proxmox cluster (`topology.kubernetes.io/region`) | driver admin |
| `${zone}` | Proxmox node the disk is created on (`topology.kubernetes.io/zone`) | Kubernetes scheduler |

* **The PVC namespace, name and PV name** are passed to the driver by `csi-provisioner`
  started with `--extra-create-metadata`. The Helm chart always sets it. If you deploy
  the driver another way, add the flag, otherwise provisioning fails with an error that
  names it.
* **Annotation keys** may contain `.` and `/`: everything between
  `${pvc.metadata.annotations.` and `}` is the key, for example
  `${pvc.metadata.annotations.example.com/disk}`. The driver reads the PVC for this.
* **`${pvc.metadata.annotations.volume.kubernetes.io/selected-node}`** is the Kubernetes
  node the pod was scheduled to (set with `volumeBindingMode: WaitForFirstConsumer`).
  Careful: if the pod later runs on another node, a recreated PVC gets another name and
  therefore a new disk.
* **`${k8sClusterName}` is the Kubernetes cluster**, not the Proxmox cluster; for the
  Proxmox cluster use `${region}`.
* A missing or empty value is an error, the driver never falls back to another name.

## Naming rules

* The final name is `vm-<controllerVmID>-<template>`, for example `vm-9999-myns.data`.
  The `vm-<vmid>-` prefix is required by Proxmox; the template controls the rest.
* **Allowed characters** are `A-Z a-z 0-9 . _ -`, both in the template and in the values
  of the variables. Other characters are rejected, never replaced, because replacing
  could turn two different names into the same one.
* **Length:** the full disk name may have at most 120 characters, and the full volume ID
  (`<region>/<zone>/<storage>/<disk name>`) at most 128. Longer names are rejected, never
  cut short.
* **LVM:** device-mapper names (`<volume group>-<disk name>`) are limited to 127
  characters, and every `-` in them counts double. The driver checks this for `lvm` and
  `lvmthin` storages. With the default volume group `pve`, a 120 character name may
  contain only about 3 hyphens; typical names are far below the limit
  (`vm-9999-myns.data.prod-k8s` → 33 characters).

## Keeping namespaces apart

`${pvc.metadata.name}` and annotations are chosen by whoever creates the PVC. Without
care, a PVC in one namespace could produce the disk name of a PVC in another namespace
and get its data. So by default (`diskNameEnforceNamespace: "true"`) the template is
checked:

1. It must contain `${pvc.metadata.namespace}`, **directly followed by `.` or `_`**.
2. Before the namespace, only fixed text, `${k8sClusterName}` and `${zone}` are allowed,
   and these two variables must also be directly followed by `.` or `_`.
3. After that, anything goes, including `-` and any variable.

Why: namespaces never contain `.` or `_` (they are DNS labels), but they can contain `-`.
With `-` as separator, the namespace `team-a` with the PVC `db` and the namespace `team`
with the PVC `a-db` would both give `team-a-db`. With `.` they give `team-a.db` and
`team.a-db`: the first `.` after the namespace always marks its end, and nothing a user
chooses can imitate another namespace. `${k8sClusterName}` and `${zone}` can't contain
`.` or `_` either, so they may come first. `${region}` may (it comes from the existing
driver config), so it's only allowed after the namespace.

| Template | Result for `myns/data` | Allowed? |
|---|---|---|
| `${pvc.metadata.namespace}.${pvc.metadata.name}` | `myns.data` | yes |
| `${pvc.metadata.namespace}_${pvc.metadata.name}-${zone}` | `myns_data-pve-1` | yes |
| `${pvc.metadata.namespace}.${pvc.metadata.name}.${k8sClusterName}` | `myns.data.prod-k8s` | yes |
| `${k8sClusterName}.${pvc.metadata.namespace}.${pvc.metadata.name}` | `prod-k8s.myns.data` | yes |
| `k8s_${k8sClusterName}_${pvc.metadata.namespace}.${pvc.metadata.name}` | `k8s_prod-k8s_myns.data` | yes |
| `${pvc.metadata.namespace}-${pvc.metadata.name}` | `myns-data` | no: `-` after the namespace |
| `${k8sClusterName}-${pvc.metadata.namespace}.${pvc.metadata.name}` | `prod-k8s-myns.data` | no: `-` after `${k8sClusterName}` |
| `${pvc.metadata.name}.${pvc.metadata.namespace}` | `data.myns` | no: PVC name before the namespace |
| `${region}.${pvc.metadata.namespace}.${pvc.metadata.name}` | | no: `${region}` before the namespace |

A template that breaks a rule is rejected when a PVC is provisioned, with an error that
names the rule.

### `diskNameEnforceNamespace: "false"`

Turns the check off: the template can then have any structure, and doesn't need to
contain the namespace at all, for example to share disks by name across namespaces on
purpose. The naming rules above still apply. Keeping namespaces apart is then your
responsibility.

## Protection against sharing a disk

Because names repeat, the driver makes sure a disk is never used twice:

* **When a PVC is provisioned:** if another PersistentVolume that is `Bound`,
  `Available` or `Pending` already refers to the disk, provisioning fails with an error
  that names that PV. A `Released` (or `Failed`) PV doesn't block reuse, that is the
  purpose of `diskName`. Example: a Deployment scaled to 0 keeps its PV `Bound`, so its
  disk is protected even though it's not attached.
* **When a disk is attached:** if the disk is attached to any other VM (another
  Kubernetes node, a VM of another Kubernetes cluster, or a VM it was attached to by
  hand), attaching fails and the pod stays in `ContainerCreating` with an event naming
  that VM.

| Situation | Result |
|---|---|
| Disk attached to a VM outside this Kubernetes cluster | attach fails, the event names the VM |
| Disk attached to another node of this cluster | attach fails, the event names the VM |
| Disk not attached, no active PV refers to it | reused and attached |
| Disk not attached, but a `Bound`/`Available`/`Pending` PV refers to it | provisioning fails, the error names the PV |
| Disk already attached to the node the pod runs on | success |
| No disk with that name | a new disk is created |
| Disk exists on another Proxmox node (local storage), topology doesn't allow that node | provisioning fails, no second disk is created |

## Local storage on several Proxmox nodes

Local storages (LVM, ZFS, directories) exist separately on every Proxmox node. Before
creating a disk, the driver searches all nodes that have the storage:

* With `volumeBindingMode: Immediate`, a disk found on another node is used there, and
  the pod is scheduled to that node.
* With `WaitForFirstConsumer`, the scheduler has already picked a node. If the disk is on
  another node, provisioning fails with an error instead of creating a second, empty disk
  with the same name.

So for disks that should be reused across nodes, use shared storage or `Immediate`
binding. Templates containing `${zone}` give every node its own name, so nothing is
searched.

## Caveats

* **Don't change `controllerVmID` or `k8sClusterName` later.** Both are part of the disk
  name, so existing disks would no longer be found.
* **Never create a Proxmox VM with the controller VM ID** (default 9999). Proxmox deletes
  all disks owned by a VM when it's destroyed, and with `--destroy-unreferenced-disks`
  every `vm-9999-*` disk on every storage: that is every disk of this driver.
* **Drain a Kubernetes node before destroying its VM.** `qm destroy` also deletes the
  disks attached to the VM, including PV disks.

## Recipes

One Kubernetes cluster:

```yaml
diskName: "${pvc.metadata.namespace}.${pvc.metadata.name}"
```

Several Kubernetes clusters sharing one Proxmox storage, each with its own
`features.k8sClusterName` in the [driver config](config.md). All disks of a cluster are
listed together in Proxmox:

```yaml
diskName: "${k8sClusterName}.${pvc.metadata.namespace}.${pvc.metadata.name}"
```

One disk per Proxmox node, for local storage:

```yaml
diskName: "${pvc.metadata.namespace}.${pvc.metadata.name}.${zone}"
```
