/*
Copyright 2017 The Kubernetes Authors.

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

package refract

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/golang/glog"
	"github.com/hanwen/go-fuse/v2/fuse"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/resource"
	utilexec "k8s.io/utils/exec"

	"github.com/falven/csi-driver-refract/pkg/state"
)

const (
	kib    int64 = 1024
	mib    int64 = kib * 1024
	gib    int64 = mib * 1024
	gib100 int64 = gib * 100
	tib    int64 = gib * 1024
	tib100 int64 = tib * 100

	// storageKind is the special parameter which requests
	// storage of a certain kind (only affects capacity checks).
	storageKind = "kind"
)

type refract struct {
	config Config

	// gRPC calls involving any of the fields below must be serialized
	// by locking this mutex before starting. Internal helper
	// functions assume that the mutex has been locked.
	mutex sync.Mutex
	state state.State

	// Map to keep track of server instances per volume
	fuseServers map[string]*fuse.Server
}

type Config struct {
	DriverName                    string
	Endpoint                      string
	ProxyEndpoint                 string
	NodeID                        string
	VendorVersion                 string
	StateDir                      string
	RootDir                       string
	MaxVolumesPerNode             int64
	MaxVolumeSize                 int64
	AttachLimit                   int64
	Capacity                      Capacity
	Ephemeral                     bool
	ShowVersion                   bool
	EnableAttach                  bool
	EnableTopology                bool
	EnableVolumeExpansion         bool
	EnableControllerModifyVolume  bool
	AcceptedMutableParameterNames StringArray
	DisableControllerExpansion    bool
	DisableNodeExpansion          bool
	MaxVolumeExpansionSizeNode    int64
	CheckVolumeLifecycle          bool
}

var (
	vendorVersion = "dev"
)

const (
	// Extension with which snapshot files will be saved.
	snapshotExt = ".snap"
)

func NewRefractDriver(cfg Config) (*refract, error) {
	if cfg.DriverName == "" {
		return nil, errors.New("no driver name provided")
	}

	if cfg.NodeID == "" {
		return nil, errors.New("no node id provided")
	}

	if cfg.Endpoint == "" {
		return nil, errors.New("no driver endpoint provided")
	}

	if err := os.MkdirAll(cfg.StateDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create dataRoot: %v", err)
	}

	glog.Infof("Driver: %v ", cfg.DriverName)
	glog.Infof("Version: %s", cfg.VendorVersion)

	s, err := state.New(path.Join(cfg.StateDir, "state.json"))
	if err != nil {
		return nil, err
	}

	rf := &refract{
		config: cfg,
		state:  s,
	}

	return rf, nil
}

func (rf *refract) Run() error {
	s := NewNonBlockingGRPCServer()
	// rf itself implements ControllerServer, NodeServer, and IdentityServer.
	s.Start(rf.config.Endpoint, rf, rf, rf, rf)
	s.Wait()

	return nil
}

// getVolumePath returns the canonical path for refract volume
func (rf *refract) getVolumePath(volID string) string {
	return filepath.Join(rf.config.StateDir, volID)
}

// getSnapshotPath returns the full path to where the snapshot is stored
func (rf *refract) getSnapshotPath(snapshotID string) string {
	return filepath.Join(rf.config.StateDir, fmt.Sprintf("%s%s", snapshotID, snapshotExt))
}

// createVolume allocates capacity, creates the directory for the refract volume, and
// adds the volume to the list.
//
// It returns the volume path or err if one occurs. That error is suitable as result of a gRPC call.
func (rf *refract) createVolume(volID, name string, cap int64, volAccessType state.AccessType, ephemeral bool, kind string, rootDir string) (*state.Volume, error) {
	// Check for maximum available capacity
	if cap > rf.config.MaxVolumeSize {
		return nil, status.Errorf(codes.OutOfRange, "Requested capacity %d exceeds maximum allowed %d", cap, rf.config.MaxVolumeSize)
	}
	if rf.config.Capacity.Enabled() {
		if kind == "" {
			// Pick some kind with sufficient remaining capacity.
			for k, c := range rf.config.Capacity {
				if rf.sumVolumeSizes(k)+cap <= c.Value() {
					kind = k
					break
				}
			}
		}
		if kind == "" {
			// Still nothing?!
			return nil, status.Errorf(codes.ResourceExhausted, "requested capacity %d of arbitrary storage exceeds all remaining capacity", cap)
		}
		used := rf.sumVolumeSizes(kind)
		available := rf.config.Capacity[kind]
		if used+cap > available.Value() {
			return nil, status.Errorf(codes.ResourceExhausted, "requested capacity %d exceeds remaining capacity for %q, %s out of %s already used",
				cap, kind, resource.NewQuantity(used, resource.BinarySI).String(), available.String())
		}
	} else if kind != "" {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("capacity tracking disabled, specifying kind %q is invalid", kind))
	}

	if rootDir != "" {
		glog.V(4).Infof("changing rootDir from %v to %v", rf.config.RootDir, rootDir)
		rf.config.RootDir = rootDir
	}
	path := rf.getVolumePath(volID)

	switch volAccessType {
	case state.MountAccess:
		err := os.MkdirAll(path, 0777)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported access type %v", volAccessType)
	}

	volume := state.Volume{
		VolID:         volID,
		VolName:       name,
		VolSize:       cap,
		VolPath:       path,
		VolAccessType: volAccessType,
		Ephemeral:     ephemeral,
		Kind:          kind,
	}
	glog.V(4).Infof("adding hostpath volume: %s = %+v", volID, volume)
	if err := rf.state.UpdateVolume(volume); err != nil {
		return nil, err
	}
	return &volume, nil
}

// deleteVolume deletes the directory for the refract volume.
func (rf *refract) deleteVolume(volID string) error {
	glog.V(4).Infof("starting to delete hostpath volume: %s", volID)

	vol, err := rf.state.GetVolumeByID(volID)
	if err != nil {
		// Return OK if the volume is not found.
		return nil
	}

	path := rf.getVolumePath(volID)
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := rf.state.DeleteVolume(volID); err != nil {
		return err
	}
	glog.V(4).Infof("deleted hostpath volume: %s = %+v", volID, vol)
	return nil
}

func (rf *refract) sumVolumeSizes(kind string) (sum int64) {
	for _, volume := range rf.state.GetVolumes() {
		if volume.Kind == kind {
			sum += volume.VolSize
		}
	}
	return
}

// refractIsEmpty is a simple check to determine if the specified refract directory
// is empty or not.
func refractIsEmpty(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return true, fmt.Errorf("unable to open hostpath volume, error: %v", err)
	}
	defer f.Close()

	_, err = f.Readdir(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err
}

// loadFromSnapshot populates the given destPath with data from the snapshotID
func (rf *refract) loadFromSnapshot(size int64, snapshotId, destPath string, mode state.AccessType) error {
	snapshot, err := rf.state.GetSnapshotByID(snapshotId)
	if err != nil {
		return err
	}
	if !snapshot.ReadyToUse {
		return fmt.Errorf("snapshot %v is not yet ready to use", snapshotId)
	}
	if snapshot.SizeBytes > size {
		return status.Errorf(codes.InvalidArgument, "snapshot %v size %v is greater than requested volume size %v", snapshotId, snapshot.SizeBytes, size)
	}
	snapshotPath := snapshot.Path

	var cmd []string
	switch mode {
	case state.MountAccess:
		cmd = []string{"tar", "zxvf", snapshotPath, "-C", destPath}
	default:
		return status.Errorf(codes.InvalidArgument, "unknown accessType: %d", mode)
	}

	executor := utilexec.New()
	glog.V(4).Infof("Command Start: %v", cmd)
	out, err := executor.Command(cmd[0], cmd[1:]...).CombinedOutput()
	glog.V(4).Infof("Command Finish: %v", string(out))
	if err != nil {
		return fmt.Errorf("failed pre-populate data from snapshot %v: %w: %s", snapshotId, err, out)
	}
	return nil
}

// loadFromVolume populates the given destPath with data from the srcVolumeID
func (rf *refract) loadFromVolume(size int64, srcVolumeId, destPath string, mode state.AccessType) error {
	refractVolume, err := rf.state.GetVolumeByID(srcVolumeId)
	if err != nil {
		return err
	}
	if refractVolume.VolSize > size {
		return status.Errorf(codes.InvalidArgument, "volume %v size %v is greater than requested volume size %v", srcVolumeId, refractVolume.VolSize, size)
	}
	if mode != refractVolume.VolAccessType {
		return status.Errorf(codes.InvalidArgument, "volume %v mode is not compatible with requested mode", srcVolumeId)
	}

	switch mode {
	case state.MountAccess:
		return loadFromFilesystemVolume(refractVolume, destPath)
	default:
		return status.Errorf(codes.InvalidArgument, "unknown accessType: %d", mode)
	}
}

func loadFromFilesystemVolume(refractVolume state.Volume, destPath string) error {
	srcPath := refractVolume.VolPath
	isEmpty, err := refractIsEmpty(srcPath)
	if err != nil {
		return fmt.Errorf("failed verification check of source hostpath volume %v: %w", refractVolume.VolID, err)
	}

	// If the source refract volume is empty it's a noop and we just move along, otherwise the cp call will fail with a a file stat error DNE
	if !isEmpty {
		args := []string{"-a", srcPath + "/.", destPath + "/"}
		executor := utilexec.New()
		out, err := executor.Command("cp", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed pre-populate data from volume %v: %s: %w", refractVolume.VolID, out, err)
		}
	}
	return nil
}

func (rf *refract) getAttachCount() int64 {
	count := int64(0)
	for _, vol := range rf.state.GetVolumes() {
		if vol.Attached {
			count++
		}
	}
	return count
}

func (rf *refract) createSnapshotFromVolume(vol state.Volume, file string) error {
	var cmd []string
	glog.V(4).Infof("Creating snapshot of Filsystem Mode Volume")
	cmd = []string{"tar", "czf", file, "-C", vol.VolPath, "."}
	executor := utilexec.New()
	out, err := executor.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed create snapshot: %w: %s", err, out)
	}

	return nil
}
