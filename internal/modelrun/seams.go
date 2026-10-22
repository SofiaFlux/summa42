package modelrun

import (
	"os/user"
	"strings"
	"time"
)

// SetSeamsForTest replaces sudo with the given executable and the user lookup.
// It exists for the modelruntest helper only.
func SetSeamsForTest(sudo string, lookup func(string) (*user.User, error)) {
	lookupUser = lookup
	// Never the real kill -1: the fake sudo keeps the test's own uid.
	killAllBinary = "/bin/true"
	launchPrefix = func(runAs string, preserve []string) []string {
		argv := []string{sudo, "-n"}
		if len(preserve) > 0 {
			argv = append(argv, "--preserve-env="+strings.Join(preserve, ","))
		}
		return append(argv, "-u", runAs, "--")
	}
}

// SetKillGraceForTest shortens the pause between the model-side TERM and KILL
// of an early cancel and returns the restore function.
func SetKillGraceForTest(d time.Duration) func() {
	old := killGrace
	killGrace = d
	return func() { killGrace = old }
}
