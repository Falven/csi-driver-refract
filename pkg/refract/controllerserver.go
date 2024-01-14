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
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"

	"github.com/golang/glog"
	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/pborman/uuid"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/falven/csi-driver-refract/pkg/state"
)

const (
	deviceID = "deviceID"
)

func (rf *refract) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (resp *csi.CreateVolumeResponse, finalErr error) {
	if err := rf.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid create volume req: %v", req)
		return nil, err
	}

	if len(req.GetMutableParameters()) > 0 {
		if err := rf.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_MODIFY_VOLUME); err != nil {
			glog.V(3).Infof("invalid create volume req: %v", req)
			return nil, err
		}
		// Check if the mutable parameters are in the accepted list
		if err := rf.validateVolumeMutableParameters(req.MutableParameters); err != nil {
			return nil, err
		}
	}

	// Check arguments
	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}
	caps := req.GetVolumeCapabilities()
	if caps == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities missing in request")
	}

	// Keep a record of the requested access types.
	var accessTypeMount bool

	for _, cap := range caps {
		if cap.GetBlock() != nil {
			return nil, status.Error(codes.InvalidArgument, "block access type is not supported")
		}
		if cap.GetMount() != nil {
			accessTypeMount = true
		}
	}
	// A real driver would also need to check that the other
	// fields in VolumeCapabilities are sane. The check above is
	// just enough to pass the "[Testpattern: Dynamic PV (block
	// volmode)] volumeMode should fail in binding dynamic
	// provisioned PV to PVC" storage E2E test.

	if !accessTypeMount {
		return nil, status.Error(codes.InvalidArgument, "mount access type is required")
	}

	var requestedAccessType state.AccessType = state.MountAccess

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	capacity := int64(req.GetCapacityRange().GetRequiredBytes())
	topologies := []*csi.Topology{}
	if rf.config.EnableTopology {
		topologies = append(topologies, &csi.Topology{Segments: map[string]string{TopologyKeyNode: rf.config.NodeID}})
	}

	// Need to check for already existing volume name, and if found
	// check for the requested capacity and already allocated capacity
	if exVol, err := rf.state.GetVolumeByName(req.GetName()); err == nil {
		// Since err is nil, it means the volume with the same name already exists
		// need to check if the size of existing volume is the same as in new
		// request
		if exVol.VolSize < capacity {
			return nil, status.Errorf(codes.AlreadyExists, "Volume with the same name: %s but with different size already exist", req.GetName())
		}
		if req.GetVolumeContentSource() != nil {
			volumeSource := req.VolumeContentSource
			switch volumeSource.Type.(type) {
			case *csi.VolumeContentSource_Snapshot:
				if volumeSource.GetSnapshot() != nil && exVol.ParentSnapID != "" && exVol.ParentSnapID != volumeSource.GetSnapshot().GetSnapshotId() {
					return nil, status.Error(codes.AlreadyExists, "existing volume source snapshot id not matching")
				}
			case *csi.VolumeContentSource_Volume:
				if volumeSource.GetVolume() != nil && exVol.ParentVolID != volumeSource.GetVolume().GetVolumeId() {
					return nil, status.Error(codes.AlreadyExists, "existing volume source volume id not matching")
				}
			default:
				return nil, status.Errorf(codes.InvalidArgument, "%v not a proper volume source", volumeSource)
			}
		}
		// TODO (sbezverk) Do I need to make sure that volume still exists?
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:           exVol.VolID,
				CapacityBytes:      int64(exVol.VolSize),
				VolumeContext:      req.GetParameters(),
				ContentSource:      req.GetVolumeContentSource(),
				AccessibleTopology: topologies,
			},
		}, nil
	}

	volumeID := uuid.NewUUID().String()
	kind := req.GetParameters()[storageKind]
	vol, err := rf.createVolume(volumeID, req.GetName(), capacity, requestedAccessType, false /* ephemeral */, kind)
	if err != nil {
		return nil, err
	}
	glog.V(4).Infof("created volume %s at path %s", vol.VolID, vol.VolPath)

	if req.GetVolumeContentSource() != nil {
		path := rf.getVolumePath(volumeID)
		volumeSource := req.VolumeContentSource
		switch volumeSource.Type.(type) {
		case *csi.VolumeContentSource_Snapshot:
			if snapshot := volumeSource.GetSnapshot(); snapshot != nil {
				err = rf.loadFromSnapshot(capacity, snapshot.GetSnapshotId(), path, requestedAccessType)
				vol.ParentSnapID = snapshot.GetSnapshotId()
			}
		case *csi.VolumeContentSource_Volume:
			if srcVolume := volumeSource.GetVolume(); srcVolume != nil {
				err = rf.loadFromVolume(capacity, srcVolume.GetVolumeId(), path, requestedAccessType)
				vol.ParentVolID = srcVolume.GetVolumeId()
			}
		default:
			err = status.Errorf(codes.InvalidArgument, "%v not a proper volume source", volumeSource)
		}
		if err != nil {
			glog.V(4).Infof("VolumeSource error: %v", err)
			if delErr := rf.deleteVolume(volumeID); delErr != nil {
				glog.V(2).Infof("deleting hostpath volume %v failed: %v", volumeID, delErr)
			}
			return nil, err
		}
		glog.V(4).Infof("successfully populated volume %s", vol.VolID)
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:           volumeID,
			CapacityBytes:      req.GetCapacityRange().GetRequiredBytes(),
			VolumeContext:      req.GetParameters(),
			ContentSource:      req.GetVolumeContentSource(),
			AccessibleTopology: topologies,
		},
	}, nil
}

