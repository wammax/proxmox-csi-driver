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

package csi_test

import (
	"maps"
	"strings"

	proto "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sergelogvinov/go-proxmox-rest/fakeapi"
	"github.com/sergelogvinov/go-proxmox-rest/nodes/qemu"
	"github.com/sergelogvinov/go-proxmox-rest/nodes/storage"
	"github.com/sergelogvinov/proxmox-csi-plugin/pkg/csi"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	dnStorage  = "dn-lvm"
	dnTemplate = "${pvc.metadata.namespace}.${pvc.metadata.name}"
)

// setupDiskNameFixtures seeds the local storage dn-lvm on pve-1 and pve-2 with
// disks that use templated names, VMs that have some of them attached, and
// PersistentVolumes/PVCs in the fake Kubernetes client.
func (ts *configuredTestSuite) setupDiskNameFixtures() {
	disk := func(name string, size int64) fakeapi.StorageOption {
		return fakeapi.WithVolume(storage.Volume{VolID: dnStorage + ":" + name, Format: "raw", Size: size, VMID: 9999})
	}

	ts.fake.Node("pve-1").AddStorage(dnStorage, "lvm",
		fakeapi.WithCapacity(100<<30, 50<<30, 50<<30),
		disk("vm-9999-ns1.reused", 5<<30),
		disk("vm-9999-ns1.small", 1<<30),
		disk("vm-9999-ns1.inuse", 1<<30),
		disk("vm-9999-ns1.released", 1<<30),
		disk("vm-9999-ns1.elsewhere", 1<<30),
		disk("vm-9999-ns1.here", 1<<30),
		disk("vm-9999-ns1.free", 1<<30),
	)

	ts.fake.Node("pve-2").AddStorage(dnStorage, "lvm",
		fakeapi.WithCapacity(100<<30, 50<<30, 50<<30),
		disk("vm-9999-ns1.onnode2", 1<<30),
	)

	// VM 100 is the Kubernetes node cluster-1-node-1, VM 150 is not a Kubernetes node.
	ts.fake.Node("pve-1").AddVM(100, &qemu.Config{
		Name: "cluster-1-node-1",
		SCSI: map[int]qemu.Drive{
			0: {File: "local-lvm:vm-100-disk-0", Size: "10G"},
			1: {File: "local-lvm:vm-9999-pvc-123", Backup: new(false), IOThread: new(true), WWN: "0x5056432d49443031"},
			2: {File: dnStorage + ":vm-9999-ns1.here", Backup: new(false), IOThread: new(true), WWN: "0x5056432d49443032"},
		},
		SMBios1: &qemu.SMBios1{UUID: "11833f4c-341f-4bd3-aad7-f7abed000000"},
	}, fakeapi.WithStatus(qemu.VMStatusRunning))

	ts.fake.Node("pve-1").AddVM(150, &qemu.Config{
		Name: "not-a-kubernetes-node",
		SCSI: map[int]qemu.Drive{
			0: {File: "local-lvm:vm-150-disk-0", Size: "10G"},
			1: {File: dnStorage + ":vm-9999-ns1.elsewhere"},
		},
	}, fakeapi.WithStatus(qemu.VMStatusRunning))

	pv := func(name, handle string, phase corev1.PersistentVolumePhase) {
		_, err := ts.kclient.CoreV1().PersistentVolumes().Create(ts.T().Context(), &corev1.PersistentVolume{
			Name: name,
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: csi.DriverName, VolumeHandle: handle},
				},
				ClaimRef: &corev1.ObjectReference{Namespace: "ns1", Name: strings.TrimPrefix(name, "pv-")},
			},
			Status: corev1.PersistentVolumeStatus{Phase: phase},
		}, metav1.CreateOptions{})
		ts.Require().NoError(err)
	}

	pv("pv-inuse", "cluster-1/pve-1/dn-lvm/vm-9999-ns1.inuse", corev1.VolumeBound)
	pv("pv-released", "cluster-1/pve-1/dn-lvm/vm-9999-ns1.released", corev1.VolumeReleased)

	pvc := func(name string, annotations map[string]string) {
		_, err := ts.kclient.CoreV1().PersistentVolumeClaims("ns1").Create(ts.T().Context(), &corev1.PersistentVolumeClaim{
			Name:        name,
			Namespace:   "ns1",
			Annotations: annotations,
		}, metav1.CreateOptions{})
		ts.Require().NoError(err)
	}

	pvc("annot", map[string]string{"example.com/host": "h1"})
	pvc("noannot", nil)
}

