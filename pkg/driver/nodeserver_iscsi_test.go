/*
 * Copyright 2025 Synology Inc.
 */

package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
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
	// un staging backing device qui passe requireIscsiDisk (faux sysfs)
	sysfs, iscsiDev := fakeSysfs(t, "sdx", "session7", "6:0:0:1", rightIqn)
	oldSys := sysfsRoot
	sysfsRoot = sysfs
	t.Cleanup(func() { sysfsRoot = oldSys })
	origSrc := stagingMountSource
	stagingMountSource = func(path string) (string, bool) {
		if path == filepath.Join(t.TempDir(), "never") {
			return "", false
		}
		return iscsiDev, true
	}
	t.Cleanup(func() { stagingMountSource = origSrc })
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

// The kubelet can skip NodeStageVolume entirely when its cached state says
// "already staged" (reconstructed from a stale globalmount), and then bind
// whatever the staging path holds -- a node-local partition, or nothing. The
// publish is the last line of defense: it must verify that the staging mount
// is backed by an iSCSI whole disk before handing it to a pod.
func TestPublishRefusesNonIscsiBackingDevice(t *testing.T) {
	withFakeMountDetection(t)
	base := t.TempDir()
	staging, target := filepath.Join(base, "staging"), filepath.Join(base, "target")
	for _, d := range []string{staging, target} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// staging IS mounted... but by the node's local partition (the 2026-10-17
	// incident: the pod received /dev/sda4)
	fm := mount.NewFakeMounter([]mount.MountPoint{{Device: "/dev/sda4", Path: staging}})
	ns := &nodeServer{
		Mounter:    &mount.SafeFormatAndMount{Interface: fm},
		dsmService: failingDsm{},
	}
	orig := stagingMountSource
	stagingMountSource = func(path string) (string, bool) {
		if path == staging {
			return "/dev/sda4", true
		}
		return "", false
	}
	t.Cleanup(func() { stagingMountSource = orig })

	if _, err := ns.NodePublishVolume(context.Background(), iscsiMountPublishRequest(staging, target)); err == nil {
		t.Fatal("publish must refuse a staging mount backed by a non-iSCSI device")
	}
}

const pvcIqn = "iqn.2000-01.com.synology:test.pvc-1"

// liveDsm answers the two lookups the self-heal needs: the volume's iSCSI
// target identity and the DSM record behind its portal. Rebuilding a staging
// re-resolves the volume through DSM -- unlike the fast path, which must
// never call DSM at all (failingDsm pins that).
type liveDsm struct {
	interfaces.IDsmService
}

func (liveDsm) GetVolume(string) (*models.K8sVolumeRespSpec, error) {
	return &models.K8sVolumeRespSpec{
		VolumeId: "8029e1d2-8ea8-4e8a-ae5f-e4b9404885fa",
		DsmIp:    "10.10.146.6",
		Target: webapi.TargetInfo{
			Iqn:        pvcIqn,
			MappedLuns: []webapi.MappedLun{{LunUuid: "lun-pvc-1", MappingIndex: 1}},
		},
	}, nil
}

func (liveDsm) GetDsm(string) (*webapi.DSM, error) {
	return &webapi.DSM{}, nil
}

// fakeInitiator stands in for the host's iSCSI initiator: it counts logins
// (the fast path must make none) and can fail them (AC4) or, on success, lay
// the staged mount into the fake mount table -- a stand-in for the real
// FormatAndMount that follows a login in production, folded into the login
// because that is the step whose success makes a stage possible.
type fakeInitiator struct {
	logins   int
	loginErr error
	// stage, when set, runs inside a successful login: that is where the test
	// simulates the mount the real stage would perform.
	stage func()
}

func (f *fakeInitiator) login(iqn, portal string) error {
	f.logins++
	if f.loginErr != nil {
		return f.loginErr
	}
	if f.stage != nil {
		f.stage()
	}
	return nil
}

func (f *fakeInitiator) logout(string, string) error { return nil }
func (f *fakeInitiator) rescan(string) error         { return nil }

// withAbsentByPath points the by-path wait at a node with no /dev/disk at
// all -- the udev-less Talos shape the self-heal exists for: the login and
// the rescan succeed, udev never builds the links. Without this seam every
// self-heal test would wait out the real 20-second poll.
func withAbsentByPath(t *testing.T) {
	t.Helper()
	orig := waitForDevicePathToExist
	waitForDevicePathToExist = func(string) error {
		return errors.New("no /dev/disk/by-path on this fake node")
	}
	t.Cleanup(func() { waitForDevicePathToExist = orig })
}

