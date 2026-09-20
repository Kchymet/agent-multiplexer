package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"amux/internal/credentialbroker"
	"amux/internal/sessionrpc"
)

// Invoked through the daemon-owned `security` alias in a macOS session. There
// is deliberately no host fallback if the signed mailbox is unavailable.
func credentialSecurity(args []string, input io.Reader, output io.Writer) int {
	o, err := credentialbroker.ParseSecurity(args, input)
	if err != nil {
		return 1
	}
	client, err := openRestrictedSessionRPC()
	if err != nil {
		return 1
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var response sessionrpc.Result
	if o.Verb == credentialbroker.Read {
		response, err = client.Query(ctx, sessionrpc.Query{Verb: o.Verb, Fields: o.Fields()})
	} else {
		response, err = client.Action(ctx, sessionrpc.Action{Verb: o.Verb, Fields: o.Fields()})
	}
	if err != nil {
		return 1
	}
	var result credentialbroker.Result
	if json.Unmarshal(response.Body, &result) != nil {
		return 1
	}
	if result.ExitCode != 0 {
		if result.ExitCode < 1 || result.ExitCode > 255 {
			return 1
		}
		return result.ExitCode
	}
	if o.Verb == credentialbroker.Read {
		if _, err := fmt.Fprintln(output, result.Value); err != nil {
			return 1
		}
	}
	return 0
}