//nolint:maintidx
func (ts *configuredTestSuite) TestCreateVolumeDiskName() {
	ts.setupDiskNameFixtures()

	volcap := &proto.VolumeCapability{
		AccessMode: &proto.VolumeCapability_AccessMode{
			Mode: proto.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
		AccessType: &proto.VolumeCapability_Mount{
			Mount: &proto.VolumeCapability_MountVolume{FsType: "ext4"},
		},
	}

	pve1 := &proto.TopologyRequirement{
		Preferred: []*proto.Topology{
			{Segments: map[string]string{corev1.LabelTopologyRegion: "cluster-1", corev1.LabelTopologyZone: "pve-1"}},
		},
	}

	// WaitForFirstConsumer: the scheduler already picked pve-1.
	pve1Only := &proto.TopologyRequirement{
		Requisite: pve1.GetPreferred(),
		Preferred: pve1.GetPreferred(),
	}

	topology := func(zone string) []*proto.Topology {
		return []*proto.Topology{
			{Segments: map[string]string{corev1.LabelTopologyRegion: "cluster-1", corev1.LabelTopologyZone: zone}},
		}
	}

	params := func(pvcName string, extra map[string]string) map[string]string {
		p := map[string]string{
			csi.StorageIDKey:       dnStorage,
			csi.StorageDiskNameKey: dnTemplate,
			csi.PVCNamespaceKey:    "ns1",
			csi.PVCNameKey:         pvcName,
			csi.PVNameKey:          "pvc-uuid-" + pvcName,
		}
		maps.Copy(p, extra)

		for k, v := range p {
			if v == "" {
				delete(p, k)
			}
		}

		return p
	}

	volCtx := func(extra map[string]string) map[string]string {
		c := map[string]string{
			"backup":    "0",
			"iothread":  "1",
			"replicate": "0",
			"storage":   dnStorage,
			"diskName":  dnTemplate,
		}
		maps.Copy(c, extra)

		return c
	}

	small := &proto.CapacityRange{RequiredBytes: 1}

	tests := []struct {
		msg           string
		name          string
		params        map[string]string
		capacity      *proto.CapacityRange
		topology      *proto.TopologyRequirement
		expected      *proto.Volume
		expectedCode  codes.Code
		expectedError string
	}{
		{
			msg:    "NewDisk",
			name:   "new",
			params: params("new", nil),
			expected: &proto.Volume{
				VolumeId:           "cluster-1/pve-1/dn-lvm/vm-9999-ns1.new",
				VolumeContext:      volCtx(nil),
				CapacityBytes:      csi.MinChunkSizeBytes,
				AccessibleTopology: topology("pve-1"),
			},
		},
		{
			msg:    "ReusedLargerDiskReportsActualSize",
			name:   "reused",
			params: params("reused", nil),
			expected: &proto.Volume{
				VolumeId:           "cluster-1/pve-1/dn-lvm/vm-9999-ns1.reused",
				VolumeContext:      volCtx(nil),
				CapacityBytes:      5 << 30,
				AccessibleTopology: topology("pve-1"),
			},
		},
		{
			msg:           "ReusedDiskLargerThanLimit",
			name:          "reused",
			params:        params("reused", nil),
			capacity:      &proto.CapacityRange{RequiredBytes: 1, LimitBytes: 2 << 30},
			expectedCode:  codes.OutOfRange,
			expectedError: "existing disk cluster-1/pve-1/dn-lvm/vm-9999-ns1.reused is 5368709120 bytes, larger than the limit of 2147483648 bytes",
		},
		{
			msg:           "ReusedSmallerDiskIsRejected",
			name:          "small",
			params:        params("small", nil),
			capacity:      &proto.CapacityRange{RequiredBytes: 2 << 30},
			expectedCode:  codes.OutOfRange,
			expectedError: "existing disk cluster-1/pve-1/dn-lvm/vm-9999-ns1.small is 1Gi, but the PVC requests 2Gi; request at most 1Gi and expand the PVC afterwards",
		},
		{
			msg:      "ReusedDiskSameSize",
			name:     "small",
			params:   params("small", nil),
			capacity: &proto.CapacityRange{RequiredBytes: 1 << 30},
			expected: &proto.Volume{
				VolumeId:           "cluster-1/pve-1/dn-lvm/vm-9999-ns1.small",
				VolumeContext:      volCtx(nil),
				CapacityBytes:      1 << 30,
				AccessibleTopology: topology("pve-1"),
			},
		},
		{
			msg:           "DiskUsedByBoundPV",
			name:          "inuse",
			params:        params("inuse", nil),
			expectedCode:  codes.FailedPrecondition,
			expectedError: `disk cluster-1/pve-1/dn-lvm/vm-9999-ns1.inuse is still used by PersistentVolume pv-inuse (phase Bound, claim "ns1/inuse")`,
		},
		{
			msg:    "DiskOfReleasedPVIsReused",
			name:   "released",
			params: params("released", nil),
			expected: &proto.Volume{
				VolumeId:           "cluster-1/pve-1/dn-lvm/vm-9999-ns1.released",
				VolumeContext:      volCtx(nil),
				CapacityBytes:      1 << 30,
				AccessibleTopology: topology("pve-1"),
			},
		},
		{
			msg:    "DiskOnOtherNodeTopologyAllows",
			name:   "onnode2",
			params: params("onnode2", nil),
			expected: &proto.Volume{
				VolumeId:           "cluster-1/pve-2/dn-lvm/vm-9999-ns1.onnode2",
				VolumeContext:      volCtx(nil),
				CapacityBytes:      1 << 30,
				AccessibleTopology: topology("pve-2"),
			},
		},
		{
			msg:           "DiskOnOtherNodeTopologyForbids",
			name:          "onnode2",
			params:        params("onnode2", nil),
			topology:      pve1Only,
			expectedCode:  codes.FailedPrecondition,
			expectedError: "disk vm-9999-ns1.onnode2 already exists on Proxmox node pve-2, but the volume has to be created on pve-1",
		},
		{
			msg:    "Annotation",
			name:   "annot",
			params: params("annot", map[string]string{csi.StorageDiskNameKey: dnTemplate + ".${pvc.metadata.annotations.example.com/host}"}),
			expected: &proto.Volume{
				VolumeId:           "cluster-1/pve-1/dn-lvm/vm-9999-ns1.annot.h1",
				VolumeContext:      volCtx(map[string]string{"diskName": dnTemplate + ".${pvc.metadata.annotations.example.com/host}"}),
				CapacityBytes:      csi.MinChunkSizeBytes,
				AccessibleTopology: topology("pve-1"),
			},
		},
		{
			msg:           "AnnotationMissing",
			name:          "noannot",
			params:        params("noannot", map[string]string{csi.StorageDiskNameKey: dnTemplate + ".${pvc.metadata.annotations.example.com/host}"}),
			expectedCode:  codes.FailedPrecondition,
			expectedError: `PVC ns1/noannot has no annotation "example.com/host"`,
		},
		{
			msg:    "K8sClusterName",
			name:   "k",
			params: params("k", map[string]string{csi.StorageDiskNameKey: "${k8sClusterName}." + dnTemplate}),
			expected: &proto.Volume{
				VolumeId:           "cluster-1/pve-1/dn-lvm/vm-9999-test-k8s.ns1.k",
				VolumeContext:      volCtx(map[string]string{"diskName": "${k8sClusterName}." + dnTemplate}),
				CapacityBytes:      csi.MinChunkSizeBytes,
				AccessibleTopology: topology("pve-1"),
			},
		},
		{
			msg:  "NamespaceEnforcementOff",
			name: "shared",
			params: params("shared", map[string]string{
				csi.StorageDiskNameKey:                 "${pvc.metadata.name}",
				csi.StorageDiskNameEnforceNamespaceKey: "false",
			}),
			expected: &proto.Volume{
				VolumeId:           "cluster-1/pve-1/dn-lvm/vm-9999-shared",
				VolumeContext:      volCtx(map[string]string{"diskName": "${pvc.metadata.name}", "diskNameEnforceNamespace": "0"}),
				CapacityBytes:      csi.MinChunkSizeBytes,
				AccessibleTopology: topology("pve-1"),
			},
		},
		{
			msg:           "NamespaceEnforcementOn",
			name:          "x",
			params:        params("x", map[string]string{csi.StorageDiskNameKey: "${pvc.metadata.name}.${pvc.metadata.namespace}"}),
			expectedCode:  codes.InvalidArgument,
			expectedError: "${pvc.metadata.name} is not allowed before ${pvc.metadata.namespace}",
		},
		{
			msg:           "MissingMetadata",
			name:          "x",
			params:        params("x", map[string]string{csi.PVCNamespaceKey: ""}),
			expectedCode:  codes.FailedPrecondition,
			expectedError: "start csi-provisioner with --extra-create-metadata",
		},
		{
			msg:           "Replicate",
			name:          "x",
			params:        params("x", map[string]string{"replicate": "true"}),
			expectedCode:  codes.InvalidArgument,
			expectedError: "parameter diskName can't be combined with replicate",
		},
		{
			msg:           "InvalidTemplate",
			name:          "x",
			params:        params("x", map[string]string{csi.StorageDiskNameKey: "${pvc.metadata.namespace}.${host}"}),
			expectedCode:  codes.InvalidArgument,
			expectedError: "unknown variable ${host}",
		},
		{
			msg:           "DiskNameTooLong",
			name:          strings.Repeat("a", 113),
			params:        params(strings.Repeat("a", 113), map[string]string{csi.StorageDiskNameKey: "${pvc.metadata.name}", csi.StorageDiskNameEnforceNamespaceKey: "false"}),
			expectedCode:  codes.InvalidArgument,
			expectedError: "is 121 characters long, the maximum is 120",
		},
		{
			msg:           "VolumeIDTooLong",
			name:          strings.Repeat("a", 100),
			params:        params(strings.Repeat("a", 100), map[string]string{csi.StorageDiskNameKey: "${pvc.metadata.name}", csi.StorageDiskNameEnforceNamespaceKey: "false"}),
			expectedCode:  codes.InvalidArgument,
			expectedError: "is 131 bytes long, the maximum is 128",
		},
	}

	for _, testCase := range tests {
		ts.Run(testCase.msg, func() {
			capacity := testCase.capacity
			if capacity == nil {
				capacity = small
			}

			topo := testCase.topology
			if topo == nil {
				topo = pve1
			}

			resp, err := ts.s.CreateVolume(ts.T().Context(), &proto.CreateVolumeRequest{
				Name:                      "pvc-uuid-" + testCase.name,
				Parameters:                testCase.params,
				VolumeCapabilities:        []*proto.VolumeCapability{volcap},
				CapacityRange:             capacity,
				AccessibilityRequirements: topo,
			})

			if testCase.expectedError != "" {
				ts.Require().Error(err)
				ts.Require().Equal(testCase.expectedCode, status.Code(err), err.Error())
				ts.Require().Contains(status.Convert(err).Message(), testCase.expectedError)

				return
			}

			ts.Require().NoError(err)
			ts.Require().Equal(testCase.expected, resp.GetVolume())
		})
	}
}

// Without diskName, the new parameters and the extra create metadata change nothing.
func (ts *configuredTestSuite) TestCreateVolumeWithoutDiskNameUnchanged() {
	resp, err := ts.s.CreateVolume(ts.T().Context(), &proto.CreateVolumeRequest{
		Name: "pvc-exist-same-size",
		Parameters: map[string]string{
			csi.StorageIDKey:                       "local-lvm",
			csi.StorageDiskNameEnforceNamespaceKey: "false",
			csi.PVCNamespaceKey:                    "ns1",
			csi.PVCNameKey:                         "data",
			csi.PVNameKey:                          "pvc-exist-same-size",
		},
		VolumeCapabilities: []*proto.VolumeCapability{
			{
				AccessMode: &proto.VolumeCapability_AccessMode{Mode: proto.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
				AccessType: &proto.VolumeCapability_Mount{Mount: &proto.VolumeCapability_MountVolume{FsType: "ext4"}},
			},
		},
		CapacityRange: &proto.CapacityRange{RequiredBytes: 1, LimitBytes: 100 << 30},
		AccessibilityRequirements: &proto.TopologyRequirement{
			Preferred: []*proto.Topology{
				{Segments: map[string]string{corev1.LabelTopologyRegion: "cluster-1", corev1.LabelTopologyZone: "pve-1"}},
			},
		},
	})
	ts.Require().NoError(err)
	ts.Require().Equal(&proto.Volume{
		VolumeId: "cluster-1/pve-1/local-lvm/vm-9999-pvc-exist-same-size",
		VolumeContext: map[string]string{
			"backup":    "0",
			"iothread":  "1",
			"storage":   "local-lvm",
			"replicate": "0",
		},
		CapacityBytes: csi.MinChunkSizeBytes,
		AccessibleTopology: []*proto.Topology{
			{Segments: map[string]string{corev1.LabelTopologyRegion: "cluster-1", corev1.LabelTopologyZone: "pve-1"}},
		},
	}, resp.GetVolume())
}

func (ts *configuredTestSuite) TestControllerPublishVolumeDiskName() {
	ts.setupDiskNameFixtures()

	volCap := &proto.VolumeCapability{
		AccessMode: &proto.VolumeCapability_AccessMode{
			Mode: proto.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
		AccessType: &proto.VolumeCapability_Mount{
			Mount: &proto.VolumeCapability_MountVolume{FsType: "ext4"},
		},
	}

	diskNameCtx := map[string]string{
		csi.StorageIDKey:       dnStorage,
		csi.StorageDiskNameKey: dnTemplate,
	}

	plainCtx := map[string]string{
		csi.StorageIDKey: dnStorage,
	}

	tests := []struct {
		msg           string
		volumeID      string
		volumeContext map[string]string
		expected      map[string]string
		expectedCode  codes.Code
		expectedError string
	}{
		{
			msg:           "AttachedToAnotherVM",
			volumeID:      "cluster-1/pve-1/dn-lvm/vm-9999-ns1.elsewhere",
			volumeContext: diskNameCtx,
			expectedCode:  codes.FailedPrecondition,
			expectedError: "volume cluster-1/pve-1/dn-lvm/vm-9999-ns1.elsewhere is attached to VM 150, detach it there before it can be attached to VM 100",
		},
		{
			msg:           "AlreadyAttachedToTargetVM",
			volumeID:      "cluster-1/pve-1/dn-lvm/vm-9999-ns1.here",
			volumeContext: diskNameCtx,
			expected: map[string]string{
				"DevicePath": "/dev/disk/by-id/wwn-0x5056432d49443032",
				"lun":        "2",
			},
		},
		{
			msg:           "NotAttached",
			volumeID:      "cluster-1/pve-1/dn-lvm/vm-9999-ns1.free",
			volumeContext: diskNameCtx,
			expected: map[string]string{
				"DevicePath": "/dev/disk/by-id/wwn-0x5056432d49443033",
				"lun":        "3",
			},
		},
		{
			// Without diskName the other-VM check doesn't run: unchanged behavior.
			msg:           "WithoutDiskNameNotChecked",
			volumeID:      "cluster-1/pve-1/dn-lvm/vm-9999-ns1.elsewhere",
			volumeContext: plainCtx,
			expected: map[string]string{
				"DevicePath": "/dev/disk/by-id/wwn-0x5056432d49443034",
				"lun":        "4",
			},
		},
	}

	for _, testCase := range tests {
		ts.Run(testCase.msg, func() {
			resp, err := ts.s.ControllerPublishVolume(ts.T().Context(), &proto.ControllerPublishVolumeRequest{
				NodeId:           "cluster-1-node-1",
				VolumeId:         testCase.volumeID,
				VolumeCapability: volCap,
				VolumeContext:    testCase.volumeContext,
			})

			if testCase.expectedError != "" {
				ts.Require().Error(err)
				ts.Require().Equal(testCase.expectedCode, status.Code(err), err.Error())
				ts.Require().Equal(testCase.expectedError, status.Convert(err).Message())

				return
			}

			ts.Require().NoError(err)
			ts.Require().Equal(testCase.expected, resp.GetPublishContext())
		})
	}
}
