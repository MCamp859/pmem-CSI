/*
Copyright 2017 The Kubernetes Authors.

SPDX-License-Identifier: Apache-2.0
*/

package pmemcsidriver

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"github.com/google/uuid"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi/v0"

	"github.com/intel/pmem-csi/pkg/ndctl"
	"github.com/intel/pmem-csi/pkg/pmem-common"
	"github.com/intel/pmem-csi/pkg/pmem-grpc"
)

const (
	deviceID = "deviceID"
	// LV mode in emulated case: if LV Group named nvdimm exists, we use Lvolumes instead of libndctl
	// to achieve stable emulated env. LV storage is set up outside of this driver
	//lvgroup  = "ndbus0region0"
)

//VolumeStatus type representation for volume status
type VolumeStatus int

const (
	//Created volume created
	Created VolumeStatus = iota + 1
	//Attached volume attached on a node
	Attached
	//Unattached volume dettached on a node
	Unattached
	//Deleted volume deleted
	Deleted
)

type pmemVolume struct {
	ID     string
	Name   string
	Size   uint64
	Status VolumeStatus
	NodeID string
}

type controllerServer struct {
	*DefaultControllerServer
	mode              DriverMode
	rs                *registryServer
	ctx               *ndctl.Context
	pmemVolumes       map[string]pmemVolume //Controller: map of reqID:VolumeInfo
	publishVolumeInfo map[string]string     //Node: map of reqID:VolumeName
}

var _ csi.ControllerServer = &controllerServer{}

func NewControllerServer(driver *CSIDriver, mode DriverMode, rs *registryServer, ctx *ndctl.Context) csi.ControllerServer {
	return &controllerServer{
		DefaultControllerServer: NewDefaultControllerServer(driver),
		mode:              mode,
		rs:                rs,
		ctx:               ctx,
		pmemVolumes:       map[string]pmemVolume{},
		publishVolumeInfo: map[string]string{},
	}
}

func (cs *controllerServer) GetVolumeByID(volumeID string) *pmemVolume {
	if pmemVol, ok := cs.pmemVolumes[volumeID]; ok {
		return &pmemVol
	}
	return nil
}

func (cs *controllerServer) GetVolumeByName(Name string) *pmemVolume {
	for _, pmemVol := range cs.pmemVolumes {
		if pmemVol.Name == Name {
			return &pmemVol
		}
	}
	return nil
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	var vol *pmemVolume
	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		pmemcommon.Infof(3, ctx, "invalid create volume req: %v", req)
		return nil, err
	}

	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities missing in request")
	}

	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}

	asked := uint64(req.GetCapacityRange().GetRequiredBytes())

	glog.Infof("CreateVolume: Name: %v, Size: %v", req.Name, asked)
	if vol = cs.GetVolumeByName(req.Name); vol != nil {
		// Check if the size of exisiting volume new can cover the new request
		glog.Infof("CreateVolume: Vol %s exists, Size: %v", vol.Name, vol.Size)
		if vol.Size < asked {
			return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("Volume with the same name: %s but with different size already exist", vol.Name))
		}
	} else {
		id, _ := uuid.NewUUID() //nolint: gosec
		volumeID := id.String()
		vol = &pmemVolume{
			ID:     volumeID,
			Name:   req.GetName(),
			Size:   asked,
			Status: Created,
		}
		cs.pmemVolumes[volumeID] = *vol
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			Id:            vol.ID,
			CapacityBytes: int64(vol.Size),
			Attributes: map[string]string{
				"name": req.GetName(),
				"size": strconv.FormatUint(asked, 10),
			},
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		pmemcommon.Infof(3, ctx, "invalid delete volume req: %v", req)
		return nil, err
	}
	pmemcommon.Infof(4, ctx, "DeleteVolume: volumeID: %v", req.GetVolumeId())
	vol := cs.GetVolumeByID(req.GetVolumeId())
	if vol == nil {
		pmemcommon.Infof(3, ctx, "Volume %s not created by this controller", req.GetVolumeId())
	} else {
		if vol.Status != Unattached {
			pmemcommon.Infof(3, ctx, "Volume %s is attached to %s but not dittached", vol.Name, vol.NodeID)
		}
		delete(cs.pmemVolumes, vol.ID)
	}

	pmemcommon.Infof(4, ctx, "DeleteVolume: volume deleted %s", req.GetVolumeId())

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing in request")
	}

	vol := cs.GetVolumeByID(req.GetVolumeId())
	if vol == nil {
		return nil, status.Error(codes.NotFound, "Volume not created by this controller")
	}
	for _, cap := range req.VolumeCapabilities {
		if cap.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return &csi.ValidateVolumeCapabilitiesResponse{Supported: false, Message: ""}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Supported: true, Message: ""}, nil
}

