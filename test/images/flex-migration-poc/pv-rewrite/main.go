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

// pv-rewrite rewrites PersistentVolumes that use a legacy volume source
// (FlexVolume, or a CSI-migrated in-tree cloud type such as
// awsElasticBlockStore) into native CSI PersistentVolumes.
//
// PersistentVolume sources are immutable, so a PV is rewritten by:
//
//  1. setting persistentVolumeReclaimPolicy=Retain on the old PV, so deleting
//     it can never delete the underlying storage,
//  2. computing the replacement with the same csi-translation-lib translation
//     the kubelet and controllers already apply at runtime, so the new PV has
//     the exact volume handle and attributes that are already in use,
//  3. deleting the old PV (removing the pv-protection finalizer),
//  4. creating the new PV with the same name and claimRef (including the PVC
//     UID), restoring the original reclaim policy.
//
// Because the replacement equals what CSI migration already produced, a
// running pod sees no change: the kubelet's unique volume name, the CSI
// volume handle and the publish path are identical.
//
// Default is a dry run that prints the replacement PV; pass --apply to act.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	csitrans "k8s.io/csi-translation-lib"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

const rewrittenFromAnnotation = "storage.kubernetes.io/rewritten-from"

var (
	kubeconfig = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig")
	pvName     = flag.String("pv", "", "rewrite only this PV (default: all eligible PVs)")
	apply      = flag.Bool("apply", false, "actually rewrite PVs (default: dry run)")
	timeout    = flag.Duration("timeout", 60*time.Second, "per-PV timeout")
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	must(err)
	cs, err := kubernetes.NewForConfig(cfg)
	must(err)
	ctx := context.Background()

	mappings, err := flexMappings(ctx, cs)
	must(err)

	pvs, err := cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	must(err)
	n := 0
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if *pvName != "" && pv.Name != *pvName {
			continue
		}
		newPV, from, err := translate(pv, mappings)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", pv.Name, err)
			continue
		}
		if newPV == nil {
			continue
		}
		n++
		if !*apply {
			fmt.Printf("# DRY RUN: would rewrite PV %s (%s) as:\n", pv.Name, from)
			b, _ := yaml.Marshal(newPV)
			fmt.Println(string(b))
			continue
		}
		if err := rewrite(ctx, cs, pv, newPV); err != nil {
			fmt.Fprintf(os.Stderr, "FAILED %s: %v\n", pv.Name, err)
			os.Exit(1)
		}
		fmt.Printf("rewrote PV %s: %s -> csi/%s (handle %s)\n", pv.Name, from, newPV.Spec.CSI.Driver, newPV.Spec.CSI.VolumeHandle)
	}
	if n == 0 {
		fmt.Println("no PVs to rewrite")
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func flexMappings(ctx context.Context, cs kubernetes.Interface) (map[string]csitrans.FlexVolumeMapping, error) {
	drivers, err := cs.StorageV1().CSIDrivers().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]csitrans.FlexVolumeMapping{}
	for i := range drivers.Items {
		if m, ok := csitrans.FlexVolumeMappingFromCSIDriver(&drivers.Items[i]); ok {
			if prev, dup := out[m.FlexDriver]; !dup || m.CSIDriver < prev.CSIDriver {
				out[m.FlexDriver] = m
			}
		}
	}
	return out, nil
}

// translate returns the replacement PV, or nil if the PV needs no rewrite.
func translate(pv *v1.PersistentVolume, mappings map[string]csitrans.FlexVolumeMapping) (*v1.PersistentVolume, string, error) {
	var out *v1.PersistentVolume
	var from string
	var err error
	switch {
	case pv.Spec.FlexVolume != nil:
		from = "flexVolume/" + pv.Spec.FlexVolume.Driver
		m, ok := mappings[pv.Spec.FlexVolume.Driver]
		if !ok {
			return nil, "", fmt.Errorf("no CSIDriver declares %s=%q", csitrans.FlexVolumeMigrationAnnotation, pv.Spec.FlexVolume.Driver)
		}
		out, err = csitrans.TranslateFlexVolumePVToCSI(pv, m)
	case csitrans.New().IsPVMigratable(pv):
		from, _ = csitrans.New().GetInTreePluginNameFromSpec(pv, nil)
		out, err = csitrans.New().TranslateInTreePVToCSI(klog.Background(), pv)
	default:
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	out.ObjectMeta = metav1.ObjectMeta{
		Name:        pv.Name,
		Labels:      pv.Labels,
		Annotations: map[string]string{},
	}
	for k, v := range pv.Annotations {
		out.Annotations[k] = v
	}
	out.Annotations[rewrittenFromAnnotation] = from
	out.Status = v1.PersistentVolumeStatus{}
	return out, from, nil
}

func rewrite(ctx context.Context, cs kubernetes.Interface, old, newPV *v1.PersistentVolume) error {
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	pvs := cs.CoreV1().PersistentVolumes()

	// 1. Never let the delete reach the storage backend.
	if old.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimRetain {
		patch := []byte(`{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}`)
		if _, err := pvs.Patch(ctx, old.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("set Retain: %w", err)
		}
	}

	// Keep a backup of the original object next to the tool's working dir.
	backup, _ := json.MarshalIndent(old, "", "  ")
	_ = os.WriteFile(fmt.Sprintf("pv-backup-%s.json", old.Name), backup, 0o600)

	// 2+3. Delete the old PV. A bound PV keeps the pv-protection finalizer,
	// so drop finalizers explicitly; storage is safe because of Retain.
	if err := pvs.Delete(ctx, old.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &old.UID}}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete: %w", err)
	}
	if _, err := pvs.Patch(ctx, old.Name, types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove finalizers: %w", err)
	}
	if err := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		_, err := pvs.Get(ctx, old.Name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	}); err != nil {
		return fmt.Errorf("waiting for old PV to go away: %w", err)
	}

	// 4. Recreate with the same name and claimRef (incl. PVC UID), so the PV
	// controller rebinds it to the same claim.
	if _, err := pvs.Create(ctx, newPV, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create replacement (backup in pv-backup-%s.json): %w", old.Name, err)
	}

	// Wait for the claim to be Bound again.
	if ref := newPV.Spec.ClaimRef; ref != nil {
		return wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
			pvc, err := cs.CoreV1().PersistentVolumeClaims(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return pvc.Status.Phase == v1.ClaimBound && pvc.Spec.VolumeName == newPV.Name, nil
		})
	}
	return nil
}
