package tunnel

import "golang.org/x/sys/windows"

// restrict replaces path's inherited ACL, which under Program Files lets
// every user read, with one admitting only SYSTEM and administrators.
func restrict(path string) error { return setDACL(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)") }

// restrictDir does the same for a folder and everything created in it, so
// no file there is readable by users even before restrict runs on it.
func restrictDir(path string) error { return setDACL(path, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)") }

func setDACL(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
