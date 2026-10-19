/*
Copyright 2021 Synology Inc.

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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/mount-utils"
)

// TestNodeUnpublishVolume_CleansSubMounts reproduces the kubelet teardown
// deadlock: a propagated sub-mount (Bidirectional mountPropagation from the
// host's kubelet-dir) sits below targetPath. Without the sub-mount sweep,
// isStillMounted only looks at the exact path, os.Remove fails with ENOTEMPTY,
// kubelet's pod-dir GC also cannot remove the directory, and NodeUnstageVolume
// (which logs out the iSCSI session) is never reached.
func TestNodeUnpublishVolume_CleansSubMounts(t *testing.T) {
	target := t.TempDir()
	subdir := filepath.Join(target, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	// Fake mount table: a propagated bind mount sits below targetPath, plus
	// the main bind mount on targetPath itself. Mounts are registered in
	// the FakeMounter so Unmount mutates state; listProcMounts returns the
	// live view so isStillMounted sees post-Unmount reality.
	fm := mount.NewFakeMounter([]mount.MountPoint{
		{Device: "/dev/sda1", Path: target, Opts: []string{"rw", "bind"}},
		{Device: "/dev/sda1", Path: subdir, Opts: []string{"rw", "bind"}},
	})
	listProcMounts = func(string) ([]mount.MountPoint, error) {
		return fm.List()
	}
	defer func() { listProcMounts = mount.ListProcMounts }()

	ns := &nodeServer{Mounter: (&mount.SafeFormatAndMount{Interface: fm})}

	// FakeMounter.Unmount already removes the matching MountPoint from
	// MountPoints before calling UnmountFunc, so we just record the call
	// sequence here — touching MountPoints from inside UnmountFunc would
	// race with the iteration the real Unmount is doing.
	var unmountCalls []string
	fm.UnmountFunc = func(target string) error {
		unmountCalls = append(unmountCalls, target)
		return nil
	}

	if _, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: "vol-1", TargetPath: target}); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}

	// The sub-mount must be unmounted BEFORE the main targetPath.
	if len(unmountCalls) < 2 {
		t.Fatalf("expected at least 2 Unmount calls, got %d: %v", len(unmountCalls), unmountCalls)
	}
	if unmountCalls[0] != subdir {
		t.Errorf("first Unmount target = %s, want %s (sub-mount first)", unmountCalls[0], subdir)
	}
	if unmountCalls[1] != target {
		t.Errorf("second Unmount target = %s, want %s (main mount after)", unmountCalls[1], target)
	}

	// Leftover dir must be gone: kubelet's pod-dir GC would otherwise also
	// fail and the pod would stay in volumesInUse forever.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("targetPath %s still exists after NodeUnpublishVolume", target)
	}
}

// TestNodeUnpublishVolume_FallsBackToRemoveAll covers the safety net: when
// leftover content is NOT a live mount (e.g. stale files from a container
// that wrote into a vanished mount point), the sub-mount sweep unmounts
// nothing, and the driver must still clear the directory so kubelet can
// finish teardown.
func TestNodeUnpublishVolume_FallsBackToRemoveAll(t *testing.T) {
	target := t.TempDir()
	// Stale orphan content — not a live mount, just files.
	if err := os.WriteFile(filepath.Join(target, "stale.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A deeper nested subdir exercises the depth-sorted sweep too.
	nested := filepath.Join(target, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "leaf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake mount table: only the top-level bind mount — sub-paths carry no
	// live mount anymore. Use the FakeMounter's live view so isStillMounted
	// sees post-Unmount state.
	fm := mount.NewFakeMounter([]mount.MountPoint{
		{Device: "/dev/sda1", Path: target, Opts: []string{"rw", "bind"}},
	})
	listProcMounts = func(string) ([]mount.MountPoint, error) {
		return fm.List()
	}
	defer func() { listProcMounts = mount.ListProcMounts }()

	ns := &nodeServer{Mounter: (&mount.SafeFormatAndMount{Interface: fm})}

	if _, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: "vol-2", TargetPath: target}); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("targetPath %s still exists after NodeUnpublishVolume", target)
	}
}

// TestUnmountSubMounts_DepthDescending verifies the sweep orders mounts
// deepest-first, so a stack like /a/b/c is torn down c -> b -> a.
func TestUnmountSubMounts_DepthDescending(t *testing.T) {
	target := t.TempDir()
	c := filepath.Join(target, "a", "b", "c")
	b := filepath.Join(target, "a", "b")
	a := filepath.Join(target, "a")
	for _, d := range []string{a, b, c} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	mounts := []mount.MountPoint{
		{Path: a},
		{Path: c},
		{Path: b},
	}
	listProcMounts = func(string) ([]mount.MountPoint, error) { return mounts, nil }
	defer func() { listProcMounts = mount.ListProcMounts }()

	fm := mount.NewFakeMounter(mounts)
	var calls []string
	fm.UnmountFunc = func(target string) error {
		calls = append(calls, target)
		return nil
	}
	ns := &nodeServer{Mounter: (&mount.SafeFormatAndMount{Interface: fm})}

	if err := unmountSubMounts(ns, target); err != nil {
		t.Fatalf("unmountSubMounts: %v", err)
	}
	want := []string{c, b, a}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call[%d] = %s, want %s", i, calls[i], want[i])
		}
	}
}

// TestSubMountPathsSortedByDepth is a pure unit check on the sort predicate,
// independent of any mount.Interface wiring.
func TestSubMountPathsSortedByDepth(t *testing.T) {
	base := "/var/lib/kubelet/pods/uid/volumes/.../mount"
	in := []string{
		base,
		base + "/a",
		base + "/a/b/c",
		base + "/a/b",
		base + "/z",
	}
	got := append([]string(nil), in...)
	sort.SliceStable(got, func(i, j int) bool {
		return strings.Count(got[i], "/") > strings.Count(got[j], "/")
	})
	want := []string{
		base + "/a/b/c",
		base + "/a/b",
		base + "/a",
		base + "/z",
		base,
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}
