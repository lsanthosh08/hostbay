package store

import (
	"slices"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestSecureFolderGivesTheUserOwnershipAndAProtectedDACL(t *testing.T) {
	dir := t.TempDir()
	if err := secureFolder(dir); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Equals(me) {
		t.Fatalf("owner %v, want %v", owner, me)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("DACL is not protected: inherited entries still apply")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	// Windows may split an inheritable entry in two, so compare who is in
	// the DACL rather than how many entries it has.
	system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	admins, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	want := []*windows.SID{me, system, admins}
	seen := map[string]bool{}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !slices.ContainsFunc(want, sid.Equals) {
			t.Fatalf("DACL grants %v, who is not the user, SYSTEM or Administrators", sid)
		}
		seen[sid.String()] = true
	}
	if len(seen) != len(want) {
		t.Fatalf("DACL grants %v, want the user, SYSTEM and Administrators", seen)
	}
}
