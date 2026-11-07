#!/bin/bash
# End-to-end FlexVolume -> CSI migration demo on kind (Kubernetes main).
#
# Prereqs (see HANDOFF.md "Build recipe"): ~/flexpoc/build.sh has produced
# ~/flexpoc/bin/{kubelet,pv-rewrite,kubectl,kind}, and docker has the images
# kindest/node:main, localhost/fake-csi:poc and localhost/flexpoc-writer:poc.
#
# Usage: poc/demo.sh            recreate the cluster and run all steps
#        KEEP_CLUSTER=1 poc/demo.sh   reuse a fresh cluster created earlier
# Env: SRC (k/k checkout), BIN, FLEXPOC_KUBECONFIG, PROOF_DIR.
set -uo pipefail

POC=$(cd "$(dirname "$0")" && pwd)
WS=$(dirname "$POC")
SRC=${SRC:-$HOME/flexpoc/kubernetes}
BIN=${BIN:-$HOME/flexpoc/bin}
export KUBECONFIG=${FLEXPOC_KUBECONFIG:-$WS/kubeconfig}
K="$BIN/kubectl"
KIND="$BIN/kind"
CLUSTER=flexpoc
NODE=flexpoc-worker
FLEXDIR=/usr/libexec/kubernetes/kubelet-plugins/volume/exec/example.com~fake
OUT=${PROOF_DIR:-$POC/proof}/$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$OUT"
exec > >(tee "$OUT/demo.log") 2>&1

PASS=0 FAIL=0
step() { echo; echo "=================== $* ==================="; }
log() { echo "+ $*"; }
check() {
  local desc=$1; shift
  if "$@"; then echo "PASS: $desc"; PASS=$((PASS + 1)); else echo "FAIL: $desc"; FAIL=$((FAIL + 1)); fi
}
node() { docker exec "$NODE" sh -c "$*"; }
metrics() { $K get --raw "/api/v1/nodes/$NODE/proxy/metrics"; }
flex_metric() { # $1=state; absent gauge counts as 0
  metrics | awk -v s="$1" '$1 ~ /^volume_manager_flexvolume_volumes\{/ && $1 ~ s {n+=$2} END {print n+0}'
}
csi_metric() {
  metrics | awk -v s="$1" '$1 ~ /^volume_manager_total_volumes\{plugin_name="kubernetes.io\/csi:fake.csi.example.com"/ && $1 ~ s {n+=$2} END {print n+0}'
}
flex_calls() { node "grep -c \" $1 \" /var/log/fake-flex.log || true"; }
csi_calls() { node "grep -c '\"op\":\"$1\"' /var/lib/fake-storage/.fake-csi.log 2>/dev/null || true"; }
lines() { node "wc -l < /var/lib/fake-storage/$1/log"; }
uid() { $K -n demo get pod "$1" -o jsonpath='{.metadata.uid}'; }
restarts() { $K -n demo get pod "$1" -o jsonpath='{.status.containerStatuses[0].restartCount}'; }

restart_kubelet() { # $1 = binary on the node to install (optional)
  local bin=${1:-}
  node "systemctl stop kubelet"
  [ -n "$bin" ] && node "cp $bin /usr/bin/kubelet"
  node "systemctl start kubelet"
  for _ in $(seq 60); do node "curl -sf localhost:10248/healthz >/dev/null" && break; sleep 1; done
  $K wait --for=condition=Ready "node/$NODE" --timeout=60s >/dev/null
  sleep 8 # let the volume manager reconcile
}
set_gate() { # $1 = true|false
  node "sed -i '/^featureGates:/,\$d' /var/lib/kubelet/config.yaml; printf 'featureGates:\n  CSIMigrationFlexVolume: $1\n' >> /var/lib/kubelet/config.yaml"
}
apply_pods() {
  $K apply -f "$POC/manifests/20-writer-pv.yaml" -f "$POC/manifests/21-writer-inline.yaml"
  $K -n demo wait --for=condition=Ready pod/writer-pv pod/writer-inline --timeout=120s
  sleep 5
}
snapshot() { # $1 = label
  local d=$OUT/$1
  mkdir -p "$d"
  $K get pv,pvc,pod -A -o wide > "$d/kubectl-get.txt" 2>&1
  $K get pv flex-pv -o yaml > "$d/pv-flex-pv.yaml" 2>&1
  $K -n demo get pvc flex-pvc -o yaml > "$d/pvc.yaml" 2>&1
  $K -n demo get pods -o yaml > "$d/pods.yaml" 2>&1
  metrics | grep -E '^(# (HELP|TYPE) )?volume_manager_' > "$d/kubelet-volume-metrics.txt"
  node "cat /var/log/fake-flex.log" > "$d/fake-flex.log" 2>&1
  node "cat /var/lib/fake-storage/.fake-csi.log" > "$d/fake-csi.log" 2>&1
  node "findmnt -rn -o TARGET,SOURCE | grep /var/lib/kubelet/pods/ | grep -v -E 'kube-api-access|projected'" > "$d/pod-mounts.txt" 2>&1
  node "tail -n 3 /var/lib/fake-storage/pv-data/log /var/lib/fake-storage/inline-data/log" > "$d/data-tail.txt" 2>&1
  node "journalctl -u kubelet --no-pager" > "$d/kubelet.log" 2>&1
}

