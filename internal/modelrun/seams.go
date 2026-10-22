package modelrun

import (
	"os/user"
	"strings"
)

// SetSeamsForTest replaces sudo with the given executable and the user lookup.
// It exists for the modelruntest helper only.
func SetSeamsForTest(sudo string, lookup func(string) (*user.User, error)) {
	lookupUser = lookup
	launchPrefix = func(runAs string, preserve []string) []string {
		argv := []string{sudo, "-n"}
		if len(preserve) > 0 {
			argv = append(argv, "--preserve-env="+strings.Join(preserve, ","))
		}
		return append(argv, "-u", runAs, "--")
	}
}