func (cs *controllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	pmemcommon.Infof(3, ctx, "ListVolumes")
	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_LIST_VOLUMES); err != nil {
		pmemcommon.Infof(3, ctx, "invalid list volumes req: %v", req)
		return nil, err
	}
	// List namespaces
	var entries []*csi.ListVolumesResponse_Entry
	for _, vol := range cs.pmemVolumes {
		info := &csi.Volume{
			Id:            vol.ID,
			CapacityBytes: int64(vol.Size),
			Attributes: map[string]string{
				"name": vol.Name,
				"size": strconv.FormatUint(vol.Size, 10),
			},
		}
		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume:               info,
			XXX_NoUnkeyedLiteral: *new(struct{}),
			XXX_unrecognized:     nil,
			XXX_sizecache:        0,
		})
	}

	response := &csi.ListVolumesResponse{
		Entries:              entries,
		NextToken:            "",
		XXX_NoUnkeyedLiteral: *new(struct{}),
	}
	return response, nil
}

func (cs *controllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume ID must be provided")
	}

	if req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Node ID must be provided")
	}

	glog.Infof("Node: %s ControllerPublishVolume : volume_id: %s, node_id: %s ", cs.Driver.nodeID, req.VolumeId, req.NodeId)

	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume capability must be provided")
	}

	attrs := req.GetVolumeAttributes()
	if attrs == nil {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume attribultes must be provided")
	}

	if cs.mode == Controller {
		node, err := cs.rs.GetNodeController(req.NodeId)
		if err != nil {
			return nil, status.Error(codes.NotFound, err.Error())
		}

		conn, err := pmemgrpc.Connect(node.Endpoint, connectionTimeout)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		defer conn.Close()

		glog.Infof("Getting New Controller Client ....")
		csiClient := csi.NewControllerClient(conn)
		glog.Infof("Iniitiating Publishing volume ....")

		resp, err := csiClient.ControllerPublishVolume(ctx, req)
		glog.Infof("Got response")

		if err == nil {
			if vol, ok := cs.pmemVolumes[req.VolumeId]; ok {
				vol.Status = Attached
			}
		}
		return resp, err
	}

	/* Node/Unified */
	size, err := strconv.ParseUint(attrs["size"], 10, 64)
	if err != nil {
		return nil, err
	}

	if err := cs.createVolume(attrs["name"], size); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to create volume: %s", err.Error())
	}

	cs.publishVolumeInfo[req.VolumeId] = attrs["name"]

	return &csi.ControllerPublishVolumeResponse{
		PublishInfo: attrs,
	}, nil
}

func (cs *controllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume Volume ID must be provided")
	}

	glog.Infof("ControllerUnpublishVolume : volume_id: %s, node_id: %s ", req.VolumeId, req.NodeId)

	if cs.mode == Controller {
		node, err := cs.rs.GetNodeController(req.NodeId)
		if err != nil {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		conn, errr := pmemgrpc.Connect(node.Endpoint, connectionTimeout)
		if errr != nil {
			return nil, status.Error(codes.Internal, errr.Error())
		}

		csiClient := csi.NewControllerClient(conn)
		resp, err := csiClient.ControllerUnpublishVolume(ctx, req)
		if err == nil {
			if vol, ok := cs.pmemVolumes[req.GetVolumeId()]; ok {
				vol.Status = Unattached
			}
		}

		return resp, err
	}

	/* Node */

	name := cs.publishVolumeInfo[req.GetVolumeId()]
	if err := cs.deleteVolume(name); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to delete volume: %s", err.Error())
	}

	delete(cs.publishVolumeInfo, req.GetVolumeId())

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (cs *controllerServer) listVolumes() (map[string]pmemVolume, error) {
	volumes := map[string]pmemVolume{}
	if lvmode() == true {
		output, err := exec.Command("lvs", "--noheadings", "--nosuffix", "--options", "lv_name,lv_size", "--units", "B").CombinedOutput()
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "lvs failed"+string(output))
		}
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			fields := strings.Split(strings.TrimSpace(line), " ")
			if len(fields) == 2 {
				size, _ := strconv.ParseUint(fields[1], 10, 64) //nolint: gosec
				vol := pmemVolume{
					ID:     fields[0],
					Size:   size,
					Status: Attached,
					NodeID: cs.Driver.nodeID,
				}
				volumes[vol.ID] = vol
			}
		}
	} else {
		nss := cs.ctx.GetActiveNamespaces()

		for _, ns := range nss {
			data, _ := json.MarshalIndent(ns, "", " ")
			glog.Info("Namespace:", string(data[:]))
			//glog.Infof("namespace BlockDevName: %v, Size: %v", ns.BlockDeviceName(), ns.Size())
			vol := pmemVolume{
				ID:     ns.Name(),
				Name:   ns.Name(),
				Size:   ns.Size(),
				Status: Attached,
				NodeID: cs.Driver.nodeID,
			}
			volumes[vol.ID] = vol
		}
	}

	return volumes, nil
}

