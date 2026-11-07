#!/bin/bash
# Stretch checks. Run right after a passing demo.sh, on the same cluster
# (gate on, flex driver removed, flex-pv already rewritten to native CSI).
#
#  S1  kubelet restart while CSI-migrated volumes are mounted (reconstruction)
#  S2  inline flexVolume with secretRef, served by CSI
#  S3  rollback without drain: reinstall flex, gate off, restart kubelet
#  S4  forward flip without drain: gate on, restart kubelet
#
# S3/S4 record behavior; they assert only what a safe design must guarantee
# (pods keep running and their data is intact), and log what the kubelet did.
set -uo pipefail

POC=$(cd "$(dirname "$0")" && pwd)
WS=$(dirname "$POC")
SRC=${SRC:-$HOME/flexpoc/kubernetes}
BIN=${BIN:-$HOME/flexpoc/bin}
export KUBECONFIG=${FLEXPOC_KUBECONFIG:-$WS/kubeconfig}
K="$BIN/kubectl"
NODE=flexpoc-worker
FLEXDIR=/usr/libexec/kubernetes/kubelet-plugins/volume/exec/example.com~fake
OUT=${PROOF_DIR:-$POC/proof}/stretch-$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$OUT"
exec > >(tee "$OUT/stretch.log") 2>&1

PASS=0 FAIL=0
step() { echo; echo "=================== $* ==================="; }
check() {
  local desc=$1; shift
  if "$@"; then echo "PASS: $desc"; PASS=$((PASS + 1)); else echo "FAIL: $desc"; FAIL=$((FAIL + 1)); fi
}
node() { docker exec "$NODE" sh -c "$*"; }
uid() { $K -n demo get pod "$1" -o jsonpath='{.metadata.uid}'; }
restarts() { $K -n demo get pod "$1" -o jsonpath='{.status.containerStatuses[0].restartCount}'; }
lines() { node "wc -l < /var/lib/fake-storage/$1/log"; }
csi_calls() { node "grep -c '\"op\":\"$1\"' /var/lib/fake-storage/.fake-csi.log 2>/dev/null || true"; }
flex_lines() { node "wc -l < /var/log/fake-flex.log"; }
dup_mounts() { node "findmnt -rn -o TARGET | grep /var/lib/kubelet/pods/ | sort | uniq -d | wc -l"; }
restart_kubelet() {
  node "systemctl restart kubelet"
  for _ in $(seq 60); do node "curl -sf localhost:10248/healthz >/dev/null" && break; sleep 1; done
  $K wait --for=condition=Ready "node/$NODE" --timeout=60s >/dev/null
  sleep 10
}
set_gate() {
  node "sed -i '/^featureGates:/,\$d' /var/lib/kubelet/config.yaml; printf 'featureGates:\n  CSIMigrationFlexVolume: $1\n' >> /var/lib/kubelet/config.yaml"
}
mounts() { node "findmnt -rn -o TARGET | grep /var/lib/kubelet/pods/ | grep -v -E 'kube-api-access|projected'"; }
alive() { # pod name, uid, data dir
  local l; l=$(lines "$3"); sleep 4
  [ "$(uid "$1")" = "$2" ] && [ "$(restarts "$1")" = 0 ] && [ "$(lines "$3")" -gt "$l" ]
}

UID_PV=$(uid writer-pv) UID_IN=$(uid writer-inline)

#######################################################################
step "S1. kubelet restart with migrated volumes mounted (reconstruction)"
PUB=$(csi_calls NodePublishVolume) UNPUB=$(csi_calls NodeUnpublishVolume)
mounts > "$OUT/s1-mounts-before.txt"
restart_kubelet
mounts > "$OUT/s1-mounts-after.txt"
node "journalctl -u kubelet --since '-40s' --no-pager" > "$OUT/s1-kubelet.log"
echo "NodePublish calls: $PUB -> $(csi_calls NodePublishVolume); NodeUnpublish: $UNPUB -> $(csi_calls NodeUnpublishVolume)"
check "same pod mounts after restart" diff "$OUT/s1-mounts-before.txt" "$OUT/s1-mounts-after.txt"
check "no stacked mounts" [ "$(dup_mounts)" -eq 0 ]
check "no NodeUnpublish on restart" [ "$(csi_calls NodeUnpublishVolume)" -eq "$UNPUB" ]
check "PV pod alive and writing" alive writer-pv "$UID_PV" pv-data
check "inline pod alive and writing" alive writer-inline "$UID_IN" inline-data
check "no mount errors in kubelet log" sh -c "! grep -q 'MountVolume failed' '$OUT/s1-kubelet.log'"