#######################################################################
step "0. Fresh cluster on kindest/node:main"
if [ -z "${KEEP_CLUSTER:-}" ]; then
  $KIND delete cluster --name "$CLUSTER" >/dev/null 2>&1
  $KIND create cluster --config "$SRC/test/images/flex-migration-poc/kind/kind.yaml" --image kindest/node:main || exit 1
fi
$KIND load docker-image --name "$CLUSTER" localhost/flexpoc-writer:poc localhost/fake-csi:poc
docker cp "$BIN/kubelet" "$NODE:/usr/bin/kubelet.patched"
node "cp /usr/bin/kubelet /usr/bin/kubelet.stock"
$K wait --for=condition=Ready node --all --timeout=180s
$K version
$K get nodes -o wide

#######################################################################
step "1. Baseline: stock kubelet, FlexVolume driver example.com/fake"
node "mkdir -p '$FLEXDIR'"
docker cp "$SRC/test/images/flex-migration-poc/fake-flex/fake" "$NODE:$FLEXDIR/fake"
node "chmod +x '$FLEXDIR/fake'"
$K apply -f "$POC/manifests/00-namespace.yaml" -f "$POC/manifests/10-flex-pv.yaml"
apply_pods 2>&1 | tee "$OUT/step1-apply-warnings.txt"
UID_PV1=$(uid writer-pv) UID_IN1=$(uid writer-inline)
snapshot step1-baseline
check "flex driver mounted both volumes" [ "$(flex_calls mount)" -eq 2 ]
check "pod volume dirs are example.com~fake" node "findmnt -rn -o TARGET | grep -q '$UID_PV1/volumes/example.com~fake/flex-pv' && findmnt -rn -o TARGET | grep -q '$UID_IN1/volumes/example.com~fake/data'"
check "PV data written through flex" [ "$(lines pv-data)" -gt 0 ]
check "inline data written through flex" [ "$(lines inline-data)" -gt 0 ]

#######################################################################
step "2. Patched kubelet, CSIMigrationFlexVolume=false (regression check)"
MOUNTS_BEFORE=$(flex_calls mount)
restart_kubelet /usr/bin/kubelet.patched
$K get node "$NODE" -o jsonpath='{.status.nodeInfo.kubeletVersion}{"\n"}'
snapshot step2-gate-off
check "node runs the patched kubelet" sh -c "$K get node $NODE -o jsonpath='{.status.nodeInfo.kubeletVersion}' | grep -q flexpoc"
check "flex metric reports 2 volumes (ASW)" [ "$(flex_metric actual_state_of_world)" -eq 2 ]
check "flex metric reports 2 volumes (DSW)" [ "$(flex_metric desired_state_of_world)" -eq 2 ]
check "no flex remounts after kubelet restart" [ "$(flex_calls mount)" -eq "$MOUNTS_BEFORE" ]
check "no flex unmounts after kubelet restart" [ "$(flex_calls unmount)" -eq 0 ]
check "pods kept running" [ "$(uid writer-pv)" = "$UID_PV1" -a "$(restarts writer-pv)" = 0 ]

