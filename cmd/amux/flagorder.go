package main

import "flag"

// parseFlagsAnyOrder parses fs over args in which flags and positional operands
// may appear in any order, and returns the operands in the order given.
//
// Go's flag package stops at the first non-flag argument: everything after it is
// left in fs.Args() unparsed. For a command whose operand comes first in the
// obvious phrasing — `amux provide host:7443 --ca ca.pem` — that means the flags
// after it are silently dropped, which surfaces later as an unrelated failure:
// a --ca that was never read looks exactly like a bad certificate.
// So parse in rounds: hand the flag package what it will take, set the operand it
// stopped on aside, and hand it the rest. Flags stay flags wherever they sit, and
// a genuinely unknown one still errors instead of being ignored.
//
// An explicit "--" ends flag parsing, as always: everything after it is an
// operand even if it looks like a flag.
func parseFlagsAnyOrder(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var operands []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		operands = append(operands, rest[0])
		args = rest[1:]
	}
	return append(operands, tail...), nil
}
