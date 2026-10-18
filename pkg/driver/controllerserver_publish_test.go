/*
Copyright 2026 Synology Inc.

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
	"strconv"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

// publishFakeDsm is a minimal IDsmService stub scoped to the publish/unpublish
// tests. Only GetVolume is wired; every other method panics so an unexpected
// call fails the test loudly instead of silently returning zero values.
//
// The behaviour of GetVolume is driven by the three function fields: a
// nil getVolume means "not configured" and panics, matching the rest.
type publishFakeDsm struct {
	getVolume   func(volId string) (*models.K8sVolumeRespSpec, error)
	mutateCount int // increments on every DSM mutation (Create*, Delete*, Set*, Map*)
}

func (f *publishFakeDsm) AddDsm(common.ClientInfo) error { panic("unexpected AddDsm") }
func (f *publishFakeDsm) RemoveAllDsms()                 { panic("unexpected RemoveAllDsms") }
func (f *publishFakeDsm) GetDsm(string) (*webapi.DSM, error) {
	panic("unexpected GetDsm")
}
func (f *publishFakeDsm) GetDsmsCount() int { panic("unexpected GetDsmsCount") }
func (f *publishFakeDsm) ListDsmVolumes(string) ([]webapi.VolInfo, error) {
	panic("unexpected ListDsmVolumes")
}
func (f *publishFakeDsm) CreateVolume(*models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error) {
	f.mutateCount++
	return nil, nil
}
func (f *publishFakeDsm) DeleteVolume(string) error {
	f.mutateCount++
	return nil
}
func (f *publishFakeDsm) ListVolumes() ([]*models.K8sVolumeRespSpec, error) {
	panic("unexpected ListVolumes")
}
func (f *publishFakeDsm) GetVolume(volId string) (*models.K8sVolumeRespSpec, error) {
	if f.getVolume == nil {
		panic("unexpected GetVolume")
	}
	return f.getVolume(volId)
}
func (f *publishFakeDsm) ExpandVolume(string, int64) (*models.K8sVolumeRespSpec, error) {
	f.mutateCount++
	return nil, nil
}
func (f *publishFakeDsm) CreateSnapshot(*models.CreateK8sVolumeSnapshotSpec) (*models.K8sSnapshotRespSpec, error) {
	f.mutateCount++
	return nil, nil
}
func (f *publishFakeDsm) DeleteSnapshot(string) error {
	f.mutateCount++
	return nil
}
func (f *publishFakeDsm) ListAllSnapshots() []*models.K8sSnapshotRespSpec { return nil }
func (f *publishFakeDsm) ListSnapshots(string) []*models.K8sSnapshotRespSpec {
	return nil
}
func (f *publishFakeDsm) GetVolumeByName(string) (*models.K8sVolumeRespSpec, error) {
	return nil, nil
}
func (f *publishFakeDsm) GetSnapshotByName(string) *models.K8sSnapshotRespSpec { return nil }
func (f *publishFakeDsm) SetTargetAuth(string, string, string, string) error {
	f.mutateCount++
	return nil
}

// newPublishController builds a controllerServer with only the DSM service
// populated. Publish/Unpublish do not touch Driver, Initiator or the tools
// layer, so leaving them nil is safe for these tests.
func newPublishController(dsm *publishFakeDsm) *controllerServer {
	return &controllerServer{dsmService: dsm}
}

func iscsiVolume(t *testing.T, iqn string, mappingIndex int, sessions []webapi.ConnectedSession) *models.K8sVolumeRespSpec {
	t.Helper()
	return &models.K8sVolumeRespSpec{
		DsmIp:    "10.0.0.1",
		VolumeId: "vol-iscsi-1",
		Protocol: utils.ProtocolIscsi,
		Target: webapi.TargetInfo{
			Iqn:               iqn,
			MappedLuns:        []webapi.MappedLun{{LunUuid: "lun-1", MappingIndex: mappingIndex}},
			ConnectedSessions: sessions,
		},
	}
}

func shareVolume(protocol string) *models.K8sVolumeRespSpec {
	return &models.K8sVolumeRespSpec{
		DsmIp:    "10.0.0.1",
		VolumeId: "vol-share-1",
		Protocol: protocol,
		Share:    webapi.ShareInfo{Name: "share-1"},
	}
}

func nvmeVolume(nqn string, namespaceUuid string) *models.K8sVolumeRespSpec {
	return &models.K8sVolumeRespSpec{
		DsmIp:    "10.0.0.1",
		VolumeId: "vol-nvme-1",
		Protocol: utils.ProtocolNvme,
		Subsystem: webapi.SubsystemInfo{
			Uuid: "subsystem-uuid-1",
			Nqn:  nqn,
		},
		Namespace: webapi.NamespaceInfo{
			Uuid:          namespaceUuid,
			SubsystemUuid: "subsystem-uuid-1",
		},
	}
}

func publishRequest(volumeId, nodeId string, caps []*csi.VolumeCapability) *csi.ControllerPublishVolumeRequest {
	return &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeId,
		NodeId:           nodeId,
		VolumeCapability: caps[0],
	}
}

func oneMountCap() []*csi.VolumeCapability {
	return []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}
}

func codeOf(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	if s, ok := status.FromError(err); ok {
		return s.Code()
	}
	return codes.Unknown
}

// --- ControllerPublishVolume --------------------------------------------------

func TestControllerPublish_InvalidArg_NoVolumeId(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{})
	_, err := cs.ControllerPublishVolume(context.Background(), publishRequest("", "node-1", oneMountCap()))
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestControllerPublish_InvalidArg_NoNodeId(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{})
	_, err := cs.ControllerPublishVolume(context.Background(), publishRequest("vol-1", "", oneMountCap()))
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestControllerPublish_InvalidArg_NoCapabilities(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{})
	_, err := cs.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: "vol-1", NodeId: "node-1",
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestControllerPublish_VolumeNotFound(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) { return nil, nil },
	})
	_, err := cs.ControllerPublishVolume(context.Background(), publishRequest("vol-missing", "node-1", oneMountCap()))
	if codeOf(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestControllerPublish_DsmUnavailable(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return nil, status.Errorf(codes.Unavailable, "dsm down")
		},
	})
	_, err := cs.ControllerPublishVolume(context.Background(), publishRequest("vol-1", "node-1", oneMountCap()))
	if codeOf(err) != codes.Unavailable {
		t.Fatalf("want Unavailable, got %v", err)
	}
}

func TestControllerPublish_ISCSI_HappyPath(t *testing.T) {
	fake := &publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return iscsiVolume(t, "iqn.2021-08.com.synology:csi-k8s-csi-vol-1", 1, nil), nil
		},
	}
	cs := newPublishController(fake)
	resp, err := cs.ControllerPublishVolume(context.Background(), publishRequest("vol-iscsi-1", "node-1", oneMountCap()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		"dsm":          "10.0.0.1",
		"protocol":     utils.ProtocolIscsi,
		"iqn":          "iqn.2021-08.com.synology:csi-k8s-csi-vol-1",
		"mappingIndex": strconv.Itoa(1),
	}
	for k, v := range want {
		if resp.PublishContext[k] != v {
			t.Errorf("PublishContext[%s] = %q, want %q", k, resp.PublishContext[k], v)
		}
	}
	if fake.mutateCount != 0 {
		t.Errorf("publish must mutate DSM, got %d calls", fake.mutateCount)
	}
}

func TestControllerPublish_ISCSI_Idempotent(t *testing.T) {
	fake := &publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return iscsiVolume(t, "iqn.2021-08.com.synology:csi-k8s-csi-vol-1", 1, nil), nil
		},
	}
	cs := newPublishController(fake)
	req := publishRequest("vol-iscsi-1", "node-1", oneMountCap())
	for i := 0; i < 2; i++ {
		resp, err := cs.ControllerPublishVolume(context.Background(), req)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if resp.PublishContext["iqn"] != "iqn.2021-08.com.synology:csi-k8s-csi-vol-1" {
			t.Fatalf("call %d: iqn mismatch", i)
		}
	}
	if fake.mutateCount != 0 {
		t.Errorf("publish must never mutate DSM across idempotent calls, got %d", fake.mutateCount)
	}
}

func TestControllerPublish_ISCSI_CorruptedVolume_NoIqn(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return &models.K8sVolumeRespSpec{
				DsmIp:    "10.0.0.1",
				VolumeId: "vol-bad",
				Protocol: utils.ProtocolIscsi,
				Target: webapi.TargetInfo{
					MappedLuns: []webapi.MappedLun{{LunUuid: "lun-1", MappingIndex: 1}},
				},
			}, nil
		},
	})
	_, err := cs.ControllerPublishVolume(context.Background(), publishRequest("vol-bad", "node-1", oneMountCap()))
	if codeOf(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
}

func TestControllerPublish_ISCSI_CorruptedVolume_NoMappedLuns(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return &models.K8sVolumeRespSpec{
				DsmIp:    "10.0.0.1",
				VolumeId: "vol-bad",
				Protocol: utils.ProtocolIscsi,
				Target:   webapi.TargetInfo{Iqn: "iqn.2021-08.com.synology:csi-k8s-csi-vol-bad"},
			}, nil
		},
	})
	_, err := cs.ControllerPublishVolume(context.Background(), publishRequest("vol-bad", "node-1", oneMountCap()))
	if codeOf(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
}

func TestControllerPublish_NFS_NoOp(t *testing.T) {
	fake := &publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) { return shareVolume(utils.ProtocolNfs), nil },
	}
	cs := newPublishController(fake)
	resp, err := cs.ControllerPublishVolume(context.Background(), publishRequest("vol-share-1", "node-1", oneMountCap()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.PublishContext) != 0 {
		t.Errorf("NFS publish must emit an empty PublishContext, got %v", resp.PublishContext)
	}
	if fake.mutateCount != 0 {
		t.Errorf("NFS publish must not mutate DSM, got %d calls", fake.mutateCount)
	}
}

func TestControllerPublish_SMB_NoOp(t *testing.T) {
	fake := &publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) { return shareVolume(utils.ProtocolSmb), nil },
	}
	cs := newPublishController(fake)
	resp, err := cs.ControllerPublishVolume(context.Background(), publishRequest("vol-share-1", "node-1", oneMountCap()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.PublishContext) != 0 {
		t.Errorf("SMB publish must emit an empty PublishContext, got %v", resp.PublishContext)
	}
}

func TestControllerPublish_NVMe_HappyPath(t *testing.T) {
	fake := &publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return nvmeVolume("nqn.2021-08.com.synology:csi.k8s-csi-vol-1", "ns-uuid-1"), nil
		},
	}
	cs := newPublishController(fake)
	resp, err := cs.ControllerPublishVolume(context.Background(),
		publishRequest("vol-nvme-1", "node-1", oneMountCap()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		"dsm":           "10.0.0.1",
		"protocol":      utils.ProtocolNvme,
		"nqn":           "nqn.2021-08.com.synology:csi.k8s-csi-vol-1",
		"namespaceUuid": "ns-uuid-1",
	}
	for k, v := range want {
		if resp.PublishContext[k] != v {
			t.Errorf("PublishContext[%s] = %q, want %q", k, resp.PublishContext[k], v)
		}
	}
	if fake.mutateCount != 0 {
		t.Errorf("NVMe publish must not mutate DSM, got %d calls", fake.mutateCount)
	}
}

func TestControllerPublish_NVMe_CorruptedVolume_NoNqn(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return nvmeVolume("", "ns-uuid-1"), nil
		},
	})
	_, err := cs.ControllerPublishVolume(context.Background(),
		publishRequest("vol-nvme-1", "node-1", oneMountCap()))
	if codeOf(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
}

func TestControllerPublish_NVMe_CorruptedVolume_NoNamespace(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return nvmeVolume("nqn.2021-08.com.synology:csi.k8s-csi-vol-1", ""), nil
		},
	})
	_, err := cs.ControllerPublishVolume(context.Background(),
		publishRequest("vol-nvme-1", "node-1", oneMountCap()))
	if codeOf(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
}

func TestControllerPublish_NVMe_Idempotent(t *testing.T) {
	fake := &publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return nvmeVolume("nqn.2021-08.com.synology:csi.k8s-csi-vol-1", "ns-uuid-1"), nil
		},
	}
	cs := newPublishController(fake)
	req := publishRequest("vol-nvme-1", "node-1", oneMountCap())
	for i := 0; i < 2; i++ {
		resp, err := cs.ControllerPublishVolume(context.Background(), req)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if resp.PublishContext["nqn"] != "nqn.2021-08.com.synology:csi.k8s-csi-vol-1" {
			t.Fatalf("call %d: nqn mismatch", i)
		}
	}
	if fake.mutateCount != 0 {
		t.Errorf("NVMe publish must never mutate DSM, got %d", fake.mutateCount)
	}
}

// --- ControllerUnpublishVolume ------------------------------------------------

func TestControllerUnpublish_InvalidArg_NoVolumeId(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{})
	_, err := cs.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{NodeId: "node-1"})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestControllerUnpublish_VolumeAlreadyGone(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) { return nil, nil },
	})
	if _, err := cs.ControllerUnpublishVolume(context.Background(),
		&csi.ControllerUnpublishVolumeRequest{VolumeId: "vol-gone", NodeId: "node-1"}); err != nil {
		t.Fatalf("want idempotent OK, got %v", err)
	}
}

func TestControllerUnpublish_DsmUnavailable(t *testing.T) {
	cs := newPublishController(&publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return nil, status.Errorf(codes.Unavailable, "dsm down")
		},
	})
	_, err := cs.ControllerUnpublishVolume(context.Background(),
		&csi.ControllerUnpublishVolumeRequest{VolumeId: "vol-1", NodeId: "node-1"})
	if codeOf(err) != codes.Unavailable {
		t.Fatalf("want Unavailable, got %v", err)
	}
}

func TestControllerUnpublish_ISCSI_NoActiveSession(t *testing.T) {
	fake := &publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return iscsiVolume(t, "iqn.2021-08.com.synology:csi-k8s-csi-vol-1", 1, nil), nil
		},
	}
	cs := newPublishController(fake)
	if _, err := cs.ControllerUnpublishVolume(context.Background(),
		&csi.ControllerUnpublishVolumeRequest{VolumeId: "vol-iscsi-1", NodeId: "node-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.mutateCount != 0 {
		t.Errorf("unpublish must not mutate DSM, got %d calls", fake.mutateCount)
	}
}

func TestControllerUnpublish_ISCSI_SourceStillConnected(t *testing.T) {
	// NodeId is the initiator IQN in this driver (see createMappingTarget in
	// pkg/dsm/service/dsm.go), so ControllerUnpublishVolume matches sessions
	// by IQN, not by kubernetes node name.
	sourceIqn := "iqn.2021-08.com.synology:csi-node-1"
	fake := &publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return iscsiVolume(t, "iqn.2021-08.com.synology:csi-k8s-csi-vol-1", 1,
				[]webapi.ConnectedSession{{Iqn: sourceIqn, Ip: "10.0.0.5"}}), nil
		},
	}
	cs := newPublishController(fake)
	_, err := cs.ControllerUnpublishVolume(context.Background(),
		&csi.ControllerUnpublishVolumeRequest{VolumeId: "vol-iscsi-1", NodeId: sourceIqn})
	if codeOf(err) != codes.Unavailable {
		t.Fatalf("want Unavailable while source still attached, got %v", err)
	}
}

func TestControllerUnpublish_NFS_NoOp(t *testing.T) {
	fake := &publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) { return shareVolume(utils.ProtocolNfs), nil },
	}
	cs := newPublishController(fake)
	if _, err := cs.ControllerUnpublishVolume(context.Background(),
		&csi.ControllerUnpublishVolumeRequest{VolumeId: "vol-share-1", NodeId: "node-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.mutateCount != 0 {
		t.Errorf("NFS unpublish must not mutate DSM, got %d calls", fake.mutateCount)
	}
}

func TestControllerUnpublish_NVMe_NoOp(t *testing.T) {
	// NVMe-oF returns OK unconditionally: the DSM webapi layer does not
	// expose connected-initiator information for subsystems, so no session
	// check is possible. The volume is gone from the CSI perspective, which
	// is the contract the external-attacher needs to finalize.
	fake := &publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return nvmeVolume("nqn.2021-08.com.synology:csi.k8s-csi-vol-1", "ns-uuid-1"), nil
		},
	}
	cs := newPublishController(fake)
	if _, err := cs.ControllerUnpublishVolume(context.Background(),
		&csi.ControllerUnpublishVolumeRequest{VolumeId: "vol-nvme-1", NodeId: "node-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.mutateCount != 0 {
		t.Errorf("NVMe unpublish must not mutate DSM, got %d calls", fake.mutateCount)
	}
}

func TestUnpublish_Before_Publish_Coherent(t *testing.T) {
	// Simulate the external-attacher reconcile ordering: a volume that was
	// published on an earlier run and is now being unpublished. The unpublish
	// must observe the same target state and succeed idempotently.
	fake := &publishFakeDsm{
		getVolume: func(string) (*models.K8sVolumeRespSpec, error) {
			return iscsiVolume(t, "iqn.2021-08.com.synology:csi-k8s-csi-vol-1", 1, nil), nil
		},
	}
	cs := newPublishController(fake)

	pub, err := cs.ControllerPublishVolume(context.Background(), publishRequest("vol-iscsi-1", "node-1", oneMountCap()))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.PublishContext["iqn"] == "" {
		t.Fatalf("publish returned empty iqn")
	}
	if _, err := cs.ControllerUnpublishVolume(context.Background(),
		&csi.ControllerUnpublishVolumeRequest{VolumeId: "vol-iscsi-1", NodeId: "node-1"}); err != nil {
		t.Fatalf("unpublish after publish: %v", err)
	}
	if fake.mutateCount != 0 {
		t.Errorf("neither publish nor unpublish must mutate DSM, got %d calls", fake.mutateCount)
	}
}

// --- Capability ---------------------------------------------------------------

func TestDriver_DeclaresPublishUnpublishCapability(t *testing.T) {
	d := &Driver{}
	d.addControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	})

	found := false
	for _, c := range d.csCap {
		if c.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("PUBLISH_UNPUBLISH_VOLUME not declared in controller capabilities")
	}
}
