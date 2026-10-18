/*
 * Copyright 2025 Synology Inc.
 */

package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

// failingDsm is a dsmService whose every lookup fails: it stands for an
// unreachable DSM. A filesystem-volume publish must never need it -- identity
// was verified at stage time -- so NodePublishVolume succeeding with this
// stub proves the publish path no longer re-logs-in or re-resolves devices.
type failingDsm struct {
	interfaces.IDsmService
}

func (failingDsm) GetVolume(string) (*models.K8sVolumeRespSpec, error) {
	return nil, errors.New("dsm unreachable")
}

func iscsiMountPublishRequest(staging, target string) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		VolumeId:          "8029e1d2-8ea8-4e8a-ae5f-e4b9404885fa",
		StagingTargetPath: staging,
		TargetPath:        target,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"}},
		},
		VolumeContext: map[string]string{"protocol": utils.ProtocolIscsi, "dsm": "10.10.146.6"},
	}
}

// NodePublishVolume must bind the staging path for filesystem volumes and
// never re-login or re-resolve devices: after a node migration the second
// device resolution races /dev/disk/by-* creation (no udev on Talos) and has
// been observed handing a pod the node's local partition.
func TestPublishBindsStagingWithoutTouchingDsm(t *testing.T) {
	withFakeMountDetection(t)
	base := t.TempDir()
	staging := filepath.Join(base, "staging")
	target := filepath.Join(base, "target")
	for _, d := range []string{staging, target} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	fm := mount.NewFakeMounter([]mount.MountPoint{{Device: "/dev/sdb", Path: staging}}) // staged LUN

	ns := &nodeServer{
		Mounter:    &mount.SafeFormatAndMount{Interface: fm},
		dsmService: failingDsm{},
	}

	if _, err := ns.NodePublishVolume(context.Background(), iscsiMountPublishRequest(staging, target)); err != nil {
		t.Fatalf("filesystem publish must bind staging without DSM/iSCSI access, got: %v", err)
	}

	for _, mp := range fm.MountPoints {
		if mp.Path == target && utils.SliceContains(mp.Opts, "bind") {
			return // the bind from staging happened
		}
	}
	t.Fatalf("publish did not bind staging %s onto %s; mounted: %v", staging, target, fm.MountPoints)
}

// A successful Unmount peels exactly one layer of a stacked bind. Publishing
// over an existing mount stacks layers, so unpublish must keep peeling and
// must never report success while any layer is still alive -- that lie is what
// left iSCSI sessions logged in and VolumeAttachments orphaned after
// migrations (kubelet retries unpublish forever and never reaches unstage).
// withFakeMountDetection points the isStillMounted seam at the fake mount
// table: the production default reads /proc/mounts, which knows nothing about
// the FakeMounter's simulated layers.
func withFakeMountDetection(t *testing.T) {
	t.Helper()
	orig := isStillMounted
	// IsNotMountPoint answers "is this NOT a mount point": the seam wants the
	// opposite.
	isStillMounted = func(m mount.Interface, p string) (bool, error) {
		notMounted, err := mount.IsNotMountPoint(m, p)
		return !notMounted, err
	}
	t.Cleanup(func() { isStillMounted = orig })
}

func TestUnpublishPeelsStackedMountsAndRefusesWhileMounted(t *testing.T) {
	withFakeMountDetection(t)
	target := filepath.Join(t.TempDir(), "mount")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(target); err != nil {
		t.Fatal(err)
	} else {
		target = real
	}

	fm := mount.NewFakeMounter([]mount.MountPoint{
		{Device: "/dev/sdb", Path: target},
		{Device: "/dev/sdb", Path: target},
	})
	ns := &nodeServer{Mounter: &mount.SafeFormatAndMount{Interface: fm}}

	if _, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "8029e1d2-8ea8-4e8a-ae5f-e4b9404885fa",
		TargetPath: target,
	}); err != nil {
		t.Fatalf("unpublish should peel the stacked mounts, got: %v", err)
	}

	if len(fm.MountPoints) != 0 {
		t.Fatalf("target still carries %d mount layer(s) after a successful unpublish", len(fm.MountPoints))
	}
}

// The lie this file exists for: an unmount that claims success while the path
// is still mounted must surface as an error, so kubelet retries instead of
// moving on to unstage with the session still alive.
func TestUnpublishReportsStillMountedInsteadOfLying(t *testing.T) {
	withFakeMountDetection(t)
	target := filepath.Join(t.TempDir(), "mount")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(target); err != nil {
		t.Fatal(err)
	} else {
		target = real
	}

	fm := mount.NewFakeMounter([]mount.MountPoint{{Device: "/dev/sdb", Path: target}})
	// a mounter whose Unmount claims success but never clears the layer
	ns := &nodeServer{Mounter: &mount.SafeFormatAndMount{Interface: &stickyMounter{Interface: fm}}}

	if _, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "8029e1d2-8ea8-4e8a-ae5f-e4b9404885fa",
		TargetPath: target,
	}); err == nil {
		t.Fatal("unpublish must not report success while the target is still mounted")
	}
}

