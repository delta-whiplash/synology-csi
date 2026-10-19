// Copyright 2025 Synology Inc.

package driver

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	rightIqn = "iqn.2000-01.com.synology:nas.pvc-right"
	wrongIqn = "iqn.2000-01.com.synology:nas.pvc-wrong"
)

// fakeSession adds one logged-in session to a fake sysfs root, laid out the
// way the kernel builds it: the session's device in the devices tree (under
// its own SCSI host, holding targetX:Y:Z, the lun dir, and the lun's disk
// under block/), the block device backed by it, and the class entry
// symlinked to the session's iscsi_session directory.
func fakeSession(t *testing.T, root, dev, session, host, target, hctl, iqn string) {
	t.Helper()

	canonical := filepath.Join(root, "devices", "platform", host, session, target, hctl)
	if err := os.MkdirAll(filepath.Join(canonical, "block", dev), 0o755); err != nil {
		t.Fatal(err)
	}
	blockDir := filepath.Join(root, "block", dev)
	if err := os.MkdirAll(blockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canonical, filepath.Join(blockDir, "device")); err != nil {
		t.Fatal(err)
	}

	// The kernel hangs a session off its device in the devices tree and
	// symlinks the class entry to it: /sys/class/iscsi_session/<session> ->
	// .../<host>/<session>/iscsi_session/<session>.
	inner := filepath.Join(root, "devices", "platform", host, session, "iscsi_session", session)
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "targetname"), []byte(iqn+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	classDir := filepath.Join(root, "class", "iscsi_session")
	if err := os.MkdirAll(classDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inner, filepath.Join(classDir, session)); err != nil {
		t.Fatal(err)
	}
}

// fakeSysfs builds the slice of sysfs the verifier and the session walk
// traverse, with one logged-in session.
func fakeSysfs(t *testing.T, dev, session, hctl, iqn string) (sysfs, devPath string) {
	t.Helper()
	root := t.TempDir()
	fakeSession(t, root, dev, session, "host6", "target6:0:0", hctl, iqn)

	// A stand-in for /dev/<dev>: the verifier only needs its resolved basename.
	devPath = filepath.Join(root, dev)
	if err := os.WriteFile(devPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, devPath
}

func TestDeviceThatMatchesIsAccepted(t *testing.T) {
	sysfs, dev := fakeSysfs(t, "sdx", "session7", "6:0:0:1", rightIqn)
	if real, err := verifyIscsiDevice(dev, rightIqn, 1, sysfs); err != nil {
		t.Errorf("a device that belongs to the expected target must be accepted, got: %v", err)
	} else if real != dev {
		t.Errorf("the verifier must hand back the resolved device path, got %q", real)
	}
}

// The failure being chased: the by-path link hands over a name that meanwhile
// belongs to a different volume's session. Mounting it reads someone else's
// bytes with no error anywhere, so this must refuse.
func TestDeviceFromAnotherTargetIsRefused(t *testing.T) {
	sysfs, dev := fakeSysfs(t, "sdx", "session7", "6:0:0:1", wrongIqn)
	_, err := verifyIscsiDevice(dev, rightIqn, 1, sysfs)
	if err == nil {
		t.Fatal("a device belonging to another volume's target must be refused")
	}
	for _, want := range []string{rightIqn, wrongIqn} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name both identities, missing %q in: %v", want, err)
		}
	}
}

func TestDeviceWithTheWrongLunIsRefused(t *testing.T) {
	sysfs, dev := fakeSysfs(t, "sdx", "session7", "6:0:0:2", rightIqn)
	if _, err := verifyIscsiDevice(dev, rightIqn, 1, sysfs); err == nil {
		t.Fatal("a device carrying a different LUN must be refused; " +
			"logoutTarget already assumes one LUN per target, this is where that assumption gets checked")
	}
}

