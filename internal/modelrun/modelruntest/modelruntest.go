// Package modelruntest lets tests run the model launcher without privileges:
// it swaps sudo for a script that drops its options and runs the rest, and the
// user database for one fake model user. Production code must not import it.
package modelruntest

import (
	"os"
	"os/user"
	"path/filepath"

	"github.com/SofiaFlux/summa42/internal/modelrun"
)

const (
	User = "summa42-model-test"
	// ValidationUser is the fake second dedicated user (candidate code).
	ValidationUser = "summa42-validate-test"
)

var validationHome, modelHome string

// LockModelHome makes the fake model home unreadable (mode 0), as the real one
// is for the validation user. Validation refuses to run otherwise, because the
// fake sudo keeps the test's own uid. The cleanup from Install restores it.
func LockModelHome() error { return os.Chmod(modelHome, 0) }

// ValidationHome is the fake validation user's home of the latest Install.
func ValidationHome() string { return validationHome }

// Install configures the seams and returns the fake model home and the file the
// fake sudo appends its argv to. Call cleanup when done.
func Install() (home, sudoLog string, cleanup func(), err error) {
	return InstallScript("")
}

// InstallScript is Install with a custom fake sudo body (a /bin/sh script
// without the shebang; the argv is already logged). Empty means the default
// behaviour: drop the sudo options and exec the rest.
func InstallScript(body string) (home, sudoLog string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "modelrun-test")
	if err != nil {
		return "", "", nil, err
	}
	home = filepath.Join(dir, "home")
	modelHome = home
	if err := os.MkdirAll(filepath.Join(home, "tmp"), 0o755); err != nil {
		return "", "", nil, err
	}
	validationHome = filepath.Join(dir, "validate-home")
	if err := os.MkdirAll(filepath.Join(validationHome, "tmp"), 0o755); err != nil {
		return "", "", nil, err
	}
	sudoLog = filepath.Join(dir, "sudo.log")
	fake := filepath.Join(dir, "fakesudo")
	if body == "" {
		body = "while [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + sudoLog + "\n" + body
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		return "", "", nil, err
	}
	modelrun.SetSeamsForTest(fake, func(name string) (*user.User, error) {
		switch name {
		case User:
			return &user.User{Uid: "4242", Username: name, HomeDir: home}, nil
		case ValidationUser:
			return &user.User{Uid: "4343", Username: name, HomeDir: validationHome}, nil
		case "root":
			return &user.User{Uid: "0", Username: name, HomeDir: "/root"}, nil
		}
		return user.Lookup(name)
	})
	return home, sudoLog, func() { os.Chmod(home, 0o700); os.RemoveAll(dir) }, nil
}
