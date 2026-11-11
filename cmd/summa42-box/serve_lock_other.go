//go:build !unix

package main

import "errors"

var errServeRunning = errors.New("another summa42-box serve is already running on this Collective")

// lockServe: the single-coordinator lock uses flock, so serve is refused
// where that is missing rather than run without the guarantee.
func lockServe(string) (func(), error) {
	return nil, errors.New("summa42-box serve needs a Unix host: the single-coordinator lock is not implemented here")
}
