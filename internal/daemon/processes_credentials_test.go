package daemon

import "testing"

func TestProcessCredentialScope(t *testing.T) {
	status := "Uid:\t1000\t1000\t1000\t1000\nGid:\t1000\t1000\t1000\t1000\nCapPrm:\t00000000\nCapEff:\t00000000\n"
	caller, err := parseProcessCredentials(status)
	if err != nil {
		t.Fatal(err)
	}
	if !inspectableCredentials(caller, caller) {
		t.Fatal("ordinary helper excluded")
	}
	for i := 0; i < 4; i++ {
		target := caller
		target.UIDs[i] = 0
		if inspectableCredentials(target, caller) {
			t.Fatal("privileged UID accepted")
		}
		target = caller
		target.GIDs[i] = 0
		if inspectableCredentials(target, caller) {
			t.Fatal("privileged GID accepted")
		}
	}
	target := caller
	target.Permitted = 1
	if inspectableCredentials(target, caller) {
		t.Fatal("extra capability accepted")
	}
	caller.Effective = 1
	if !inspectableCredentials(target, caller) {
		t.Fatal("matching capability excluded")
	}
	for _, bad := range []string{"", "Uid: 1000", "Uid: x x x x\nGid: 1000 1000 1000 1000\nCapPrm: 0\nCapEff: 0", status + "CapEff: nope"} {
		if _, err := parseProcessCredentials(bad); err == nil {
			t.Fatalf("accepted malformed status %q", bad)
		}
	}
}
