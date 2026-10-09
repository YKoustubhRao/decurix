#!/bin/sh
# Takes the app's WSL distro back to "nobody has used Decurix here" while
# keeping what is only a download or a build: the worker images, the shared
# tools and cache volumes, and the models the bundled Ollama pulled. Deleted:
# every container (the workers, the control plane, Ollama's container) and
# every other volume - decurix-data (the records, chats, skills learned,
# settings saved in the dashboard) and each worker's home. Run as root by
# scripts/windows/build_app.ps1 -Fresh; unregistering the distro instead threw
# the images away, and the next start spent ten minutes building them again.
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

containers=$(docker ps -aq)
[ -n "$containers" ] && docker rm -f $containers >/dev/null

for volume in $(docker volume ls -q); do
    case " $keep_volumes " in
        *" $volume "*) ;;
        *) docker volume rm -f "$volume" >/dev/null ;;
    esac
done

echo "decurix-reset: kept images: $(docker images --format '{{.Repository}}:{{.Tag}}' | grep -c .), volumes: $(docker volume ls -q | tr '\n' ' ')"

# A daemon started only for this is stopped again; the app starts its own.
if [ -n "$started" ]; then
    pkill -x dockerd
fi
exit 0