#######################################################################
step "S2. inline flexVolume with secretRef (flex requires Secret type == driver name)"
$K -n demo create secret generic flex-secret --type=example.com/fake --from-literal=user=alice --from-literal=password=s3cret --dry-run=client -o yaml | $K apply -f -
sed -e 's/name: writer-inline/name: writer-secret/' -e 's/path: inline-data/path: secret-data/' \
  "$POC/manifests/21-writer-inline.yaml" > "$OUT/s2-writer-secret.yaml"
printf '      secretRef:\n        name: flex-secret\n' >> "$OUT/s2-writer-secret.yaml"
cat "$OUT/s2-writer-secret.yaml"
$K apply -f "$OUT/s2-writer-secret.yaml"
$K -n demo wait --for=condition=Ready pod/writer-secret --timeout=90s
sleep 4
UID_SEC=$(uid writer-secret)
node "grep '$UID_SEC' /var/lib/fake-storage/.fake-csi.log" | tee "$OUT/s2-fake-csi.txt"
check "secret pod mounted through CSI" grep -q NodePublishVolume "$OUT/s2-fake-csi.txt"
check "CSI driver received both secret keys" grep -qE '"secretKeys":"(password,user,|user,password,)"' "$OUT/s2-fake-csi.txt"
check "secret pod writing" [ "$(lines secret-data)" -gt 0 ]

#######################################################################
step "S3. rollback without drain: flex back, gate off, kubelet restart"
node "mkdir -p '$FLEXDIR'"
docker cp "$SRC/test/images/flex-migration-poc/fake-flex/fake" "$NODE:$FLEXDIR/fake"
node "chmod +x '$FLEXDIR/fake'"
FL=$(flex_lines) UNPUB=$(csi_calls NodeUnpublishVolume)
mounts > "$OUT/s3-mounts-before.txt"
set_gate false
restart_kubelet
mounts > "$OUT/s3-mounts-after.txt"
node "tail -n +$((FL + 1)) /var/log/fake-flex.log" | tee "$OUT/s3-new-flex-calls.txt"
echo "NodeUnpublish calls: $UNPUB -> $(csi_calls NodeUnpublishVolume)"
diff "$OUT/s3-mounts-before.txt" "$OUT/s3-mounts-after.txt" | tee "$OUT/s3-mounts.diff"
check "rewritten PV pod untouched by rollback" alive writer-pv "$UID_PV" pv-data
check "inline pods kept running" alive writer-inline "$UID_IN" inline-data
check "inline secret pod kept running" alive writer-secret "$UID_SEC" secret-data
check "both inline volumes remounted by flex (incl. secret)" [ "$(grep -c ' mount ' "$OUT/s3-new-flex-calls.txt")" -eq 2 ]
echo "OBSERVED: rollback unpublishes CSI and remounts flex under running pods"

#######################################################################
step "S4. forward flip without drain: gate on, kubelet restart"
FL=$(flex_lines) PUB=$(csi_calls NodePublishVolume)
set_gate true
restart_kubelet
mounts > "$OUT/s4-mounts-after.txt"
node "tail -n +$((FL + 1)) /var/log/fake-flex.log" | tee "$OUT/s4-new-flex-calls.txt"
echo "NodePublish calls: $PUB -> $(csi_calls NodePublishVolume)"
check "inline pods kept running" alive writer-inline "$UID_IN" inline-data
check "inline secret pod kept running" alive writer-secret "$UID_SEC" secret-data
check "no stacked mounts" [ "$(dup_mounts)" -eq 0 ]
echo "OBSERVED: flex unmounts under running pods: $(grep -c ' unmount ' "$OUT/s4-new-flex-calls.txt")"

node "rm -rf '$FLEXDIR'"
step "RESULT: $PASS passed, $FAIL failed"
echo "proof dir: $OUT"
[ "$FAIL" -eq 0 ]
