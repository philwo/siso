package ninjautil

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkManifestParser(b *testing.B) {
	// Find home directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		panic(fmt.Sprintf("Failed to get home directory: %v", err))
	}

	// Prepare the execRoot, outDir, and change to the target directory
	execRoot := filepath.Join(homeDir, "chromium/src")
	if !filepath.IsAbs(execRoot) {
		panic(fmt.Sprintf("execRoot must be absolute path: %q", execRoot))
	}
	outDir := "out/siso-native"
	if err := os.Chdir(filepath.Join(execRoot, outDir)); err != nil {
		panic(fmt.Sprintf("Failed to change to directory %s: %v", outDir, err))
	}

	for b.Loop() {
		state := NewState()
		state.AddBinding("exec_root", execRoot)
		state.AddBinding("working_directory", outDir)
		p := NewManifestParser(state)
		err := p.Load(b.Context(), "build.ninja")
		if err != nil {
			b.Fatalf("Failed to load manifest: %v", err)
		}
	}
}
