package buildinfo

import (
	"strings"
	"testing"
)

func TestCurrentAndString(t *testing.T) {
	info := Current()
	if info.Version == "" || !strings.Contains(String(), "commit=") || !strings.Contains(String(), "built_by=") {
		t.Fatalf("info=%#v string=%q", info, String())
	}
}
