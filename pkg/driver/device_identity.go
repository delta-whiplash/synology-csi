/*
 * Copyright 2025 Synology Inc.
 */

package driver

import (
	"errors"
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

var targetRe = regexp.MustCompile(`^target\d+:\d+:\d+$`)

// ErrSessionNotFound means no iSCSI session carries the wanted target iqn:
// the login did not happen (or died) and has to be redone. Callers classify
// findSessionDevice failures with errors.Is to decide between re-login
// (this) and rescan (ErrLunNotFound).
var ErrSessionNotFound = errors.New("iscsi session not found")

// ErrLunNotFound means a session is logged in to the target but the lun has
// no block device behind it (yet): only a scan is missing. See
// ErrSessionNotFound for the classification contract.
var ErrLunNotFound = errors.New("iscsi lun not found")

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

		// An hctl that is not H:C:T:L carries no comparable lun: -1 skips
		// the lun check instead of failing the identity.
		gotLun, ok := lunOf(filepath.Base(canonical))
		if !ok {
			gotLun = -1
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

// findSessionDevice locates the whole block device the kernel attached to
// (iqn, lun) by walking sysfs -- the kernel's own view, no udev involved.
// This is the discovery counterpart of verifyIscsiDevice: on udev-less nodes
// (Talos) nothing scans a freshly logged-in session, so no device node ever
// appears and the by-path links never come into existence; the only record
// of the login is sysfs itself.
//
// The walk follows the kernel's layout: /sys/class/iscsi_session/<session>/
// holds one entry per logged-in session with the target IQN in targetname;
// that class entry resolves into the devices tree, where the session's
// device directory holds a targetX:Y:Z directory per portal, each holding a
// H:C:T:L directory per LUN, and each LUN directory exposes its disk under
// block/<name>. Returns /dev/<name> of the first block device found for
// (iqn, lun).
//
// The two failures are distinct on purpose, and surfaced as sentinel errors
// so callers classify with errors.Is instead of matching substrings:
// ErrSessionNotFound means the login did not happen (or died) and has to be
// redone; ErrLunNotFound means the login happened but the device is not
// attached yet and only a scan is missing. Callers retry both, but for
// different reasons.
func findSessionDevice(iqn string, lun int, sysfs string) (string, error) {
	classDir := filepath.Join(sysfs, "class", "iscsi_session")
	sessions, err := os.ReadDir(classDir)
	if err != nil {
		return "", fmt.Errorf("couldn't list the iSCSI sessions under %s: %v", classDir, err)
	}
	var matched []string
	for _, s := range sessions {
		data, err := os.ReadFile(filepath.Join(classDir, s.Name(), "targetname"))
		if err != nil {
			// A session mid-login or mid-teardown may carry no targetname
			// yet; it is not ours to judge, keep looking.
			continue
		}
		if strings.TrimSpace(string(data)) == iqn {
			matched = append(matched, s.Name())
		}
	}
	if len(matched) == 0 {
		return "", fmt.Errorf("%w for target[%s] under %s -- the login did not "+
			"happen or is already gone", ErrSessionNotFound, iqn, classDir)
	}

	// One iqn is routinely logged in on several sessions -- one per portal,
	// each with its own SCSI host -- and the kernel scans them
	// independently: the lun can already be attached on another session
	// while this one has not been scanned yet. Walk them all; only a walk
	// that came up empty on every session gets to say the lun is absent.
	for _, session := range matched {
		dev, err := sessionDevice(session, iqn, lun, classDir)
		if err == nil {
			return dev, nil
		}
		if !errors.Is(err, ErrLunNotFound) {
			// This session's sysfs cannot be read, which is not evidence
			// that the lun is absent; report it instead of guessing.
			return "", err
		}
	}
	return "", fmt.Errorf("%w: none of the %d session(s) logged in to target[%s] "+
		"carries a block device for lun %d -- the device is not attached (yet)",
		ErrLunNotFound, len(matched), iqn, lun)
}

// sessionDevice walks one session's subtree -- class entry resolved into the
// devices tree, then targetX:Y:Z, then H:C:T:L, then the lun's disk under
// block/ -- and returns /dev/<name> of the first block device found, or an
// ErrLunNotFound-wrapped error if this session does not carry the lun.
func sessionDevice(session, iqn string, lun int, classDir string) (string, error) {
	// The class entry is a symlink into the devices tree:
	// .../<host>/<session>/iscsi_session/<session>. The session's device
	// directory -- the one holding the targetX:Y:Z entries -- sits two
	// levels up.
	canonical, err := filepath.EvalSymlinks(filepath.Join(classDir, session))
	if err != nil {
		return "", fmt.Errorf("couldn't resolve the sysfs path of session %s (target[%s]): %v",
			session, iqn, err)
	}
	// Assert the shape before deriving sessionDir by Dir(Dir(...)): a class
	// entry that resolves anywhere else would send the walk into an
	// invented parent and come back as a misleading "no device for lun",
	// sending the caller rescanning a login that is fine.
	sessionDir := filepath.Dir(filepath.Dir(canonical))
	if filepath.Base(canonical) != session ||
		filepath.Base(filepath.Dir(canonical)) != "iscsi_session" ||
		filepath.Base(sessionDir) != session {
		return "", fmt.Errorf("session %s resolves to %s, which is not the shape "+
			".../<session>/iscsi_session/<session> -- unexpected sysfs layout, refusing to guess",
			session, canonical)
	}

	targets, err := os.ReadDir(sessionDir)
	if err != nil {
		return "", fmt.Errorf("couldn't list the targets of session %s (target[%s]): %v",
			session, iqn, err)
	}
	for _, tgt := range targets {
		if !targetRe.MatchString(tgt.Name()) {
			continue
		}
		lunDirs, err := os.ReadDir(filepath.Join(sessionDir, tgt.Name()))
		if err != nil {
			// Target mid-teardown; a live one will match elsewhere or the
			// walk below reports the lun as missing.
			continue
		}
		for _, h := range lunDirs {
			gotLun, ok := lunOf(h.Name())
			if !ok || gotLun != lun {
				continue
			}
			blocks, err := os.ReadDir(filepath.Join(sessionDir, tgt.Name(), h.Name(), "block"))
			if err != nil {
				// The lun exists but carries no disk (yet): exactly the
				// window this function exists for. Keep looking, report
				// once the whole session has been walked.
				continue
			}
			if len(blocks) > 0 {
				// ReadDir sorts by name, so the pick is deterministic.
				return filepath.Join("/dev", blocks[0].Name()), nil
			}
		}
	}
	return "", fmt.Errorf("%w: session %s carries no block device for lun %d",
		ErrLunNotFound, session, lun)
}

// lunOf parses the trailing LUN out of an H:C:T:L sysfs directory name.
func lunOf(hctl string) (int, bool) {
	parts := strings.Split(hctl, ":")
	if len(parts) != 4 {
		return -1, false
	}
	v, err := strconv.Atoi(parts[3])
	if err != nil {
		return -1, false
	}
	return v, true
}
