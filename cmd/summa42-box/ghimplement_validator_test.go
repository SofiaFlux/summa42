package main

import (
	"flag"
	"slices"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/modelrun/modelruntest"
)

func fakeValidation(user string) validationConfig {
	return validationConfig{User: user, Timeout: 10 * time.Minute, LockPath: "/tmp/summa42-test-validation.lock"}
}

func TestValidatorFailsClosedWithoutADistinctValidationUser(t *testing.T) {
	_, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, user := range []string{"", "root", "no-such-user-xyz", modelruntest.User} {
		if _, err := validatorFor(fakeValidation(user), modelruntest.User); err == nil {
			t.Fatalf("validator built for validation user %q", user)
		}
	}
	if _, err := validatorFor(fakeValidation(modelruntest.ValidationUser), ""); err == nil {
		t.Fatal("validator built without a model user")
	}
	if _, err := validatorFor(fakeValidation(modelruntest.ValidationUser), modelruntest.User); err != nil {
		t.Fatal(err)
	}
}

func TestValidatorReadsExplicitToolchainDirs(t *testing.T) {
	_, _, cleanup, err := modelruntest.Install()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	t.Setenv(validationToolchainEnv, "/usr/local/go1.27/bin: /opt/tc/bin ::")
	if _, err := validatorFor(fakeValidation(modelruntest.ValidationUser), modelruntest.User); err != nil {
		t.Fatal(err)
	}
	if got := splitPathList("/a::/b"); !slices.Equal(got, []string{"/a", "/b"}) {
		t.Fatalf("%v", got)
	}
	t.Setenv(validationToolchainEnv, "relative/bin")
	if _, err := validatorFor(fakeValidation(modelruntest.ValidationUser), modelruntest.User); err == nil {
		t.Fatal("relative toolchain dir accepted")
	}
}

func TestValidationFlagsReadEnvAndFlagsAndDefaultToTenMinutes(t *testing.T) {
	parse := func(args ...string) (validationConfig, error) {
		flags := flag.NewFlagSet("x", flag.ContinueOnError)
		registerValidationFlags(flags)
		if err := flags.Parse(args); err != nil {
			return validationConfig{}, err
		}
		return validationConfigFrom(flags)
	}
	t.Setenv("SUMMA42_VALIDATION_USER", "")
	t.Setenv("SUMMA42_VALIDATION_TIMEOUT", "")
	got, err := parse()
	if err != nil || got.User != "" || got.Timeout != 10*time.Minute {
		t.Fatalf("%+v %v", got, err)
	}
	t.Setenv("SUMMA42_VALIDATION_USER", "envuser")
	t.Setenv("SUMMA42_VALIDATION_TIMEOUT", "25m")
	if got, err = parse(); err != nil || got.User != "envuser" || got.Timeout != 25*time.Minute {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err = parse("--validation-user=flaguser", "--validation-timeout=1h"); err != nil || got.User != "flaguser" || got.Timeout != time.Hour {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"0", "-1m", "soon"} {
		if _, err := parse("--validation-timeout=" + bad); err == nil {
			t.Fatalf("timeout %q accepted", bad)
		}
	}
}
