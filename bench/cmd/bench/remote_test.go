package main

import (
	"reflect"
	"testing"
)

// The rig is normally two hosts reached over SSH, but a containerised dry run
// reaches them with `docker exec`. The command shape differs, so the template
// carries it rather than the call sites branching on transport.
func TestRemoteArgv(t *testing.T) {
	cases := []struct {
		name     string
		template string
		host     string
		cmd      string
		want     []string
	}{
		{
			name: "ssh", template: "ssh {host}", host: "rig-proxy", cmd: "pgrep -x nginx",
			want: []string{"ssh", "rig-proxy", "pgrep -x nginx"},
		},
		{
			name: "docker", template: "docker exec {host} sh -c", host: "rig-proxy", cmd: "pgrep -x nginx",
			want: []string{"docker", "exec", "rig-proxy", "sh", "-c", "pgrep -x nginx"},
		},
	}
	for _, c := range cases {
		got := newRemote(c.template, c.host).argv(c.cmd)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: argv = %q, want %q", c.name, got, c.want)
		}
	}
}

// The command must survive as ONE argv element. Splitting it would let a
// multi-word remote command be reinterpreted as separate arguments.
func TestRemoteKeepsCommandAsSingleArg(t *testing.T) {
	got := newRemote("ssh {host}", "h").argv("cat /proc/1/stat; echo; echo '---'")
	if len(got) != 3 {
		t.Fatalf("argv = %q, want 3 elements", got)
	}
	if got[2] != "cat /proc/1/stat; echo; echo '---'" {
		t.Errorf("command element = %q, want it unsplit", got[2])
	}
}

// A single-container dry run has no remote host at all; the command must run
// locally rather than gaining a stray empty argument.
func TestRemoteEmptyHostRunsLocally(t *testing.T) {
	got := newRemote("sh -c", "").argv("pgrep -x nginx")
	want := []string{"sh", "-c", "pgrep -x nginx"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestRemoteTemplateWithoutHostPlaceholderStillWorks(t *testing.T) {
	got := newRemote("ssh", "rig-proxy").argv("uptime")
	want := []string{"ssh", "rig-proxy", "uptime"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
}
