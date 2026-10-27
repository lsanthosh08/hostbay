package store

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// Go reports every Windows folder as 0777, so the owner and the DACL are
// read and set through the security API instead (KTD42).

func currentUserSID() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid.Copy()
}

// checkFolder refuses a folder whose owner is not this user.
func checkFolder(dir string) error {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	me, err := currentUserSID()
	if err != nil {
		return err
	}
	if !owner.Equals(me) {
		return fmt.Errorf("%s belongs to another account", dir)
	}
	return nil
}

// secureFolder checks dir, then makes this user its owner and gives it a
// protected DACL, so inherited entries for other accounts no longer apply:
// only the user, SYSTEM and Administrators get in, and what Hostbay
// creates inside inherits the same.
func secureFolder(dir string) error {
	if err := checkFolder(dir); err != nil {
		return err
	}
	me, err := currentUserSID()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	var access []windows.EXPLICIT_ACCESS
	for _, sid := range []*windows.SID{me, system, admins} {
		access = append(access, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(access, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		me, nil, acl, nil)
}
