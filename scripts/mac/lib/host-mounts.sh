#!/bin/bash
#
# Shared by scripts/mac/build.sh and scripts/mac/rebuild.sh. Meant to be
# sourced, not executed: it only defines export_host_mounts.
#
# docker-compose.yaml mounts two host folders into the control plane
# (DECURIX_HOST_HOME at /host-home, DECURIX_HOST_MNT_C at /mnt/c) and
# defaults them to /home and /mnt/c - Linux and WSL paths. A Mac has neither,
# and Docker Desktop for Mac only shares a few folders with its VM (your home
# folder is one, /home and /mnt/c are not), so with those defaults the mount is
# refused or quietly points at nothing.
#
# Your home folder is what the app pickers look in on a Mac, and an empty folder
# stands in for the Windows drive that a Mac does not have.

# export_host_mounts sets DECURIX_HOST_HOME and DECURIX_HOST_MNT_C for compose.
export_host_mounts() {
    local empty="${HOME}/.decurix/no-windows-drive"
    mkdir -p "$empty"
    export DECURIX_HOST_HOME="${HOME}"
    export DECURIX_HOST_MNT_C="$empty"
}
