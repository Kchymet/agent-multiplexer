// Package credentialbroker defines the narrow native credential operations
// forwarded over amux's authenticated per-session mailbox. It never accepts a
// caller-selected executable, keychain path, or shell command on the host.
package credentialbroker

import (
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	Read   = "credential-read"
	Write  = "credential-write"
	Delete = "credential-delete"
	// Leave room for JSON escaping and the signed RPC envelope's 64 KiB body.
	MaxValue = 24 << 10
)

var ErrInvalid = errors.New("unsupported credential operation")

type Operation struct {
	Verb    string
	Service string
	Account string
	Value   string
}

type Result struct {
	Value    string `json:"value,omitempty"`
	ExitCode int    `json:"exit_code"`
}

func IsVerb(verb string) bool { return verb == Read || verb == Write || verb == Delete }

func (o Operation) Fields() map[string]string {
	fields := map[string]string{"service": o.Service, "account": o.Account}
	if o.Verb == Write {
		fields["value"] = o.Value
	}
	return fields
}

func FromFields(verb string, fields map[string]string) (Operation, error) {
	o := Operation{Verb: verb, Service: fields["service"], Account: fields["account"], Value: fields["value"]}
	want := 2
	if verb == Write {
		want = 3
	}
	if !IsVerb(verb) || len(fields) != want || o.Service == "" || o.Account == "" ||
		len(o.Service) > 256 || len(o.Account) > 256 || len(o.Value) > MaxValue ||
		!utf8.ValidString(o.Value) || strings.ContainsAny(o.Service+o.Account, "\x00\r\n") {
		return Operation{}, ErrInvalid
	}
	for k := range fields {
		if k != "service" && k != "account" && !(k == "value" && verb == Write) {
			return Operation{}, ErrInvalid
		}
	}
	return o, nil
}

// ParseSecurity implements only the commands Claude uses. Interactive mode is
// tokenized as data, accepts exactly one command, and is never passed to a shell.
func ParseSecurity(args []string, input io.Reader) (Operation, error) {
	if len(args) == 1 && args[0] == "-i" {
		b, err := io.ReadAll(io.LimitReader(input, 2*MaxValue+2049))
		if err != nil || len(b) > 2*MaxValue+2048 {
			return Operation{}, ErrInvalid
		}
		args, err = splitCommand(string(b))
		if err != nil {
			return Operation{}, err
		}
	}
	if len(args) == 0 {
		return Operation{}, ErrInvalid
	}
	var verb string
	switch args[0] {
	case "find-generic-password":
		verb = Read
	case "add-generic-password":
		verb = Write
	case "delete-generic-password":
		verb = Delete
	default:
		return Operation{}, ErrInvalid
	}
	fields := map[string]string{}
	seen := map[string]bool{}
	for i := 1; i < len(args); i++ {
		flag := args[i]
		if seen[flag] {
			return Operation{}, ErrInvalid
		}
		seen[flag] = true
		if flag == "-U" && verb == Write {
			continue
		}
		if flag == "-w" && verb == Read {
			continue
		}
		if i+1 == len(args) {
			return Operation{}, ErrInvalid
		}
		i++
		switch flag {
		case "-a":
			fields["account"] = args[i]
		case "-s":
			fields["service"] = args[i]
		case "-X", "-w":
			if verb != Write {
				return Operation{}, ErrInvalid
			}
			if _, ok := fields["value"]; ok {
				return Operation{}, ErrInvalid
			}
			value := args[i]
			if flag == "-X" {
				b, err := hex.DecodeString(value)
				if err != nil {
					return Operation{}, ErrInvalid
				}
				value = string(b)
			}
			fields["value"] = value
		default:
			return Operation{}, ErrInvalid
		}
	}
	if verb == Read && !seen["-w"] {
		return Operation{}, ErrInvalid
	}
	if verb == Write && !seen["-U"] {
		return Operation{}, ErrInvalid
	}
	return FromFields(verb, fields)
}

func splitCommand(s string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	escape, started := false, false
	for _, c := range strings.TrimSpace(s) {
		if c == '\n' || c == '\r' || c == 0 {
			return nil, ErrInvalid
		}
		if escape {
			word.WriteRune(c)
			escape = false
			started = true
			continue
		}
		if c == '\\' {
			escape = true
			started = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				word.WriteRune(c)
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			started = true
			continue
		}
		if unicode.IsSpace(c) {
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		word.WriteRune(c)
		started = true
	}
	if quote != 0 || escape {
		return nil, ErrInvalid
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}
