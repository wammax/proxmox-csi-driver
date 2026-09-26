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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDiskNameTemplate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		msg           string
		template      string
		expectedError string
	}{
		{msg: "Empty", template: "", expectedError: "must not be empty"},
		{msg: "LiteralOnly", template: "shared-disk_1.a"},
		{msg: "AllVariables", template: "${pvc.metadata.namespace}.${pvc.metadata.name}.${pv.metadata.name}.${k8sClusterName}.${region}.${zone}"},
		{msg: "AnnotationWithDotsAndSlash", template: "${pvc.metadata.namespace}.${pvc.metadata.annotations.volume.kubernetes.io/selected-node}"},
		{msg: "UnknownVariable", template: "${pvc.metadata.namespace}.${host}", expectedError: "unknown variable ${host}"},
		{msg: "EmptyAnnotationKey", template: "${pvc.metadata.annotations.}", expectedError: "unknown variable"},
		{msg: "Unterminated", template: "${pvc.metadata.namespace", expectedError: `unterminated "${"`},
		{msg: "InvalidLiteral", template: "${pvc.metadata.namespace}/${pvc.metadata.name}", expectedError: `invalid character '/'`},
		{msg: "InvalidLiteralSpace", template: "my disk", expectedError: `invalid character ' '`},
		{msg: "InvalidLiteralComma", template: "a,b", expectedError: `invalid character ','`},
	}

	for _, testCase := range tests {
		t.Run(testCase.msg, func(t *testing.T) {
			t.Parallel()

			tpl, err := parseDiskNameTemplate(testCase.template)
			if testCase.expectedError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), testCase.expectedError)

				return
			}

			require.NoError(t, err)
			assert.NotNil(t, tpl)
		})
	}
}

func TestDiskNameNamespaceIsolation(t *testing.T) {
	t.Parallel()

	values := diskNameValues{
		PVCNamespace:   "myns",
		PVCName:        "data",
		PVName:         "pvc-123",
		K8sClusterName: "prod-k8s",
		Region:         "Region-1",
		Zone:           "pve-1",
	}

	tests := []struct {
		template      string
		expected      string
		expectedError string
	}{
		// The example table from the documentation.
		{template: "${pvc.metadata.namespace}.${pvc.metadata.name}", expected: "myns.data"},
		{template: "${pvc.metadata.namespace}_${pvc.metadata.name}-${zone}", expected: "myns_data-pve-1"},
		{template: "${pvc.metadata.namespace}.${pvc.metadata.name}.${k8sClusterName}", expected: "myns.data.prod-k8s"},
		{template: "${k8sClusterName}.${pvc.metadata.namespace}.${pvc.metadata.name}", expected: "prod-k8s.myns.data"},
		{template: "k8s_${k8sClusterName}_${pvc.metadata.namespace}.${pvc.metadata.name}", expected: "k8s_prod-k8s_myns.data"},
		{template: "${pvc.metadata.namespace}-${pvc.metadata.name}", expectedError: `"-" after ${pvc.metadata.namespace} is not allowed`},
		{template: "${k8sClusterName}-${pvc.metadata.namespace}.${pvc.metadata.name}", expectedError: `${k8sClusterName} before ${pvc.metadata.namespace} must be directly followed by "." or "_"`},
		{template: "${pvc.metadata.name}.${pvc.metadata.namespace}", expectedError: "${pvc.metadata.name} is not allowed before ${pvc.metadata.namespace}"},
		{template: "${region}.${pvc.metadata.namespace}.${pvc.metadata.name}", expectedError: "${region} is not allowed before ${pvc.metadata.namespace}"},

		// More cases.
		{template: "${zone}_${pvc.metadata.namespace}.${pvc.metadata.name}", expected: "pve-1_myns.data"},
		{template: "k8s-${pvc.metadata.namespace}.${pvc.metadata.name}", expected: "k8s-myns.data"},
		{template: "${pvc.metadata.namespace}.${pvc.metadata.name}.${region}", expected: "myns.data.Region-1"},
		{template: "${pvc.metadata.namespace}._${pv.metadata.name}", expected: "myns._pvc-123"},
		{template: "${pvc.metadata.name}", expectedError: "must contain ${pvc.metadata.namespace}"},
		{template: "fixed", expectedError: "must contain ${pvc.metadata.namespace}"},
		{template: "${pvc.metadata.namespace}", expectedError: `${pvc.metadata.namespace} must be directly followed by "." or "_"`},
		{template: "${pvc.metadata.namespace}${pvc.metadata.name}", expectedError: `${pvc.metadata.namespace} must be directly followed by "." or "_"`},
		{template: "${zone}${pvc.metadata.namespace}.${pvc.metadata.name}", expectedError: "${zone} before ${pvc.metadata.namespace} must be directly followed"},
		{template: "${pv.metadata.name}.${pvc.metadata.namespace}.x", expectedError: "${pv.metadata.name} is not allowed before"},
		{template: "${pvc.metadata.annotations.a}.${pvc.metadata.namespace}.x", expectedError: "${pvc.metadata.annotations.a} is not allowed before"},
	}

	for _, testCase := range tests {
		t.Run(testCase.template, func(t *testing.T) {
			t.Parallel()

			tpl, err := parseDiskNameTemplate(testCase.template)
			require.NoError(t, err)

			err = tpl.checkNamespaceIsolation()
			if testCase.expectedError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), testCase.expectedError)
				assert.Contains(t, err.Error(), `diskNameEnforceNamespace: "false"`)

				return
			}

			require.NoError(t, err)

			name, err := tpl.expand(values)
			require.NoError(t, err)
			assert.Equal(t, testCase.expected, name)
		})
	}
}