#######################################################################
step "3. Enable migration: deploy fake-csi, drain, flip the gate, recreate pods"
$K apply -f "$POC/manifests/30-fake-csi.yaml"
$K -n kube-system rollout status ds/fake-csi --timeout=120s
for _ in $(seq 30); do $K get csinode "$NODE" -o jsonpath='{.spec.drivers[*].name}' | grep -q fake.csi.example.com && break; sleep 1; done
$K get csinode "$NODE" -o jsonpath='{.spec.drivers}{"\n"}'
PV_LINES=$(lines pv-data) IN_LINES=$(lines inline-data)
log "drain: running pods keep the flex mount, so they must leave the node before the gate flips"
$K drain "$NODE" --ignore-daemonsets --force --delete-emptydir-data --timeout=120s
$K -n demo wait --for=delete pod/writer-pv pod/writer-inline --timeout=60s 2>/dev/null
set_gate true
restart_kubelet
$K uncordon "$NODE"
MOUNTS_BEFORE=$(flex_calls mount)
apply_pods
UID_PV2=$(uid writer-pv) UID_IN2=$(uid writer-inline)
snapshot step3-gate-on

#######################################################################
step "4. Pass criteria after migration"
check "no new flex mount calls" [ "$(flex_calls mount)" -eq "$MOUNTS_BEFORE" ]
check "fake-csi NodePublish for the PV" node "grep NodePublishVolume /var/lib/fake-storage/.fake-csi.log | grep -q '\"pod\":\"demo/writer-pv\".*$UID_PV2'"
check "fake-csi NodePublish for the inline volume" node "grep NodePublishVolume /var/lib/fake-storage/.fake-csi.log | grep -q '\"pod\":\"demo/writer-inline\".*$UID_IN2'"
check "fake-csi saw the original flex driver attribute" node "grep -q '\"flexDriver\":\"example.com/fake\"' /var/lib/fake-storage/.fake-csi.log"
check "pod volume dirs are kubernetes.io~csi" node "findmnt -rn -o TARGET | grep -q '$UID_PV2/volumes/kubernetes.io~csi/flex-pv/mount' && findmnt -rn -o TARGET | grep -q '$UID_IN2/volumes/kubernetes.io~csi/fake.csi.example.com-flex-'"
check "flex metric is 0" [ "$(flex_metric actual_state_of_world)" -eq 0 ]
check "CSI metric shows 2 volumes" [ "$(csi_metric actual_state_of_world)" -eq 2 ]
check "PV data intact (old pod's lines kept)" node "grep -q 'uid=$UID_PV1' /var/lib/fake-storage/pv-data/log"
check "inline data intact (old pod's lines kept)" node "grep -q 'uid=$UID_IN1' /var/lib/fake-storage/inline-data/log"
check "new PV pod appends to the same data" node "grep -q 'uid=$UID_PV2' /var/lib/fake-storage/pv-data/log"
check "new inline pod appends to the same data" node "grep -q 'uid=$UID_IN2' /var/lib/fake-storage/inline-data/log"
check "line counts only grew" [ "$(lines pv-data)" -gt "$PV_LINES" -a "$(lines inline-data)" -gt "$IN_LINES" ]
check "pod sees the data through CSI" sh -c "$K -n demo exec writer-pv -- /writer cat | grep -q 'uid=$UID_PV1'"