// A by-path link resolving to something that is not an iSCSI disk at all means
// the link itself is lying; that is a refusal, not a shrug.
func TestNonIscsiDeviceIsRefused(t *testing.T) {
	root := t.TempDir()
	canonical := filepath.Join(root, "devices", "pci0000:00", "ata1", "target0:0:0", "0:0:0:0")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	blockDir := filepath.Join(root, "block", "sda")
	if err := os.MkdirAll(blockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canonical, filepath.Join(blockDir, "device")); err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(root, "sda")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := verifyIscsiDevice(dev, rightIqn, 1, root); err == nil {
		t.Fatal("a local disk behind an iSCSI by-path link must be refused")
	}
}

// Multipath devices are verified through their legs.
func TestMultipathSlavesAreVerified(t *testing.T) {
	sysfs, _ := fakeSysfs(t, "sdx", "session7", "6:0:0:1", wrongIqn)

	dmDir := filepath.Join(sysfs, "block", "dm-0", "slaves", "sdx")
	if err := os.MkdirAll(dmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(sysfs, "dm-0")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := verifyIscsiDevice(dev, rightIqn, 1, sysfs); err == nil {
		t.Fatal("a multipath device whose leg belongs to another target must be refused")
	}
}

// Not being able to tell is logged, not fatal: the identity was never shown to
// be wrong, and failing every mount on an unfamiliar sysfs layout would trade
// a rare corruption for a common outage.
// Contract change (prod 2026-10-17: a pod was handed the node's local
// partition through exactly this hole): an indeterminate identity now fails
// the mount instead of passing vacuously. kubelet retries; a human looks.
func TestUnreadableSysfsIsRefused(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "sdq") // no block/sdq at all
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyIscsiDevice(dev, rightIqn, 1, root); err == nil {
		t.Fatal("an unidentified device must be refused, not passed")
	}
}

