package config

import (
	"golang.org/x/sys/windows"
	"os"
	"unsafe"
)

// Windows mode bits cannot express owner-only access. Use a protected DACL,
// allowing the current user, SYSTEM, and local administrators only.
func PrivatePath(path string) error {
	user, e := windows.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		return e
	}
	flags := ""
	if info, e := os.Stat(path); e == nil && info.IsDir() {
		flags = "OICI"
	}
	sd, e := windows.SecurityDescriptorFromString("D:P(A;" + flags + ";FA;;;SY)(A;" + flags + ";FA;;;BA)(A;" + flags + ";FA;;;" + user.User.Sid.String() + ")")
	if e != nil {
		return e
	}
	acl, _, e := sd.DACL()
	if e != nil {
		return e
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
func IsPrivate(path string) bool {
	info, e := os.Lstat(path)
	if e != nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	sd, e := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if e != nil {
		return false
	}
	acl, _, e := sd.DACL()
	if e != nil || acl == nil {
		return false
	}
	user, e := windows.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		return false
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ptr *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &ptr) != nil {
			return false
		}
		if ptr.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ptr.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ptr.SidStart))
		if !sid.Equals(user.User.Sid) && !sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			return false
		}
	}
	return true
}
