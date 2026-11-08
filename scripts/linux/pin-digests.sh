#!/bin/bash
#
# Prints the digest of every image Decurix still takes by tag alone (B-24), as the lines to paste: each Dockerfile
# FROM, and the images the control plane runs itself (the bundled Ollama, the host hop's alpine). It changes no file:
# a pin is moved on purpose, by a person who read what it now points at.
#
#   ./scripts/linux/pin-digests.sh            # needs the docker CLI and the registry (Docker Hub)
#
# A digest is read with `docker buildx imagetools inspect` (the multi-platform index, nothing pulled), or, without
# buildx, by pulling the image and reading its RepoDigests. A reference already pinned (@sha256:), a local image
# (decurix-*), scratch and a build stage's own name are skipped. Exits 1 if any digest could not be read.
#
# Twin: scripts/windows/pin-digests.ps1.

set -u

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
while [ "$root" != "/" ] && [ ! -f "$root/docker/desktop.basic.Dockerfile" ]; do
    root="$(dirname "$root")"
done
if [ ! -f "$root/docker/desktop.basic.Dockerfile" ]; then
    echo "ERROR: no docker/desktop.basic.Dockerfile found above $(dirname "${BASH_SOURCE[0]}")." >&2
    exit 1
fi
cd "$root" || exit 1

if ! docker version >/dev/null 2>&1; then
    echo "ERROR: docker is not reachable; start Docker first." >&2
    exit 1
fi

declare -A digests
digest=""
failed=0

# resolve REF: sets digest to REF's digest (sha256:...), or to nothing when it cannot be read; each REF is asked once.
resolve() {
    local ref=$1 d=""
    if [ -n "${digests[$ref]+x}" ]; then
        digest=${digests[$ref]}
        return
    fi
    d=$(docker buildx imagetools inspect --format '{{.Manifest.Digest}}' "$ref" 2>/dev/null | tr -d '[:space:]')
    case "$d" in
        sha256:*) ;;
        *)
            d=""
            if docker pull -q "$ref" >/dev/null 2>&1; then
                d=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$ref" 2>/dev/null | head -n 1)
                d=${d#*@}
            fi
            case "$d" in sha256:*) ;; *) d="" ;; esac
            ;;
    esac
    digests[$ref]=$d
    digest=$d
}

# pin FILE LINE TEXT OLD [REF [QUOTE]]: prints where, and TEXT with OLD replaced by REF@digest (REF is OLD unless
# given; QUOTE=1 puts the replacement in double quotes, for a Go string).
pin() {
    local file=$1 line=$2 text=$3 old=$4 ref=${5:-$4} quote=${6:-0} d new
    resolve "$ref"
    d=$digest
    echo "# $file:$line"
    if [ -z "$d" ]; then
        echo "#   could not read the digest of $ref"
        failed=1
        return
    fi
    new="$ref@$d"
    [ "$quote" = 1 ] && new="\"$new\""
    echo "${text/"$old"/"$new"}"
}

echo "# Digests read $(date -u +%Y-%m-%dT%H:%M:%SZ). Paste each line over the one named above it."
echo

# Every Dockerfile FROM: the image, its flags (--platform=...) skipped, and not a stage defined above it.
for f in docker/Dockerfile docker/*.Dockerfile desktop/electron/stack/Dockerfile bench/*/*.Dockerfile; do
    [ -f "$f" ] || continue
    stages=" "
    n=0
    while IFS= read -r text || [ -n "$text" ]; do
        n=$((n + 1))
        text=${text%$'\r'}
        set -f
        # shellcheck disable=SC2086 # split into words on purpose
        set -- $text
        set +f
        [ "$#" -ge 2 ] || continue
        case "$1" in [Ff][Rr][Oo][Mm]) ;; *) continue ;; esac
        shift
        while [ "$#" -gt 0 ] && [ "${1#--}" != "$1" ]; do shift; done
        [ "$#" -ge 1 ] || continue
        ref=$1
        if [ "$#" -ge 3 ] && { [ "$2" = "AS" ] || [ "$2" = "as" ]; }; then
            stages="$stages$3 "
        fi
        case "$ref" in *@sha256:*|scratch|decurix-*) continue ;; esac
        case "$stages" in *" $ref "*) continue ;; esac
        pin "$f" "$n" "$text" "$ref"
    done < "$f"
done

# The images the control plane runs itself.
while IFS=: read -r n text; do
    ref=$(printf '%s' "$text" | sed -n 's/.*containerImage *= *"\([^"@]*\)".*/\1/p')
    [ -n "$ref" ] && pin internal/ollama/ollama.go "$n" "$text" "$ref"
done < <(grep -n 'containerImage *= *"' internal/ollama/ollama.go)
# The host hop names "alpine" with no tag at all (latest); its pin takes the tag the control plane's own image uses.
alpine_tag=$(sed -n 's/^FROM \(alpine:[^ @]*\).*/\1/p' docker/Dockerfile | head -n 1)
while IFS=: read -r n text; do
    pin internal/desktop/hop.go "$n" "$text" '"alpine"' "${alpine_tag:-alpine:3.24}" 1
done < <(grep -n '"--pid=host", "alpine"' internal/desktop/hop.go)

exit "$failed"
