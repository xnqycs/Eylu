//go:build windows

package mcpclient

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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
	sddl := descriptor.String()
	if !strings.Contains(sddl, user.User.Sid.String()) || !strings.Contains(sddl, ";;;SY)") {
		t.Fatalf("credential DACL = %q", sddl)
	}
	if !strings.Contains(sddl, "D:P") {
		t.Fatalf("credential DACL is not protected from inheritance: %q", sddl)
	}
}
