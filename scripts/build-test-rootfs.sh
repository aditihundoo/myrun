#!/bin/sh
# Builds a minimal busybox-based rootfs at ./rootfs-minimal for testing
# myrun locally, without needing to pull a real image from a registry.
#
# This exists purely so Milestone 1 (namespaces + chroot) can be tested
# end-to-end before Milestone 3 adds real OCI image support. On a machine
# with internet access to Docker Hub, you'd instead pull and unpack a real
# alpine image -- see internal/image (Milestone 3) for that path.
set -e

ROOTFS_DIR="$(dirname "$0")/../rootfs-minimal"
mkdir -p "$ROOTFS_DIR/bin" "$ROOTFS_DIR/proc" "$ROOTFS_DIR/etc" "$ROOTFS_DIR/tmp"

BUSYBOX_BIN="$(command -v busybox)"
if [ -z "$BUSYBOX_BIN" ]; then
    echo "busybox not found on PATH -- install busybox-static first" >&2
    exit 1
fi

cp "$BUSYBOX_BIN" "$ROOTFS_DIR/bin/busybox"

# Symlink the common applets busybox provides, so /bin/sh, /bin/ls etc.
# all work inside the chroot.
for applet in sh ls ps cat echo mount umount hostname ps ps mkdir sleep; do
    ln -sf busybox "$ROOTFS_DIR/bin/$applet"
done

echo "Built minimal rootfs at $ROOTFS_DIR"
