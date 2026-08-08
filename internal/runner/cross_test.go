package runner

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The platform split is exactly the kind of thing that compiles on the machine
// it was written on and breaks everywhere else: the Windows file is invisible
// to a Linux build and the Unix file is invisible to a Windows one, so half
// the code in this package is never seen by the compiler that ran the tests.
//
// Building every target is the only way to notice. It is slow enough to skip
// under -short, and rules 3 and 4 of the project make it non-negotiable
// otherwise: the whole promise is one binary that cross-compiles without CGO.
func TestEveryTargetCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiling every target takes tens of seconds")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go toolchain is not on PATH")
	}

	targets := []struct{ goos, goarch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"windows", "amd64"},
		// Not a supported deployment target, but developers use it, and it is
		// the third compiler to see the !windows file.
		{"darwin", "arm64"},
	}

	for _, target := range targets {
		t.Run(target.goos+"/"+target.goarch, func(t *testing.T) {
			t.Parallel()

			cmd := exec.Command("go", "build", "./...")
			cmd.Dir = ".."
			cmd.Env = append(os.Environ(),
				"GOOS="+target.goos,
				"GOARCH="+target.goarch,
				// The single-binary rule depends on the pure-Go SQLite driver;
				// a build that needs a C toolchain would not cross-compile.
				"CGO_ENABLED=0",
			)

			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("building for %s/%s failed: %v\n%s",
					target.goos, target.goarch, err, strings.TrimSpace(string(out)))
			}
		})
	}
}
