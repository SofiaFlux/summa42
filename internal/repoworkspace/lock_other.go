//go:build !unix

package repoworkspace

import (
	"context"
	"errors"
)

// lockFile: validation runs as a dedicated Unix user, so it is Unix-only.
func lockFile(context.Context, string) (func(), error) {
	return nil, errors.New("validation needs a Unix host")
}
