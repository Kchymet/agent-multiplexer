# amux shell shim — source this from ~/.zshrc (and/or ~/.bashrc).
#
#   [ -f "$HOME/.config/amux/amux.sh" ] && . "$HOME/.config/amux/amux.sh"
#
# On an interactive shell that is NOT already inside tmux, it replaces the shell
# with the amux native TUI. Escape hatches:
#   AMUX_SKIP=1   -> never auto-launch (plain shell)
#
# It is intentionally defensive: if anything is missing it silently does nothing
# so it can never lock you out of a shell.

_amux_autostart() {
  # Only for interactive shells.
  case $- in
    *i*) ;;
    *) return ;;
  esac

  # Respect opt-out and don't nest inside tmux (run `amux` by hand there).
  [ -n "$AMUX_SKIP" ] && return
  [ -n "$TMUX" ] && return

  # Need the binary and a real terminal.
  command -v amux >/dev/null 2>&1 || return
  [ -t 1 ] || return

  exec amux
}

_amux_autostart
