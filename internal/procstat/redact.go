package procstat

import (
	"regexp"
	"strings"
)

// redacted replaces a secret in a published command line.
const redacted = "<redacted>"

// secretWord names a credential-bearing flag, key or header.
const secretWord = `(?:token|secret|passw(?:or)?d|pwd|api[-_]?key|access[-_]?key|private[-_]?key|auth(?:orization)?|bearer|credentials?|cookie|session[-_]?key)`

var (
	// --github-token, -password, --api_key: the NEXT argument is the secret.
	secretFlag = regexp.MustCompile(`(?i)^-{1,2}[\w.-]*` + secretWord + `[\w.-]*$`)
	// token=…, --password=…, Authorization: Bearer …, X-Api-Key:… : the
	// value after the separator is the secret.
	secretAssign = regexp.MustCompile(`(?i)([\w.-]*` + secretWord + `[\w.-]*\s*[=:]\s*)\S.*`)
	// Well-known token shapes anywhere in an argument (a URL, a header).
	secretShape = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{8,}|github_pat_[A-Za-z0-9_]{8,}|sk-[A-Za-z0-9_-]{16,}|xox[abprs]-[A-Za-z0-9-]{8,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+)`)
	// scheme://user:password@host
	urlPassword = regexp.MustCompile(`(://[^/\s:@]+:)[^/\s@]+@`)
)

// redactArgv returns argv with the values of secret-looking flags, key=value
// pairs, headers and well-known token shapes replaced. It is applied to every
// command line the sampler publishes (Process.Cmd), which reaches `amux top
// --json` and `amux status --json`; attribution uses the raw argv, which never
// leaves the sampler. It errs towards over-redacting: a --password-file path
// is hidden too.
func redactArgv(argv []string) []string {
	out := make([]string, len(argv))
	redactNext := false
	for i, arg := range argv {
		if redactNext {
			out[i] = redacted
			redactNext = false
			continue
		}
		if secretFlag.MatchString(arg) || strings.EqualFold(arg, "bearer") || strings.EqualFold(arg, "basic") {
			out[i] = arg
			redactNext = true
			continue
		}
		arg = secretAssign.ReplaceAllString(arg, "${1}"+redacted)
		arg = urlPassword.ReplaceAllString(arg, "${1}"+redacted+"@")
		arg = secretShape.ReplaceAllString(arg, redacted)
		out[i] = arg
	}
	return out
}