// withFakeSysfs points sysfsRoot and devDir at a fabricated sysfs holding one
// logged-in iSCSI session for iqn (lun 1): the discovery walk reads the
// session there and resolves the /dev/<name> node inside the same tree, the
// way kernel sysfs and /dev agree on a real node. Returns the fake device
// path the walk hands out.
func withFakeSysfs(t *testing.T, iqn string) (sysfs, dev string) {
	t.Helper()
	sysfs, dev = fakeSysfs(t, "sdx", "session7", "6:0:0:1", iqn)
	oldSys, oldDev := sysfsRoot, devDir
	sysfsRoot, devDir = sysfs, sysfs
	t.Cleanup(func() { sysfsRoot, devDir = oldSys, oldDev })
	return sysfs, dev
}

// selfHealNode wires a node server for the self-heal tests: a DSM that
// answers, a counting initiator, and a host executor reporting no iscsiadm
// sessions (nothing to rescan after the fake login).
func selfHealNode(fm *mount.FakeMounter, initr *fakeInitiator) *nodeServer {
	return &nodeServer{
		Mounter:    &mount.SafeFormatAndMount{Interface: fm},
		dsmService: liveDsm{},
		Initiator:  initr,
		tools:      tools{executor: &recordingExecutor{}},
	}
}

// assertBind checks that the publish bound the staging path onto the target.
func assertBind(t *testing.T, fm *mount.FakeMounter, staging, target string) {
	t.Helper()
	for _, mp := range fm.MountPoints {
		if mp.Path == target && utils.SliceContains(mp.Opts, "bind") {
			return
		}
	}
	t.Fatalf("publish did not bind staging %s onto %s; mounted: %v", staging, target, fm.MountPoints)
}

// AC1: the kubelet's cached state said "staged" but the staging path holds no
// mount at all (node rebuilt under it). Publish used to refuse with
// FailedPrecondition and the pod stayed wedged forever; it must now resolve
// the volume through DSM, log the target in, stage the LUN at the staging
// path and bind it -- all of it through the sysfs walk, since this node has
// no by-path links.
func TestPublishSelfHealsAnUnstagedStaging(t *testing.T) {
	withFakeMountDetection(t)
	withAbsentByPath(t)
	_, iscsiDev := withFakeSysfs(t, pvcIqn)

	base := t.TempDir()
	staging, target := filepath.Join(base, "staging"), filepath.Join(base, "target")
	for _, d := range []string{staging, target} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	fm := mount.NewFakeMounter(nil)
	initr := &fakeInitiator{}
	// the login succeeding is what makes a stage possible: from that point on
	// the staging path carries the LUN, and the mount table says so.
	staged := false
	initr.stage = func() {
		staged = true
		if err := fm.Mount(iscsiDev, staging, "xfs", []string{"rw"}); err != nil {
			t.Errorf("fake stage mount failed: %v", err)
		}
	}
	origSrc := stagingMountSource
	stagingMountSource = func(path string) (string, bool) {
		if path != staging {
			return "", false
		}
		if staged {
			return iscsiDev, true
		}
		return "/dev/sda4", true
	}
	t.Cleanup(func() { stagingMountSource = origSrc })

	if _, err := selfHealNode(fm, initr).NodePublishVolume(context.Background(), iscsiMountPublishRequest(staging, target)); err != nil {
		t.Fatalf("publish must self-heal an unstaged staging, got: %v", err)
	}
	assertBind(t, fm, staging, target)
}

