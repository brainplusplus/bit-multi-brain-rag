#!/bin/sh
# rancher-nvidia-install.sh
# ─────────────────────────────────────────────────────────────────────────────
# Install NVIDIA Container Toolkit on Rancher Desktop's Alpine WSL distro.
#
# Run inside `wsl -d rancher-desktop` as root.
#
# This script is idempotent — safe to re-run after `Reset Kubernetes` or full
# Rancher Desktop reinstall (which wipes the distro).
#
# What it does:
#   1. Installs sgerrand alpine-pkg-glibc 2.35-r1 (glibc shim)
#   2. Symlinks real glibc ld-linux-x86-64.so.2 into /lib /lib64 /sbin
#   3. Extracts NVIDIA Container Toolkit 1.19.1 (Ubuntu 18.04 .debs)
#   4. Installs nvidia-ctk + nvidia-container-runtime + nvidia-cdi-hook
#      to /usr/local/bin, libnvidia-container.so.1 to /usr/local/lib
#   5. Generates CDI spec at /var/run/cdi/nvidia.yaml
#   6. Configures dockerd with nvidia runtime + CDI feature
#   7. Restarts dockerd
#   8. Tests: docker run --runtime=nvidia nvidia/cuda:12.4.0 nvidia-smi
#
# NOTE: nvidia-container-cli (the C binary) does NOT work on Alpine because of
# libseccomp IFUNC/glibc ABI conflicts. We use CDI mode which doesn't need it.
# ─────────────────────────────────────────────────────────────────────────────

set -e

NVIDIA_VERSION="${NVIDIA_VERSION:-1.19.1}"
GLIBC_VERSION="${GLIBC_VERSION:-2.35-r1}"
WORKDIR="${WORKDIR:-/opt/nvidia-ctk-install}"