// A partition (sda4-style) has no /sys/block/<name>/device at all. The walk
// cannot tell what it is handing over -- and a local partition is exactly
// what must never be mounted as a volume, so this is an error, not a
// vacuous pass (prod: vmsingle booted on the node's ephemeral partition).
func TestPartitionDeviceIsRejected(t *testing.T) {
	root := t.TempDir()
	devPath := filepath.Join(root, "sda4")
	if err := os.WriteFile(devPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// no sysfs tree whatsoever: nothing can identify this device
	if _, err := verifyIscsiDevice(devPath, rightIqn, 1, filepath.Join(root, "sys")); err == nil {
		t.Fatal("a device that cannot be identified must be refused, not passed")
	}
}

// A devPath that does not resolve (a by-path link left dangling by a logout)
// has an unknowable identity: refuse and let kubelet retry once the tree
// settles.
func TestUnresolvableDeviceIsRejected(t *testing.T) {
	root := t.TempDir()
	dangling := filepath.Join(root, "by-path-stale")
	if err := os.Symlink(filepath.Join(root, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyIscsiDevice(dangling, rightIqn, 1, filepath.Join(root, "sys")); err == nil {
		t.Fatal("a devPath that does not resolve must be refused, not passed")
	}
}

// findSessionDevice is the discovery half of the story: on udev-less nodes
// (Talos) nothing scans a freshly logged-in session, so no device node ever
// appears and the by-path links are never created. Discovery has to start
// from the session itself and walk the kernel's sysfs view.
func TestFindSessionDeviceFindsTheLun(t *testing.T) {
	sysfs, _ := fakeSysfs(t, "sdx", "session7", "6:0:0:1", rightIqn)
	got, err := findSessionDevice(rightIqn, 1, sysfs)
	if err != nil {
		t.Fatalf("the session and the lun are both present, they must be found: %v", err)
	}
	if want := "/dev/sdx"; got != want {
		t.Errorf("the device must come back as /dev/<name>, got %q want %q", got, want)
	}
}

// One iqn is logged in on several sessions under real multipath -- one per
// portal, each with its own SCSI host -- and the kernel scans them
// independently. If the first session has not got the lun while the second
// has, the walk must still find it: declaring the lun absent off the first
// session is a false negative the caller would answer with a re-login.
func TestFindSessionDeviceWalksEverySessionOfTheIqn(t *testing.T) {
	root := t.TempDir()
	// session7 (host6): right iqn, but only lun 2 attached so far.
	fakeSession(t, root, "sdx", "session7", "host6", "target6:0:0", "6:0:0:2", rightIqn)
	// session8 (host7): same iqn through the second portal, lun 1 attached.
	fakeSession(t, root, "sdy", "session8", "host7", "target7:0:0", "7:0:0:1", rightIqn)

	got, err := findSessionDevice(rightIqn, 1, root)
	if err != nil {
		t.Fatalf("the lun is attached on the second session, it must be found: %v", err)
	}
	if want := "/dev/sdy"; got != want {
		t.Errorf("the device from the second session must be returned, got %q want %q", got, want)
	}
}

// No session carries the iqn: the login did not happen or is already gone.
// The caller (publish self-heal) classifies this as "needs a re-login", so
// the error must carry the ErrSessionNotFound sentinel, not a substring to
// match against.
func TestFindSessionDeviceWithoutSessionIsAnError(t *testing.T) {
	sysfs, _ := fakeSysfs(t, "sdx", "session7", "6:0:0:1", wrongIqn)
	_, err := findSessionDevice(rightIqn, 1, sysfs)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("a missing session must classify as ErrSessionNotFound, got: %v", err)
	}
	if !strings.Contains(err.Error(), rightIqn) {
		t.Errorf("the error should name the target no session serves, missing %q in: %v", rightIqn, err)
	}
}

// The session is logged in but the lun has no block device behind it yet --
// exactly the window between login and scan that self-healing has to ride
// out. Must classify as ErrLunNotFound (rescan, not re-login) and carry the
// lun it did not find.
func TestFindSessionDeviceWithoutLunIsAnError(t *testing.T) {
	sysfs, _ := fakeSysfs(t, "sdx", "session7", "6:0:0:2", rightIqn)
	_, err := findSessionDevice(rightIqn, 1, sysfs)
	if !errors.Is(err, ErrLunNotFound) {
		t.Fatalf("a session without the lun must classify as ErrLunNotFound, got: %v", err)
	}
	if !strings.Contains(err.Error(), "lun 1") {
		t.Errorf("the error should carry the lun that was not found, missing %q in: %v", "lun 1", err)
	}
}

// A class entry that does not resolve to .../<session>/iscsi_session/<session>
// in the devices tree is a sysfs shape the walk cannot read. It must report
// itself as such: falling through to "no device for lun" would send the
// caller rescanning a login that is fine.
func TestFindSessionDeviceWithUnfamiliarLayoutNamesTheLayout(t *testing.T) {
	root := t.TempDir()
	// Flat class layout (real dir, no devices tree behind it): not a shape
	// the walk reads.
	sessDir := filepath.Join(root, "class", "iscsi_session", "session7")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "targetname"), []byte(rightIqn+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := findSessionDevice(rightIqn, 1, root)
	if err == nil {
		t.Fatal("an unreadable sysfs layout must be an error")
	}
	if errors.Is(err, ErrLunNotFound) || errors.Is(err, ErrSessionNotFound) {
		t.Errorf("the error must name the layout, not disguise itself as a missing login or lun: %v", err)
	}
}

// A dm device whose legs are gone reports no slaves: zero evidence, zero
// trust -- this must be refused, not passed like an empty check.
func TestDmDeviceWithNoSlavesIsRefused(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "dm-1")
	if err := os.WriteFile(real, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "block", "dm-1", "slaves"), 0o755); err != nil {
		t.Fatal(err)
	}
	devPath := filepath.Join(root, "mapper", "vol")
	if err := os.MkdirAll(filepath.Join(root, "mapper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, devPath); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyIscsiDevice(devPath, rightIqn, 1, root); err == nil {
		t.Fatal("a dm device with no slaves must be refused")
	}
}