// AC2: the kubelet believed the volume staged, but the staging path is
// mounted from the node's own partition (the 2026-10-17 incident). Publish
// must tear that mount down, re-run the full stage against the volume's LUN,
// re-verify what the staging now holds and only then bind.
func TestPublishSelfHealsAStagingBackedByAnotherDisk(t *testing.T) {
	withFakeMountDetection(t)
	withAbsentByPath(t)
	_, iscsiDev := withFakeSysfs(t, pvcIqn)

	base := t.TempDir()
	staging, target := filepath.Join(base, "staging"), filepath.Join(base, "target")
	for _, d := range []string{staging, target} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	// the staging IS mounted -- by the node's local partition
	fm := mount.NewFakeMounter([]mount.MountPoint{{Device: "/dev/sda4", Path: staging}})
	initr := &fakeInitiator{}
	staged := false
	initr.stage = func() {
		staged = true
		if err := fm.Mount(iscsiDev, staging, "xfs", []string{"rw"}); err != nil {
			t.Errorf("fake stage mount failed: %v", err)
		}
	}
	origSrc := stagingMountSource
	stagingMountSource = func(path string) (string, bool) {
		if path != staging {
			return "", false
		}
		if staged {
			return iscsiDev, true
		}
		return "/dev/sda4", true
	}
	t.Cleanup(func() { stagingMountSource = origSrc })

	if _, err := selfHealNode(fm, initr).NodePublishVolume(context.Background(), iscsiMountPublishRequest(staging, target)); err != nil {
		t.Fatalf("publish must replace a staging backed by another disk, got: %v", err)
	}

	// the final stack: the LUN mounted at staging (the rebuilt stage) and the
	// bind of staging onto the target, with no trace of the partition.
	bound, restaged := false, false
	for _, mp := range fm.MountPoints {
		if mp.Path == target && utils.SliceContains(mp.Opts, "bind") {
			bound = true
		}
		if mp.Path == staging && mp.Device == iscsiDev {
			restaged = true
		}
		if mp.Device == "/dev/sda4" {
			t.Fatalf("the stale partition mount must be gone, final stack: %v", fm.MountPoints)
		}
	}
	if !bound || !restaged {
		t.Fatalf("expected the LUN re-staged at %s and bound onto %s; mounted: %v", staging, target, fm.MountPoints)
	}
}

// AC3: the staging path already holds a verified iSCSI disk -- the fast
// path. Binding must be the whole story: zero logins, zero DSM.
func TestPublishBindsAVerifiedStagingWithoutAnyLogin(t *testing.T) {
	withFakeMountDetection(t)
	_, iscsiDev := withFakeSysfs(t, pvcIqn)

	base := t.TempDir()
	staging, target := filepath.Join(base, "staging"), filepath.Join(base, "target")
	for _, d := range []string{staging, target} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	fm := mount.NewFakeMounter([]mount.MountPoint{{Device: iscsiDev, Path: staging}})
	origSrc := stagingMountSource
	stagingMountSource = func(path string) (string, bool) {
		if path == staging {
			return iscsiDev, true
		}
		return "", false
	}
	t.Cleanup(func() { stagingMountSource = origSrc })

	initr := &fakeInitiator{}
	ns := &nodeServer{
		Mounter:    &mount.SafeFormatAndMount{Interface: fm},
		dsmService: failingDsm{},
		Initiator:  initr,
	}
	if _, err := ns.NodePublishVolume(context.Background(), iscsiMountPublishRequest(staging, target)); err != nil {
		t.Fatalf("publish must bind a verified staging as-is, got: %v", err)
	}
	if initr.logins != 0 {
		t.Fatalf("the fast path must not log in, got %d login(s)", initr.logins)
	}
	assertBind(t, fm, staging, target)
}

// AC4: the self-heal needs a login and the login fails (portal down). The
// publish must surface the failure and bind nothing -- but it must have
// TRIED the login, which is what separates this from the old refusal.
func TestPublishReportsAFailedSelfHealLoginAndBindsNothing(t *testing.T) {
	withFakeMountDetection(t)
	withFakeSysfs(t, pvcIqn)

	base := t.TempDir()
	staging, target := filepath.Join(base, "staging"), filepath.Join(base, "target")
	for _, d := range []string{staging, target} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	fm := mount.NewFakeMounter(nil)
	initr := &fakeInitiator{loginErr: errors.New("login failed: portal unreachable")}

	if _, err := selfHealNode(fm, initr).NodePublishVolume(context.Background(), iscsiMountPublishRequest(staging, target)); err == nil {
		t.Fatal("publish must report a self-heal whose login fails")
	}
	if initr.logins == 0 {
		t.Fatal("the self-heal must have attempted the login before giving up")
	}
	for _, mp := range fm.MountPoints {
		if mp.Path == target {
			t.Fatalf("nothing may be bound onto the target on a failed self-heal: %v", fm.MountPoints)
		}
	}
}

