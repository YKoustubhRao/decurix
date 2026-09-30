#!/bin/bash
# Shared by the Linux build scripts. WSL has a VM-wide memory limit; keep
# BuildKit inside a smaller container and run one build step at a time.

configure_build_resources() {
    DECURIX_BUILD_BUILDER=""

    if ! grep -qi microsoft /proc/sys/kernel/osrelease 2>/dev/null; then
        return 0
    fi

    # Some Docker packages install Buildx outside the CLI plugin directory.
    if ! docker buildx version >/dev/null 2>&1 \
        && [ -f /usr/libexec/docker/cli-plugins/docker-buildx ]; then
        mkdir -p ~/.docker/cli-plugins
        ln -sf /usr/libexec/docker/cli-plugins/docker-buildx \
            ~/.docker/cli-plugins/docker-buildx
    fi
    if ! docker buildx version >/dev/null 2>&1; then
        echo "ERROR: Docker Buildx is required for memory-limited WSL builds." >&2
        return 1
    fi

    local total_kib limit_mib swap_mib
    total_kib="$(awk '/^MemTotal:/ { print $2 }' /proc/meminfo)"
    limit_mib=$((total_kib / 2048))
    if [ "$limit_mib" -gt 2048 ]; then
        limit_mib=2048
    fi
    if [ "$limit_mib" -lt 512 ]; then
        echo "ERROR: WSL has too little RAM available for a Docker build." >&2
        return 1
    fi
    swap_mib=$((limit_mib * 2))

    # Include the limit in the name so a changed WSL memory setting gets a
    # correctly sized builder without discarding the old builder's cache.
    DECURIX_BUILD_BUILDER="decurix-wsl-${limit_mib}m"
    if ! docker buildx inspect "$DECURIX_BUILD_BUILDER" >/dev/null 2>&1; then
        docker buildx create \
            --name "$DECURIX_BUILD_BUILDER" \
            --driver docker-container \
            --driver-opt "memory=${limit_mib}m,memory-swap=${swap_mib}m,default-load=true" \
            --buildkitd-config ./scripts/linux/lib/buildkitd.toml >/dev/null
    fi
    docker buildx inspect "$DECURIX_BUILD_BUILDER" --bootstrap >/dev/null
    export DECURIX_BUILD_BUILDER
    echo "WSL build: one step at a time, with a ${limit_mib} MiB BuildKit memory limit."
}
