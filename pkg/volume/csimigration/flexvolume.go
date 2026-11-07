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

package csimigration

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	csitrans "k8s.io/csi-translation-lib"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/volume"
)

// FlexVolumeMappingLookup resolves the CSI driver that replaces a FlexVolume driver.
type FlexVolumeMappingLookup interface {
	LookupFlexVolumeMapping(flexDriver string) (csitrans.FlexVolumeMapping, bool)
}

// CSIDriverFlexVolumeMappingLookup finds FlexVolume mappings declared on
// CSIDriver objects with the csitrans.FlexVolumeMigrationAnnotation annotation.
type CSIDriverFlexVolumeMappingLookup struct {
	Lister storagelisters.CSIDriverLister
}

// LookupFlexVolumeMapping implements FlexVolumeMappingLookup. If several
// CSIDrivers claim the same FlexVolume driver, the lexically first one wins so
// the result is deterministic.
func (l CSIDriverFlexVolumeMappingLookup) LookupFlexVolumeMapping(flexDriver string) (csitrans.FlexVolumeMapping, bool) {
	if l.Lister == nil || flexDriver == "" {
		return csitrans.FlexVolumeMapping{}, false
	}
	drivers, err := l.Lister.List(labels.Everything())
	if err != nil {
		return csitrans.FlexVolumeMapping{}, false
	}
	var found *csitrans.FlexVolumeMapping
	for _, d := range drivers {
		m, ok := csitrans.FlexVolumeMappingFromCSIDriver(d)
		if !ok || m.FlexDriver != flexDriver {
			continue
		}
		if found == nil || m.CSIDriver < found.CSIDriver {
			mm := m
			found = &mm
		}
	}
	if found == nil {
		return csitrans.FlexVolumeMapping{}, false
	}
	return *found, true
}

// isFlexVolumeMigratable reports whether the spec is a FlexVolume whose driver
// has a CSI replacement and CSIMigrationFlexVolume is enabled.
func (pm PluginManager) isFlexVolumeMigratable(spec *volume.Spec) (bool, bool) {
	driver := csitrans.FlexVolumeDriverFromSpec(spec.PersistentVolume, spec.Volume)
	if driver == "" {
		return false, false
	}
	if pm.flexLookup == nil || !utilfeature.DefaultFeatureGate.Enabled(features.CSIMigrationFlexVolume) {
		return true, false
	}
	_, ok := pm.flexLookup.LookupFlexVolumeMapping(driver)
	return true, ok
}

// WithFlexVolumeMigration returns a copy of the PluginManager that also treats
// FlexVolume volumes with a CSI replacement as migratable.
func (pm PluginManager) WithFlexVolumeMigration(lookup FlexVolumeMappingLookup) PluginManager {
	pm.flexLookup = lookup
	return pm
}

// flexVolumeTranslator wraps an InTreeToCSITranslator and adds FlexVolume translation.
type flexVolumeTranslator struct {
	InTreeToCSITranslator
	lookup FlexVolumeMappingLookup
}

// NewFlexVolumeAwareTranslator returns a translator that translates FlexVolume
// volumes using lookup, and delegates all other volumes to base.
func NewFlexVolumeAwareTranslator(base InTreeToCSITranslator, lookup FlexVolumeMappingLookup) InTreeToCSITranslator {
	return &flexVolumeTranslator{InTreeToCSITranslator: base, lookup: lookup}
}

func (t *flexVolumeTranslator) mapping(flexDriver string) (csitrans.FlexVolumeMapping, error) {
	m, ok := t.lookup.LookupFlexVolumeMapping(flexDriver)
	if !ok {
		return m, fmt.Errorf("no CSIDriver declares %s=%q", csitrans.FlexVolumeMigrationAnnotation, flexDriver)
	}
	return m, nil
}

func (t *flexVolumeTranslator) TranslateInTreePVToCSI(logger klog.Logger, pv *v1.PersistentVolume) (*v1.PersistentVolume, error) {
	if pv == nil || pv.Spec.FlexVolume == nil {
		return t.InTreeToCSITranslator.TranslateInTreePVToCSI(logger, pv)
	}
	m, err := t.mapping(pv.Spec.FlexVolume.Driver)
	if err != nil {
		return nil, err
	}
	logger.V(4).Info("Translating FlexVolume PV to CSI", "pv", pv.Name, "flexDriver", m.FlexDriver, "csiDriver", m.CSIDriver)
	return csitrans.TranslateFlexVolumePVToCSI(pv, m)
}

func (t *flexVolumeTranslator) TranslateInTreeInlineVolumeToCSI(logger klog.Logger, vol *v1.Volume, podNamespace string) (*v1.PersistentVolume, error) {
	if vol == nil || vol.FlexVolume == nil {
		return t.InTreeToCSITranslator.TranslateInTreeInlineVolumeToCSI(logger, vol, podNamespace)
	}
	m, err := t.mapping(vol.FlexVolume.Driver)
	if err != nil {
		return nil, err
	}
	logger.V(4).Info("Translating inline FlexVolume to CSI", "volume", vol.Name, "flexDriver", m.FlexDriver, "csiDriver", m.CSIDriver)
	return csitrans.TranslateFlexVolumeInlineVolumeToCSI(vol, podNamespace, m)
}
