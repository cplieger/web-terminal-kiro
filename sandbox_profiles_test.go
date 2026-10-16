package main

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// sandboxPrivilegedSyscalls are the calls Docker's default seccomp profile
// allows only with CAP_SYS_ADMIN, or only in a narrow form, because they create
// namespaces or change mounts.
var sandboxPrivilegedSyscalls = []string{
	"clone", "clone3", "fsmount", "fsopen", "fspick", "mount", "mount_setattr",
	"move_mount", "open_tree", "pivot_root", "setns", "umount2", "unshare",
}

// canonicalSeccompRule renders a rule without its comment, with sorted keys, so
// two rules compare by what they allow.
func canonicalSeccompRule(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("seccomp rule %s: %v", raw, err)
	}
	delete(m, "comment")
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSandboxProfiles_seccompAllowsOnlyTheMeasuredNeed locks every rule that
// reaches a namespace or mount syscall without CAP_SYS_ADMIN, so widening the
// shipped profile is a deliberate test change.
func TestSandboxProfiles_seccompAllowsOnlyTheMeasuredNeed(t *testing.T) {
	raw, err := os.ReadFile("sandbox/seccomp.json")
	if err != nil {
		t.Fatal(err)
	}
	var profile struct {
		DefaultAction string            `json:"defaultAction"`
		Syscalls      []json.RawMessage `json:"syscalls"`
	}
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatalf("sandbox/seccomp.json does not parse: %v", err)
	}
	if profile.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Errorf("defaultAction = %q, want Docker's SCMP_ACT_ERRNO", profile.DefaultAction)
	}
	var got []string
	for _, rule := range profile.Syscalls {
		var r struct {
			Includes struct {
				Caps []string `json:"caps"`
			} `json:"includes"`
			Names []string `json:"names"`
		}
		if err := json.Unmarshal(rule, &r); err != nil {
			t.Fatalf("seccomp rule %s: %v", rule, err)
		}
		if slices.Contains(r.Includes.Caps, "CAP_SYS_ADMIN") {
			continue
		}
		if slices.ContainsFunc(r.Names, func(n string) bool { return slices.Contains(sandboxPrivilegedSyscalls, n) }) {
			got = append(got, canonicalSeccompRule(t, rule))
		}
	}
	want := []string{
		// Docker's own: clone with no namespace flag, on both argument orders,
		// and clone3 refused with ENOSYS so libc falls back to clone.
		`{"action":"SCMP_ACT_ALLOW","args":[{"index":0,"op":"SCMP_CMP_MASKED_EQ","value":2114060288}],"excludes":{"arches":["s390","s390x"],"caps":["CAP_SYS_ADMIN"]},"names":["clone"]}`,
		`{"action":"SCMP_ACT_ALLOW","args":[{"index":1,"op":"SCMP_CMP_MASKED_EQ","value":2114060288}],"excludes":{"caps":["CAP_SYS_ADMIN"]},"includes":{"arches":["s390","s390x"]},"names":["clone"]}`,
		`{"action":"SCMP_ACT_ERRNO","errnoRet":38,"excludes":{"caps":["CAP_SYS_ADMIN"]},"names":["clone3"]}`,
		// The sandbox's: exactly NEWNS|NEWUSER|NEWPID with and without NEWNET
		// (0x70020000, 0x30020000), then the mount calls bubblewrap makes.
		`{"action":"SCMP_ACT_ALLOW","args":[{"index":0,"op":"SCMP_CMP_MASKED_EQ","value":2114060288,"valueTwo":1879179264}],"excludes":{"arches":["s390","s390x"]},"names":["clone"]}`,
		`{"action":"SCMP_ACT_ALLOW","args":[{"index":0,"op":"SCMP_CMP_MASKED_EQ","value":2114060288,"valueTwo":805437440}],"excludes":{"arches":["s390","s390x"]},"names":["clone"]}`,
		`{"action":"SCMP_ACT_ALLOW","names":["mount","pivot_root","umount2"]}`,
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("namespace and mount rules without CAP_SYS_ADMIN:\n got  %q\n want %q", got, want)
	}
}

// TestSandboxProfiles_apparmorAllowsOnlyTheMeasuredNeed locks the AppArmor
// profile's mount rules to the ones bubblewrap needs.
func TestSandboxProfiles_apparmorAllowsOnlyTheMeasuredNeed(t *testing.T) {
	raw, err := os.ReadFile("sandbox/web-terminal-kiro-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	var rules []string
	for line := range strings.Lines(string(raw)) {
		rule := strings.TrimSpace(line)
		if rule == "" || strings.HasPrefix(rule, "#") {
			continue
		}
		rules = append(rules, rule)
	}
	t.Run("declares the profile name the docs use", func(t *testing.T) {
		if !slices.ContainsFunc(rules, func(r string) bool {
			return strings.HasPrefix(r, `profile "web-terminal-kiro-sandbox" flags=(attach_disconnected,mediate_deleted) {`)
		}) {
			t.Errorf("no profile \"web-terminal-kiro-sandbox\" declaration in %q", rules)
		}
	})
	t.Run("keeps Docker's ABI and adds no userns or unconfined rule", func(t *testing.T) {
		if !slices.Contains(rules, "abi <abi/3.0>,") {
			t.Errorf("rules = %q, want Docker's abi <abi/3.0>", rules)
		}
		for _, r := range rules {
			if strings.HasPrefix(r, "userns") || strings.Contains(r, "unconfined)") || strings.Contains(r, "complain") {
				t.Errorf("rule %q widens the profile beyond the mount allowances", r)
			}
		}
	})
	t.Run("mount rules are exactly bubblewrap's", func(t *testing.T) {
		var got []string
		for _, r := range rules {
			if strings.HasPrefix(r, "mount") || strings.HasPrefix(r, "remount") ||
				strings.HasPrefix(r, "pivot_root") || strings.HasPrefix(r, "deny mount") || strings.HasPrefix(r, "umount") {
				got = append(got, r)
			}
		}
		want := []string{
			"umount,",
			"mount options in (rslave, silent) -> /,",
			"mount fstype=tmpfs options in (nodev, nosuid, silent) -> /**,",
			"mount fstype=devpts options in (noexec, nosuid, silent) -> /**,",
			"mount options in (bind, rbind, silent) -> /**,",
			"mount options in (rprivate, silent) -> /**,",
			"remount /**,",
			"pivot_root,",
			"deny mount fstype=proc,",
		}
		if !slices.Equal(got, want) {
			t.Errorf("mount rules:\n got  %q\n want %q", got, want)
		}
	})
}
