#!/usr/bin/env bash
BOLD="\033[1m"
GREEN="\033[32m"
CYAN="\033[36m"
RESET="\033[0m"

echo -e "${BOLD}${CYAN}=== System RAM & KSM Memory Deduplication Report ===${RESET}"
free -h
echo ""
KSM="/sys/kernel/mm/ksm"
if [ -d "${KSM}" ] && [ "$(cat ${KSM}/run 2>/dev/null)" = "1" ]; then
    PAGES_SHARED=$(cat ${KSM}/pages_shared 2>/dev/null || echo 0)
    PAGES_SHARING=$(cat ${KSM}/pages_sharing 2>/dev/null || echo 0)
    PAGES_UNSHARED=$(cat ${KSM}/pages_unshared 2>/dev/null || echo 0)
    
    # Standard 4096 bytes per page
    SAVINGS_MB=$(( PAGES_SHARING * 4096 / 1024 / 1024 ))
    
    echo -e "${BOLD}KSM Status:${RESET} ${GREEN}ACTIVE${RESET}"
    echo -e "  • Shared pages in memory:   ${PAGES_SHARED}"
    echo -e "  • Duplicate pages merged:   ${PAGES_SHARING}"
    echo -e "  • Unshared candidate pages: ${PAGES_UNSHARED}"
    echo -e "  • ${BOLD}${GREEN}Direct RAM Saved by Deduplication: ${SAVINGS_MB} MB${RESET}"
else
    echo -e "${BOLD}KSM Status:${RESET} Not running or not enabled on this kernel."
fi
