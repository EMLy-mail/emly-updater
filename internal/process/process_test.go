package process

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathUnder(t *testing.T) {
	cases := []struct {
		path, dir string
		want      bool
	}{
		{`C:\3gIT\X\a.exe`, `C:\3gIT\X`, true},
		{`C:\3gIT\X\a.exe`, `C:\3gIT\X\`, true},
		{`c:\3git\x\sub\a.exe`, `C:\3gIT\X`, true},
		{`C:\3gIT\X\a.exe`, `C:/3gIT/X`, true},
		{`C:\3gIT\XY\a.exe`, `C:\3gIT\X`, false},
		{`C:\3gIT\X`, `C:\3gIT\X`, false},
		{`C:\3gIT\a.exe`, `C:\3gIT\X`, false},
		{`D:\3gIT\X\a.exe`, `C:\3gIT\X`, false},
		{`C:\3gIT\X\..\Y\a.exe`, `C:\3gIT\X`, false},
		{`C:\a.exe`, `C:\`, true},
		{`C:\3gIT\X\a.exe`, ``, false},
		{`C:\3gIT\X\a.exe`, `3gIT\X`, false},
	}
	for _, c := range cases {
		if got := pathUnder(c.path, c.dir); got != c.want {
			t.Errorf("pathUnder(%q, %q) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
}

// The test binary itself is a process running under its own directory, and
// under no other.
func TestIsRunningUnderFindsTheTestBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	name, dir := filepath.Base(self), filepath.Dir(self)
	if !IsRunningUnder(name, dir) {
		t.Fatalf("IsRunningUnder(%q, %q) = false, want true", name, dir)
	}
	if IsRunningUnder(name, t.TempDir()) {
		t.Fatalf("IsRunningUnder(%q, <empty temp dir>) = true, want false", name)
	}
}
