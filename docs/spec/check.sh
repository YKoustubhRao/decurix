#!/bin/sh
# Model-check the interaction specs with TLC, one property per run, so that every violation gets its own
# shortest counterexample (breadth-first search with one worker). Needs Java 11+ and tla2tools.jar in this
# folder; README.md gives the Docker command. Results: out/<run>.txt, and a summary on stdout.
#
#   sh check.sh            every run
#   sh check.sh Interaction ApprovalIntegrity     one property of one module
#
# TLA_JAR (default tla2tools.jar), TLC_HEAP (default 1200m) and TLC_FLAGS (default -noGenerateSpecTE, which TLC
# 1.7.x does not know: set it to "" there) can be set from outside.
set -u
cd "$(dirname "$0")"
mkdir -p out
HEAP=${TLC_HEAP:-1200m}
JAR=${TLA_JAR:-tla2tools.jar}
FLAGS=${TLC_FLAGS--noGenerateSpecTE}

tlc() { # cfg module -> out file
    rm -rf "out/states-$1"
    # shellcheck disable=SC2086
    java -XX:+UseParallelGC -Xmx"$HEAP" -cp "$JAR" tlc2.TLC -workers 1 $FLAGS \
        -metadir "out/states-$1" -config "out/$1.cfg" "$2.tla" > "out/$1.txt" 2>&1
    rm -rf "out/states-$1"
    if grep -q "is violated" "out/$1.txt"; then v=VIOLATION
    elif grep -q "Temporal properties were violated" "out/$1.txt"; then v=VIOLATION
    elif grep -q "Deadlock reached" "out/$1.txt"; then v=DEADLOCK
    elif grep -q "No error has been found" "out/$1.txt"; then v=PASS
    else v=ERROR; fi
    states=$(grep -E "states generated" "out/$1.txt" | tail -1 | sed 's/^[^0-9]*//')
    depth=$(grep -E "depth of the complete state graph search is|The depth of" "out/$1.txt" | tail -1 | sed 's/.* is //')
    len=$(grep -cE "^State [0-9]+:" "out/$1.txt")
    printf '%-46s %-9s trace=%-3s depth=%-4s %s\n' "$1" "$v" "$len" "$depth" "$states"
}

safety() { # module base-cfg name invariant [sed edit]
    sed '/^INVARIANTS/,$d' "$2" > "out/$3.cfg"
    if [ -n "${5:-}" ]; then sed -i "$5" "out/$3.cfg"; fi
    printf 'INVARIANT\n    %s\n' "$4" >> "out/$3.cfg"
    # The property's VIEW (Interaction.tla, end): the control state and the ghost fields it reads.
    if grep -q "^V_$4 " "$1.tla"; then printf 'VIEW\n    V_%s\n' "$4" >> "out/$3.cfg"; fi
    tlc "$3" "$1"
}

live() { # module base-cfg name [sed edit]
    cp "$2" "out/$3.cfg"
    if [ -n "${4:-}" ]; then sed -i "$4" "out/$3.cfg"; fi
    tlc "$3" "$1"
}

ONLY_MODULE=${1:-}
ONLY_PROP=${2:-}

want() { # module property
    [ -z "$ONLY_MODULE" ] || [ "$ONLY_MODULE" = "$1" ] || return 1
    [ -z "$ONLY_PROP" ] || [ "$ONLY_PROP" = "$2" ] || return 1
    return 0
}

for p in TypeOK NoDeadlock ApprovalIntegrity AtMostOneAskPerAction DoubtsAccountedFor RefusalScopedToAction \
         BareRefusalEndsTask RefusedActionNotReasked BoundedQuestions NoAnswerLost TransientNotFatal StopHonoured; do
    if want Interaction "$p"; then
        safety Interaction Interaction.cfg "Interaction-$p" "$p"
        # The same, with a worker model that never re-sends an action the person declined in its job: what is left
        # is the harness's own doing.
        safety Interaction Interaction.cfg "Interaction-$p-obedient" "$p" 's/ObedientModel = FALSE/ObedientModel = TRUE/'
    fi
done
if want Interaction Termination; then
    live Interaction InteractionLive.cfg "Interaction-Live"
    live Interaction InteractionLive.cfg "Interaction-Live-noScreenDoubt" 's/ScreenRequest = TRUE/ScreenRequest = FALSE/'
fi
for p in TypeOK NoDeadlock NoAnswerLost RightQuestionShown StopOnlyByPerson; do
    if want PlannedRun "$p"; then safety PlannedRun PlannedRun.cfg "PlannedRun-$p" "$p"; fi
done
if want PlannedRun AnswerApplied; then live PlannedRun PlannedRunLive.cfg "PlannedRun-Live"; fi