// stickyMounter wraps the fake and makes Unmount a no-op success while the
// mount layer stays registered: exactly what a stuck EBUSY-less unmount looks
// like to the caller.
type stickyMounter struct {
	mount.Interface
}

func (s *stickyMounter) Unmount(string) error { return nil }

// Raw-block volumes skip staging: their publish is the one path that still
// resolves a device through DSM+iSCSI. This pins the branch split.
func TestRawBlockPublishStillResolvesThroughDsm(t *testing.T) {
	target := filepath.Join(t.TempDir(), "blockdev")
	f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, 0o750)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	ns := &nodeServer{
		Mounter:    &mount.SafeFormatAndMount{Interface: mount.NewFakeMounter(nil)},
		dsmService: failingDsm{},
	}
	req := &csi.NodePublishVolumeRequest{
		VolumeId:          "8029e1d2-8ea8-4e8a-ae5f-e4b9404885fa",
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
		TargetPath:        target,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		},
		VolumeContext: map[string]string{"protocol": utils.ProtocolIscsi, "dsm": "10.10.146.6"},
	}
	if _, err := ns.NodePublishVolume(context.Background(), req); err == nil {
		t.Fatal("raw-block publish must go through device resolution (DSM), not bind anything")
	}
}

// Filesystem publish over an unstaged staging path must be refused: binding
// an empty directory is a silently empty filesystem for the pod.
func TestPublishRefusesUnstagedStaging(t *testing.T) {
	withFakeMountDetection(t)
	base := t.TempDir()
	staging, target := filepath.Join(base, "staging"), filepath.Join(base, "target")
	for _, d := range []string{staging, target} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	ns := &nodeServer{
		Mounter:    &mount.SafeFormatAndMount{Interface: mount.NewFakeMounter(nil)},
		dsmService: failingDsm{},
	}
	if _, err := ns.NodePublishVolume(context.Background(), iscsiMountPublishRequest(staging, target)); err == nil {
		t.Fatal("publish must refuse an unstaged staging path")
	}
}

// Unpublish of a path that no longer exists is a success (kubelet relies on
// the idempotency when reconstructing state).
func TestUnpublishNonexistentTargetIsIdempotent(t *testing.T) {
	ns := &nodeServer{Mounter: &mount.SafeFormatAndMount{Interface: mount.NewFakeMounter(nil)}}
	if _, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "8029e1d2-8ea8-4e8a-ae5f-e4b9404885fa",
		TargetPath: filepath.Join(t.TempDir(), "absent"),
	}); err != nil {
		t.Fatalf("unpublish of an absent target must succeed, got: %v", err)
	}
}

// The peel bound: a mounter that never clears the layer must surface an error
// after exactly maxUnmountPeels attempts, not loop forever.
func TestUnpublishGivesUpAfterThePeelBound(t *testing.T) {
	target := filepath.Join(t.TempDir(), "mount")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	ns := &nodeServer{Mounter: &mount.SafeFormatAndMount{Interface: &stickyMounter{Interface: mount.NewFakeMounter(nil)}}}
	calls := 0
	orig := isStillMounted
	isStillMounted = func(m mount.Interface, p string) (bool, error) {
		calls++
		return true, nil
	}
	t.Cleanup(func() { isStillMounted = orig })
	if _, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "8029e1d2-8ea8-4e8a-ae5f-e4b9404885fa",
		TargetPath: target,
	}); err == nil {
		t.Fatal("unpublish must report the still-mounted layer as an error")
	}
	if calls != maxUnmountPeels+1 { // attempts + the final verification
		t.Fatalf("expected %d seam reads (%d attempts + final check), got %d", maxUnmountPeels+1, maxUnmountPeels, calls)
	}
}

// The mount is verifiably gone but the target directory carries leftover
// content (a bad earlier state can leave a whole filesystem's worth of files
// in it): removing that content is the kubelet's pod-dir GC business, not this
// RPC's. ENOTEMPTY must not turn into a wedged pod.
func TestUnpublishToleratesLeftoverDirectoryContent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "mount")
	if err := os.MkdirAll(filepath.Join(target, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "flock.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// no fake mount layer: the mount is already gone, only the files remain
	ns := &nodeServer{Mounter: &mount.SafeFormatAndMount{Interface: mount.NewFakeMounter(nil)}}
	if _, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "8029e1d2-8ea8-4e8a-ae5f-e4b9404885fa",
		TargetPath: target,
	}); err != nil {
		t.Fatalf("unpublish must succeed once the mount is verified gone, got: %v", err)
	}
}
