#!/usr/bin/env bash
# Example agent launcher — AGENT-OWNED autonomy, not amux's.
#
# amux is a UI layer: it launches the agent and exports the session's intent as
# environment variables, but it does NOT decide how the agent runs. This wrapper
# is where the session/agent owns that policy. Opt in by pointing amux at it:
#
#     export AMUX_CLAUDE_BIN="$HOME/.config/amux/claude-launch.sh"
#
# amux sets, on the agent's window:
#   AMUX_MODE       task | loop
#   AMUX_WORKSPACE  the workspace id
#   AMUX_AGENT      the agent kind (claude)
#
# Tune the two knobs below to taste; this is yours to edit.
set -euo pipefail

flags=()
case "${AMUX_MODE:-task}" in
  loop)
    # (2) more autonomous permissions for a long-running, hands-off session.
    flags+=(--permission-mode acceptEdits)
    # (3) drive it as a loop via the /loop skill. amux passes the task as the
    # positional prompt ("$@"); we wrap it so the agent keeps working.
    if [ "$#" -gt 0 ]; then
      exec claude "${flags[@]}" "/loop $*"
    fi
    exec claude "${flags[@]}"
    ;;
  *)
    # task: a normal, single, interactive session seeded with the prompt.
    exec claude "$@"
    ;;
esac
