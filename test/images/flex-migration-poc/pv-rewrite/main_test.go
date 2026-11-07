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

package main

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	csitrans "k8s.io/csi-translation-lib"
)

func TestTranslateFlexPV(t *testing.T) {
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "flex-pv",
			UID:        "old-uid",
			Finalizers: []string{"kubernetes.io/pv-protection"},
			Annotations: map[string]string{
				v1.LastAppliedConfigAnnotation: `{"spec":{"flexVolume":{}}}`,
				"keep":                         "me",
			},
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: v1.PersistentVolumeReclaimDelete,
			ClaimRef:                      &v1.ObjectReference{Namespace: "demo", Name: "c", UID: "pvc-uid"},
			PersistentVolumeSource: v1.PersistentVolumeSource{FlexVolume: &v1.FlexPersistentVolumeSource{
				Driver: "example.com/fake", Options: map[string]string{"path": "p"},
			}},
		},
		Status: v1.PersistentVolumeStatus{Phase: v1.VolumeBound},
	}
	mappings := map[string]csitrans.FlexVolumeMapping{
		"example.com/fake": {FlexDriver: "example.com/fake", CSIDriver: "fake.csi.example.com"},
	}
	out, from, err := translate(pv, mappings)
	if err != nil {
		t.Fatal(err)
	}
	if from != "flexVolume/example.com/fake" || out.Spec.FlexVolume != nil || out.Spec.CSI == nil {
		t.Fatalf("not translated: from=%q spec=%+v", from, out.Spec.PersistentVolumeSource)
	}
	if out.UID != "" || len(out.Finalizers) != 0 || out.Status.Phase != "" {
		t.Errorf("server-set fields must be cleared: %+v %+v", out.ObjectMeta, out.Status)
	}
	if _, ok := out.Annotations[v1.LastAppliedConfigAnnotation]; ok {
		t.Error("last-applied-configuration must be dropped")
	}
	if out.Annotations["keep"] != "me" || out.Annotations[rewrittenFromAnnotation] != from {
		t.Errorf("unexpected annotations %v", out.Annotations)
	}
	if out.Spec.ClaimRef.UID != "pvc-uid" || out.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimDelete {
		t.Errorf("claimRef and original reclaim policy must be kept: %+v", out.Spec)
	}
}

func TestTranslateSkipsUnmappedAndNative(t *testing.T) {
	flex := &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{
		FlexVolume: &v1.FlexPersistentVolumeSource{Driver: "other/driver"},
	}}}
	if _, _, err := translate(flex, nil); err == nil {
		t.Error("unmapped flex driver must be an error")
	}
	native := &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{PersistentVolumeSource: v1.PersistentVolumeSource{
		CSI: &v1.CSIPersistentVolumeSource{Driver: "d", VolumeHandle: "h"},
	}}}
	if out, _, err := translate(native, nil); err != nil || out != nil {
		t.Errorf("native CSI PV must be skipped, got %v %v", out, err)
	}
}
