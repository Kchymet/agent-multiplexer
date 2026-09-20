package credentialbroker

import (
	"context"
	"encoding/hex"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Keychain executes operations using the host environment.
type Keychain struct{ Env []string }

// Execute runs only a pre-authorized operation. The caller supplies the account
// and service selected from host state, never an arbitrary client command.
// Small updates use hex on stdin; larger updates follow Claude's own argv
// fallback because security's interactive parser has a 4 KiB line limit.
func (k Keychain) Execute(ctx context.Context, o Operation) (Result, error) {
	if runtime.GOOS != "darwin" {
		return Result{}, ErrInvalid
	}
	if _, err := FromFields(o.Verb, o.Fields()); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var args []string
	var input string
	switch o.Verb {
	case Read:
		args = []string{"find-generic-password", "-a", o.Account, "-s", o.Service, "-w"}
	case Delete:
		args = []string{"delete-generic-password", "-a", o.Account, "-s", o.Service}
	case Write:
		args = []string{"-i"}
		quote := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
		input = "add-generic-password -U -a " + quote(o.Account) + " -s " + quote(o.Service) + " -X " + quote(hex.EncodeToString([]byte(o.Value))) + "\n"
		if len(input) > 4032 {
			args = []string{"add-generic-password", "-U", "-a", o.Account, "-s", o.Service, "-X", hex.EncodeToString([]byte(o.Value))}
			input = ""
		}
	default:
		return Result{}, ErrInvalid
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	cmd.Env = k.Env
	cmd.Stdin = strings.NewReader(input)
	// Never relay native errors: security may quote input or credential data.
	var stderr boundedOutput
	cmd.Stderr = &stderr
	var output boundedOutput
	cmd.Stdout = &output
	err := cmd.Run()
	if ctx.Err() != nil {
		return Result{}, errors.New("credential service timed out")
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return Result{ExitCode: exit.ExitCode()}, nil
		}
		return Result{}, errors.New("credential service failed")
	}
	if output.overflow {
		return Result{}, errors.New("credential exceeds transport limit")
	}
	// security -i can exit successfully even when its command failed.
	if o.Verb == Write && strings.Contains(stderr.String(), "SecKeychain") {
		return Result{}, errors.New("credential update failed")
	}
	if o.Verb == Read {
		return Result{Value: strings.TrimSuffix(output.String(), "\n")}, nil
	}
	return Result{}, nil
}

type boundedOutput struct {
	strings.Builder
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > MaxValue+1 {
		b.overflow = true
		return len(p), nil
	}
	return b.Builder.Write(p)
}