func (rf *refract) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if err := rf.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid delete volume req: %v", req)
		return nil, err
	}

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	volId := req.GetVolumeId()
	vol, err := rf.state.GetVolumeByID(volId)
	if err != nil {
		// Volume not found: might have already deleted
		return &csi.DeleteVolumeResponse{}, nil
	}

	if vol.Attached || !vol.Published.Empty() || !vol.Staged.Empty() {
		msg := fmt.Sprintf("Volume '%s' is still used (attached: %v, staged: %v, published: %v) by '%s' node",
			vol.VolID, vol.Attached, vol.Staged, vol.Published, vol.NodeID)
		if rf.config.CheckVolumeLifecycle {
			return nil, status.Error(codes.Internal, msg)
		}
		klog.Warning(msg)
	}

	if err := rf.deleteVolume(volId); err != nil {
		return nil, fmt.Errorf("failed to delete volume %v: %w", volId, err)
	}
	glog.V(4).Infof("volume %v successfully deleted", volId)

	return &csi.DeleteVolumeResponse{}, nil
}

func (rf *refract) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: rf.getControllerServiceCapabilities(),
	}, nil
}

func (rf *refract) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}
	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, req.VolumeId)
	}

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	if _, err := rf.state.GetVolumeByID(req.GetVolumeId()); err != nil {
		return nil, err
	}

	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetBlock() != nil {
			// Block storage is not supported
			return nil, status.Error(codes.InvalidArgument, "Block access type is not supported")
		}

		if cap.GetMount() == nil {
			// Mount access type is required
			return nil, status.Error(codes.InvalidArgument, "Mount access type is required")
		}

		// A real driver would check the capabilities of the given volume with
		// the set of requested capabilities.
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeContext:      req.GetVolumeContext(),
			VolumeCapabilities: req.GetVolumeCapabilities(),
			Parameters:         req.GetParameters(),
		},
	}, nil
}

func (rf *refract) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if !rf.config.EnableAttach {
		return nil, status.Error(codes.Unimplemented, "ControllerPublishVolume is not supported")
	}

	if len(req.VolumeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}
	if len(req.NodeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Node ID cannot be empty")
	}
	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities cannot be empty")
	}

	if req.NodeId != rf.config.NodeID {
		return nil, status.Errorf(codes.NotFound, "Not matching Node ID %s to hostpath Node ID %s", req.NodeId, rf.config.NodeID)
	}

	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	vol, err := rf.state.GetVolumeByID(req.VolumeId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}

	// Check to see if the volume is already published.
	if vol.Attached {
		// Check if readonly flag is compatible with the publish request.
		if req.GetReadonly() != vol.ReadOnlyAttach {
			return nil, status.Error(codes.AlreadyExists, "Volume published but has incompatible readonly flag")
		}

		return &csi.ControllerPublishVolumeResponse{
			PublishContext: map[string]string{},
		}, nil
	}

	// Check attach limit before publishing.
	if rf.config.AttachLimit > 0 && rf.getAttachCount() >= rf.config.AttachLimit {
		return nil, status.Errorf(codes.ResourceExhausted, "Cannot attach any more volumes to this node ('%s')", rf.config.NodeID)
	}

	vol.Attached = true
	vol.ReadOnlyAttach = req.GetReadonly()
	if err := rf.state.UpdateVolume(vol); err != nil {
		return nil, err
	}

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{},
	}, nil
}