log() { printf "\n\033[1;36m[%s]\033[0m %s\n" "$(date +%H:%M:%S)" "$*"; }
die() { printf "\n\033[1;31m[FATAL]\033[0m %s\n" "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "Must run as root (sudo or root shell)"

# ─── 1. Install build deps ──────────────────────────────────────────────────
log "Installing prerequisites (binutils, jq, wget)"
apk add --no-cache binutils jq wget tar >/dev/null

# ─── 2. Install sgerrand glibc shim ─────────────────────────────────────────
if [ ! -d /usr/glibc-compat ]; then
    log "Installing sgerrand alpine-pkg-glibc $GLIBC_VERSION"
    wget -q "https://alpine-pkgs.sgerrand.com/sgerrand.rsa.pub" \
        -O /etc/apk/keys/sgerrand.rsa.pub \
        || wget -q "https://raw.githubusercontent.com/sgerrand/alpine-pkg-glibc/master/sgerrand.rsa.pub" \
            -O /etc/apk/keys/sgerrand.rsa.pub
    wget -q "https://github.com/sgerrand/alpine-pkg-glibc/releases/download/$GLIBC_VERSION/glibc-$GLIBC_VERSION.apk" -O /tmp/glibc.apk
    wget -q "https://github.com/sgerrand/alpine-pkg-glibc/releases/download/$GLIBC_VERSION/glibc-bin-$GLIBC_VERSION.apk" -O /tmp/glibc-bin.apk
    apk add --no-cache --allow-untrusted /tmp/glibc.apk /tmp/glibc-bin.apk 2>&1 | tail -5 || true
    rm -f /tmp/glibc.apk /tmp/glibc-bin.apk
else
    log "glibc-compat already installed at /usr/glibc-compat"
fi

# ─── 3. Symlink real glibc ld-linux into standard paths ─────────────────────
log "Symlinking glibc ld-linux into /lib /lib64 /sbin"
ln -sf /usr/glibc-compat/lib/ld-linux-x86-64.so.2 /lib/ld-linux-x86-64.so.2
mkdir -p /lib64 /sbin
ln -sf /usr/glibc-compat/lib/ld-linux-x86-64.so.2 /lib64/ld-linux-x86-64.so.2
ln -sf /usr/glibc-compat/sbin/ldconfig /sbin/ldconfig

# ─── 4. Restore musl loader path (critical — Rancher overwrites this) ──────
log "Ensuring musl loader has full path"
if ! grep -q '/usr/local/lib' /etc/ld-musl-x86_64.path 2>/dev/null; then
    echo '/lib:/usr/lib:/usr/local/lib:/usr/lib/wsl/lib' > /etc/ld-musl-x86_64.path
fi

# ─── 5. Configure glibc ld.so.conf ─────────────────────────────────────────
log "Configuring glibc ld.so.conf with NVIDIA + WSL paths"
cat > /usr/glibc-compat/etc/ld.so.conf <<'EOF'
/usr/local/lib
/usr/lib/wsl/lib
/usr/glibc-compat/lib
/lib
/usr/lib
EOF
ln -sf /usr/glibc-compat/etc/ld.so.cache /etc/ld.so.cache 2>/dev/null || true
/usr/glibc-compat/sbin/ldconfig 2>/dev/null || true

# ─── 6. Download + extract NVIDIA Container Toolkit ────────────────────────
mkdir -p "$WORKDIR"
TARBALL="$WORKDIR/release-v${NVIDIA_VERSION}-stable.tar.gz"
if [ ! -f "$TARBALL" ]; then
    log "Downloading NVIDIA Container Toolkit $NVIDIA_VERSION"
    wget -q "https://github.com/NVIDIA/nvidia-container-toolkit/releases/download/v${NVIDIA_VERSION}/release-v${NVIDIA_VERSION}-stable.tar.gz" \
        -O "$TARBALL"
fi
EXTRACTED="$WORKDIR/release-v${NVIDIA_VERSION}-stable"
if [ ! -d "$EXTRACTED" ]; then
    log "Extracting tarball"
    tar xzf "$TARBALL" -C "$WORKDIR"
fi

DEBS_DIR="$EXTRACTED/packages/ubuntu18.04/amd64"
[ -d "$DEBS_DIR" ] || die "Missing $DEBS_DIR after extraction"

log "Extracting deb packages"
NVIDIA_FILES_DIR=/opt/nvidia-extracted
mkdir -p "$NVIDIA_FILES_DIR"
for deb in \
    libnvidia-container1_${NVIDIA_VERSION}-1_amd64.deb \
    libnvidia-container-tools_${NVIDIA_VERSION}-1_amd64.deb \
    nvidia-container-toolkit-base_${NVIDIA_VERSION}-1_amd64.deb \
    nvidia-container-toolkit_${NVIDIA_VERSION}-1_amd64.deb; do
    cd /tmp && rm -rf debx && mkdir debx && cd debx
    ar x "$DEBS_DIR/$deb"
    tar xf data.tar.* -C "$NVIDIA_FILES_DIR/"
done

# ─── 7. Install binaries + libs to /usr/local ──────────────────────────────
log "Installing nvidia binaries and libraries"
cp -f "$NVIDIA_FILES_DIR"/usr/bin/* /usr/local/bin/
chmod +x /usr/local/bin/nvidia-*
mkdir -p /usr/local/lib
cp -f "$NVIDIA_FILES_DIR"/usr/lib/x86_64-linux-gnu/libnvidia-container.so.${NVIDIA_VERSION} /usr/local/lib/
cp -f "$NVIDIA_FILES_DIR"/usr/lib/x86_64-linux-gnu/libnvidia-container-go.so.${NVIDIA_VERSION} /usr/local/lib/
ln -sf /usr/local/lib/libnvidia-container.so.${NVIDIA_VERSION} /usr/local/lib/libnvidia-container.so.1
ln -sf /usr/local/lib/libnvidia-container.so.${NVIDIA_VERSION} /usr/local/lib/libnvidia-container.so
ln -sf /usr/local/lib/libnvidia-container-go.so.${NVIDIA_VERSION} /usr/local/lib/libnvidia-container-go.so.1
ln -sf /usr/local/lib/libnvidia-container-go.so.${NVIDIA_VERSION} /usr/local/lib/libnvidia-container-go.so
/usr/glibc-compat/sbin/ldconfig 2>/dev/null || true

# ─── 8. Verify Go binaries (skip the C cli that segfaults on Alpine) ───────
log "Verifying binaries"
/usr/local/bin/nvidia-ctk --version || die "nvidia-ctk broken"
/usr/local/bin/nvidia-container-runtime --version || die "nvidia-container-runtime broken"
/usr/local/bin/nvidia-cdi-hook --version || die "nvidia-cdi-hook broken"

# ─── 9. Generate CDI spec ──────────────────────────────────────────────────
log "Generating CDI spec for GPU"
mkdir -p /var/run/cdi /etc/cdi
/usr/local/bin/nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml 2>&1 | tail -3
/usr/local/bin/nvidia-ctk cdi list

# ─── 10. Configure dockerd ─────────────────────────────────────────────────
log "Configuring dockerd with nvidia runtime + CDI"
DAEMON=/etc/docker/daemon.json
[ -f "$DAEMON" ] || echo '{}' > "$DAEMON"
/usr/local/bin/nvidia-ctk runtime configure --runtime=docker --cdi.enabled --config="$DAEMON"

# Preserve containerd-snapshotter feature that Rancher needs
jq '.features."containerd-snapshotter" = true' "$DAEMON" > /tmp/daemon.json.new
mv /tmp/daemon.json.new "$DAEMON"
cat "$DAEMON"

# ─── 11. Restart dockerd ───────────────────────────────────────────────────
log "Restarting dockerd"
rc-service docker stop 2>&1 || true
sleep 2
pkill -9 -f 'dockerd|docker-proxy|wsl-helper.*docker' 2>/dev/null || true
sleep 1
rm -f /var/run/docker.pid
rc-service docker start
sleep 8

# ─── 12. Verify ────────────────────────────────────────────────────────────
log "Verifying nvidia runtime registered"
if docker info 2>/dev/null | grep -q 'nvidia'; then
    echo "✅ nvidia runtime registered"
else
    die "nvidia runtime NOT registered"
fi

log "Done! Test with:"
echo "  docker run --rm --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES=all \\"
echo "      nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi"
