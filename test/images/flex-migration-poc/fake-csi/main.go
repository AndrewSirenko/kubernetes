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

// fake-csi is a minimal node-only CSI driver for the FlexVolume -> CSI
// migration proof of concept. It is the CSI replacement for the
// "example.com/fake" FlexVolume driver (test/images/flex-migration-poc/fake-flex):
// both bind-mount the same backing directory, /var/lib/fake-storage/<path>,
// so data written through FlexVolume is visible through CSI.
//
// It registers itself with the kubelet plugin watcher, so it needs no
// node-driver-registrar sidecar.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

var (
	driverName  = flag.String("driver-name", "fake.csi.example.com", "CSI driver name")
	pluginDir   = flag.String("plugin-dir", "/var/lib/kubelet/plugins/fake.csi.example.com", "directory for the CSI socket (host path)")
	registryDir = flag.String("registry-dir", "/var/lib/kubelet/plugins_registry", "kubelet plugin registration directory")
	storageDir  = flag.String("storage-dir", "/var/lib/fake-storage", "backing storage directory (host path)")
	nodeID      = flag.String("node-id", os.Getenv("NODE_NAME"), "node ID")
)

type driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedNodeServer
	registerapi.UnimplementedRegistrationServer
	mu sync.Mutex
}

// audit appends one JSON line per call to <storage-dir>/.fake-csi.log, so the
// PoC can show which driver served each mount.
func (d *driver) audit(op string, fields map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fields["op"] = op
	fields["time"] = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(fields)
	f, err := os.OpenFile(filepath.Join(*storageDir, ".fake-csi.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintln(f, string(b))
		_ = f.Close()
	}
	log.Println(string(b))
}

// Identity

func (d *driver) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: *driverName, VendorVersion: "0.0.1-poc"}, nil
}

func (d *driver) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

func (d *driver) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

// Node

func (d *driver) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: *nodeID}, nil
}

func (d *driver) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

// backingDir returns the backing directory for a volume. It honors the
// FlexVolume-compatible "path" attribute (the option the fake FlexVolume
// driver uses), falling back to the volume handle for native CSI volumes.
func backingDir(req interface {
	GetVolumeId() string
	GetVolumeContext() map[string]string
}) (string, error) {
	p := req.GetVolumeContext()["path"]
	if p == "" {
		p = req.GetVolumeId()
	}
	clean := filepath.Clean("/" + p)
	if clean == "/" {
		return "", status.Error(codes.InvalidArgument, "empty path")
	}
	return filepath.Join(*storageDir, clean), nil
}

// isMountPoint reads mountinfo, because a bind mount from the same filesystem
// has the same st_dev as its parent.
func isMountPoint(p string) bool {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	p = filepath.Clean(p)
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && strings.ReplaceAll(f[4], `\040`, " ") == p {
			return true
		}
	}
	return false
}

func (d *driver) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if req.GetVolumeId() == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and target path are required")
	}
	src, err := backingDir(req)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(src, 0o777); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir %s: %v", src, err)
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir %s: %v", target, err)
	}
	if !isMountPoint(target) {
		if err := syscall.Mount(src, target, "", syscall.MS_BIND, ""); err != nil {
			return nil, status.Errorf(codes.Internal, "bind mount %s -> %s: %v", src, target, err)
		}
		if req.GetReadonly() {
			if err := syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
				return nil, status.Errorf(codes.Internal, "remount ro %s: %v", target, err)
			}
		}
	}
	secretKeys := ""
	for k := range req.GetSecrets() {
		secretKeys += k + ","
	}
	d.audit("NodePublishVolume", map[string]string{
		"volumeID":   req.GetVolumeId(),
		"target":     target,
		"backing":    src,
		"flexDriver": req.GetVolumeContext()["storage.kubernetes.io/flexvolume-driver"],
		"pod":        req.GetVolumeContext()["csi.storage.k8s.io/pod.namespace"] + "/" + req.GetVolumeContext()["csi.storage.k8s.io/pod.name"],
		"secretKeys": secretKeys,
		"readOnly":   fmt.Sprint(req.GetReadonly()),
	})
	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *driver) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if isMountPoint(target) {
		if err := syscall.Unmount(target, 0); err != nil {
			return nil, status.Errorf(codes.Internal, "unmount %s: %v", target, err)
		}
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "remove %s: %v", target, err)
	}
	d.audit("NodeUnpublishVolume", map[string]string{"volumeID": req.GetVolumeId(), "target": target})
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// Kubelet plugin registration

func (d *driver) GetInfo(context.Context, *registerapi.InfoRequest) (*registerapi.PluginInfo, error) {
	return &registerapi.PluginInfo{
		Type:              registerapi.CSIPlugin,
		Name:              *driverName,
		Endpoint:          filepath.Join(*pluginDir, "csi.sock"),
		SupportedVersions: []string{"1.0.0"},
	}, nil
}

func (d *driver) NotifyRegistrationStatus(_ context.Context, s *registerapi.RegistrationStatus) (*registerapi.RegistrationStatusResponse, error) {
	log.Printf("kubelet registration status: registered=%v error=%q", s.PluginRegistered, s.Error)
	return &registerapi.RegistrationStatusResponse{}, nil
}

func listen(path string) net.Listener {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		log.Fatal(err)
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		log.Fatalf("listen %s: %v", path, err)
	}
	return l
}

func main() {
	flag.Parse()
	if *nodeID == "" {
		h, _ := os.Hostname()
		*nodeID = h
	}
	if err := os.MkdirAll(*storageDir, 0o777); err != nil {
		log.Fatal(err)
	}
	d := &driver{}

	csiSrv := grpc.NewServer()
	csi.RegisterIdentityServer(csiSrv, d)
	csi.RegisterNodeServer(csiSrv, d)
	go func() { log.Fatal(csiSrv.Serve(listen(filepath.Join(*pluginDir, "csi.sock")))) }()

	regSrv := grpc.NewServer()
	registerapi.RegisterRegistrationServer(regSrv, d)
	log.Printf("fake-csi %s serving; registering with kubelet", *driverName)
	log.Fatal(regSrv.Serve(listen(filepath.Join(*registryDir, *driverName+"-reg.sock"))))
}