func (rf *refract) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if !rf.config.EnableAttach {
		return nil, status.Error(codes.Unimplemented, "ControllerUnpublishVolume is not supported")
	}

	if len(req.VolumeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}

	// Empty node id is not a failure as per Spec
	if req.NodeId != "" && req.NodeId != rf.config.NodeID {
		return nil, status.Errorf(codes.NotFound, "Node ID %s does not match to expected Node ID %s", req.NodeId, rf.config.NodeID)
	}

	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	vol, err := rf.state.GetVolumeByID(req.VolumeId)
	if err != nil {
		// Not an error: a non-existent volume is not published.
		// See also https://github.com/kubernetes-csi/external-attacher/pull/165
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	// Check to see if the volume is staged/published on a node
	if !vol.Published.Empty() || !vol.Staged.Empty() {
		msg := fmt.Sprintf("Volume '%s' is still used (staged: %v, published: %v) by '%s' node",
			vol.VolID, vol.Staged, vol.Published, vol.NodeID)
		if rf.config.CheckVolumeLifecycle {
			return nil, status.Error(codes.Internal, msg)
		}
		klog.Warning(msg)
	}

	vol.Attached = false
	if err := rf.state.UpdateVolume(vol); err != nil {
		return nil, status.Errorf(codes.Internal, "could not update volume %s: %v", vol.VolID, err)
	}

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (rf *refract) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	// Topology and capabilities are irrelevant. We only
	// distinguish based on the "kind" parameter, if at all.
	// Without configured capacity, we just have the maximum size.
	available := rf.config.MaxVolumeSize
	if rf.config.Capacity.Enabled() {
		// Empty "kind" will return "zero capacity". There is no fallback
		// to some arbitrary kind here because in practice it always should
		// be set.
		kind := req.GetParameters()[storageKind]
		quantity := rf.config.Capacity[kind]
		allocated := rf.sumVolumeSizes(kind)
		available = quantity.Value() - allocated
	}
	maxVolumeSize := rf.config.MaxVolumeSize
	if maxVolumeSize > available {
		maxVolumeSize = available
	}

	return &csi.GetCapacityResponse{
		AvailableCapacity: available,
		MaximumVolumeSize: &wrappers.Int64Value{Value: maxVolumeSize},

		// We don't have a minimum volume size, so we might as well report that.
		// Better explicit than implicit...
		MinimumVolumeSize: &wrappers.Int64Value{Value: 0},
	}, nil
}

func (rf *refract) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	volumeRes := &csi.ListVolumesResponse{
		Entries: []*csi.ListVolumesResponse_Entry{},
	}

	var (
		startIdx, volumesLength, maxLength int64
		rfVolume                           state.Volume
	)

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	// Sort by volume ID.
	volumes := rf.state.GetVolumes()
	sort.Slice(volumes, func(i, j int) bool {
		return volumes[i].VolID < volumes[j].VolID
	})

	if req.StartingToken == "" {
		req.StartingToken = "1"
	}

	startIdx, err := strconv.ParseInt(req.StartingToken, 10, 32)
	if err != nil {
		return nil, status.Error(codes.Aborted, "The type of startingToken should be integer")
	}

	volumesLength = int64(len(volumes))
	maxLength = int64(req.MaxEntries)

	if maxLength > volumesLength || maxLength <= 0 {
		maxLength = volumesLength
	}

	for index := startIdx - 1; index < volumesLength && index < maxLength; index++ {
		rfVolume = volumes[index]
		healthy, msg := rf.doHealthCheckInControllerSide(rfVolume.VolID)
		glog.V(3).Infof("Healthy state: %s Volume: %t", rfVolume.VolName, healthy)
		volumeRes.Entries = append(volumeRes.Entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId:      rfVolume.VolID,
				CapacityBytes: rfVolume.VolSize,
			},
			Status: &csi.ListVolumesResponse_VolumeStatus{
				PublishedNodeIds: []string{rfVolume.NodeID},
				VolumeCondition: &csi.VolumeCondition{
					Abnormal: !healthy,
					Message:  msg,
				},
			},
		})
	}

	glog.V(5).Infof("Volumes are: %+v", *volumeRes)
	return volumeRes, nil
}