func (cs *controllerServer) createVolume(name string, size uint64) error {
	glog.Infof("Creating volume/namespace '%s' with size '%v", name, size)

	if lvmode() == true {
		// pick a region, few possible strategies:
		// 1. pick first with enough available space: simplest, regions get filled in order;
		// 2. pick first with largest available space: regions get used round-robin, i.e. load-balanced, but does not leave large unused;
		// 3. pick first with smallest available which satisfies the request: ordered initially, but later leaves bigger free available;
		// Let's implement strategy 1 for now, simplest to code as no need to compare sizes in all regions
		// NOTE: We walk buses and regions in ndctl context, but avail.size we check in LV context
		for _, bus := range cs.ctx.GetBuses() {
			glog.Infof("CreateVolume: Bus: %v", bus.DeviceName())
			for _, r := range bus.ActiveRegions() {
				glog.Infof("CreateVolume: Region: %v", r.DeviceName())
				vgName := vgName(bus, r)
				glog.Infof("CreateVolume: vgName: %v", vgName)
				output, err := exec.Command("vgs", "--noheadings", "--nosuffix", "--options", "vg_free", "--units", "B", vgName).CombinedOutput()
				if err != nil {
					return err
				}
				vgAvailStr := strings.TrimSpace(string(output))
				vgAvail, _ := strconv.ParseUint(vgAvailStr, 10, 64)
				glog.Infof("CreateVolume: vgAvail in %v: [%v]", vgName, vgAvail)
				if vgAvail >= size {
					// lvcreate takes size in MBytes if no unit
					sizeM := int(size / (1024 * 1024))
					sz := strconv.Itoa(sizeM)
					output, err := exec.Command("lvcreate", "-L", sz, "-n", name, vgName).CombinedOutput()
					glog.Infof("lvcreate output: %s\n", string(output))
					if err != nil {
						glog.Infof("CreateVolume: lvcreate failed: %v", string(output))
					} else {
						glog.Infof("CreateVolume: LVol %v with size=%v MB created", name, sz)
					}
					// return in all cases, otherwise loop will create LVs in other regions
					return err
				} else {
					glog.Infof("This volime size %v is not having enough space required(%v)", vgAvail, size)
				}
			}
		}
	} else {
		ns, err := cs.ctx.CreateNamespace(ndctl.CreateNamespaceOpts{
			Name: name,
			Size: size,
		})
		if err != nil {
			return err
		}
		data, _ := ns.MarshalJSON() //nolint: gosec
		glog.Infof("Namespace crated: %v", data)
	}

	return nil
}

func (cs *controllerServer) deleteVolume(name string) error {

	if lvmode() {
		lvpath, err := lvPath(name)
		if err != nil {
			return err
		}
		var output []byte
		glog.Infof("DeleteVolume: Matching LVpath: %v", lvpath)
		output, err = exec.Command("lvremove", "-fy", lvpath).CombinedOutput()
		glog.Infof("lvremove output: %s\n", string(output))
		return err
	} else {
		return cs.ctx.DestroyNamespaceByName(name)
	}

	return nil
}

// Return device path for based on LV name
func lvPath(volumeID string) (string, error) {
	output, err := exec.Command("lvs", "--noheadings", "--options", "lv_name,lv_path").CombinedOutput()
	if err != nil {
		return "", status.Error(codes.InvalidArgument, "lvs failed"+string(output))
	}
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		// we have a line like this here: [nspace1 /dev/ndbus0region1/nspace1]
		glog.Infof("lvPath: Line from lvs: [%v]", line)
		fields := strings.Fields(line)
		if len(fields) == 2 {
			if volumeID == fields[0] {
				return fields[1], nil
			}
		}
	}
	return "", status.Error(codes.InvalidArgument, "no such volume")
}

func vgName(bus *ndctl.Bus, region *ndctl.Region) string {
	return bus.DeviceName() + region.DeviceName()
}
