//go:build windows

package mcpclient

import (
	"context"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCredentialStoreWindowsDACLAllowsOnlyCurrentUserAndSystem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp_credentials.json")
	store := NewCredentialStore(path)
	if err := store.Put(context.Background(), "key", OAuthCredential{AccessToken: "access"}); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || defaulted || dacl.AceCount != 2 {
		t.Fatalf("credential DACL count/defaulted = %v/%v; descriptor = %q", dacl, defaulted, descriptor.String())
	}
	const fileAllAccess = windows.ACCESS_MASK(windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff)
	seenUser, seenSystem := false, false
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatalf("read credential DACL ACE %d: %v", index, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask != fileAllAccess {
			t.Fatalf("credential DACL ACE %d type/mask = %d/%#x; descriptor = %q", index, ace.Header.AceType, ace.Mask, descriptor.String())
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(user.User.Sid):
			seenUser = true
		case sid.Equals(systemSID):
			seenSystem = true
		default:
			t.Fatalf("credential DACL ACE %d grants unexpected SID %s; descriptor = %q", index, sid.String(), descriptor.String())
		}
	}
	if !seenUser || !seenSystem {
		t.Fatalf("credential DACL user/system grants = %v/%v; descriptor = %q", seenUser, seenSystem, descriptor.String())
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("credential DACL is not protected from inheritance: %q", descriptor.String())
	}
}
