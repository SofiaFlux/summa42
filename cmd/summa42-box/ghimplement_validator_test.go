package main

import (
	"slices"
	"testing"

	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
)

func TestValidatorFromEnvFailsClosedWithoutAModelUser(t *testing.T) {
	for _, user := range []string{"", "root", "no-such-user-xyz"} {
		if _, err := validatorFromEnv(user); err == nil {
			t.Fatalf("validator built for model user %q", user)
		}
	}
}

func TestValidatorFromEnvReadsExplicitToolchainDirs(t *testing.T) {
	_, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	t.Setenv(validationToolchainEnv, "/usr/local/go/bin: /opt/tc/bin ::")
	if _, err := validatorFromEnv(modelruntest.User); err != nil {
		t.Fatal(err)
	}
	if got := splitPathList("/a::/b"); !slices.Equal(got, []string{"/a", "/b"}) {
		t.Fatalf("%v", got)
	}
	t.Setenv(validationToolchainEnv, "relative/bin")
	if _, err := validatorFromEnv(modelruntest.User); err == nil {
		t.Fatal("relative toolchain dir accepted")
	}
}
