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
	"strings"
)

// Disk name templates (the diskName StorageClass parameter), see docs/disk-name.md.

const (
	// PVCNameKey is the PVC name, added to the CreateVolume parameters by
	// csi-provisioner started with --extra-create-metadata.
	PVCNameKey = "csi.storage.k8s.io/pvc/name"
	// PVCNamespaceKey is the PVC namespace (--extra-create-metadata).
	PVCNamespaceKey = "csi.storage.k8s.io/pvc/namespace"
	// PVNameKey is the PV name (--extra-create-metadata).
	PVNameKey = "csi.storage.k8s.io/pv/name"

	// MaxDiskNameLength is the maximum length of a templated disk name, including the vm-<vmid>- prefix.
	MaxDiskNameLength = 120
	// MaxVolumeIDLength is the CSI size limit of a volume ID.
	MaxVolumeIDLength = 128
	// MaxLVMDeviceNameLength is the device-mapper name limit (<vg>-<lv> with every "-" doubled).
	MaxLVMDeviceNameLength = 127

	diskNameVarPVCNamespace     = "pvc.metadata.namespace"
	diskNameVarPVCName          = "pvc.metadata.name"
	diskNameVarPVName           = "pv.metadata.name"
	diskNameVarAnnotationPrefix = "pvc.metadata.annotations."
	diskNameVarK8sClusterName   = "k8sClusterName"
	diskNameVarRegion           = "region"
	diskNameVarZone             = "zone"

	diskNameAllowedChars = "A-Z a-z 0-9 . _ -"
)

// diskNameToken is either literal text or a ${variable}.
type diskNameToken struct {
	literal  string
	variable string
}

// diskNameTemplate is a parsed diskName parameter.
type diskNameTemplate struct {
	raw    string
	tokens []diskNameToken
}

// diskNameValues holds the values the template variables expand to.
type diskNameValues struct {
	PVCNamespace   string
	PVCName        string
	PVName         string
	K8sClusterName string
	Region         string
	Zone           string
	// Annotations of the PVC, only needed if the template uses them.
	Annotations map[string]string
}

// parseDiskNameTemplate splits tpl into literals and variables and rejects
// unknown variables and literal characters outside the allowed set.
func parseDiskNameTemplate(tpl string) (*diskNameTemplate, error) {
	t := &diskNameTemplate{raw: tpl}

	rest := tpl
	for rest != "" {
		start := strings.Index(rest, "${")
		if start < 0 {
			t.tokens = append(t.tokens, diskNameToken{literal: rest})

			break
		}

		if start > 0 {
			t.tokens = append(t.tokens, diskNameToken{literal: rest[:start]})
		}

		end := strings.Index(rest[start:], "}")
		if end < 0 {
			return nil, fmt.Errorf("diskName %q: unterminated \"${\"", tpl)
		}

		name := rest[start+2 : start+end]
		if !isDiskNameVariable(name) {
			return nil, fmt.Errorf("diskName %q: unknown variable ${%s}", tpl, name)
		}

		t.tokens = append(t.tokens, diskNameToken{variable: name})
		rest = rest[start+end+1:]
	}

	if len(t.tokens) == 0 {
		return nil, fmt.Errorf("diskName must not be empty")
	}

	for _, tok := range t.tokens {
		if c, ok := invalidDiskNameChar(tok.literal); ok {
			return nil, fmt.Errorf("diskName %q: invalid character %q, allowed are %s", tpl, c, diskNameAllowedChars)
		}
	}

	return t, nil
}

func isDiskNameVariable(name string) bool {
	switch name {
	case diskNameVarPVCNamespace, diskNameVarPVCName, diskNameVarPVName,
		diskNameVarK8sClusterName, diskNameVarRegion, diskNameVarZone:
		return true
	}

	return strings.HasPrefix(name, diskNameVarAnnotationPrefix) && len(name) > len(diskNameVarAnnotationPrefix)
}

// invalidDiskNameChar returns the first character of s outside [A-Za-z0-9._-].
func invalidDiskNameChar(s string) (rune, bool) {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return c, true
		}
	}

	return 0, false
}

// uses reports whether the template contains the variable name.
func (t *diskNameTemplate) uses(name string) bool {
	for _, tok := range t.tokens {
		if tok.variable == name {
			return true
		}
	}

	return false
}

// usesAnnotations reports whether the template contains a ${pvc.metadata.annotations.*} variable.
func (t *diskNameTemplate) usesAnnotations() bool {
	for _, tok := range t.tokens {
		if strings.HasPrefix(tok.variable, diskNameVarAnnotationPrefix) {
			return true
		}
	}

	return false
}

// followedBySeparator reports whether token i is directly followed by literal text starting with "." or "_".
func (t *diskNameTemplate) followedBySeparator(i int) bool {
	if i+1 >= len(t.tokens) || t.tokens[i+1].variable != "" {
		return false
	}

	return strings.HasPrefix(t.tokens[i+1].literal, ".") || strings.HasPrefix(t.tokens[i+1].literal, "_")
}

