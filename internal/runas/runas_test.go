package runas

import (
	"slices"
	"testing"
)

func TestParseTarget(t *testing.T) {
	uid, gid, err := parseTarget("65532:65532")
	if err != nil || uid != 65532 || gid != 65532 {
		t.Fatalf("parseTarget = %d, %d, %v", uid, gid, err)
	}
	for _, value := range []string{"", "65532", "nonroot:nonroot", "-1:0", "1:x", "0:0", "0:1000", "1000:0"} {
		if _, _, err := parseTarget(value); err == nil {
			t.Errorf("parseTarget(%q) accepted", value)
		}
	}
}

func TestIdentityMapped(t *testing.T) {
	cases := map[string]bool{
		"         0          0 4294967295\n": true,
		"         0       1000          1\n": false,
		"0 1000 1\n1 100000 65536\n":         false,
		"":                                   false,
	}
	for uidMap, want := range cases {
		if got := identityMapped(uidMap); got != want {
			t.Errorf("identityMapped(%q) = %v", uidMap, got)
		}
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		owner, target int
		empty         bool
		want          action
	}{
		{65532, 65532, false, keep},
		{0, 65532, true, take},
		{1000, 65532, true, take},
		{1000, 65532, false, refuse},
	}
	for _, testCase := range cases {
		if got := decide(testCase.owner, testCase.target, testCase.empty); got != testCase.want {
			t.Errorf("decide(%+v) = %v", testCase, got)
		}
	}
}

func TestGroupsFor(t *testing.T) {
	cases := []struct {
		name        string
		inherited   []int
		socketGroup int
		socketFound bool
		want        []int
	}{
		{"root's group is dropped, the socket's added", []int{0, 999}, 988, true, []int{999, 988}},
		{"no socket keeps what was given", []int{0, 999, 999}, 0, false, []int{999}},
		{"a socket owned by root's group (Synology)", []int{0}, 0, true, []int{0}},
	}
	for _, testCase := range cases {
		if got := groupsFor(testCase.inherited, testCase.socketGroup, testCase.socketFound); !slices.Equal(got, testCase.want) {
			t.Errorf("%s: groupsFor = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}
