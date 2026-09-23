#!/usr/bin/env bash
# Example pass-through launcher for Claude Code.
#
# amux supplies the model, permission policy, resume flags and prompt as separate
# arguments. A wrapper must preserve those boundaries, including on restart.
# Copy this file to the path below and make it executable before opting in:
#
#     export AMUX_CLAUDE_BIN="$HOME/.config/amux/claude-launch.sh"
#
# amux sets, on the agent's window:
#   AMUX_MODE       task | loop
#   AMUX_WORKGROUP  the agent's own amux id (AMUX_WORKSPACE = back-compat alias)
#   AMUX_ROOT       the id of the workgroup it belongs to
#   AMUX_AGENT      the agent kind (claude)
#
# Customize host launch settings through AMUX_PERMISSION_MODE before starting
# the daemon. AMUX_MODE is intent metadata; use Claude's native conversation
# commands or an explicit task prompt to request a loop. Do not turn "$*" into
# a prompt: it would consume amux's --resume/--model/--permission-mode flags.
# Ensure `claude` on PATH resolves to the real binary, not this wrapper.
set -euo pipefail

exec claude "$@"