// checkNamespaceIsolation makes sure the namespace can always be read back from
// the expanded name, so no PVC can produce the disk name of another namespace.
//
// Namespaces are DNS labels and never contain "." or "_". So the name is safe if
// ${pvc.metadata.namespace} is directly followed by "." or "_" and everything
// before it has a known end: fixed text, or ${k8sClusterName} / ${zone} (which
// can't contain "." or "_" either) directly followed by "." or "_".
func (t *diskNameTemplate) checkNamespaceIsolation() error {
	const hint = `, or set diskNameEnforceNamespace: "false"`

	nsIdx := -1

	for i, tok := range t.tokens {
		if tok.variable == diskNameVarPVCNamespace {
			nsIdx = i

			break
		}
	}

	if nsIdx < 0 {
		return fmt.Errorf("diskName %q must contain ${%s} to keep the disks of different namespaces apart%s",
			t.raw, diskNameVarPVCNamespace, hint)
	}

	for i, tok := range t.tokens[:nsIdx] {
		switch tok.variable {
		case "":
		case diskNameVarK8sClusterName, diskNameVarZone:
			if !t.followedBySeparator(i) {
				return fmt.Errorf(`diskName %q: ${%s} before ${%s} must be directly followed by "." or "_"%s`,
					t.raw, tok.variable, diskNameVarPVCNamespace, hint)
			}
		case diskNameVarRegion:
			return fmt.Errorf(`diskName %q: ${%s} is not allowed before ${%s}, because region names may contain "." or "_"; move it after the namespace%s`,
				t.raw, tok.variable, diskNameVarPVCNamespace, hint)
		default:
			return fmt.Errorf(`diskName %q: ${%s} is not allowed before ${%s}; only fixed text, ${%s} and ${%s} may come first, so that no PVC can imitate another namespace%s`,
				t.raw, tok.variable, diskNameVarPVCNamespace, diskNameVarK8sClusterName, diskNameVarZone, hint)
		}
	}

	if !t.followedBySeparator(nsIdx) {
		if nsIdx+1 < len(t.tokens) && strings.HasPrefix(t.tokens[nsIdx+1].literal, "-") {
			return fmt.Errorf(`diskName %q: "-" after ${%s} is not allowed, because namespaces may contain "-"; use "." or "_"%s`,
				t.raw, diskNameVarPVCNamespace, hint)
		}

		return fmt.Errorf(`diskName %q: ${%s} must be directly followed by "." or "_"%s`,
			t.raw, diskNameVarPVCNamespace, hint)
	}

	return nil
}

// expand returns the template with all variables replaced by their values.
// Missing or empty values and values with invalid characters are errors.
func (t *diskNameTemplate) expand(v diskNameValues) (string, error) {
	var b strings.Builder

	for _, tok := range t.tokens {
		if tok.variable == "" {
			b.WriteString(tok.literal)

			continue
		}

		val, err := v.lookup(tok.variable)
		if err != nil {
			return "", fmt.Errorf("diskName %q: %w", t.raw, err)
		}

		if c, ok := invalidDiskNameChar(val); ok {
			return "", fmt.Errorf("diskName %q: ${%s} expands to %q, which contains the invalid character %q; allowed are %s",
				t.raw, tok.variable, val, c, diskNameAllowedChars)
		}

		b.WriteString(val)
	}

	return b.String(), nil
}

func (v diskNameValues) lookup(name string) (string, error) {
	const metadataHint = "; start csi-provisioner with --extra-create-metadata"

	var val string

	switch name {
	case diskNameVarPVCNamespace:
		if v.PVCNamespace == "" {
			return "", fmt.Errorf("${%s} is used, but the request has no %q parameter%s", name, PVCNamespaceKey, metadataHint)
		}

		val = v.PVCNamespace
	case diskNameVarPVCName:
		if v.PVCName == "" {
			return "", fmt.Errorf("${%s} is used, but the request has no %q parameter%s", name, PVCNameKey, metadataHint)
		}

		val = v.PVCName
	case diskNameVarPVName:
		if v.PVName == "" {
			return "", fmt.Errorf("${%s} is used, but the request has no %q parameter%s", name, PVNameKey, metadataHint)
		}

		val = v.PVName
	case diskNameVarK8sClusterName:
		if v.K8sClusterName == "" {
			return "", fmt.Errorf("${%s} is used, but features.k8sClusterName is not set in the driver config", name)
		}

		val = v.K8sClusterName
	case diskNameVarRegion:
		val = v.Region
	case diskNameVarZone:
		val = v.Zone
	default:
		key := strings.TrimPrefix(name, diskNameVarAnnotationPrefix)

		a, ok := v.Annotations[key]
		if !ok {
			return "", fmt.Errorf("${%s} is used, but PVC %s/%s has no annotation %q", name, v.PVCNamespace, v.PVCName, key)
		}

		val = a
	}

	if val == "" {
		return "", fmt.Errorf("${%s} is empty", name)
	}

	return val, nil
}

// templatedDiskName returns the full Proxmox disk name vm-<vmid>-<suffix> and checks its length.
func templatedDiskName(vmID int, suffix string) (string, error) {
	name := fmt.Sprintf("vm-%d-%s", vmID, suffix)
	if len(name) > MaxDiskNameLength {
		return "", fmt.Errorf("disk name %q is %d characters long, the maximum is %d", name, len(name), MaxDiskNameLength)
	}

	return name, nil
}

// lvmDeviceNameLength returns the length of the device-mapper name of an LVM
// volume: "<vg>-<lv>", where device-mapper doubles every "-" inside the names.
func lvmDeviceNameLength(vg, lv string) int {
	return len(vg) + strings.Count(vg, "-") + 1 + len(lv) + strings.Count(lv, "-")
}

// checkLVMDeviceName rejects disk names whose device-mapper name would be too long.
func checkLVMDeviceName(vg, name string) error {
	if n := lvmDeviceNameLength(vg, name); n > MaxLVMDeviceNameLength {
		return fmt.Errorf("disk name %q is too long for LVM: its device name %q would be %d characters (hyphens count double), the maximum is %d",
			name, vg+"-"+name, n, MaxLVMDeviceNameLength)
	}

	return nil
}
