/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package csi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"github.com/patrickmn/go-cache"
	"github.com/siderolabs/go-retry/retry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pxpool "github.com/sergelogvinov/go-proxmox-pool"
	proxmoxrest "github.com/sergelogvinov/go-proxmox-rest"
	"github.com/sergelogvinov/go-proxmox-rest/cluster"
	csiconfig "github.com/sergelogvinov/proxmox-csi-plugin/pkg/config"
	"github.com/sergelogvinov/proxmox-csi-plugin/pkg/metrics"
	toolsproxmox "github.com/sergelogvinov/proxmox-csi-plugin/pkg/tools/proxmox"
	utilsnode "github.com/sergelogvinov/proxmox-csi-plugin/pkg/utils/node"
	volume "github.com/sergelogvinov/proxmox-csi-plugin/pkg/utils/volume"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	deviceNamePrefix = "scsi"

	// resizeRequired is the key for the volume context parameter to indicate whether resize is required after restore from snapshot
	resizeRequired = "resizeRequired"
)

var controllerCaps = []csi.ControllerServiceCapability_RPC_Type{
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	csi.ControllerServiceCapability_RPC_GET_CAPACITY,
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
	csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
	csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
	csi.ControllerServiceCapability_RPC_GET_VOLUME,
	csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	csi.ControllerServiceCapability_RPC_MODIFY_VOLUME,
}

// ControllerService is the controller service for the CSI driver
type ControllerService struct {
	csi.UnimplementedControllerServer

	pxpool   *pxpool.ProxmoxPool
	kclient  kubernetes.Interface
	Provider csiconfig.Provider
	vmID     int

	// k8sClusterName is the value of ${k8sClusterName} in diskName templates.
	k8sClusterName string

	storageCapacity *cache.Cache
	vmLocks         *VMLocks
}

// NewControllerService returns a new controller service
func NewControllerService(kclient kubernetes.Interface, cloudConfig string) (*ControllerService, error) {
	cfg, err := csiconfig.ReadCloudConfigFromFile(cloudConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %v", err)
	}

	px, err := pxpool.NewProxmoxPool(cfg.Clusters)
	if err != nil {
		return nil, fmt.Errorf("failed to create proxmox cluster client: %v", err)
	}

	d := &ControllerService{
		pxpool:   px,
		kclient:  kclient,
		Provider: cfg.Features.Provider,
		vmID:     cfg.Features.ControllerVMID,

		k8sClusterName: cfg.Features.K8sClusterName,
	}

	d.Init()

	return d, nil
}

// ProxmoxPool returns the controller's Proxmox client pool. Intended for tests
// that need to point a region's client at an in-memory fake server after the
// service has already been constructed from static configuration.
func (d *ControllerService) ProxmoxPool() *pxpool.ProxmoxPool {
	return d.pxpool
}

// Init initializes the controller service
func (d *ControllerService) Init() {
	if d.vmLocks == nil {
		d.vmLocks = NewVMLocks()
	}

	if d.storageCapacity == nil {
		d.storageCapacity = cache.New(time.Minute, 5*time.Minute)
	}
}

