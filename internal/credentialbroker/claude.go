package credentialbroker

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/user"
	"regexp"

	"golang.org/x/text/unicode/norm"
)

// ClaudeServices matches Claude's native secure-storage namespace, including
// its exact-string/NFC rules. No caller-supplied directory selects host access.
func ClaudeServices(selector string) []string {
	suffix := ""
	if selector != "" {
		hash := sha256.Sum256([]byte(norm.NFC.String(selector)))
		suffix = fmt.Sprintf("-%x", hash[:4])
	}
	return []string{"Claude Code" + suffix, "Claude Code-credentials" + suffix}
}

var accountPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func HostAccount() string {
	account := os.Getenv("USER")
	if account == "" {
		if current, err := user.Current(); err == nil {
			account = current.Username
		}
	}
	if !accountPattern.MatchString(account) {
		return "claude-code-user"
	}
	return account
}
