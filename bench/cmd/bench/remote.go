package main

import "strings"

// remote builds the argv that runs a shell command on a rig host.
//
// The normal rig is two machines reached over SSH. A containerised dry run
// reaches them with `docker exec`, which needs an explicit `sh -c`. Rather than
// branching on transport at every call site, the caller supplies a template and
// the command is always appended as a single trailing argument.
//
//	ssh {host}                  -> ssh HOST "<cmd>"
//	docker exec {host} sh -c    -> docker exec HOST sh -c "<cmd>"
//	sh -c        (empty host)   -> sh -c "<cmd>"      (single-container dry run)
type remote struct {
	prefix []string
}

func newRemote(template, host string) remote {
	fields := strings.Fields(template)
	prefix := make([]string, 0, len(fields)+1)
	substituted := false
	for _, f := range fields {
		if strings.Contains(f, "{host}") {
			f = strings.ReplaceAll(f, "{host}", host)
			substituted = true
		}
		prefix = append(prefix, f)
	}
	// A template with no placeholder (plain "ssh") still needs the host, and
	// putting it directly after the prefix is the only sensible position. An
	// empty host means the command runs locally, as in a single-container dry
	// run, so nothing is appended.
	if !substituted && host != "" {
		prefix = append(prefix, host)
	}
	return remote{prefix: prefix}
}

// argv appends cmd as one element, never split, so a multi-word remote command
// cannot be reinterpreted as separate arguments.
func (r remote) argv(cmd string) []string {
	return append(append([]string(nil), r.prefix...), cmd)
}
