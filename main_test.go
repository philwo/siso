package main

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func TestSisoMain(t *testing.T) {
	os.Args = []string{
		"siso",
		"ninja",
		"-C", "out/siso-native",
		"-clobber",
		"-project", "rbe-chrome-untrusted",
		"-reapi_instance", "default_instance",
		"base",
	}

	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("Failed to get current user: %v", err)
	}

	err = os.Chdir(filepath.Join(currentUser.HomeDir, "chromium", "src"))
	if err != nil {
		t.Fatalf("Failed to change directory: %v", err)
	}

	exitCode := sisoMain()
	if exitCode != 0 {
		t.Fatalf("sisoMain() returned exit code %d", exitCode)
	}
}