#######################################################################
step "5. pv-rewrite: flex PV -> native CSI PV, pod running"
(cd "$OUT" && "$BIN/pv-rewrite" > "$OUT/pv-rewrite-dry-run.yaml" 2>&1)
cat "$OUT/pv-rewrite-dry-run.yaml"
diff -u "$OUT/step3-gate-on/pv-flex-pv.yaml" "$OUT/pv-rewrite-dry-run.yaml" > "$OUT/pv-rewrite-dry-run.diff"
check "dry run did not change the PV" sh -c "$K get pv flex-pv -o jsonpath='{.spec.flexVolume.driver}' | grep -q example.com/fake"
PUB_BEFORE=$(csi_calls NodePublishVolume) UNPUB_BEFORE=$(csi_calls NodeUnpublishVolume)
$K -n demo get pvc flex-pvc -w -o jsonpath='{.metadata.resourceVersion} {.status.phase}{"\n"}' > "$OUT/pvc-phase-watch.txt" 2>&1 &
WATCH=$!
$K get pv flex-pv -w -o jsonpath='{.metadata.uid} {.status.phase}{"\n"}' > "$OUT/pv-phase-watch.txt" 2>&1 &
WATCH2=$!
sleep 1
(cd "$OUT" && "$BIN/pv-rewrite" --apply 2>&1 | tee "$OUT/pv-rewrite-apply.txt")
sleep 10
kill $WATCH $WATCH2 2>/dev/null
echo "PVC phases seen during rewrite:"; cat "$OUT/pvc-phase-watch.txt"
echo "PV uid/phases seen during rewrite:"; cat "$OUT/pv-phase-watch.txt"
snapshot step5-rewritten
check "PV is now native CSI" sh -c "$K get pv flex-pv -o jsonpath='{.spec.csi.driver}/{.spec.csi.volumeHandle}' | grep -qx fake.csi.example.com/flex-pv"
check "PV has no flexVolume" sh -c "[ -z \"\$($K get pv flex-pv -o jsonpath='{.spec.flexVolume}')\" ]"
check "PV annotated rewritten-from" sh -c "$K get pv flex-pv -o jsonpath='{.metadata.annotations.storage\.kubernetes\.io/rewritten-from}' | grep -qx flexVolume/example.com/fake"
check "PV reclaim policy preserved (Retain)" sh -c "$K get pv flex-pv -o jsonpath='{.spec.persistentVolumeReclaimPolicy}' | grep -qx Retain"
check "PVC Bound to flex-pv" sh -c "$K -n demo get pvc flex-pvc -o jsonpath='{.status.phase}/{.spec.volumeName}' | grep -qx Bound/flex-pv"
check "backup of the old PV written" [ -s "$OUT/pv-backup-flex-pv.json" ]
check "pod not restarted" [ "$(uid writer-pv)" = "$UID_PV2" -a "$(restarts writer-pv)" = 0 ]
check "kubelet did not republish or unpublish" [ "$(csi_calls NodePublishVolume)" -eq "$PUB_BEFORE" -a "$(csi_calls NodeUnpublishVolume)" -eq "$UNPUB_BEFORE" ]
L=$(lines pv-data); sleep 4
check "pod still writing" [ "$(lines pv-data)" -gt "$L" ]

#######################################################################
step "6. Remove the FlexVolume driver from the node"
node "rm -rf '$FLEXDIR'"
FLEX_LOG_BEFORE=$(node "wc -l < /var/log/fake-flex.log")
restart_kubelet
$K -n demo delete pod writer-pv writer-inline --wait=true
apply_pods
UID_PV3=$(uid writer-pv) UID_IN3=$(uid writer-inline)
snapshot step6-flex-removed
check "flex driver dir is gone" node "[ ! -e '$FLEXDIR' ]"
check "fake-flex never ran again" [ "$(node "wc -l < /var/log/fake-flex.log")" -eq "$FLEX_LOG_BEFORE" ]
check "rewritten PV mounts through CSI" node "grep NodePublishVolume /var/lib/fake-storage/.fake-csi.log | grep -q '$UID_PV3'"
check "inline flexVolume pod (unchanged spec) mounts through CSI" node "grep NodePublishVolume /var/lib/fake-storage/.fake-csi.log | grep -q '$UID_IN3'"
check "PV data from all three pods intact" node "grep -q 'uid=$UID_PV1' /var/lib/fake-storage/pv-data/log && grep -q 'uid=$UID_PV2' /var/lib/fake-storage/pv-data/log && grep -q 'uid=$UID_PV3' /var/lib/fake-storage/pv-data/log"
check "inline data from all three pods intact" node "grep -q 'uid=$UID_IN1' /var/lib/fake-storage/inline-data/log && grep -q 'uid=$UID_IN3' /var/lib/fake-storage/inline-data/log"
check "flex metric is 0" [ "$(flex_metric actual_state_of_world)" -eq 0 ]

#######################################################################
step "RESULT: $PASS passed, $FAIL failed"
echo "proof dir: $OUT"
[ "$FAIL" -eq 0 ]
