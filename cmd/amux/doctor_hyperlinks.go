package main

import (
	"os/exec"
	"strings"
)

// tmuxHyperlinksFix is the line that makes tmux forward OSC 8 hyperlinks to the
// outer terminal. tmux strips OSC 8 unless the client's terminal-features
// include "hyperlinks", and its built-in defaults don't add it for xterm*
// (what iTerm2 and most terminals report), so stock tmux drops every link.
const tmuxHyperlinksFix = `(tmux ≥ 3.4) run: tmux set -as terminal-features ",*:hyperlinks" — add the same line, set -as terminal-features ",*:hyperlinks", to ~/.tmux.conf — then re-attach (detach, tmux attach)`

// checkHyperlinks reports whether the OSC 8 hyperlinks agents print (Claude
// Code and Codex link URLs and file paths) can reach the outer terminal, where
// Cmd/Ctrl-click opens them. amux re-encodes each link faithfully; the layer
// it cannot control is a tmux the TUI runs inside. clientFeatures returns the
// attached tmux client's active terminal features (#{client_termfeatures}).
// Returns the lines to print, or nil when amux is not inside tmux.
func checkHyperlinks(getenv func(string) string, clientFeatures func() (string, error)) []string {
	if getenv("TMUX") == "" {
		return nil
	}
	feats, err := clientFeatures()
	if err != nil {
		return []string{
			"  · links     inside tmux, but its client features are unreadable",
			"              if Cmd-clicking agent links does nothing: " + tmuxHyperlinksFix,
		}
	}
	for _, f := range strings.Split(feats, ",") {
		if strings.TrimSpace(f) == "hyperlinks" {
			return []string{"  ✓ links     tmux forwards OSC 8 hyperlinks — agent links are clickable"}
		}
	}
	return []string{
		"  ⚠ links     tmux strips OSC 8 hyperlinks — agent URLs and file links aren't clickable",
		"              fix: " + tmuxHyperlinksFix,
	}
}

// tmuxClientFeatures asks the tmux server amux runs under for the active
// terminal features of the client attached to this pane's session.
func tmuxClientFeatures() (string, error) {
	out, err := exec.Command("tmux", "display-message", "-p", "#{client_termfeatures}").Output()
	return strings.TrimSpace(string(out)), err
}
