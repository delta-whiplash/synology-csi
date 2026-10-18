/*
 * Copyright 2025 Synology Inc.
 */

package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// sysfsRoot is a var so tests can point the walk at a fabricated tree.
var sysfsRoot = "/sys"

var sessionRe = regexp.MustCompile(`(^|/)(session\d+)(/|$)`)

// verifyIscsiDevice checks that the device a by-path link resolved to really
// belongs to the target and LUN this volume expects.
//
// The by-path link is udev bookkeeping, not the kernel's own view. Links are
// created and removed asynchronously as sessions come and go, and the E2E
// failures being chased all involve many volumes logging in and out on one
// node at once -- exactly the churn in which a link can outlive its device or
// be read while the name it points to has already been given to someone else's
// LUN. Mounting whatever the link says without checking is how a pod ends up
// reading a different volume's bytes with no error anywhere: the mount itself
// works fine, it is just the wrong disk.
//
// The kernel's view is authoritative and cheap to read: a SCSI disk's sysfs
// device path names the iSCSI session it hangs off, the session publishes the
// target IQN it is logged in to, and the last component carries H:C:T:L with
// the LUN number. Comparing those against what the volume expects decides,
// per mount, whether by-path told the truth.
//
// A mismatch is an error, and so is an identity that cannot be read: sysfs
// shaped unexpectedly, session gone mid-read, partition or local disk in the
// way. Handing over an unidentified device is how a pod ends up living on the
// node's own disk; failing the stage costs one kubelet retry, mounting blind
// costs the volume. Returns the resolved device path so callers mount the
// exact device whose identity was verified, not a link that could be
// repointed afterwards.
func verifyIscsiDevice(devPath, wantIqn string, wantLun int, sysfs string) (string, error) {
	real, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return "", fmt.Errorf("device path %s does not resolve: %v -- an identity that "+
			"cannot be read is never handed over (fail closed, kubelet will retry)", devPath, err)
	}

	names := []string{filepath.Base(real)}
	if strings.HasPrefix(filepath.Base(real), "dm-") {
		// A multipath device is only as trustworthy as its legs.
		slaves, err := os.ReadDir(filepath.Join(sysfs, "block", filepath.Base(real), "slaves"))
		if err != nil {
			return "", fmt.Errorf("couldn't list the slaves of %s to verify its identity: %v", real, err)
		}
		if len(slaves) == 0 {
			// A dm device mid-teardown: no legs, no evidence, no mount.
			return "", fmt.Errorf("multipath device %s has no slaves to verify its identity against", real)
		}
		names = names[:0]
		for _, s := range slaves {
			names = append(names, s.Name())
		}
	}

	for _, name := range names {
		canonical, err := filepath.EvalSymlinks(filepath.Join(sysfs, "block", name, "device"))
		if err != nil {
			// Partitions (sda4-style) and non-SCSI disks have no device link in
			// sysfs: their identity is unknowable, and mounting one is how a pod
			// ends up living on the node's local disk instead of its LUN.
			return "", fmt.Errorf("couldn't read the kernel's view of %s (%s): %v -- refusing "+
				"an unidentified device", devPath, name, err)
		}

		m := sessionRe.FindStringSubmatch(canonical)
		if m == nil {
			return "", fmt.Errorf("device %s (%s) is not an iSCSI disk (sysfs path %s); "+
				"the by-path link for target[%s] points at something that never came from that target",
				devPath, name, canonical, wantIqn)
		}
		session := m[2]

		hctl := filepath.Base(canonical)
		parts := strings.Split(hctl, ":")
		gotLun := -1
		if len(parts) == 4 {
			if v, err := strconv.Atoi(parts[3]); err == nil {
				gotLun = v
			}
		}

		data, err := os.ReadFile(filepath.Join(sysfs, "class", "iscsi_session", session, "targetname"))
		if err != nil {
			return "", fmt.Errorf("couldn't read the target of %s (%s): %v -- refusing an "+
				"unidentified device", devPath, name, err)
		}
		gotIqn := strings.TrimSpace(string(data))

		if gotIqn != wantIqn || (gotLun != -1 && gotLun != wantLun) {
			return "", fmt.Errorf("device %s (%s) belongs to target[%s] lun %d, "+
				"but this volume expects target[%s] lun %d -- refusing to hand over another volume's disk",
				devPath, name, gotIqn, gotLun, wantIqn, wantLun)
		}
	}
	return real, nil
}

// requireIscsiDisk checks that devPath is backed by an iSCSI whole disk as the
// kernel sees it, without target matching -- publish carries no DSM context.
// It is the last line of defense against a staging mount whose backing device
// is the node's own disk: the kubelet can skip NodeStageVolume when its cached
// state says "already staged", and bind whatever the staging path holds.
func requireIscsiDisk(devPath, sysfs string) error {
	real, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return fmt.Errorf("device %s does not resolve: %v", devPath, err)
	}
	names := []string{filepath.Base(real)}
	if strings.HasPrefix(names[0], "dm-") {
		slaves, err := os.ReadDir(filepath.Join(sysfs, "block", names[0], "slaves"))
		if err != nil || len(slaves) == 0 {
			return fmt.Errorf("multipath device %s has no verifiable slaves", real)
		}
		names = names[:0]
		for _, s := range slaves {
			names = append(names, s.Name())
		}
	}
	for _, name := range names {
		canonical, err := filepath.EvalSymlinks(filepath.Join(sysfs, "block", name, "device"))
		if err != nil {
			return fmt.Errorf("no kernel device for %s (%s): %v -- refusing a staging "+
				"mount not backed by an iSCSI disk", devPath, name, err)
		}
		if sessionRe.FindStringSubmatch(canonical) == nil {
			return fmt.Errorf("%s (%s) is not an iSCSI disk (sysfs path %s) -- refusing "+
				"the staging mount", devPath, name, canonical)
		}
	}
	return nil
}
