#!/sbin/openrc-run
# /etc/init.d/nvidia-cdi
# Regenerates CDI spec on every boot (because /var/run is tmpfs).
# Install: cp this to /etc/init.d/nvidia-cdi && chmod +x && rc-update add nvidia-cdi default

description="Regenerate NVIDIA CDI spec at boot"

depend() {
    need localmount
    before docker
}

start() {
    ebegin "Regenerating NVIDIA CDI spec"
    mkdir -p /var/run/cdi /etc/cdi
    # Ensure ldconfig symlink (CDI hook needs /sbin/ldconfig)
    [ -L /sbin/ldconfig ] || ln -sf /usr/glibc-compat/sbin/ldconfig /sbin/ldconfig
    # Ensure musl path is sane (Rancher may regenerate)
    grep -q '/usr/local/lib' /etc/ld-musl-x86_64.path 2>/dev/null \
        || echo '/lib:/usr/lib:/usr/local/lib:/usr/lib/wsl/lib' > /etc/ld-musl-x86_64.path
    # Generate CDI spec
    /usr/local/bin/nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml 2>&1 \
        | grep -E 'level=(warn|error|info msg=.Generated)' | tail -5
    eend $?
}

stop() {
    return 0
}