// With the namespace followed by "." or "_", namespaces that differ only in
// where a "-" is placed can't produce the same name, and with "-" they would.
func TestDiskNameNamespaceCollision(t *testing.T) {
	t.Parallel()

	safe, err := parseDiskNameTemplate("${pvc.metadata.namespace}.${pvc.metadata.name}")
	require.NoError(t, err)
	require.NoError(t, safe.checkNamespaceIsolation())

	a, err := safe.expand(diskNameValues{PVCNamespace: "team-a", PVCName: "db"})
	require.NoError(t, err)

	b, err := safe.expand(diskNameValues{PVCNamespace: "team", PVCName: "a-db"})
	require.NoError(t, err)

	assert.NotEqual(t, a, b)

	unsafe, err := parseDiskNameTemplate("${pvc.metadata.namespace}-${pvc.metadata.name}")
	require.NoError(t, err)
	require.Error(t, unsafe.checkNamespaceIsolation())

	a, err = unsafe.expand(diskNameValues{PVCNamespace: "team-a", PVCName: "db"})
	require.NoError(t, err)

	b, err = unsafe.expand(diskNameValues{PVCNamespace: "team", PVCName: "a-db"})
	require.NoError(t, err)

	assert.Equal(t, a, b, "this is the collision the namespace check prevents")
}