func (rf *refract) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	volume, err := rf.state.GetVolumeByID(req.GetVolumeId())
	if err != nil {
		// ControllerGetVolume should report abnormal volume condition if volume is not found
		return &csi.ControllerGetVolumeResponse{
			Volume: &csi.Volume{
				VolumeId: req.GetVolumeId(),
			},
			Status: &csi.ControllerGetVolumeResponse_VolumeStatus{
				VolumeCondition: &csi.VolumeCondition{
					Abnormal: true,
					Message:  err.Error(),
				},
			},
		}, nil
	}

	healthy, msg := rf.doHealthCheckInControllerSide(req.GetVolumeId())
	glog.V(3).Infof("Healthy state: %s Volume: %t", volume.VolName, healthy)
	return &csi.ControllerGetVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volume.VolID,
			CapacityBytes: volume.VolSize,
		},
		Status: &csi.ControllerGetVolumeResponse_VolumeStatus{
			PublishedNodeIds: []string{volume.NodeID},
			VolumeCondition: &csi.VolumeCondition{
				Abnormal: !healthy,
				Message:  msg,
			},
		},
	}, nil
}

func (rf *refract) ControllerModifyVolume(ctx context.Context, req *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	if err := rf.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_MODIFY_VOLUME); err != nil {
		return nil, err
	}

	// Check arguments
	if len(req.VolumeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}
	if len(req.MutableParameters) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Mutable parameters cannot be empty")
	}

	// Check if the mutable parameters are in the accepted list
	if err := rf.validateVolumeMutableParameters(req.MutableParameters); err != nil {
		return nil, err
	}

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	// Get the volume
	_, err := rf.state.GetVolumeByID(req.VolumeId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}

	return &csi.ControllerModifyVolumeResponse{}, nil
}

// CreateSnapshot uses tar command to create snapshot for hostpath volume. The tar command can quickly create
// archives of entire directories. The host image must have "tar" binaries in /bin, /usr/sbin, or /usr/bin.
func (rf *refract) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	if err := rf.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT); err != nil {
		glog.V(3).Infof("invalid create snapshot req: %v", req)
		return nil, err
	}

	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}
	// Check arguments
	if len(req.GetSourceVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "SourceVolumeId missing in request")
	}

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	// Need to check for already existing snapshot name, and if found check for the
	// requested sourceVolumeId and sourceVolumeId of snapshot that has been created.
	if exSnap, err := rf.state.GetSnapshotByName(req.GetName()); err == nil {
		// Since err is nil, it means the snapshot with the same name already exists need
		// to check if the sourceVolumeId of existing snapshot is the same as in new request.
		if exSnap.VolID == req.GetSourceVolumeId() {
			// same snapshot has been created.
			return &csi.CreateSnapshotResponse{
				Snapshot: &csi.Snapshot{
					SnapshotId:     exSnap.Id,
					SourceVolumeId: exSnap.VolID,
					CreationTime:   exSnap.CreationTime,
					SizeBytes:      exSnap.SizeBytes,
					ReadyToUse:     exSnap.ReadyToUse,
				},
			}, nil
		}
		return nil, status.Errorf(codes.AlreadyExists, "snapshot with the same name: %s but with different SourceVolumeId already exist", req.GetName())
	}

	volumeID := req.GetSourceVolumeId()
	hostPathVolume, err := rf.state.GetVolumeByID(volumeID)
	if err != nil {
		return nil, err
	}

	snapshotID := uuid.NewUUID().String()
	creationTime := timestamppb.Now()
	file := rf.getSnapshotPath(snapshotID)

	if err := rf.createSnapshotFromVolume(hostPathVolume, file); err != nil {
		return nil, err
	}

	glog.V(4).Infof("create volume snapshot %s", file)
	snapshot := state.Snapshot{}
	snapshot.Name = req.GetName()
	snapshot.Id = snapshotID
	snapshot.VolID = volumeID
	snapshot.Path = file
	snapshot.CreationTime = creationTime
	snapshot.SizeBytes = hostPathVolume.VolSize
	snapshot.ReadyToUse = true

	if err := rf.state.UpdateSnapshot(snapshot); err != nil {
		return nil, err
	}
	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SnapshotId:     snapshot.Id,
			SourceVolumeId: snapshot.VolID,
			CreationTime:   snapshot.CreationTime,
			SizeBytes:      snapshot.SizeBytes,
			ReadyToUse:     snapshot.ReadyToUse,
		},
	}, nil
}