// CreateVolume creates a volume
//
//nolint:gocyclo,cyclop
func (d *ControllerService) CreateVolume(ctx context.Context, request *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	klog.V(4).InfoS("CreateVolume: called", "args", protosanitizer.StripSecrets(request))

	pvc := request.GetName()
	if len(pvc) == 0 {
		return nil, status.Error(codes.InvalidArgument, "VolumeName must be provided")
	}

	volCapabilities := request.GetVolumeCapabilities()
	if volCapabilities == nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeCapabilities must be provided")
	}

	params, err := ExtractParameters(request.GetParameters())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	paramsVAC, err := ExtractModifyVolumeParameters(request.GetMutableParameters())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	klog.V(5).InfoS("CreateVolume: parameters", "parameters", params, "modifyParameters", paramsVAC)

	if params.StorageID == "" {
		return nil, status.Error(codes.InvalidArgument, "parameter storage must be provided")
	}

	var diskTemplate *diskNameTemplate

	if params.DiskName != "" {
		if params.Replicate {
			return nil, status.Error(codes.InvalidArgument, "parameter diskName can't be combined with replicate")
		}

		diskTemplate, err = parseDiskNameTemplate(params.DiskName)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}

		if params.DiskNameEnforceNamespace == nil || *params.DiskNameEnforceNamespace {
			if err = diskTemplate.checkNamespaceIsolation(); err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
		}
	}

	volSizeBytes := DefaultVolumeSizeBytes
	if request.GetCapacityRange() != nil {
		volSizeBytes = RoundUpSizeBytes(request.GetCapacityRange().GetRequiredBytes(), MinChunkSizeBytes)
	}

	accessibleTopology := request.GetAccessibilityRequirements()

	region, zone := locationFromTopologyRequirement(accessibleTopology)
	if region == "" {
		err := status.Error(codes.Internal, "cannot find best region")
		klog.ErrorS(err, "CreateVolume: region is empty", "accessibleTopology", accessibleTopology)

		return nil, err
	}

	var srcVol *volume.Volume

	contentSource := request.GetVolumeContentSource()
	if contentSource != nil {
		if contentSource.GetVolume() != nil {
			srcVol, err = volume.NewVolumeFromVolumeID(contentSource.GetVolume().GetVolumeId())
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
		}

		if contentSource.GetSnapshot() != nil {
			srcVol, err = volume.NewVolumeFromVolumeID(contentSource.GetSnapshot().GetSnapshotId())
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
		}
	}

	if srcVol != nil {
		if srcVol.Region() != region {
			err := status.Error(codes.InvalidArgument, "source snapshot region does not match the requested region")
			klog.ErrorS(err, "CreateVolume: source snapshot region does not match the requested region", "sourceRegion", srcVol.Region(), "requestedRegion", region)

			return nil, err
		}

		if _, err = d.checkVolume(ctx, srcVol); err != nil {
			if status.Code(err) == codes.NotFound {
				klog.ErrorS(err, "CreateVolume: zone or volume not found", "contentSourceID", srcVol.VolumeID())

				return nil, err
			}

			klog.ErrorS(err, "CreateVolume: failed to check volume", "cluster", srcVol.Cluster(), "contentSourceID", srcVol.VolumeID())

			return nil, err
		}
	}

	cl, err := d.pxpool.Get(region)
	if err != nil {
		klog.ErrorS(err, "CreateVolume: failed to get proxmox cluster", "cluster", region)

		return nil, status.Error(codes.Internal, err.Error())
	}

	if zone == "" {
		zones, err := storageNodes(ctx, cl, params.StorageID)
		if err != nil {
			klog.ErrorS(err, "CreateVolume: failed to get zones with storage", "cluster", region, "storage", params.StorageID)

			return nil, status.Errorf(codes.Internal, "failed to get zones with storage %s: %v", params.StorageID, err)
		}

		if len(zones) == 0 {
			klog.ErrorS(err, "CreateVolume: failed to find best zone: no nodes with the storage", "cluster", region, "storage", params.StorageID)

			return nil, status.Errorf(codes.Internal, "failed to find best zone: no nodes with the storage %s", params.StorageID)
		}

		zone = zones[0]
	}

	storageConfig, err := cl.Storage().Get(ctx, params.StorageID)
	if err != nil {
		klog.ErrorS(err, "CreateVolume: failed to get proxmox storage config", "cluster", region, "storage", params.StorageID)

		return nil, status.Errorf(codes.Internal, "failed to get proxmox storage config: %v", err)
	}

	klog.V(5).InfoS("CreateVolume: storage config", "storage", storageConfig)

	topology := []*csi.Topology{
		{
			Segments: map[string]string{
				corev1.LabelTopologyRegion: region,
				corev1.LabelTopologyZone:   zone,
			},
		},
	}

	if storageConfig.Shared {
		switch storageConfig.Type {
		case "cifs", "pbs": // nolint: goconst
			return nil, status.Error(codes.Internal, "error: shared storage type cifs, pbs are not supported")
		}

		config, err := cl.Storage().Get(ctx, params.StorageID)
		if err != nil {
			klog.ErrorS(err, "CreateVolume: failed to get proxmox storage config", "cluster", region, "storageID", params.StorageID)

			return nil, status.Errorf(codes.Internal, "failed to get proxmox storage config: %v", err)
		}

		topology = []*csi.Topology{}

		for _, node := range config.Nodes {
			if node == "" {
				continue
			}

			topology = append(topology, &csi.Topology{
				Segments: map[string]string{
					corev1.LabelTopologyRegion: region,
					corev1.LabelTopologyZone:   node,
				},
			})
		}

		if len(topology) == 0 {
			topology = append(topology, &csi.Topology{
				Segments: map[string]string{
					corev1.LabelTopologyRegion: region,
				},
			})
		}
	}

	id := d.vmID

	if params.Replicate {
		if storageConfig.Type != "zfspool" {
			return nil, status.Error(codes.Internal, "error: storage type is not zfs in replication mode")
		}

		id, err = prepareReplication(ctx, cl, zone, pvc, d.vmID)
		if err != nil {
			klog.ErrorS(err, "CreateVolume: failed to prepare replication", "cluster", region, "zone", zone)

			return nil, status.Error(codes.Internal, err.Error())
		}

		topology = []*csi.Topology{}

		for z := range strings.SplitSeq(params.ReplicateZones, ",") {
			topology = append(topology, &csi.Topology{
				Segments: map[string]string{
					corev1.LabelTopologyRegion: region,
					corev1.LabelTopologyZone:   z,
				},
			})
		}
	}

	format := ""

	switch storageConfig.Type {
	case "lvm":
		// LVM Snapshots as Volume-Chain are a technology preview.
		if storageConfig.SnapshotAsVolumeChain != nil && *storageConfig.SnapshotAsVolumeChain {
			format = "raw"
		}

		if params.StorageFormat == "qcow2" {
			format = params.StorageFormat
		}
	case "dir", "nfs", "cifs", "cephfs", "btrfs": // nolint: goconst
		format = "raw"
		if params.StorageFormat == "qcow2" {
			format = params.StorageFormat
		}
	}

	diskName := fmt.Sprintf("vm-%d-%s", id, pvc)

	if diskTemplate != nil {
		diskName, err = d.expandDiskName(ctx, diskTemplate, request.GetParameters(), region, zone)
		if err != nil {
			klog.ErrorS(err, "CreateVolume: failed to build disk name", "cluster", region, "diskName", params.DiskName)

			return nil, err
		}

		if (storageConfig.Type == "lvm" || storageConfig.Type == "lvmthin") && storageConfig.VGName != nil { // nolint: goconst
			if err = checkLVMDeviceName(*storageConfig.VGName, diskName); err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
		}
	}

	vol := volume.NewVolume(region, zone, params.StorageID, diskName, format)

	if diskTemplate != nil {
		// A local disk may already exist on another node. Reuse it there if the
		// topology allows it, never create a second disk with the same name.
		// A template with ${zone} gives every node its own name, nothing to search.
		if !storageConfig.Shared && !diskTemplate.uses(diskNameVarZone) {
			found, err := findLocalDisk(ctx, cl, region, zone, params.StorageID, diskName, format)
			if err != nil {
				klog.ErrorS(err, "CreateVolume: failed to search for existing disk", "cluster", region, "disk", diskName)

				return nil, status.Errorf(codes.Internal, "failed to search for existing disk %s: %v", diskName, err)
			}

			if found != "" && found != zone {
				if !topologyAllowsZone(accessibleTopology, region, found) {
					return nil, status.Errorf(codes.FailedPrecondition,
						"disk %s already exists on Proxmox node %s, but the volume has to be created on %s; use volumeBindingMode: Immediate or shared storage to reuse disks across nodes",
						diskName, found, zone)
				}

				klog.InfoS("CreateVolume: disk exists on another node, using that node", "cluster", region, "disk", diskName, "zone", found, "requestedZone", zone)

				zone = found
				vol = volume.NewVolume(region, zone, params.StorageID, diskName, format)
				topology = []*csi.Topology{
					{
						Segments: map[string]string{
							corev1.LabelTopologyRegion: region,
							corev1.LabelTopologyZone:   zone,
						},
					},
				}
			}
		}

		if len(vol.VolumeID()) > MaxVolumeIDLength {
			return nil, status.Errorf(codes.InvalidArgument, "volume ID %q is %d bytes long, the maximum is %d; shorten the diskName template (disk name %q is %d characters)",
				vol.VolumeID(), len(vol.VolumeID()), MaxVolumeIDLength, vol.Disk(), len(vol.Disk()))
		}

		if err = d.checkDiskNotInUse(ctx, vol.VolumeID(), vol.VolumeSharedID()); err != nil {
			return nil, err
		}
	}

	klog.V(5).InfoS("CreateVolume: creating volume", "cluster", region, "zone", zone, "volumeID", vol.VolumeID(), "size", volSizeBytes)

	size, err := getVolumeSize(ctx, cl, vol)
	if err == nil && diskTemplate != nil {
		// A reused disk is never grown here: Kubernetes may keep the old attachment
		// of the same volume handle and never call ControllerPublishVolume, so a
		// grow on attach isn't reliable. Growing goes through PVC expansion.
		if size < volSizeBytes {
			return nil, status.Errorf(codes.OutOfRange,
				"existing disk %s is %s, but the PVC requests %s; request at most %s and expand the PVC afterwards",
				vol.VolumeID(), resource.NewQuantity(size, resource.BinarySI), resource.NewQuantity(volSizeBytes, resource.BinarySI), resource.NewQuantity(size, resource.BinarySI))
		}

		klog.InfoS("CreateVolume: reusing existing disk", "cluster", region, "volumeID", vol.VolumeID(), "size", size, "requestedSize", volSizeBytes)
	}

	if err != nil {
		if err.Error() != ErrorNotFound {
			klog.ErrorS(err, "CreateVolume: failed to check volume", "cluster", region, "volumeID", vol.VolumeID())

			return nil, status.Errorf(codes.Internal, "failed to check volume: %v", err)
		}

		mc := metrics.NewMetricContext("createVolume")

		if srcVol != nil {
			size, err := getVolumeSize(ctx, cl, srcVol)
			if err != nil {
				if err.Error() != ErrorNotFound {
					klog.ErrorS(err, "CreateVolume: failed to check volume", "cluster", region, "volumeID", srcVol.VolumeID())

					return nil, status.Error(codes.Internal, err.Error())
				}

				return nil, status.Errorf(codes.NotFound, "snapshot %s is not found", srcVol.VolumeID())
			}

			if size == 0 {
				return nil, status.Errorf(codes.Unavailable, "snapshot %s is not yet available", srcVol.VolumeID())
			}

			klog.V(5).InfoS("CreateVolume: creating volume from snapshot", "volumeID", vol.VolumeID(), "snapshotID", srcVol.VolumeID())

			if vol.Storage() != srcVol.Storage() {
				return nil, status.Errorf(codes.InvalidArgument, "storage mismatch: requested storage %s does not match snapshot storage %s", vol.Storage(), srcVol.Storage())
			}

			if err = copyVolume(ctx, cl, srcVol, vol); mc.ObserveRequest(err) != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		} else {
			if err = createVolume(ctx, cl, vol, volSizeBytes); mc.ObserveRequest(err) != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		}

		err := retry.Constant(TaskTimeout*time.Second, retry.WithUnits(TaskStatusCheckInterval*time.Second)).RetryWithContext(ctx, func(ctx context.Context) error {
			size, err = getVolumeSize(ctx, cl, vol)
			if err != nil {
				if err.Error() == ErrorNotFound {
					klog.V(5).InfoS("CreateVolume: failed to get volume size, retrying", "cluster", region, "volumeID", vol.VolumeID())

					return retry.ExpectedError(err)
				}

				return fmt.Errorf("failed to get volume size: %v", err)
			}

			return nil
		})
		if err != nil {
			klog.ErrorS(err, "CreateVolume: failed to get volume size after creation", "cluster", region, "volumeID", vol.VolumeID())

			return nil, status.Errorf(codes.Internal, "failed to get volume size after creation: %s %s %v", vol.VolumeID(), vol.VolID(), err)
		}
	}

	if size < volSizeBytes {
		if srcVol != nil {
			if size == 0 {
				return nil, status.Errorf(codes.Unavailable, "volume %s is not yet available", srcVol.VolumeID())
			}

			params.ResizeRequired = new(true)
			params.ResizeSizeBytes = volSizeBytes
		}

		if srcVol == nil {
			klog.InfoS("CreateVolume: volume has been created with different capacity", "cluster", region, "volumeID", vol.VolumeID(), "size", size, "requestedSize", volSizeBytes)
		}
	}

	capacityBytes := volSizeBytes

	if diskTemplate != nil && size > volSizeBytes {
		// Proxmox can't shrink disks, report the real size of a larger reused disk.
		if limit := request.GetCapacityRange().GetLimitBytes(); limit > 0 && size > limit {
			return nil, status.Errorf(codes.OutOfRange, "existing disk %s is %d bytes, larger than the limit of %d bytes", vol.VolumeID(), size, limit)
		}

		capacityBytes = size
	}

	volumeID := vol.VolumeID()

	if params.Replicate {
		err = createReplication(ctx, cl, id, vol, params)
		if err != nil {
			klog.ErrorS(err, "CreateVolume: failed to create replication", "cluster", region, "volumeID", vol.VolumeID(), "vmID", id)

			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if storageConfig.Shared || params.Replicate {
		volumeID = vol.VolumeSharedID()
	}

	klog.V(3).InfoS("CreateVolume: volume created", "cluster", vol.Cluster(), "volumeID", volumeID, "size", capacityBytes)

	volume := csi.Volume{
		VolumeId:           volumeID,
		VolumeContext:      paramsVAC.MergeMap(params.ToMap()),
		ContentSource:      contentSource,
		CapacityBytes:      capacityBytes,
		AccessibleTopology: topology,
	}

	return &csi.CreateVolumeResponse{Volume: &volume}, nil
}

// DeleteVolume deletes a volume.
func (d *ControllerService) DeleteVolume(ctx context.Context, request *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.V(4).InfoS("DeleteVolume: called", "args", protosanitizer.StripSecrets(request))

	vol, err := volume.NewVolumeFromVolumeID(request.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cl, err := d.pxpool.Get(vol.Cluster())
	if err != nil {
		klog.ErrorS(err, "DeleteVolume: failed to get proxmox cluster", "cluster", vol.Cluster())

		return nil, status.Error(codes.Internal, err.Error())
	}

	err = deleteReplication(ctx, cl, vol, d.vmID)
	if err != nil {
		klog.ErrorS(err, "DeleteVolume: failed to delete replication", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to delete replication: %s, %v", vol.VolumeID(), err))
	}

	_, err = d.checkVolume(ctx, vol)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			klog.V(3).InfoS("DeleteVolume: zone or volume not found", "volumeID", vol.VolumeID())

			return &csi.DeleteVolumeResponse{}, nil
		}

		klog.ErrorS(err, "DeleteVolume: failed to check volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

		return nil, err
	}

	node := vol.Node()
	if node == "" {
		if node, err = getNodeForVolume(ctx, cl, vol); err != nil {
			klog.ErrorS(err, "DeleteVolume: failed to get node for volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

			return nil, status.Error(codes.Internal, fmt.Sprintf("failed to get node for volume: %s, %v", vol.VolumeID(), err))
		}
	}

	mc := metrics.NewMetricContext("deleteVolume")
	if err := toolsproxmox.DeleteStorageVolume(ctx, cl, node, vol.Storage(), vol.Disk()); mc.ObserveRequest(err) != nil {
		klog.ErrorS(err, "DeleteVolume: failed to delete volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to delete volume: %s, %v", vol.VolumeID(), err))
	}

	klog.V(3).InfoS("DeleteVolume: volume deleted", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerGetCapabilities get controller capabilities.
func (d *ControllerService) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(4).InfoS("ControllerGetCapabilities: called")

	caps := make([]*csi.ControllerServiceCapability, 0, len(controllerCaps))

	for _, cap := range controllerCaps {
		c := &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}

	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

// ControllerPublishVolume publish a volume
func (d *ControllerService) ControllerPublishVolume(ctx context.Context, request *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	klog.V(4).InfoS("ControllerPublishVolume: called", "args", protosanitizer.StripSecrets(request))

	n, err := utilsnode.ParseNodeID(request.GetNodeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if request.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeCapability must be provided")
	}

	params, err := ExtractParameters(request.GetVolumeContext())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	vol, err := volume.NewVolumeFromVolumeID(request.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cl, err := d.pxpool.Get(vol.Cluster())
	if err != nil {
		klog.ErrorS(err, "ControllerPublishVolume: failed to get proxmox cluster", "cluster", vol.Cluster())

		return nil, status.Error(codes.Internal, err.Error())
	}

	// Temporary workaround for unsafe mount, better to use a VolumeAttributesClass resource
	// It should be removed in the future, use backup=true/false in the volume attributes instead
	unsafeEnv := os.Getenv("UNSAFEMOUNT")
	if unsafeEnv == "true" { // nolint: goconst
		params.Backup = nil
	}

	if request.GetReadonly() {
		params.ReadOnly = new(true)
	}

	id, err := n.GetVMID()
	if err != nil || id == 0 {
		klog.V(5).InfoS("ControllerPublishVolume: VM ID not found in NodeID, will lookup by node name", "nodeID", n.String())

		id, _, err = d.getVMIDbyNode(ctx, n.GetNodeName())
		if err != nil {
			return nil, err
		}
	}

	size, err := d.checkVolume(ctx, vol)
	if err != nil {
		klog.ErrorS(err, "ControllerPublishVolume: failed to check volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

		return nil, err
	}

	// A disk with a templated name may be reused, so it could still be attached
	// somewhere else: never attach it to a second VM.
	if params.DiskName != "" {
		ids, err := vmsWithAttachedVolume(ctx, cl, vol)
		if err != nil {
			klog.ErrorS(err, "ControllerPublishVolume: failed to check volume attachments", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

			return nil, status.Error(codes.Internal, err.Error())
		}

		for _, other := range ids {
			if other != id {
				return nil, status.Errorf(codes.FailedPrecondition, "volume %s is attached to VM %d, detach it there before it can be attached to VM %d", vol.VolumeID(), other, id)
			}
		}
	}

	d.vmLocks.Lock(n.GetNodeName())
	defer d.vmLocks.Unlock(n.GetNodeName())

	if params.Replicate {
		err = migrateReplication(ctx, cl, id, vol, d.vmID)
		if err != nil {
			klog.ErrorS(err, "ControllerPublishVolume: failed to migrate/sync replication", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "vmID", id)

			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	mc := metrics.NewMetricContext("attachVolume")

	pvInfo, err := attachVolume(ctx, cl, id, vol, params.ToCFG())
	if mc.ObserveRequest(err) != nil {
		klog.ErrorS(err, "ControllerPublishVolume: failed to attach volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "vmID", id)

		return nil, status.Error(codes.Internal, err.Error())
	}

	if size < params.ResizeSizeBytes {
		klog.V(5).InfoS("ControllerPublishVolume: expandVolume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "vmID", id)

		mc := metrics.NewMetricContext("expandVolume")

		device := deviceNamePrefix + pvInfo["lun"]
		if err = resizeVMDisk(ctx, cl, vol.Node(), id, device, fmt.Sprintf("%dM", params.ResizeSizeBytes/MiB)); mc.ObserveRequest(err) != nil {
			klog.ErrorS(err, "ControllerPublishVolume: failed to resize vm disk", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "vmID", id)

			return nil, status.Error(codes.Internal, err.Error())
		}

		pvInfo[resizeRequired] = "true" // nolint: goconst
	}

	klog.V(3).InfoS("ControllerPublishVolume: volume published", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "nodeID", n.String())

	return &csi.ControllerPublishVolumeResponse{PublishContext: pvInfo}, nil
}

// ControllerUnpublishVolume unpublish a volume
func (d *ControllerService) ControllerUnpublishVolume(ctx context.Context, request *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	klog.V(4).InfoS("ControllerUnpublishVolume: called", "args", protosanitizer.StripSecrets(request))

	n, err := utilsnode.ParseNodeID(request.GetNodeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	vol, err := volume.NewVolumeFromVolumeID(request.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cl, err := d.pxpool.Get(vol.Cluster())
	if err != nil {
		klog.ErrorS(err, "ControllerUnpublishVolume: failed to get proxmox cluster", "cluster", vol.Cluster())

		return nil, status.Error(codes.Internal, err.Error())
	}

	_, err = d.checkVolume(ctx, vol)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			klog.V(3).InfoS("ControllerUnpublishVolume: zone or volume not found", "volumeID", vol.VolumeID())

			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}

		klog.ErrorS(err, "ControllerUnpublishVolume: failed to check volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

		return nil, err
	}

	id, err := n.GetVMID()
	if err != nil || id == 0 {
		klog.V(5).InfoS("ControllerUnpublishVolume: VM ID not found in NodeID, will lookup by node name", "nodeID", n.String())

		id, _, err = d.getVMIDbyNode(ctx, n.GetNodeName())
		if err != nil {
			if k8serrors.IsNotFound(err) {
				klog.V(3).InfoS("ControllerUnpublishVolume: VM not found for node, assuming volume is already unpublished", "nodeID", n.String())

				return &csi.ControllerUnpublishVolumeResponse{}, nil
			}

			return nil, err
		}
	}

	d.vmLocks.Lock(n.GetNodeName())
	defer d.vmLocks.Unlock(n.GetNodeName())

	mc := metrics.NewMetricContext("detachVolume")
	if err := detachVolume(ctx, cl, id, vol); mc.ObserveRequest(err) != nil {
		klog.ErrorS(err, "ControllerUnpublishVolume: failed to detach volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "vmID", id)

		return nil, status.Error(codes.Internal, err.Error())
	}

	if err := waitDetachVolume(ctx, cl, id, vol); err != nil {
		klog.ErrorS(err, "ControllerUnpublishVolume: failed to wait for volume detachment", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "vmID", id)

		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.V(3).InfoS("ControllerUnpublishVolume: volume unpublished", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "nodeID", n.String())

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities validate volume capabilities
func (d *ControllerService) ValidateVolumeCapabilities(_ context.Context, request *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	klog.V(4).InfoS("ValidateVolumeCapabilities: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ListVolumes list volumes
func (d *ControllerService) ListVolumes(_ context.Context, request *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	klog.V(4).InfoS("ListVolumes: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// GetCapacity get capacity
func (d *ControllerService) GetCapacity(ctx context.Context, request *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	klog.V(6).InfoS("GetCapacity: called", "args", protosanitizer.StripSecrets(request))

	topology := request.GetAccessibleTopology()
	if topology != nil {
		region, zone := GetNodeTopology(topology.GetSegments())
		storageID := request.GetParameters()[StorageIDKey]

		if region == "" || storageID == "" {
			return nil, status.Error(codes.InvalidArgument, "region and storage must be provided")
		}

		cl, err := d.pxpool.Get(region)
		if err != nil {
			klog.ErrorS(err, "GetCapacity: failed to get proxmox cluster", "cluster", region)

			return nil, status.Error(codes.Internal, err.Error())
		}

		storageConfig, err := storageResource(ctx, cl, storageID)
		if err != nil {
			klog.ErrorS(err, "GetCapacity: failed to get proxmox storage config", "cluster", region, "storageID", storageID)

			return nil, status.Error(codes.Internal, err.Error())
		}

		if zone == "" {
			if storageConfig.Shared == 0 {
				return nil, status.Error(codes.InvalidArgument, "zone must be provided")
			}

			zones, err := storageNodes(ctx, cl, storageID)
			if err != nil {
				klog.ErrorS(err, "GetCapacity: failed to get zones with storage", "cluster", region, "storage", storageID)

				return nil, status.Errorf(codes.Internal, "failed to get zones with storage %s: %v", storageID, err)
			}

			if len(zones) == 0 {
				klog.ErrorS(err, "GetCapacity: failed to find best zone: no nodes with the storage", "cluster", region, "storage", storageID)

				return nil, status.Errorf(codes.Internal, "failed to find best zone: no nodes with the storage %s", storageID)
			}

			zone = zones[0]
		}

		availableCapacity := int64(0)
		key := strings.Join([]string{region, zone, storageID}, "/")

		if v, ok := d.storageCapacity.Get(key); ok {
			if capacity, ok := v.(int64); ok {
				availableCapacity = capacity
			}
		}

		if availableCapacity == 0 {
			mc := metrics.NewMetricContext("storageStatus")

			storageStatus, err := cl.Nodes(zone).Storage().Status(ctx, storageID)
			if mc.ObserveRequest(err) != nil {
				klog.ErrorS(err, "GetCapacity: failed to get storage status", "cluster", region, "storageID", storageID, "storageConfig", storageConfig)

				if !proxmoxrest.IsNotFound(err) {
					return nil, status.Error(codes.Internal, err.Error())
				}
			} else {
				availableCapacity = storageStatus.AvailableSpace
				d.storageCapacity.SetDefault(key, availableCapacity)
			}
		}

		klog.V(6).InfoS("GetCapacity: collected", "region", region, "zone", zone, "storageID", storageID, "size", availableCapacity)

		return &csi.GetCapacityResponse{
			AvailableCapacity: availableCapacity,
		}, nil
	}

	return nil, status.Error(codes.InvalidArgument, "no topology specified")
}

// CreateSnapshot create a snapshot
func (d *ControllerService) CreateSnapshot(ctx context.Context, request *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	klog.V(4).InfoS("CreateSnapshot: called", "args", protosanitizer.StripSecrets(request))

	vol, err := volume.NewVolumeFromVolumeID(request.GetSourceVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	name := request.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "Name must be provided")
	}

	params := request.GetParameters()
	if params == nil {
		params = map[string]string{}
	}

	cl, err := d.pxpool.Get(vol.Cluster())
	if err != nil {
		klog.ErrorS(err, "CreateSnapshot: failed to get proxmox cluster", "cluster", vol.Cluster())

		return nil, status.Error(codes.Internal, err.Error())
	}

	storageConfig, err := cl.Storage().Get(ctx, vol.Storage())
	if err != nil {
		klog.ErrorS(err, "CreateSnapshot: failed to get proxmox storage config", "cluster", vol.Cluster(), "storageID", vol.Storage())

		return nil, status.Error(codes.Internal, err.Error())
	}

	switch storageConfig.Type {
	case "cifs", "pbs":
		err = status.Error(codes.Internal, "storage type cifs, pbs do not support snapshot")
		klog.ErrorS(err, "CreateSnapshot: unsupported storage type for snapshot", "cluster", vol.Cluster(), "storageID", vol.Storage(), "storageType", storageConfig.Type)

		return nil, err

	case "rbd":
		err = status.Error(codes.Internal, "storage type rbd(ceph) does not support snapshot")
		klog.ErrorS(err, "CreateSnapshot: unsupported storage type for snapshot", "cluster", vol.Cluster(), "storageID", vol.Storage(), "storageType", storageConfig.Type)

		return nil, err
	}

	if storageConfig.Shared {
		err = status.Error(codes.Internal, "shared storage does not support snapshot")
		klog.ErrorS(err, "CreateSnapshot: unsupported storage type for snapshot", "cluster", vol.Cluster(), "storageID", vol.Storage(), "storageType", storageConfig.Type)

		return nil, err
	}

	_, err = d.checkVolume(ctx, vol)
	if err != nil {
		klog.ErrorS(err, "CreateSnapshot: failed to check volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

		return nil, err
	}

	if vol.Node() == "" {
		node, err := getNodeForVolume(ctx, cl, vol)
		if err != nil {
			klog.ErrorS(err, "CreateSnapshot: failed to get node for volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

			return nil, status.Error(codes.Internal, fmt.Sprintf("failed to get node for volume: %s, %v", vol.VolumeID(), err))
		}

		vol.SetNode(node)
	}

	snapshotID := vol.CopyVolume(fmt.Sprintf("vm-%d-%s", d.vmID, name))

	if params["zone"] != "" {
		if len(storageConfig.Nodes) > 0 {
			if !slices.Contains(storageConfig.Nodes, params["zone"]) {
				err = status.Error(codes.InvalidArgument, "zone specified in parameters is not valid for the storage")
				klog.ErrorS(err, "CreateSnapshot: invalid zone in parameters", "cluster", vol.Cluster(), "storageID", vol.Storage(), "zone", params["zone"])

				return nil, err
			}
		}

		snapshotID.SetZone(params["zone"])
	}

	klog.V(5).InfoS("CreateSnapshot", "storageConfig", storageConfig, "volumeID", vol.VolumeID(), "snapshotID", snapshotID.VolumeID(), "params", params)

	size, err := getVolumeSize(ctx, cl, snapshotID)
	if err != nil {
		if err.Error() != ErrorNotFound {
			klog.ErrorS(err, "CreateSnapshot: failed to check volume", "cluster", vol.Cluster(), "snapshotID", snapshotID.VolumeID())

			return nil, status.Error(codes.Internal, err.Error())
		}

		err = copyVolume(ctx, cl, vol, snapshotID)
		if err != nil {
			klog.ErrorS(err, "CreateSnapshot: failed to create snapshot", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "snapshotID", snapshotID.VolumeID())

			return nil, status.Error(codes.Internal, err.Error())
		}

		size, err = getVolumeSize(ctx, cl, snapshotID)
		if err != nil {
			klog.ErrorS(err, "CreateSnapshot: failed to get snapshots after creation", "cluster", vol.Cluster(), "snapshotID", snapshotID.VolumeID())

			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	klog.V(3).InfoS("CreateSnapshot: snapshot created", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "snapshotID", snapshotID.VolumeID())

	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			CreationTime:   timestamppb.New(time.Now()),
			SnapshotId:     snapshotID.VolumeID(),
			SourceVolumeId: vol.VolumeID(),
			SizeBytes:      size,
			ReadyToUse:     size > 0,
		},
	}, nil
}

// DeleteSnapshot delete a snapshot
func (d *ControllerService) DeleteSnapshot(ctx context.Context, request *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	klog.V(4).InfoS("DeleteSnapshot: called", "args", protosanitizer.StripSecrets(request))

	vol, err := volume.NewVolumeFromVolumeID(request.GetSnapshotId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cl, err := d.pxpool.Get(vol.Cluster())
	if err != nil {
		klog.ErrorS(err, "DeleteSnapshot: failed to get proxmox cluster", "cluster", vol.Cluster())

		return nil, status.Error(codes.Internal, err.Error())
	}

	_, err = d.checkVolume(ctx, vol)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			klog.V(3).InfoS("DeleteSnapshot: zone or volume not found", "volumeID", vol.VolumeID())

			return &csi.DeleteSnapshotResponse{}, nil
		}

		klog.ErrorS(err, "DeleteSnapshot: failed to check volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

		return nil, err
	}

	node := vol.Node()
	if node == "" {
		if node, err = getNodeForVolume(ctx, cl, vol); err != nil {
			klog.ErrorS(err, "DeleteSnapshot: failed to get node for volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

			return nil, status.Error(codes.Internal, fmt.Sprintf("failed to get node for volume: %s, %v", vol.VolumeID(), err))
		}
	}

	mc := metrics.NewMetricContext("deleteVolume")
	if err := toolsproxmox.DeleteStorageVolume(ctx, cl, node, vol.Storage(), vol.Disk()); mc.ObserveRequest(err) != nil {
		klog.ErrorS(err, "DeleteSnapshot: failed to delete volume", "cluster", vol.Cluster(), "volumeName", vol.Disk())

		return nil, status.Error(codes.Internal, fmt.Sprintf("failed to delete volume: %s", vol.Disk()))
	}

	klog.V(3).InfoS("DeleteSnapshot: snapshot deleted", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

	return &csi.DeleteSnapshotResponse{}, nil
}

// ListSnapshots list snapshots
func (d *ControllerService) ListSnapshots(_ context.Context, request *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	klog.V(4).InfoS("ListSnapshots: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerExpandVolume expand a volume
func (d *ControllerService) ControllerExpandVolume(ctx context.Context, request *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	klog.V(4).InfoS("ControllerExpandVolume: called", "args", protosanitizer.StripSecrets(request))

	capacityRange := request.GetCapacityRange()
	if capacityRange == nil {
		return nil, status.Error(codes.InvalidArgument, "CapacityRange must be provided")
	}

	volSizeBytes := RoundUpSizeBytes(capacityRange.GetRequiredBytes(), MinChunkSizeBytes)
	maxVolSize := capacityRange.GetLimitBytes()

	if maxVolSize > 0 && maxVolSize < volSizeBytes {
		return nil, status.Error(codes.OutOfRange, "after round-up, volume size exceeds the limit specified")
	}

	vol, err := volume.NewVolumeFromVolumeID(request.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cl, err := d.pxpool.Get(vol.Cluster())
	if err != nil {
		klog.ErrorS(err, "ControllerExpandVolume: failed to get proxmox cluster", "cluster", vol.Cluster())

		return nil, status.Error(codes.Internal, err.Error())
	}

	_, err = d.checkVolume(ctx, vol)
	if err != nil {
		klog.ErrorS(err, "ControllerExpandVolume: failed to check volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

		return nil, err
	}

	// FIXME: check current size and skip resize if not needed

	id, lun, err := getVMByAttachedVolume(ctx, cl, vol)
	if err != nil || id == 0 {
		if errors.Is(err, errVirtualMachineNotFound) {
			klog.V(3).InfoS("ControllerExpandVolume: volume is not published, cannot resize unpublished volumeID", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

			return nil, status.Error(codes.Internal, "cannot resize unpublished")
		}

		klog.ErrorS(err, "ControllerExpandVolume: failed to get vm by attached volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

		return nil, status.Error(codes.Internal, err.Error())
	}

	mc := metrics.NewMetricContext("expandVolume")

	device := deviceNamePrefix + strconv.Itoa(lun)
	if err = resizeVMDisk(ctx, cl, vol.Node(), id, device, fmt.Sprintf("%dM", volSizeBytes/MiB)); mc.ObserveRequest(err) != nil {
		klog.ErrorS(err, "ControllerExpandVolume: failed to resize vm disk", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "vmID", id)

		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.V(3).InfoS("ControllerExpandVolume: volume expanded", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "vmID", id, "size", volSizeBytes)

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         volSizeBytes,
		NodeExpansionRequired: true,
	}, nil
}

// ControllerGetVolume get a volume
func (d *ControllerService) ControllerGetVolume(_ context.Context, request *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	klog.V(4).InfoS("ControllerGetVolume: called", "args", protosanitizer.StripSecrets(request))

	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerModifyVolume modify a volume
func (d *ControllerService) ControllerModifyVolume(ctx context.Context, request *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	klog.V(4).InfoS("ControllerModifyVolume: called", "args", protosanitizer.StripSecrets(request))

	volumeID := request.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeID must be provided")
	}

	params, err := ExtractModifyVolumeParameters(request.GetMutableParameters())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	vol, err := volume.NewVolumeFromVolumeID(volumeID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cl, err := d.pxpool.Get(vol.Cluster())
	if err != nil {
		klog.ErrorS(err, "ControllerModifyVolume: failed to get proxmox cluster", "cluster", vol.Cluster())

		return nil, status.Error(codes.Internal, err.Error())
	}

	_, err = d.checkVolume(ctx, vol)
	if err != nil {
		klog.ErrorS(err, "ControllerModifyVolume: failed to check volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

		return nil, err
	}

	id, _, err := getVMByAttachedVolume(ctx, cl, vol)
	if err != nil || id == 0 {
		if errors.Is(err, errVirtualMachineNotFound) {
			klog.V(3).InfoS("ControllerModifyVolume: volume is not published, cannot modify unpublished volumeID", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

			return nil, status.Error(codes.NotFound, "volume is not published")
		}

		klog.ErrorS(err, "ControllerModifyVolume: failed to get vm by attached volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID())

		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.V(5).InfoS("ControllerModifyVolume: update volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "vmID", id, "parameters", params.ToCFG())

	mc := metrics.NewMetricContext("updateVolume")
	if err = updateVolume(ctx, cl, id, vol, params.ToCFG()); mc.ObserveRequest(err) != nil {
		klog.ErrorS(err, "ControllerModifyVolume: failed to update volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "vmID", id)

		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.V(3).InfoS("ControllerModifyVolume: volume modified", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "vmID", id)

	return &csi.ControllerModifyVolumeResponse{}, nil
}

func (d *ControllerService) getVMIDbyNode(ctx context.Context, nodeName string) (int, string, error) { // nolint:unparam
	node, err := d.kclient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return 0, "", err
	}

	id, err := ProxmoxVMIDbyNode(node)
	if err != nil {
		if d.Provider == csiconfig.ProviderCapmox {
			id, region, err := findVMByUUID(ctx, d.pxpool, node.Status.NodeInfo.SystemUUID)
			if err != nil {
				return 0, "", status.Error(codes.Internal, err.Error())
			}

			return id, region, nil
		}

		klog.InfoS("failed to get proxmox VMID from ProviderID", "nodeID", nodeName, "providerID", node.Spec.ProviderID)

		id, region, err := findVMByNode(ctx, d.pxpool, node)
		if err != nil {
			klog.ErrorS(err, "failed to get vm ref by nodeID", "nodeID", nodeName)

			return 0, "", status.Error(codes.Internal, err.Error())
		}

		return id, region, nil
	}

	return id, "", nil
}

func (d *ControllerService) checkVolume(ctx context.Context, vol *volume.Volume) (int64, error) {
	cl, err := d.pxpool.Get(vol.Cluster())
	if err != nil {
		return 0, status.Error(codes.Internal, err.Error())
	}

	if vol.Zone() != "" {
		nodes, err := cl.Cluster().Resources().List(ctx, cluster.ListFilter{Type: cluster.ResourceTypeNode})
		if err != nil {
			return 0, status.Error(codes.Internal, err.Error())
		}

		if !slices.ContainsFunc(nodes, func(rs cluster.Resource) bool { return rs.Node == vol.Zone() }) {
			return 0, status.Errorf(codes.NotFound, "zone %s not found in cluster %s", vol.Zone(), vol.Cluster())
		}
	}

	// Check shared storage volumes across all nodes in the cluster
	if vol.Node() == "" {
		probeVol, err := volume.NewVolumeFromVolumeID(vol.VolumeID())
		if err != nil {
			return 0, status.Error(codes.Internal, err.Error())
		}

		nodes, err := storageNodes(ctx, cl, probeVol.Storage())
		if err != nil {
			return 0, status.Error(codes.Internal, err.Error())
		}

		for _, n := range nodes {
			probeVol.SetNode(n)

			size, err := getVolumeSize(ctx, cl, probeVol)
			if err != nil {
				if err.Error() == ErrorNotFound {
					continue
				}

				return 0, status.Error(codes.Internal, err.Error())
			}

			klog.V(5).InfoS("checkVolume: determined node for volume", "cluster", vol.Cluster(), "volumeID", vol.VolumeID(), "node", n)

			return size, nil
		}

		return 0, status.Errorf(codes.NotFound, "volume %s not found in any node for storage %s", vol.VolumeID(), vol.Storage())
	}

	size, err := getVolumeSize(ctx, cl, vol)
	if err != nil {
		if err.Error() == ErrorNotFound {
			return 0, status.Errorf(codes.NotFound, "volume %s not found", vol.VolumeID())
		}

		return 0, status.Error(codes.Internal, err.Error())
	}

	return size, nil
}

// expandDiskName builds the full disk name from a diskName template.
// It returns gRPC status errors.
func (d *ControllerService) expandDiskName(ctx context.Context, tpl *diskNameTemplate, parameters map[string]string, region, zone string) (string, error) {
	values := diskNameValues{
		PVCNamespace:   parameters[PVCNamespaceKey],
		PVCName:        parameters[PVCNameKey],
		PVName:         parameters[PVNameKey],
		K8sClusterName: d.k8sClusterName,
		Region:         region,
		Zone:           zone,
	}

	if tpl.usesAnnotations() {
		if values.PVCNamespace == "" || values.PVCName == "" {
			return "", status.Errorf(codes.FailedPrecondition,
				"diskName %q uses PVC annotations, but the request has no %q/%q parameters; start csi-provisioner with --extra-create-metadata",
				tpl.raw, PVCNamespaceKey, PVCNameKey)
		}

		if d.kclient == nil {
			return "", status.Error(codes.Internal, "kubernetes client is not configured, can't read PVC annotations")
		}

		pvc, err := d.kclient.CoreV1().PersistentVolumeClaims(values.PVCNamespace).Get(ctx, values.PVCName, metav1.GetOptions{})
		if err != nil {
			return "", status.Errorf(codes.Internal, "failed to get PVC %s/%s: %v", values.PVCNamespace, values.PVCName, err)
		}

		values.Annotations = pvc.Annotations
	}

	suffix, err := tpl.expand(values)
	if err != nil {
		return "", status.Error(codes.FailedPrecondition, err.Error())
	}

	name, err := templatedDiskName(d.vmID, suffix)
	if err != nil {
		return "", status.Error(codes.InvalidArgument, err.Error())
	}

	return name, nil
}

// checkDiskNotInUse fails if another PersistentVolume that is still in use
// (Bound, Available or Pending) refers to the same volume handle.
// Released and Failed PVs are ignored: reusing their disk is the purpose of diskName.
func (d *ControllerService) checkDiskNotInUse(ctx context.Context, volumeIDs ...string) error {
	if d.kclient == nil {
		return status.Error(codes.Internal, "kubernetes client is not configured, can't check PersistentVolumes")
	}

	pvs, err := d.kclient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return status.Errorf(codes.Internal, "failed to list PersistentVolumes: %v", err)
	}

	for _, pv := range pvs.Items {
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != DriverName || !slices.Contains(volumeIDs, pv.Spec.CSI.VolumeHandle) {
			continue
		}

		if pv.Status.Phase == corev1.VolumeReleased || pv.Status.Phase == corev1.VolumeFailed {
			continue
		}

		claim := ""
		if ref := pv.Spec.ClaimRef; ref != nil {
			claim = ref.Namespace + "/" + ref.Name
		}

		return status.Errorf(codes.FailedPrecondition,
			"disk %s is still used by PersistentVolume %s (phase %s, claim %q); a disk is only reused when its old PersistentVolume is Released",
			pv.Spec.CSI.VolumeHandle, pv.Name, pv.Status.Phase, claim)
	}

	return nil
}