func TestDiskNameExpand(t *testing.T) {
	t.Parallel()

	values := diskNameValues{
		PVCNamespace:   "myns",
		PVCName:        "data",
		PVName:         "pvc-123",
		K8sClusterName: "prod-k8s",
		Region:         "Region-1",
		Zone:           "Pve-3",
		Annotations: map[string]string{
			"volume.kubernetes.io/selected-node": "k8s-a",
			"example.com/empty":                  "",
			"example.com/bad":                    "a/b",
			"example.com/upper":                  "Host_1",
		},
	}

	tests := []struct {
		msg           string
		template      string
		values        *diskNameValues
		expected      string
		expectedError string
	}{
		{msg: "AllVariables", template: "${pvc.metadata.namespace}.${pvc.metadata.name}.${pv.metadata.name}.${k8sClusterName}.${region}.${zone}", expected: "myns.data.pvc-123.prod-k8s.Region-1.Pve-3"},
		{msg: "Annotation", template: "${pvc.metadata.namespace}.${pvc.metadata.annotations.volume.kubernetes.io/selected-node}", expected: "myns.k8s-a"},
		{msg: "AnnotationUppercase", template: "${pvc.metadata.namespace}.${pvc.metadata.annotations.example.com/upper}", expected: "myns.Host_1"},
		{msg: "AnnotationMissing", template: "${pvc.metadata.namespace}.${pvc.metadata.annotations.example.com/missing}", expectedError: `PVC myns/data has no annotation "example.com/missing"`},
		{msg: "AnnotationEmpty", template: "${pvc.metadata.namespace}.${pvc.metadata.annotations.example.com/empty}", expectedError: "${pvc.metadata.annotations.example.com/empty} is empty"},
		{msg: "AnnotationInvalidChar", template: "${pvc.metadata.namespace}.${pvc.metadata.annotations.example.com/bad}", expectedError: `expands to "a/b", which contains the invalid character '/'`},
		{
			msg:           "MissingNamespace",
			template:      "${pvc.metadata.namespace}.x",
			values:        &diskNameValues{},
			expectedError: `no "csi.storage.k8s.io/pvc/namespace" parameter; start csi-provisioner with --extra-create-metadata`,
		},
		{msg: "MissingPVCName", template: "x.${pvc.metadata.name}", values: &diskNameValues{}, expectedError: `no "csi.storage.k8s.io/pvc/name" parameter`},
		{msg: "MissingPVName", template: "x.${pv.metadata.name}", values: &diskNameValues{}, expectedError: `no "csi.storage.k8s.io/pv/name" parameter`},
		{msg: "MissingK8sClusterName", template: "x.${k8sClusterName}", values: &diskNameValues{}, expectedError: "features.k8sClusterName is not set in the driver config"},
		{msg: "EmptyZone", template: "x.${zone}", values: &diskNameValues{}, expectedError: "${zone} is empty"},
	}

	for _, testCase := range tests {
		t.Run(testCase.msg, func(t *testing.T) {
			t.Parallel()

			tpl, err := parseDiskNameTemplate(testCase.template)
			require.NoError(t, err)

			v := values
			if testCase.values != nil {
				v = *testCase.values
			}

			name, err := tpl.expand(v)
			if testCase.expectedError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), testCase.expectedError)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, testCase.expected, name)
		})
	}
}

func TestTemplatedDiskName(t *testing.T) {
	t.Parallel()

	name, err := templatedDiskName(9999, "myns.data")
	require.NoError(t, err)
	assert.Equal(t, "vm-9999-myns.data", name)

	// "vm-9999-" is 8 characters.
	name, err = templatedDiskName(9999, strings.Repeat("a", MaxDiskNameLength-8))
	require.NoError(t, err)
	assert.Len(t, name, MaxDiskNameLength)

	_, err = templatedDiskName(9999, strings.Repeat("a", MaxDiskNameLength-7))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is 121 characters long, the maximum is 120")
}

func TestCheckLVMDeviceName(t *testing.T) {
	t.Parallel()

	// Typical names are far below the limit: "pve-vm--9999--myns.data.prod--k8s".
	assert.Equal(t, 33, lvmDeviceNameLength("pve", "vm-9999-myns.data.prod-k8s"))
	require.NoError(t, checkLVMDeviceName("pve", "vm-9999-myns.data.prod-k8s"))

	// A 120 character name without extra hyphens fits: 4 + 120 + 2 = 126.
	plain := "vm-9999-" + strings.Repeat("a", 112)
	require.NoError(t, checkLVMDeviceName("pve", plain))

	// The name that failed on a real Proxmox host: 120 characters, 56 hyphens.
	hyphens := "vm-9999-dnv" + strings.Repeat("-a", 54) + "x"
	require.Len(t, hyphens, 120)

	err := checkLVMDeviceName("pve", hyphens)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "would be 180 characters (hyphens count double), the maximum is 127")

	// Hyphens in the volume group name count double too.
	assert.Equal(t, len("my--vg")+1+len("vm--9999--a"), lvmDeviceNameLength("my-vg", "vm-9999-a"))
}