func (rf *refract) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	// Check arguments
	if len(req.GetSnapshotId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Snapshot ID missing in request")
	}

	if err := rf.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT); err != nil {
		glog.V(3).Infof("invalid delete snapshot req: %v", req)
		return nil, err
	}
	snapshotID := req.GetSnapshotId()

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	// If the snapshot has a GroupSnapshotID, deletion is not allowed and should return InvalidArgument.
	if snapshot, err := rf.state.GetSnapshotByID(snapshotID); err != nil && snapshot.GroupSnapshotID != "" {
		return nil, status.Errorf(codes.InvalidArgument, "Snapshot with ID %s is part of groupsnapshot %s", snapshotID, snapshot.GroupSnapshotID)
	}

	glog.V(4).Infof("deleting snapshot %s", snapshotID)
	path := rf.getSnapshotPath(snapshotID)
	os.RemoveAll(path)
	if err := rf.state.DeleteSnapshot(snapshotID); err != nil {
		return nil, err
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

func (rf *refract) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	if err := rf.validateControllerServiceRequest(csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS); err != nil {
		glog.V(3).Infof("invalid list snapshot req: %v", req)
		return nil, err
	}

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	// case 1: SnapshotId is not empty, return snapshots that match the snapshot id,
	// none if not found.
	if len(req.GetSnapshotId()) != 0 {
		snapshotID := req.SnapshotId
		if snapshot, err := rf.state.GetSnapshotByID(snapshotID); err == nil {
			return convertSnapshot(snapshot), nil
		}
		return &csi.ListSnapshotsResponse{}, nil
	}

	// case 2: SourceVolumeId is not empty, return snapshots that match the source volume id,
	// none if not found.
	if len(req.GetSourceVolumeId()) != 0 {
		for _, snapshot := range rf.state.GetSnapshots() {
			if snapshot.VolID == req.SourceVolumeId {
				return convertSnapshot(snapshot), nil
			}
		}
		return &csi.ListSnapshotsResponse{}, nil
	}

	var snapshots []csi.Snapshot
	// case 3: no parameter is set, so we return all the snapshots.
	rfSnapshots := rf.state.GetSnapshots()
	sort.Slice(rfSnapshots, func(i, j int) bool {
		return rfSnapshots[i].Id < rfSnapshots[j].Id
	})

	for _, snap := range rfSnapshots {
		snapshot := csi.Snapshot{
			SnapshotId:      snap.Id,
			SourceVolumeId:  snap.VolID,
			CreationTime:    snap.CreationTime,
			SizeBytes:       snap.SizeBytes,
			ReadyToUse:      snap.ReadyToUse,
			GroupSnapshotId: snap.GroupSnapshotID,
		}
		snapshots = append(snapshots, snapshot)
	}

	var (
		ulenSnapshots = int32(len(snapshots))
		maxEntries    = req.MaxEntries
		startingToken int32
		maxToken      = uint32(math.MaxUint32)
	)

	if v := req.StartingToken; v != "" {
		i, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return nil, status.Errorf(
				codes.Aborted,
				"startingToken=%d !< int32=%d",
				startingToken, maxToken)
		}
		startingToken = int32(i)
	}

	if startingToken > ulenSnapshots {
		return nil, status.Errorf(
			codes.Aborted,
			"startingToken=%d > len(snapshots)=%d",
			startingToken, ulenSnapshots)
	}

	// Discern the number of remaining entries.
	rem := ulenSnapshots - startingToken

	// If maxEntries is 0 or greater than the number of remaining entries then
	// set maxEntries to the number of remaining entries.
	if maxEntries == 0 || maxEntries > rem {
		maxEntries = rem
	}

	var (
		i       int
		j       = startingToken
		entries = make(
			[]*csi.ListSnapshotsResponse_Entry,
			maxEntries)
	)

	for i = 0; i < len(entries); i++ {
		entries[i] = &csi.ListSnapshotsResponse_Entry{
			Snapshot: &snapshots[j],
		}
		j++
	}

	var nextToken string
	if j < ulenSnapshots {
		nextToken = fmt.Sprintf("%d", j)
	}

	return &csi.ListSnapshotsResponse{
		Entries:   entries,
		NextToken: nextToken,
	}, nil
}

