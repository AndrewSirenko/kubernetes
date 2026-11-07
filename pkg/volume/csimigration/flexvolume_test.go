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
	"testing"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	csitrans "k8s.io/csi-translation-lib"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/volume"
)

func fakeCSIDriverLister(t *testing.T, drivers ...*storagev1.CSIDriver) storagelisters.CSIDriverLister {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, d := range drivers {
		if err := indexer.Add(d); err != nil {
			t.Fatal(err)
		}
	}
	return storagelisters.NewCSIDriverLister(indexer)
}

func flexCSIDriver(name, flexDriver string) *storagev1.CSIDriver {
	return &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Annotations: map[string]string{csitrans.FlexVolumeMigrationAnnotation: flexDriver},
	}}
}

func flexInlineSpec(driver string) *volume.Spec {
	return volume.NewSpecFromVolume(&v1.Volume{Name: "v", VolumeSource: v1.VolumeSource{
		FlexVolume: &v1.FlexVolumeSource{Driver: driver, Options: map[string]string{"path": "x"}},
	}})
}

func TestCSIDriverFlexVolumeMappingLookup(t *testing.T) {
	l := CSIDriverFlexVolumeMappingLookup{Lister: fakeCSIDriverLister(t,
		flexCSIDriver("b.csi.example.com", "example.com/fake"),
		flexCSIDriver("a.csi.example.com", "example.com/fake"),
		&storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "plain.csi.example.com"}},
	)}
	m, ok := l.LookupFlexVolumeMapping("example.com/fake")
	if !ok || m.CSIDriver != "a.csi.example.com" {
		t.Fatalf("expected deterministic first match, got %+v %v", m, ok)
	}
	if _, ok := l.LookupFlexVolumeMapping("other/driver"); ok {
		t.Fatal("unexpected mapping")
	}
}

func TestFlexVolumeIsMigratable(t *testing.T) {
	lookup := CSIDriverFlexVolumeMappingLookup{Lister: fakeCSIDriverLister(t, flexCSIDriver("fake.csi.example.com", "example.com/fake"))}
	pm := NewPluginManager(csitrans.New()).WithFlexVolumeMigration(lookup)

	tests := []struct {
		name string
		gate bool
		spec *volume.Spec
		pm   PluginManager
		want bool
	}{
		{"gate off", false, flexInlineSpec("example.com/fake"), pm, false},
		{"gate on, mapped", true, flexInlineSpec("example.com/fake"), pm, true},
		{"gate on, unmapped driver", true, flexInlineSpec("other/driver"), pm, false},
		{"gate on, no lookup configured", true, flexInlineSpec("example.com/fake"), NewPluginManager(csitrans.New()), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CSIMigrationFlexVolume, tc.gate)
			got, err := tc.pm.IsMigratable(tc.spec)
			if err != nil || got != tc.want {
				t.Fatalf("IsMigratable = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestFlexVolumeAwareTranslator(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CSIMigrationFlexVolume, true)
	lookup := CSIDriverFlexVolumeMappingLookup{Lister: fakeCSIDriverLister(t, flexCSIDriver("fake.csi.example.com", "example.com/fake"))}
	tr := NewFlexVolumeAwareTranslator(csitrans.New(), lookup)

	spec, err := TranslateInTreeSpecToCSI(logger, flexInlineSpec("example.com/fake"), "ns", tr)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Migrated || !spec.InlineVolumeSpecForCSIMigration || spec.PersistentVolume.Spec.CSI.Driver != "fake.csi.example.com" {
		t.Fatalf("unexpected translated spec %+v", spec)
	}

	// Non-flex volumes still go to the base translator.
	ebs := volume.NewSpecFromVolume(&v1.Volume{Name: "e", VolumeSource: v1.VolumeSource{
		AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{VolumeID: "vol-1"},
	}})
	spec, err = TranslateInTreeSpecToCSI(logger, ebs, "ns", tr)
	if err != nil || spec.PersistentVolume.Spec.CSI.Driver != "ebs.csi.aws.com" {
		t.Fatalf("base translation broken: %+v %v", spec, err)
	}

	if _, err := TranslateInTreeSpecToCSI(logger, flexInlineSpec("other/driver"), "ns", tr); err == nil {
		t.Fatal("expected error for unmapped driver")
	}
}
