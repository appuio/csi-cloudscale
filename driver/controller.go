/*
Copyright cloudscale.ch
Copyright 2018 DigitalOcean

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

package driver

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudscale-ch/cloudscale-go-sdk"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	_  = iota
	KB = 1 << (10 * iota)
	MB
	GB
	TB
)

const (
	// allowed size increments for SSDs
	SSDStepSizeGB = 1

	// allowed size increments for bulk disks
	BulkStepSizeGB = 100

	// PublishInfoVolumeName is used to pass the volume name from
	// `ControllerPublishVolume` to `NodeStageVolume or `NodePublishVolume`
	PublishInfoVolumeName = DriverName + "/volume-name"

	// Storage type of the volume, must be either "ssd" or "bulk"
	StorageTypeAttribute = DriverName + "/volume-type"
)

var (
	// cloudscale.ch currently only support a single node to be attached to a
	// single node in read/write mode. This corresponds to
	// `accessModes.ReadWriteOnce` in a PVC resource on Kubernetes
	supportedAccessMode = &csi.VolumeCapability_AccessMode{
		Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	}

	// maxVolumesPerServerErrorMessage is the error message returned by the cloudscale.ch
	// API when the per-server volume limit would be exceeded.
	maxVolumesPerServerErrorMessageRe = regexp.MustCompile("Due to internal limitations, it is currently not possible to attach more than \\d+ volumes")
)

// CreateVolume creates a new volume from the given request. The function is
// idempotent.
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Name must be provided")
	}

	if req.VolumeCapabilities == nil || len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Volume capabilities must be provided")
	}

	if violations := validateCapabilities(req.VolumeCapabilities); len(violations) > 0 {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("volume capabilities cannot be satisified: %s", strings.Join(violations, "; ")))
	}

	if req.AccessibilityRequirements != nil {
		for _, t := range req.AccessibilityRequirements.Requisite {
			zone, ok := t.Segments["zone"]
			if !ok {
				continue // nothing to do
			}
			if zone != d.zone {
				return nil, status.Errorf(codes.ResourceExhausted, "volume can be only created in zone: %q, got: %q", d.zone, zone)
			}
		}
	}

	storageType := req.Parameters[StorageTypeAttribute]
	if storageType == "" {
		// default storage type unless specified otherwise
		storageType = "ssd"
	}
	if storageType != "ssd" && storageType != "bulk" {
		return nil, status.Error(codes.InvalidArgument, "invalid volume type requested. Only 'ssd' or 'bulk' are supported")
	}

	sizeGB, err := calculateStorageGB(req.CapacityRange, storageType)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	volumeName := req.Name

	luksEncrypted := "false"
	if req.Parameters[LuksEncryptedAttribute] == "true" {
		if violations := validateLuksCapabilities(req.VolumeCapabilities); len(violations) > 0 {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("volume capabilities cannot be satisified: %s", strings.Join(violations, "; ")))
		}
		luksEncrypted = "true"
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_name":             volumeName,
		"storage_size_giga_bytes": sizeGB,
		"method":                  "create_volume",
		"volume_capabilities":     req.VolumeCapabilities,
		"type":                    storageType,
		"luks_encrypted":          luksEncrypted,
	})
	ll.Info("create volume called")

	// get volume first, if it's created do no thing
	volumes, err := d.cloudscaleClient.Volumes.List(ctx, cloudscale.WithNameFilter(volumeName))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	csiVolume := csi.Volume{
		CapacityBytes: int64(sizeGB) * GB,
		AccessibleTopology: []*csi.Topology{
			{
				Segments: map[string]string{
					"zone": d.zone,
				},
			},
		},
		VolumeContext: map[string]string{
			PublishInfoVolumeName:  volumeName,
			LuksEncryptedAttribute: luksEncrypted,
		},
	}

	if luksEncrypted == "true" {
		csiVolume.VolumeContext[LuksCipherAttribute] = req.Parameters[LuksCipherAttribute]
		csiVolume.VolumeContext[LuksKeySizeAttribute] = req.Parameters[LuksKeySizeAttribute]
	}

	// volume already exist, do nothing
	if len(volumes) != 0 {
		if len(volumes) > 1 {
			return nil, fmt.Errorf("fatal issue: duplicate volume %q exists", volumeName)
		}
		vol := volumes[0]

		if vol.SizeGB != sizeGB {
			return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("invalid option requested size: %d", sizeGB))
		}

		ll.Info("volume already created")
		csiVolume.VolumeId = vol.UUID
		return &csi.CreateVolumeResponse{Volume: &csiVolume}, nil
	}

	volumeReq := &cloudscale.VolumeRequest{
		Name:   volumeName,
		SizeGB: sizeGB,
		Type:   storageType,
	}
	volumeReq.Zone = d.zone

	ll.WithField("volume_req", volumeReq).Info("creating volume")
	vol, err := d.cloudscaleClient.Volumes.Create(ctx, volumeReq)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	csiVolume.VolumeId = vol.UUID
	resp := &csi.CreateVolumeResponse{Volume: &csiVolume}

	ll.WithField("response", resp).Info("volume created")
	return resp, nil
}

// DeleteVolume deletes the given volume. The function is idempotent.
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "DeleteVolume Volume ID must be provided")
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"method":    "delete_volume",
	})
	ll.Info("delete volume called")

	err := d.cloudscaleClient.Volumes.Delete(ctx, req.VolumeId)
	if err != nil {
		errorResponse, ok := err.(*cloudscale.ErrorResponse)
		if ok {
			if errorResponse.StatusCode == http.StatusNotFound {
				// To make it idempotent, the volume might already have been
				// deleted, so a 404 is ok.
				ll.WithFields(logrus.Fields{
					"error": err,
					"resp":  errorResponse,
				}).Warn("assuming volume is already deleted")
				return &csi.DeleteVolumeResponse{}, nil
			}
		}
		return nil, err
	}

	ll.Info("volume is deleted")
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume attaches the given volume to the node
func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume ID must be provided")
	}

	if req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Node ID must be provided")
	}

	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume capability must be provided")
	}

	if req.Readonly {
		// TODO(arslan): we should return codes.InvalidArgument, but the CSI
		// test fails, because according to the CSI Spec, this flag cannot be
		// changed on the same volume. However we don't use this flag at all,
		// as there are no `readonly` attachable volumes.
		return nil, status.Error(codes.AlreadyExists, "read only Volumes are not supported")
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"node_id":   req.NodeId,
		"method":    "controller_publish_volume",
	})
	ll.Info("controller publish volume called")

	attachRequest := &cloudscale.VolumeRequest{
		ServerUUIDs: &[]string{req.NodeId},
	}
	err := d.cloudscaleClient.Volumes.Update(ctx, req.VolumeId, attachRequest)
	if err != nil {
		if maxVolumesPerServerErrorMessageRe.MatchString(err.Error()) {
			return nil, status.Errorf(codes.ResourceExhausted, err.Error())
		}

		return nil, reraiseNotFound(err, ll, "attaching volume")
	}

	ll.Info("volume is attached")
	volume, err := d.cloudscaleClient.Volumes.Get(ctx, req.VolumeId)
	if err != nil {
		return nil, reraiseNotFound(err, ll, "fetch volume")
	}
	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{
			PublishInfoVolumeName:  volume.Name,
			LuksEncryptedAttribute: req.VolumeContext[LuksEncryptedAttribute],
			LuksCipherAttribute:    req.VolumeContext[LuksCipherAttribute],
			LuksKeySizeAttribute:   req.VolumeContext[LuksKeySizeAttribute],
		},
	}, nil
}

// ControllerUnpublishVolume deattaches the given volume from the node
func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume ID must be provided")
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"node_id":   req.NodeId,
		"method":    "controller_unpublish_volume",
	})
	ll.Info("controller unpublish volume called")

	// check if volume exist before trying to detach it
	_, err := d.cloudscaleClient.Volumes.Get(ctx, req.VolumeId)
	if err != nil {
		errorResponse, ok := err.(*cloudscale.ErrorResponse)
		if ok {
			if errorResponse.StatusCode == http.StatusNotFound {
				ll.Info("assuming volume is detached because it does not exist")
				return &csi.ControllerUnpublishVolumeResponse{}, nil
			}
		}
		return nil, err
	}

	detachRequest := &cloudscale.VolumeRequest{
		ServerUUIDs: &[]string{},
	}
	err = d.cloudscaleClient.Volumes.Update(ctx, req.VolumeId, detachRequest)
	if err != nil {
		return nil, reraiseNotFound(err, ll, "unpublish volume")
	}

	ll.Info("volume is detached")
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities checks whether the volume capabilities requested
// are supported.
func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume ID must be provided")
	}

	if req.VolumeCapabilities == nil {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities Volume Capabilities must be provided")
	}

	ll := d.log.WithFields(logrus.Fields{
		"volume_id":              req.VolumeId,
		"volume_capabilities":    req.VolumeCapabilities,
		"supported_capabilities": supportedAccessMode,
		"method":                 "validate_volume_capabilities",
	})
	ll.Info("validate volume capabilities called")

	// check if volume exist before trying to validate it it
	_, err := d.cloudscaleClient.Volumes.Get(ctx, req.VolumeId)
	if err != nil {
		return nil, reraiseNotFound(err, ll, "fetch volume to validate capabilities")
	}

	// if it's not supported (i.e: wrong region), we shouldn't override it
	resp := &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: []*csi.VolumeCapability{
				{
					AccessMode: supportedAccessMode,
				},
			},
		},
	}

	ll.WithField("confirmed", resp.Confirmed).Info("supported capabilities")
	return resp, nil
}

// ListVolumes returns a list of all requested volumes
func (d *Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	if req.StartingToken != "" {
		// StartingToken is for pagination, which we don't use, but csi-test checks it
		//  see also: https://github.com/kubernetes-csi/csi-test/issues/222

		// According to spec:
		//    Caller SHOULD start the ListVolumes operation again with an empty starting_token.
		// when sending aborted code see https://github.com/container-storage-interface/spec/blob/master/spec.md
		return nil, status.Errorf(codes.Aborted, "pagination not supported")
	}

	ll := d.log.WithFields(logrus.Fields{
		"req_starting_token": req.StartingToken,
		"method":             "list_volumes",
	})
	ll.Info("list volumes called")

	volumes, err := d.cloudscaleClient.Volumes.List(ctx)
	if err != nil {
		return nil, err
	}

	var entries []*csi.ListVolumesResponse_Entry
	for _, vol := range volumes {
		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId:      vol.UUID,
				CapacityBytes: int64(vol.SizeGB * GB),
			},
		})
	}

	resp := &csi.ListVolumesResponse{
		Entries: entries,
	}

	ll.WithField("response", resp).Info("volumes listed")
	return resp, nil
}

// GetCapacity returns the capacity of the storage pool
func (d *Driver) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	// TODO(arslan): check if we can provide this information somehow
	d.log.WithFields(logrus.Fields{
		"params": req.Parameters,
		"method": "get_capacity",
	}).Warn("get capacity is not implemented")
	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerGetCapabilities returns the capabilities of the controller service.
func (d *Driver) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	newCap := func(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	// TODO(arslan): checkout if the capabilities are worth supporting
	var caps []*csi.ControllerServiceCapability
	for _, capability := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,

		// TODO(arslan): enable once snapshotting is supported
		// csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		// csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,

		// TODO: check if this can be implemented
		// csi.ControllerServiceCapability_RPC_GET_CAPACITY,
	} {
		caps = append(caps, newCap(capability))
	}

	resp := &csi.ControllerGetCapabilitiesResponse{
		Capabilities: caps,
	}

	d.log.WithFields(logrus.Fields{
		"response": resp,
		"method":   "controller_get_capabilities",
	}).Info("controller get capabilities called")
	return resp, nil
}

// CreateSnapshot will be called by the CO to create a new snapshot from a
// source volume on behalf of a user.
func (d *Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	d.log.WithFields(logrus.Fields{
		"req":    req,
		"method": "create_snapshot",
	}).Warn("create snapshot is not implemented")
	return nil, status.Error(codes.Unimplemented, "")
}

// DeleteSnapshost will be called by the CO to delete a snapshot.
func (d *Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	d.log.WithFields(logrus.Fields{
		"req":    req,
		"method": "delete_snapshot",
	}).Warn("delete snapshot is not implemented")
	return nil, status.Error(codes.Unimplemented, "")
}

// ListSnapshots returns the information about all snapshots on the storage
// system within the given parameters regardless of how they were created.
// ListSnapshots shold not list a snapshot that is being created but has not
// been cut successfully yet.
func (d *Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	d.log.WithFields(logrus.Fields{
		"req":    req,
		"method": "list_snapshots",
	}).Warn("list snapshots is not implemented")
	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerExpandVolume is called from the resizer to increase the volume size.
func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	volID := req.GetVolumeId()

	if len(volID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerExpandVolume volume ID missing in request")
	}
	volume, err := d.cloudscaleClient.Volumes.Get(ctx, volID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ControllerExpandVolume could not retrieve existing volume: %v", err)
	}

	resizeGigaBytes, err := calculateStorageGB(req.GetCapacityRange(), volume.Type)
	if err != nil {
		return nil, status.Errorf(codes.OutOfRange, "ControllerExpandVolume invalid capacity range: %v", err)
	}

	log := d.log.WithFields(logrus.Fields{
		"volume_id": req.VolumeId,
		"method":    "controller_expand_volume",
	})

	log.Info("controller expand volume called")

	if resizeGigaBytes <= volume.SizeGB {
		log.WithFields(logrus.Fields{
			"current_volume_size":   volume.SizeGB,
			"requested_volume_size": resizeGigaBytes,
		}).Info("skipping volume resize because current volume size exceeds requested volume size")
		// even if the volume is resized independently from the control panel, we still need to resize the node fs when resize is requested
		// in this case, the claim capacity will be resized to the volume capacity, requested capcity will be ignored to make the PV and PVC capacities consistent
		return &csi.ControllerExpandVolumeResponse{CapacityBytes: int64(volume.SizeGB) * GB, NodeExpansionRequired: true}, nil
	}

	volumeReq := &cloudscale.VolumeRequest{
		SizeGB: resizeGigaBytes,
	}
	err = d.cloudscaleClient.Volumes.Update(ctx, volume.UUID, volumeReq)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cannot resize volume %s: %s", req.GetVolumeId(), err.Error())
	}

	log = log.WithField("new_volume_size", resizeGigaBytes)
	log.Info("volume was resized")

	nodeExpansionRequired := true
	if req.GetVolumeCapability() != nil {
		switch req.GetVolumeCapability().GetAccessType().(type) {
		case *csi.VolumeCapability_Block:
			log.Info("node expansion is not required for block volumes")
			nodeExpansionRequired = false
		}
	}

	return &csi.ControllerExpandVolumeResponse{CapacityBytes: int64(resizeGigaBytes) * GB, NodeExpansionRequired: nodeExpansionRequired}, nil
}

// ControllerGetVolume gets a specific volume.
// The call is used for the CSI health check feature
// (https://github.com/kubernetes/enhancements/pull/1077) which we do not
// support yet.
func (d *Driver) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// calculateStorageGB extracts the storage size in GB from the given capacity
// range. If the capacity range is not satisfied it returns the default volume
// size.
func calculateStorageGB(capRange *csi.CapacityRange, storageType string) (int, error) {
	sizeIncrements := SSDStepSizeGB
	if storageType == "bulk" {
		sizeIncrements = BulkStepSizeGB
	}
	if capRange == nil {
		return sizeIncrements, nil
	}

	// Volume MUST be at least this big. This field is OPTIONAL.
	// A value of 0 is equal to an unspecified field value.
	// The value of this field MUST NOT be negative.
	requiredBytes := capRange.GetRequiredBytes()
	requiredSet := 0 < requiredBytes

	// Volume MUST not be bigger than this. This field is OPTIONAL.
	// A value of 0 is equal to an unspecified field value.
	// The value of this field MUST NOT be negative.
	limitBytes := capRange.GetLimitBytes()
	limitSet := 0 < limitBytes

	if !requiredSet && !limitSet {
		return sizeIncrements, nil
	}
	if requiredSet && limitSet && limitBytes < requiredBytes {
		return 0, fmt.Errorf("limit (%v) can not be less than required (%v) size", formatBytes(limitBytes), formatBytes(requiredBytes))
	}

	if limitSet && limitBytes < (int64(sizeIncrements)*GB) {
		return 0, fmt.Errorf("limit (%v) can not be less than minimum supported volume size for type '%s' (%v)", formatBytes(limitBytes), storageType, formatBytes(int64(sizeIncrements)*GB))
	}

	steps := requiredBytes / GB / int64(sizeIncrements)
	if steps*GB*int64(sizeIncrements) < requiredBytes {
		steps += 1
	}

	sizeGB := steps * int64(sizeIncrements)

	if limitSet && limitBytes < (int64(sizeGB)*GB) {
		return 0, fmt.Errorf("for required (%v) limit (%v) must be at least %v for type '%s'", formatBytes(requiredBytes), formatBytes(limitBytes), formatBytes(int64(sizeGB)*GB), storageType)
	}
	return int(sizeGB), nil
}

func formatBytes(inputBytes int64) string {
	output := float64(inputBytes)
	unit := ""

	switch {
	case inputBytes >= TB:
		output = output / TB
		unit = "Ti"
	case inputBytes >= GB:
		output = output / GB
		unit = "Gi"
	case inputBytes >= MB:
		output = output / MB
		unit = "Mi"
	case inputBytes >= KB:
		output = output / KB
		unit = "Ki"
	case inputBytes == 0:
		return "0"
	}

	result := strconv.FormatFloat(output, 'f', 1, 64)
	result = strings.TrimSuffix(result, ".0")
	return result + unit
}

// validateCapabilities validates the requested capabilities. It returns a list
// of violations which may be empty if no violatons were found.
func validateCapabilities(caps []*csi.VolumeCapability) []string {
	violations := sets.NewString()
	for _, cap := range caps {
		if cap.GetAccessMode().GetMode() != supportedAccessMode.GetMode() {
			violations.Insert(fmt.Sprintf("unsupported access mode %s", cap.GetAccessMode().GetMode().String()))
		}

		accessType := cap.GetAccessType()
		switch accessType.(type) {
		case *csi.VolumeCapability_Block:
		case *csi.VolumeCapability_Mount:
		default:
			violations.Insert("unsupported access type")
		}
	}

	return violations.List()
}

func validateLuksCapabilities(caps []*csi.VolumeCapability) []string {
	violations := sets.NewString()
	for _, cap := range caps {
		accessType := cap.GetAccessType()
		switch accessType.(type) {
		case *csi.VolumeCapability_Block:
			violations.Insert("Cannot use LUKS with block volumes")
		case *csi.VolumeCapability_Mount:
		}
	}
	return violations.List()
}

func reraiseNotFound(err error, log *logrus.Entry, operation string) error {
	errorResponse, ok := err.(*cloudscale.ErrorResponse)
	if ok {
		lt := log.WithFields(logrus.Fields{
			"error":         err,
			"errorResponse": errorResponse,
		})
		if errorResponse.StatusCode == http.StatusNotFound {
			lt.Warnf("%q: Server or volume not found", operation)
			return status.Errorf(codes.NotFound, err.Error())
		} else {
			lt.Warnf("%q: operation failed", operation)
			return status.Errorf(codes.Aborted, operation+": Request failed")
		}
	}
	log.Warnf("%q: random error", operation)
	return status.Errorf(codes.Aborted, operation+": Random error")
}
