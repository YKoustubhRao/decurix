#!/bin/bash
#
# Checks the .env handling the Linux and Mac Ollama setup share
# (scripts/{linux,mac}/lib/ollama-setup.sh): values are read as compose reads
# them, written without being mangled, and a fallback is used for one run
# only - it never reaches .env. Needs only bash and awk; run from anywhere:
#
#   bash scripts/testing/ollama-env.test.sh

set -u
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
failures=0

check() {
    if [ "$2" = "$3" ]; then
        echo "ok   $1"
    else
        echo "FAIL $1: got [$2], want [$3]"
        failures=$((failures + 1))
    fi
}

for lib in scripts/linux/lib/ollama-setup.sh scripts/mac/lib/ollama-setup.sh; do
    work="$(mktemp -d)"
    (
        cd "$work" || exit 1
        # shellcheck disable=SC1090
        source "$repo/$lib"
        printf 'A=1\r\nOLLAMA_URL="http://mine:11434"\r\nOLLAMA_MODEL=qwen3:4b # my pick\r\nQ='"'"'quoted'"'"'\r\nB=a|b&c\r\n' > .env
        before="$(cat .env)"

        check "$lib: quoted value, CRLF" "$(read_env_var OLLAMA_URL)" "http://mine:11434"
        check "$lib: trailing comment" "$(read_env_var OLLAMA_MODEL)" "qwen3:4b"
        check "$lib: single quotes" "$(read_env_var Q)" "quoted"
        check "$lib: missing name" "$(read_env_var NOPE)" ""

        run_only OLLAMA_MODEL "qwen2.5-coder:3b" >/dev/null
        run_only OLLAMA_URL "http://decurix-ollama:11434" >/dev/null
        run_only BAD 'a"b' >/dev/null
        write_run_override
        check "$lib: a fallback leaves .env as it was" "$(cat .env)" "$before"
        check "$lib: the run uses the fallback" "$(effective_env_var OLLAMA_MODEL)" "qwen2.5-coder:3b"
        check "$lib: the rest still comes from .env" "$(effective_env_var A)" "1"
        check "$lib: the override names the run-only values" \
            "$(grep -c -e 'OLLAMA_MODEL: "qwen2.5-coder:3b"' -e 'OLLAMA_URL: "http://decurix-ollama:11434"' "$DECURIX_COMPOSE_OVERRIDE")" "2"
        check "$lib: a value with a quote is not used" "$(grep -c BAD "$DECURIX_COMPOSE_OVERRIDE")" "0"
        rm -f "$DECURIX_COMPOSE_OVERRIDE"

        set_env_var B 'x|y&z'
        set_env_var NEW v
        check "$lib: a written value keeps | and &" "$(read_env_var B)" "x|y&z"
        check "$lib: one line per name" "$(grep -c '^B=' .env)" "1"
        check "$lib: a new name is added" "$(read_env_var NEW)" "v"
        exit "$failures"
    )
    failures=$((failures + $?))
    rm -rf "$work"
done

if [ "$failures" -ne 0 ]; then
    echo "$failures check(s) failed"
    exit 1
fi
echo "all checks passed"