func (rf *refract) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	if !rf.config.EnableVolumeExpansion {
		return nil, status.Error(codes.Unimplemented, "ControllerExpandVolume is not supported")
	}

	volID := req.GetVolumeId()
	if len(volID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	capRange := req.GetCapacityRange()
	if capRange == nil {
		return nil, status.Error(codes.InvalidArgument, "Capacity range not provided")
	}

	capacity := int64(capRange.GetRequiredBytes())
	if capacity > rf.config.MaxVolumeSize {
		return nil, status.Errorf(codes.OutOfRange, "Requested capacity %d exceeds maximum allowed %d", capacity, rf.config.MaxVolumeSize)
	}

	// Lock before acting on global state. A production-quality
	// driver might use more fine-grained locking.
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	exVol, err := rf.state.GetVolumeByID(volID)
	if err != nil {
		return nil, err
	}

	if exVol.VolSize < capacity {
		exVol.VolSize = capacity
		if err := rf.state.UpdateVolume(exVol); err != nil {
			return nil, err
		}
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         exVol.VolSize,
		NodeExpansionRequired: true,
	}, nil
}

func convertSnapshot(snap state.Snapshot) *csi.ListSnapshotsResponse {
	entries := []*csi.ListSnapshotsResponse_Entry{
		{
			Snapshot: &csi.Snapshot{
				SnapshotId:      snap.Id,
				SourceVolumeId:  snap.VolID,
				CreationTime:    snap.CreationTime,
				SizeBytes:       snap.SizeBytes,
				ReadyToUse:      snap.ReadyToUse,
				GroupSnapshotId: snap.GroupSnapshotID,
			},
		},
	}

	rsp := &csi.ListSnapshotsResponse{
		Entries: entries,
	}

	return rsp
}

// validateVolumeMutableParameters is a helper function to check if the mutable parameters are in the accepted list
func (rf *refract) validateVolumeMutableParameters(params map[string]string) error {
	if len(rf.config.AcceptedMutableParameterNames) == 0 {
		return nil
	}

	accepts := sets.New(rf.config.AcceptedMutableParameterNames...)
	unsupported := []string{}
	for k := range params {
		if !accepts.Has(k) {
			unsupported = append(unsupported, k)
		}
	}
	if len(unsupported) > 0 {
		return status.Errorf(codes.InvalidArgument, "invalid parameters: %v", unsupported)
	}
	return nil
}

func (rf *refract) validateControllerServiceRequest(c csi.ControllerServiceCapability_RPC_Type) error {
	if c == csi.ControllerServiceCapability_RPC_UNKNOWN {
		return nil
	}

	for _, cap := range rf.getControllerServiceCapabilities() {
		if c == cap.GetRpc().GetType() {
			return nil
		}
	}
	return status.Errorf(codes.InvalidArgument, "unsupported capability %s", c)
}

func (rf *refract) getControllerServiceCapabilities() []*csi.ControllerServiceCapability {
	var cl []csi.ControllerServiceCapability_RPC_Type
	if !rf.config.Ephemeral {
		cl = []csi.ControllerServiceCapability_RPC_Type{
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
			csi.ControllerServiceCapability_RPC_GET_VOLUME,
			csi.ControllerServiceCapability_RPC_GET_CAPACITY,
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
			csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
			csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
			csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
			csi.ControllerServiceCapability_RPC_VOLUME_CONDITION,
			csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
		}
		if rf.config.EnableVolumeExpansion && !rf.config.DisableControllerExpansion {
			cl = append(cl, csi.ControllerServiceCapability_RPC_EXPAND_VOLUME)
		}
		if rf.config.EnableAttach {
			cl = append(cl, csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME)
		}
		if rf.config.EnableControllerModifyVolume {
			cl = append(cl, csi.ControllerServiceCapability_RPC_MODIFY_VOLUME)
		}
	}

	var csc []*csi.ControllerServiceCapability

	for _, cap := range cl {
		csc = append(csc, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		})
	}

	return csc
}
