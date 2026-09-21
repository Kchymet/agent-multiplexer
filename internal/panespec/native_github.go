package panespec

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"amux/internal/credentialbroker"

	"github.com/cli/go-gh/v2/pkg/config"
)

func publishNativeAlias(parent, name, target string) error {
	alias := filepath.Join(parent, name)
	if current, err := os.Readlink(alias); err == nil && current == target {
		return nil
	}
	tmp, err := os.CreateTemp(parent, ".alias-*")
	if err != nil {
		return err
	}
	tmp.Close()
	os.Remove(tmp.Name())
	if err := os.Symlink(target, tmp.Name()); err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	return os.Rename(tmp.Name(), alias)
}

func publishNativeGitHub(parent string) error {
	native, err := credentialbroker.NativeGitHub()
	if err != nil {
		// GitHub CLI is optional. Remove stale aliases after a host uninstall.
		for _, name := range []string{"gh", "gh-real", "gitconfig"} {
			if err := os.Remove(filepath.Join(parent, name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	if err := publishNativeAlias(parent, "gh-real", native); err != nil {
		return err
	}
	if err := publishNativeAlias(parent, "gh", "amux"); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var content strings.Builder
	// Preserve Git's normal global config order, then replace configured GitHub
	// helpers (including absolute Homebrew gh paths) with the protected alias.
	fmt.Fprintf(&content, "[include]\n\tpath = %s\n\tpath = %s\n", strconv.Quote(filepath.Join(home, ".config", "git", "config")), strconv.Quote(filepath.Join(home, ".gitconfig")))
	hosts := credentialbroker.GitHubHosts()
	if slices.Contains(hosts, "github.com") {
		hosts = append(hosts, "gist.github.com")
	}
	helper := "!'" + strings.ReplaceAll(filepath.Join(parent, "gh"), "'", "'\\''") + "' auth git-credential"
	for _, host := range hosts {
		fmt.Fprintf(&content, "[credential %s]\n\thelper =\n\thelper = %s\n", strconv.Quote("https://"+host), strconv.Quote(helper))
	}
	tmp, err := os.CreateTemp(parent, ".gitconfig-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0400); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(parent, "gitconfig"))
}

func nativeGitHubEnv(toolDir string) []string {
	if _, err := os.Stat(filepath.Join(toolDir, "gh-real")); err != nil {
		return nil
	}
	env := []string{"GH_CONFIG_DIR=" + config.ConfigDir(), "GIT_CONFIG_GLOBAL=" + filepath.Join(toolDir, "gitconfig")}
	if host := os.Getenv("GH_HOST"); host != "" {
		env = append(env, "GH_HOST="+host)
	}
	return env
}
