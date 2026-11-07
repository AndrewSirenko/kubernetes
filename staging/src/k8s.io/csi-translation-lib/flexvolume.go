/*
Copyright 2026 The Kubernetes Authors.

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

package csitranslation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	v1 "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FlexVolume -> CSI translation.
//
// Unlike the cloud-provider in-tree plugins, FlexVolume driver names are
// arbitrary ("vendor/driver"), so the CSI driver that replaces a FlexVolume
// driver cannot be a static table in this library. Instead, a CSIDriver object
// opts in to replacing a FlexVolume driver with an annotation, and callers
// resolve the mapping (for example from a CSIDriver lister) and pass it in.
//
// The translation is deliberately generic: FlexVolume options become CSI
// volume attributes verbatim, and the FlexVolume secret becomes the CSI
// node-publish secret. Driver-specific logic belongs in the replacement CSI
// driver (for example a "FlexVolume compatible" mode that understands the old
// option keys), not in Kubernetes.

const (
	// FlexVolumeMigrationAnnotation is set on a CSIDriver object to declare
	// that the CSI driver replaces the named FlexVolume driver, for example
	// "example.com/fake".
	FlexVolumeMigrationAnnotation = "storage.kubernetes.io/migrated-from-flexvolume"

	// FlexVolumeHandleOptionAnnotation is optionally set on a CSIDriver object
	// to name the FlexVolume option whose value is used as the CSI volume
	// handle. When unset, the handle is derived from the volume source.
	FlexVolumeHandleOptionAnnotation = "storage.kubernetes.io/flexvolume-handle-option"

	// FlexVolumeDriverAttribute is the CSI volume attribute that carries the
	// original FlexVolume driver name, so the CSI driver can tell migrated
	// volumes apart.
	FlexVolumeDriverAttribute = "storage.kubernetes.io/flexvolume-driver"
)

// FlexVolumeMapping describes how volumes of one FlexVolume driver translate to CSI.
type FlexVolumeMapping struct {
	// FlexDriver is the FlexVolume driver name, e.g. "example.com/fake".
	FlexDriver string
	// CSIDriver is the name of the CSI driver that replaces it.
	CSIDriver string
	// HandleOption, if set, names the FlexVolume option used as the CSI volume handle.
	HandleOption string
}

// FlexVolumeMappingFromCSIDriver returns the FlexVolume mapping declared by a
// CSIDriver object, if any.
func FlexVolumeMappingFromCSIDriver(d *storage.CSIDriver) (FlexVolumeMapping, bool) {
	if d == nil {
		return FlexVolumeMapping{}, false
	}
	flexDriver := d.Annotations[FlexVolumeMigrationAnnotation]
	if flexDriver == "" {
		return FlexVolumeMapping{}, false
	}
	return FlexVolumeMapping{
		FlexDriver:   flexDriver,
		CSIDriver:    d.Name,
		HandleOption: d.Annotations[FlexVolumeHandleOptionAnnotation],
	}, true
}

// FlexVolumeDriverFromSpec returns the FlexVolume driver name used by a PV or
// inline volume, or "" if it is not a FlexVolume.
func FlexVolumeDriverFromSpec(pv *v1.PersistentVolume, vol *v1.Volume) string {
	switch {
	case pv != nil && pv.Spec.FlexVolume != nil:
		return pv.Spec.FlexVolume.Driver
	case vol != nil && vol.FlexVolume != nil:
		return vol.FlexVolume.Driver
	default:
		return ""
	}
}

// TranslateFlexVolumePVToCSI translates a FlexVolume PersistentVolume to a CSI
// PersistentVolume using the given mapping. The input is not modified.
func TranslateFlexVolumePVToCSI(pv *v1.PersistentVolume, m FlexVolumeMapping) (*v1.PersistentVolume, error) {
	if pv == nil || pv.Spec.FlexVolume == nil {
		return nil, errors.New("pv is nil or FlexVolume not defined on pv")
	}
	if err := m.validate(pv.Spec.FlexVolume.Driver); err != nil {
		return nil, err
	}
	flex := pv.Spec.FlexVolume
	handle, err := m.volumeHandle(flex.Options, pv.Name)
	if err != nil {
		return nil, err
	}

	out := pv.DeepCopy()
	out.Spec.FlexVolume = nil
	out.Spec.CSI = &v1.CSIPersistentVolumeSource{
		Driver:           m.CSIDriver,
		VolumeHandle:     handle,
		ReadOnly:         flex.ReadOnly,
		FSType:           flex.FSType,
		VolumeAttributes: m.volumeAttributes(flex.Options),
	}
	if flex.SecretRef != nil {
		ref := *flex.SecretRef
		out.Spec.CSI.NodePublishSecretRef = &ref
	}
	return out, nil
}

// TranslateFlexVolumeInlineVolumeToCSI translates an inline FlexVolume pod
// volume to a CSI PersistentVolume, the same way the in-tree CSI migration
// handles inline cloud volumes.
func TranslateFlexVolumeInlineVolumeToCSI(vol *v1.Volume, podNamespace string, m FlexVolumeMapping) (*v1.PersistentVolume, error) {
	if vol == nil || vol.FlexVolume == nil {
		return nil, errors.New("volume is nil or FlexVolume not defined on volume")
	}
	if err := m.validate(vol.FlexVolume.Driver); err != nil {
		return nil, err
	}
	flex := vol.FlexVolume
	handle, err := m.volumeHandle(flex.Options, "")
	if err != nil {
		return nil, err
	}

	// Inline volumes are always Filesystem; nothing defaults it for us because
	// this PV never goes through the API server.
	fsMode := v1.PersistentVolumeFilesystem
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			// Must be unique per volume: it becomes part of the staging path.
			Name: fmt.Sprintf("%s-%s", m.CSIDriver, handle),
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:           m.CSIDriver,
					VolumeHandle:     handle,
					ReadOnly:         flex.ReadOnly,
					FSType:           flex.FSType,
					VolumeAttributes: m.volumeAttributes(flex.Options),
				},
			},
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			VolumeMode:  &fsMode,
		},
	}
	if flex.SecretRef != nil && flex.SecretRef.Name != "" {
		pv.Spec.CSI.NodePublishSecretRef = &v1.SecretReference{
			Name:      flex.SecretRef.Name,
			Namespace: podNamespace,
		}
	}
	return pv, nil
}

func (m FlexVolumeMapping) validate(flexDriver string) error {
	if m.CSIDriver == "" {
		return errors.New("no CSI driver in FlexVolume mapping")
	}
	if m.FlexDriver != flexDriver {
		return fmt.Errorf("FlexVolume mapping is for driver %q, not %q", m.FlexDriver, flexDriver)
	}
	return nil
}

// volumeHandle picks a stable CSI volume handle. Preference order:
//  1. the value of the option named by HandleOption,
//  2. the PV name (unique cluster-wide),
//  3. for inline volumes, a hash of the driver and options, so the same
//     FlexVolume source always maps to the same CSI volume.
func (m FlexVolumeMapping) volumeHandle(options map[string]string, pvName string) (string, error) {
	if m.HandleOption != "" {
		if h := options[m.HandleOption]; h != "" {
			return h, nil
		}
		return "", fmt.Errorf("FlexVolume option %q (volume handle) is missing or empty", m.HandleOption)
	}
	if pvName != "" {
		return pvName, nil
	}
	keys := make([]string, 0, len(options))
	for k := range options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(m.FlexDriver))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(options[k]))
	}
	return "flex-" + hex.EncodeToString(h.Sum(nil))[:20], nil
}

func (m FlexVolumeMapping) volumeAttributes(options map[string]string) map[string]string {
	attrs := make(map[string]string, len(options)+1)
	for k, v := range options {
		attrs[k] = v
	}
	attrs[FlexVolumeDriverAttribute] = m.FlexDriver
	return attrs
}