// The kernel scans a freshly logged-in session asynchronously: the first
// sysfs read after a login legitimately misses the lun. The stage must ride
// that window out with a bounded poll -- failing the first read makes every
// kubelet retry pay the whole login again.
func TestPublishSelfHealToleratesALateKernelScan(t *testing.T) {
	withFakeMountDetection(t)
	withAbsentByPath(t)
	sysfs, iscsiDev := withFakeSysfs(t, pvcIqn)
	// logged in but not scanned: the lun carries no disk yet
	canonical, err := filepath.EvalSymlinks(filepath.Join(sysfs, "block", "sdx", "device"))
	if err != nil {
		t.Fatal(err)
	}
	blockDir := filepath.Join(canonical, "block")
	if err := os.RemoveAll(blockDir); err != nil {
		t.Fatal(err)
	}
	origInterval := sessionPollInterval
	sessionPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { sessionPollInterval = origInterval })

	base := t.TempDir()
	staging, target := filepath.Join(base, "staging"), filepath.Join(base, "target")
	for _, d := range []string{staging, target} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	fm := mount.NewFakeMounter(nil)
	initr := &fakeInitiator{}
	staged := false
	initr.stage = func() {
		staged = true
		// the scan completes a few polls after the login
		time.AfterFunc(3*sessionPollInterval, func() {
			_ = os.MkdirAll(filepath.Join(blockDir, "sdx"), 0o755)
		})
		if err := fm.Mount(iscsiDev, staging, "xfs", []string{"rw"}); err != nil {
			t.Errorf("fake stage mount failed: %v", err)
		}
	}
	origSrc := stagingMountSource
	stagingMountSource = func(path string) (string, bool) {
		if path != staging {
			return "", false
		}
		if staged {
			return iscsiDev, true
		}
		return "", false
	}
	t.Cleanup(func() { stagingMountSource = origSrc })

	if _, err := selfHealNode(fm, initr).NodePublishVolume(context.Background(), iscsiMountPublishRequest(staging, target)); err != nil {
		t.Fatalf("publish must ride out the asynchronous scan, got: %v", err)
	}
	assertBind(t, fm, staging, target)
}

// When the sysfs session already carries the lun, the by-path wait is dead
// time -- infinite on udev-less nodes. loginTarget must probe sysfs first,
// and keep the by-path in its leg list either way: the leg count drives the
// multipath decision and the path stays the stage's fallback.
func TestLoginTargetKeepsTheByPathWhenSysfsAnswers(t *testing.T) {
	withAbsentByPath(t) // the wait, if reached, fails instantly
	withFakeSysfs(t, pvcIqn)
	ns := &nodeServer{
		dsmService: liveDsm{},
		Initiator:  &fakeInitiator{},
		tools:      tools{executor: &recordingExecutor{}},
	}
	paths, iqn, lun, err := ns.loginTarget("8029e1d2-8ea8-4e8a-ae5f-e4b9404885fa")
	if err != nil {
		t.Fatalf("loginTarget must succeed when sysfs already answers, got: %v", err)
	}
	if iqn != pvcIqn || lun != 1 {
		t.Fatalf("expected target[%s] lun 1, got %q/%d", pvcIqn, iqn, lun)
	}
	if len(paths) != 1 || !strings.Contains(paths[0], "/dev/disk/by-path/") {
		t.Fatalf("the by-path link must stay in the leg list (multipath counting, stage fallback), got: %v", paths)
	}
}

// Several by-path legs mean multipath: the stage must mount the dm map that
// spans the legs -- the sysfs walk only ever sees one leg and would silently
// drop the redundancy. When the map cannot be resolved, the stage fails; it
// must never quietly mount a single leg instead.
func TestResolveStageDeviceRefusesASingleLegWhenMultipathIsExpected(t *testing.T) {
	withFakeSysfs(t, pvcIqn) // a leg IS resolvable from sysfs -- and must not be used
	if _, err := resolveStageDevice(
		[]string{
			"/dev/disk/by-path/ip-10.10.146.6:3260-iscsi-iqn.2000-01.com.synology:test.pvc-1-lun-1",
			"/dev/disk/by-path/ip-10.10.146.7:3260-iscsi-iqn.2000-01.com.synology:test.pvc-1-lun-1",
		},
		pvcIqn, 1); err == nil {
		t.Fatal("an unresolvable multipath map must fail the stage, not fall back to a single leg")
	}
}

// Mono-path keeps the sysfs walk as the primary source: udev-less nodes have
// no by-path links at all.
func TestResolveStageDeviceWalksSysfsOnMonoPath(t *testing.T) {
	_, dev := withFakeSysfs(t, pvcIqn) // under the seam, devDir is the fake tree
	got, err := resolveStageDevice(nil, pvcIqn, 1)
	if err != nil {
		t.Fatalf("the sysfs walk must resolve the mono-path device, got: %v", err)
	}
	if got != dev {
		t.Errorf("expected the walked device %s, got %q", dev, got)
	}
}
