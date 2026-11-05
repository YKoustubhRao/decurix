#!/bin/sh
# Takes the app's WSL distro back to "nobody has used Decurix here" while
# keeping what is only a download or a build: the worker images, the shared
# tools and cache volumes, and the models the bundled Ollama pulled. Deleted:
# every container (the workers, the control plane, Ollama's container) and
# every other volume - decurix-data (the records, chats, skills learned,
# settings saved in the dashboard) and each worker's home. Run as root by
# scripts/windows/build_app.ps1 -Fresh; unregistering the distro instead threw
# the images away, and the next start spent ten minutes building them again.
#
# It says "clean" only when it has looked and nothing it was to delete is
# left: a removal that fails is counted, the rest still runs, and the script
# ends non-zero naming what is still there.
set -u

keep_volumes='decurix-cache decurix-tools decurix_ollama'

started=''
if ! docker version >/dev/null 2>&1; then
    /opt/decurix/dockerd.sh >/dev/null 2>&1 &
    started=$!
    tries=0
    until docker version >/dev/null 2>&1; do
        tries=$((tries + 1))
        if [ "$tries" -gt 60 ]; then
            echo "decurix-reset: Docker did not start" >&2
            exit 1
        fi
        sleep 1
    done
fi

failed=0
for container in $(docker ps -aq); do
    docker rm -f "$container" >/dev/null || failed=$((failed + 1))
done

for volume in $(docker volume ls -q); do
    case " $keep_volumes " in
        *" $volume "*) ;;
        *) docker volume rm -f "$volume" >/dev/null || failed=$((failed + 1)) ;;
    esac
done

# What is left, looked at rather than assumed.
left_containers=$(docker ps -aq | wc -l | tr -d ' ')
left_volumes=''
for volume in $(docker volume ls -q); do
    case " $keep_volumes " in
        *" $volume "*) ;;
        *) left_volumes="$left_volumes $volume" ;;
    esac
done

kept_images=$(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -c .)

# A daemon started only for this is stopped again; the app starts its own.
if [ -n "$started" ]; then
    pkill -x dockerd
fi

if [ "$failed" -ne 0 ] || [ "$left_containers" != "0" ] || [ -n "$left_volumes" ]; then
    echo "decurix-reset: NOT clean - $failed removal(s) failed; left: $left_containers container(s), volumes:${left_volumes:- none}" >&2
    exit 1
fi
echo "decurix-reset: clean. Kept images: $kept_images, volumes: $keep_volumes"
exit 0
