package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/localconfig"
)

// summa42-box serve runs the control daemon, intake, the pipeline and the worker in one process
// (development plan, card M1-T4). It adds no behavior of its own: each loop is
// the existing run-* command, started with that command's own flags. What
// serve adds is supervision: a loop that stops is logged and started again
// with a growing delay, and one loop stopping does not stop the others.
//
//	summa42-box serve [--restart-min D] [--restart-max D] \
//	    --control \
//	    --intake   <run-gh-intake flags> \
//	    --pipeline <run-gh-pipeline flags, with --interval> \
//	    --worker   <run-worker flags>
//
// Each section marker starts the flags of that command; a section that is not
// given does not run. Until a Project manifest replaces the flags (card
// M2-T3), this keeps every flag with the meaning it has in its own command.
//
// serve is the single coordinator of a Collective: a second serve on the same
// state directory refuses to start.

// serveLoop is one supervised command.
type serveLoop struct {
	name string
	args []string
	// validate checks the flags without starting anything, so that a
	// configuration error stops serve at startup instead of being retried.
	validate func(args []string) error
	run      func(ctx context.Context, args []string) error
}

var serveMarkers = []string{"--control", "--intake", "--pipeline", "--worker"}

// splitServeArgs separates serve's own flags from the flags of each section.
func splitServeArgs(args []string) (own []string, sections map[string][]string, err error) {
	sections = map[string][]string{}
	current := ""
	for _, arg := range args {
		isMarker := false
		for _, marker := range serveMarkers {
			if arg == marker {
				isMarker = true
			}
		}
		switch {
		case isMarker:
			if _, seen := sections[arg]; seen {
				return nil, nil, fmt.Errorf("serve: section %s is given twice", arg)
			}
			sections[arg] = []string{}
			current = arg
		case current == "":
			own = append(own, arg)
		default:
			sections[current] = append(sections[current], arg)
		}
	}
	if len(sections) == 0 {
		return nil, nil, errors.New("serve requires at least one of --control, --intake, --pipeline, --worker; the last three are each followed by that command's flags")
	}
	return own, sections, nil
}

func serveLoops(sections map[string][]string) []serveLoop {
	known := map[string]serveLoop{
		// The local control daemon: what `summa42 status`, `mission`, `approvals`
		// and `approve` talk to. It is the Box started without a subcommand.
		"--control": {name: "control", run: func(ctx context.Context, _ []string) error { return run(ctx) }, validate: func(args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("the control daemon takes no flags, got %q", args)
			}
			return nil
		}},
		"--intake": {name: "intake", run: runGHIntake, validate: func(args []string) error {
			_, _, err := parseGHIntakeFlags(args)
			return err
		}},
		"--pipeline": {name: "pipeline", run: runGHPipeline, validate: func(args []string) error {
			_, interval, err := parsePipelineFlags(args)
			if err == nil && interval == 0 {
				err = errors.New("serve runs the pipeline as a loop: give --interval, not --once")
			}
			return err
		}},
		"--worker": {name: "worker", run: runWorker, validate: func(args []string) error {
			once, _, _, _, _, _, err := parseWorkerArgs(args)
			if err == nil && once {
				err = errors.New("serve runs the worker as a loop: remove --once")
			}
			return err
		}},
	}
	var loops []serveLoop
	for _, marker := range serveMarkers {
		if args, ok := sections[marker]; ok {
			loop := known[marker]
			loop.args = args
			loops = append(loops, loop)
		}
	}
	return loops
}

// supervise runs every loop until ctx ends. A loop that returns, with or
// without an error, or panics, is started again after a delay that doubles
// from restartMin to restartMax and returns to restartMin once a run has
// lasted longer than restartMax.
func supervise(ctx context.Context, loops []serveLoop, restartMin, restartMax time.Duration, log io.Writer) {
	var logMu sync.Mutex
	logf := func(format string, values ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(log, format+"\n", values...)
	}
	var wg sync.WaitGroup
	for _, loop := range loops {
		wg.Add(1)
		go func(loop serveLoop) {
			defer wg.Done()
			delay := restartMin
			for {
				started := time.Now()
				err := runLoopOnce(ctx, loop)
				if ctx.Err() != nil {
					logf("serve: %s stopped", loop.name)
					return
				}
				if time.Since(started) > restartMax {
					delay = restartMin
				}
				if err == nil {
					err = errors.New("the loop returned without an error")
				}
				logf("serve: %s stopped: %v; restarting in %s", loop.name, err, delay)
				select {
				case <-ctx.Done():
					logf("serve: %s stopped", loop.name)
					return
				case <-time.After(delay):
				}
				if delay *= 2; delay > restartMax {
					delay = restartMax
				}
			}
		}(loop)
	}
	wg.Wait()
}

// runLoopOnce turns a panic in one loop into an error, so it is restarted
// like any other failure and the other loops keep running.
func runLoopOnce(ctx context.Context, loop serveLoop) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return loop.run(ctx, loop.args)
}

func runServe(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	own, sections, err := splitServeArgs(args)
	if err != nil {
		return err
	}
	var restartMin, restartMax time.Duration
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.DurationVar(&restartMin, "restart-min", time.Second, "first delay before a stopped loop is started again")
	flags.DurationVar(&restartMax, "restart-max", time.Minute, "longest delay before a stopped loop is started again")
	if err := flags.Parse(own); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("serve: unexpected argument %q before the first section", flags.Arg(0))
	}
	if restartMin <= 0 || restartMax < restartMin {
		return errors.New("serve requires 0 < --restart-min <= --restart-max")
	}
	loops := serveLoops(sections)
	for _, loop := range loops {
		if err := loop.validate(loop.args); err != nil {
			return fmt.Errorf("serve: %s flags: %w", loop.name, err)
		}
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return fmt.Errorf("load initialized Collective: %w", err)
	}
	release, err := lockServe(filepath.Join(filepath.Dir(cfg.DatabasePath), "serve.lock"))
	if err != nil {
		return err
	}
	defer release()
	for _, loop := range loops {
		fmt.Fprintf(os.Stderr, "serve: starting %s\n", loop.name)
	}
	supervise(ctx, loops, restartMin, restartMax, os.Stderr)
	return nil
}
