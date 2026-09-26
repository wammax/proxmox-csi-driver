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
	"fmt"
	"testing"

	proto "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"

	"github.com/sergelogvinov/go-proxmox-rest/nodes/qemu"

	corev1 "k8s.io/api/core/v1"
)

func TestIsVolumeAttached(t *testing.T) {
	t.Parallel()

	tests := []struct {
		msg           string
		vmConfig      *qemu.Config
		pvc           string
		expectedLun   int
		expectedExist bool
	}{
		{
			msg:           "Empty VM config",
			vmConfig:      &qemu.Config{},
			pvc:           "",
			expectedLun:   0,
			expectedExist: false,
		},
		{
			msg: "Empty PVC",
			vmConfig: &qemu.Config{
				SCSI: map[int]qemu.Drive{
					0: {File: "local-lvm:vm-100-disk-0", Size: "8G"},
					5: {File: "local-lvm:vm-100-pvc-123", Size: "8G"},
				},
			},
			pvc:           "",
			expectedLun:   0,
			expectedExist: false,
		},
		{
			msg: "LUN 5",
			vmConfig: &qemu.Config{
				SCSI: map[int]qemu.Drive{
					0: {File: "local-lvm:vm-100-disk-0", Size: "8G"},
					5: {File: "local-lvm:vm-9999-pvc-123", Size: "8G"},
				},
			},
			pvc:           "local-lvm:vm-9999-pvc-123",
			expectedLun:   5,
			expectedExist: true,
		},
		{
			msg: "File based",
			vmConfig: &qemu.Config{
				SCSI: map[int]qemu.Drive{
					0: {File: "local-lvm:vm-100-disk-0", Size: "8G"},
					2: {File: "local:9999/vm-9999-pvc-123.qcow2", Size: "8G"},
				},
			},
			pvc:           "local:9999/vm-9999-pvc-123.qcow2",
			expectedLun:   2,
			expectedExist: true,
		},
		{
			msg: "Name is a prefix of another name",
			vmConfig: &qemu.Config{
				SCSI: map[int]qemu.Drive{
					0: {File: "local-lvm:vm-100-disk-0", Size: "8G"},
					3: {File: "local-lvm:vm-9999-ns.data2", Size: "8G"},
				},
			},
			pvc:           "local-lvm:vm-9999-ns.data",
			expectedLun:   0,
			expectedExist: false,
		},
		{
			msg: "Same disk name on another storage",
			vmConfig: &qemu.Config{
				SCSI: map[int]qemu.Drive{
					0: {File: "local-lvm:vm-100-disk-0", Size: "8G"},
					3: {File: "local-zfs:vm-9999-ns.data", Size: "8G"},
				},
			},
			pvc:           "local-lvm:vm-9999-ns.data",
			expectedLun:   0,
			expectedExist: false,
		},
		{
			msg: "Templated name",
			vmConfig: &qemu.Config{
				SCSI: map[int]qemu.Drive{
					0: {File: "local-lvm:vm-100-disk-0", Size: "8G"},
					3: {File: "local-lvm:vm-9999-ns.data2", Size: "8G"},
					4: {File: "local-lvm:vm-9999-ns.data", Size: "8G"},
				},
			},
			pvc:           "local-lvm:vm-9999-ns.data",
			expectedLun:   4,
			expectedExist: true,
		},
	}

	for _, testCase := range tests {
		t.Run(fmt.Sprint(testCase.msg), func(t *testing.T) {
			t.Parallel()

			lun, exist := isVolumeAttached(testCase.vmConfig, testCase.pvc)

			if testCase.expectedExist {
				assert.True(t, exist)
				assert.Equal(t, testCase.expectedLun, lun)
			} else {
				assert.False(t, exist)
				assert.Equal(t, 0, lun)
			}
		})
	}
}

func TestDriveOptions(t *testing.T) {
	t.Parallel()

	drive := driveOptions(qemu.Drive{Size: "8G", File: "local-lvm:vm-100-disk-0"}, map[string]string{
		"backup":   "0",
		"iothread": "1",
		"iops_rd":  "100",
	})

	assert.Equal(t, "8G", drive.Size)
	assert.Equal(t, "local-lvm:vm-100-disk-0", drive.File)
	assert.Equal(t, new(false), drive.Backup)
	assert.Equal(t, new(true), drive.IOThread)
	assert.Equal(t, new(100), drive.IOPSRD)
}

func TestTopologyAllowsZone(t *testing.T) {
	t.Parallel()

	segments := func(region, zone string) *proto.Topology {
		s := map[string]string{corev1.LabelTopologyRegion: region}
		if zone != "" {
			s[corev1.LabelTopologyZone] = zone
		}

		return &proto.Topology{Segments: s}
	}

	tests := []struct {
		msg      string
		tr       *proto.TopologyRequirement
		expected bool
	}{
		{msg: "No requirement", tr: nil, expected: true},
		{msg: "Preferred only", tr: &proto.TopologyRequirement{Preferred: []*proto.Topology{segments("r1", "pve-1")}}, expected: true},
		{msg: "Requisite allows zone", tr: &proto.TopologyRequirement{Requisite: []*proto.Topology{segments("r1", "pve-1"), segments("r1", "pve-2")}}, expected: true},
		{msg: "Requisite region only", tr: &proto.TopologyRequirement{Requisite: []*proto.Topology{segments("r1", "")}}, expected: true},
		{msg: "Requisite other zone", tr: &proto.TopologyRequirement{Requisite: []*proto.Topology{segments("r1", "pve-1")}}, expected: false},
		{msg: "Requisite other region", tr: &proto.TopologyRequirement{Requisite: []*proto.Topology{segments("r2", "pve-2")}}, expected: false},
	}

	for _, testCase := range tests {
		t.Run(testCase.msg, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, testCase.expected, topologyAllowsZone(testCase.tr, "r1", "pve-2"))
		})
	}
}
