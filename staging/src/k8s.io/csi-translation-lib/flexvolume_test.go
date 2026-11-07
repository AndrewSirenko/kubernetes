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
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var testMapping = FlexVolumeMapping{FlexDriver: "example.com/fake", CSIDriver: "fake.csi.example.com"}

func TestFlexVolumeMappingFromCSIDriver(t *testing.T) {
	d := &storage.CSIDriver{ObjectMeta: metav1.ObjectMeta{
		Name: "fake.csi.example.com",
		Annotations: map[string]string{
			FlexVolumeMigrationAnnotation:    "example.com/fake",
			FlexVolumeHandleOptionAnnotation: "volumeID",
		},
	}}
	m, ok := FlexVolumeMappingFromCSIDriver(d)
	if !ok || m.FlexDriver != "example.com/fake" || m.CSIDriver != "fake.csi.example.com" || m.HandleOption != "volumeID" {
		t.Fatalf("unexpected mapping %+v ok=%v", m, ok)
	}
	if _, ok := FlexVolumeMappingFromCSIDriver(&storage.CSIDriver{}); ok {
		t.Fatal("expected no mapping without annotation")
	}
}

func TestTranslateFlexVolumePVToCSI(t *testing.T) {
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv1", Labels: map[string]string{"a": "b"}},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{FlexVolume: &v1.FlexPersistentVolumeSource{
				Driver:    "example.com/fake",
				FSType:    "ext4",
				ReadOnly:  true,
				SecretRef: &v1.SecretReference{Name: "s", Namespace: "ns"},
				Options:   map[string]string{"path": "data1"},
			}},
			AccessModes:                   []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimRetain,
		},
	}
	out, err := TranslateFlexVolumePVToCSI(pv, testMapping)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Spec.FlexVolume == nil {
		t.Fatal("input was modified")
	}
	c := out.Spec.CSI
	if out.Spec.FlexVolume != nil || c == nil {
		t.Fatalf("not translated: %+v", out.Spec.PersistentVolumeSource)
	}
	if c.Driver != "fake.csi.example.com" || c.VolumeHandle != "pv1" || c.FSType != "ext4" || !c.ReadOnly {
		t.Errorf("unexpected CSI source %+v", c)
	}
	if c.VolumeAttributes["path"] != "data1" || c.VolumeAttributes[FlexVolumeDriverAttribute] != "example.com/fake" {
		t.Errorf("unexpected attributes %v", c.VolumeAttributes)
	}
	if c.NodePublishSecretRef == nil || c.NodePublishSecretRef.Name != "s" || c.NodePublishSecretRef.Namespace != "ns" {
		t.Errorf("unexpected secret ref %+v", c.NodePublishSecretRef)
	}
	if out.Labels["a"] != "b" || out.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimRetain {
		t.Error("metadata/spec not preserved")
	}
}

func TestTranslateFlexVolumeHandleOption(t *testing.T) {
	m := testMapping
	m.HandleOption = "volumeID"
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv1"},
		Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{FlexVolume: &v1.FlexPersistentVolumeSource{
			Driver: "example.com/fake", Options: map[string]string{"volumeID": "vol-123"},
		}}},
	}
	out, err := TranslateFlexVolumePVToCSI(pv, m)
	if err != nil || out.Spec.CSI.VolumeHandle != "vol-123" {
		t.Fatalf("handle=%v err=%v", out, err)
	}
	pv.Spec.FlexVolume.Options = nil
	if _, err := TranslateFlexVolumePVToCSI(pv, m); err == nil {
		t.Fatal("expected error when handle option is missing")
	}
}

func TestTranslateFlexVolumeWrongDriver(t *testing.T) {
	vol := &v1.Volume{Name: "v", VolumeSource: v1.VolumeSource{FlexVolume: &v1.FlexVolumeSource{Driver: "other/driver"}}}
	if _, err := TranslateFlexVolumeInlineVolumeToCSI(vol, "ns", testMapping); err == nil {
		t.Fatal("expected error for mismatched driver")
	}
}

func TestTranslateFlexVolumeInlineVolumeToCSI(t *testing.T) {
	mk := func(opts map[string]string) *v1.Volume {
		return &v1.Volume{Name: "v", VolumeSource: v1.VolumeSource{FlexVolume: &v1.FlexVolumeSource{
			Driver: "example.com/fake", Options: opts, SecretRef: &v1.LocalObjectReference{Name: "s"},
		}}}
	}
	a, err := TranslateFlexVolumeInlineVolumeToCSI(mk(map[string]string{"path": "x", "z": "1"}), "ns", testMapping)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := TranslateFlexVolumeInlineVolumeToCSI(mk(map[string]string{"z": "1", "path": "x"}), "ns", testMapping)
	c, _ := TranslateFlexVolumeInlineVolumeToCSI(mk(map[string]string{"path": "y"}), "ns", testMapping)
	if a.Spec.CSI.VolumeHandle != b.Spec.CSI.VolumeHandle {
		t.Error("handle must be deterministic")
	}
	if a.Spec.CSI.VolumeHandle == c.Spec.CSI.VolumeHandle {
		t.Error("different options must give different handles")
	}
	if !strings.HasPrefix(a.Spec.CSI.VolumeHandle, "flex-") || a.Name != "fake.csi.example.com-"+a.Spec.CSI.VolumeHandle {
		t.Errorf("unexpected handle/name %q %q", a.Spec.CSI.VolumeHandle, a.Name)
	}
	if r := a.Spec.CSI.NodePublishSecretRef; r == nil || r.Name != "s" || r.Namespace != "ns" {
		t.Errorf("unexpected secret ref %+v", r)
	}
}
