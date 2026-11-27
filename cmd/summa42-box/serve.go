package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/notify"
	"github.com/SofiaFlux/summa42/internal/project"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
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
//	    --worker   <run-worker flags> \
//	    --notify   <run-notifier flags>
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
	// stopsServe marks the control daemon: it returns nil without its context
	// ending only when the Owner asked the Box to shut down (POST /shutdown),
	// which stops the whole serve process instead of restarting the loop.
	stopsServe bool
}

var serveMarkers = []string{"--control", "--intake", "--pipeline", "--worker", "--notify"}

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
		return nil, nil, errors.New("serve requires at least one of --control, --intake, --pipeline, --worker, --notify; all but --control are followed by that command's flags")
	}
	return own, sections, nil
}

func serveLoops(sections map[string][]string) []serveLoop {
	known := map[string]serveLoop{
		// The local control daemon: what `summa42 status`, `mission`, `approvals`
		// and `approve` talk to. It is the Box started without a subcommand.
		"--control": {name: "control", stopsServe: true, run: func(ctx context.Context, _ []string) error { return run(ctx) }, validate: func(args []string) error {
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
		"--notify": {name: "notifier", run: runNotifier, validate: func(args []string) error {
			webhookFile, _, once, err := parseNotifierFlags(args)
			if err == nil && once {
				err = errors.New("serve runs the notifier as a loop: remove --once")
			}
			if err == nil {
				// The same checks runNotifier makes, so a missing, readable-by-others
				// or non-Discord webhook file stops serve here, not in a restart loop.
				_, err = notify.NewDiscordSender(webhookFile)
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
// lasted longer than restartMax. A stopsServe loop that returns without an
// error and without ctx ending is an Owner shutdown request: every loop is
// cancelled and supervise returns.
func supervise(parent context.Context, loops []serveLoop, restartMin, restartMax time.Duration, log io.Writer) {
	ctx, stop := context.WithCancel(parent)
	defer stop()
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
				if err == nil && loop.stopsServe {
					logf("serve: %s stopped at the Owner's request; stopping serve", loop.name)
					stop()
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

func hasServeMarker(args []string) bool {
	for _, a := range args {
		if slices.Contains(serveMarkers, a) {
			return true
		}
	}
	return false
}

func runServe(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	slug, rest, err := extractProjectFlag(args)
	if err != nil {
		return err
	}
	markersPresent := hasServeMarker(args)
	if slug != "" && markersPresent {
		return errors.New("serve: --project cannot be combined with section flags")
	}
	multiProjectMode := slug == "" && !markersPresent

	var own []string
	var sections map[string][]string
	if slug != "" || multiProjectMode {
		own = rest
	} else {
		own, sections, err = splitServeArgs(args)
		if err != nil {
			return err
		}
	}
	var restartMin, restartMax, projectInterval, projectTimeout time.Duration
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.DurationVar(&restartMin, "restart-min", time.Second, "first delay before a stopped loop is started again")
	flags.DurationVar(&restartMax, "restart-max", time.Minute, "longest delay before a stopped loop is started again")
	if multiProjectMode {
		flags.DurationVar(&projectInterval, "interval", 30*time.Second, "interval between multi-project round-robin passes")
		flags.DurationVar(&projectTimeout, "project-timeout", defaultProjectPassTimeout, "maximum duration for one Project pass before yielding to the next Project")
	}
	if err := flags.Parse(own); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("serve: unexpected argument %q before the first section", flags.Arg(0))
	}
	if restartMin <= 0 || restartMax < restartMin {
		return errors.New("serve requires 0 < --restart-min <= --restart-max")
	}
	if multiProjectMode && (projectInterval <= 0 || projectTimeout <= 0) {
		return errors.New("serve requires positive --interval and --project-timeout")
	}
	loadHomeCfg := func() (string, localconfig.Config, error) {
		home, err := localconfig.ResolveHome("")
		if err != nil {
			return "", localconfig.Config{}, err
		}
		cfg, err := localconfig.Load(home)
		if err != nil {
			return "", localconfig.Config{}, fmt.Errorf("load initialized Collective: %w", err)
		}
		return home, cfg, nil
	}
	var home string
	var cfg localconfig.Config
	var cfgLoaded bool
	if slug != "" {
		home, cfg, err = loadHomeCfg()
		if err != nil {
			return err
		}
		cfgLoaded = true
		intake, pipeline, worker, err := manifestArgsForProject(ctx, home, cfg, slug, false)
		if err != nil {
			return err
		}
		sections = map[string][]string{
			"--control":  nil,
			"--intake":   intake,
			"--pipeline": pipeline,
			"--worker":   worker,
		}
	}
	var loops []serveLoop
	if multiProjectMode {
		home, cfg, err = loadHomeCfg()
		if err != nil {
			return err
		}
		cfgLoaded = true
		coord := &multiProjectCoordinator{
			home:        home,
			cfg:         cfg,
			interval:    projectInterval,
			passTimeout: projectTimeout,
			log:         os.Stderr,
		}
		loops = append(serveLoops(map[string][]string{"--control": nil}), serveLoop{
			name:     "projects",
			validate: func([]string) error { return nil },
			run:      func(ctx context.Context, _ []string) error { return coord.run(ctx) },
		})
	} else {
		loops = serveLoops(sections)
	}
	for _, loop := range loops {
		if err := loop.validate(loop.args); err != nil {
			return fmt.Errorf("serve: %s flags: %w", loop.name, err)
		}
	}
	if !cfgLoaded {
		_, cfg, err = loadHomeCfg()
		if err != nil {
			return err
		}
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

const defaultProjectPassTimeout = 10 * time.Minute

type projectStepRunner func(ctx context.Context, slug string, intake, pipeline, worker []string) error

// multiProjectCoordinator serves every ACTIVE Project with an applied manifest
// in fair round-robin order so one busy or failing Project cannot starve peers.
type multiProjectCoordinator struct {
	home        string
	cfg         localconfig.Config
	interval    time.Duration
	passTimeout time.Duration
	log         io.Writer
	runner      projectStepRunner

	mu       sync.Mutex
	lastSlug string
}

func (c *multiProjectCoordinator) run(ctx context.Context) error {
	interval := c.interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if err := c.runRound(ctx); err != nil && ctx.Err() == nil && c.log != nil {
		fmt.Fprintf(c.log, "serve: projects round error: %v\n", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.runRound(ctx); err != nil && ctx.Err() == nil && c.log != nil {
				fmt.Fprintf(c.log, "serve: projects round error: %v\n", err)
			}
		}
	}
}

func (c *multiProjectCoordinator) runRound(ctx context.Context) error {
	runnable, err := c.listRunnableProjects(ctx)
	if err != nil {
		return err
	}
	if len(runnable) == 0 {
		return nil
	}
	c.mu.Lock()
	last := c.lastSlug
	c.mu.Unlock()

	ordered := roundRobinProjects(runnable, last)
	stepRunner := c.runner
	if stepRunner == nil {
		stepRunner = runProjectOncePass
	}
	timeout := c.passTimeout
	if timeout <= 0 {
		timeout = defaultProjectPassTimeout
	}
	for _, proj := range ordered {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.mu.Lock()
		c.lastSlug = proj.Slug
		c.mu.Unlock()

		intake, pipeline, worker, err := manifestArgsForProject(ctx, c.home, c.cfg, proj.Slug, true)
		if err != nil {
			if c.log != nil {
				fmt.Fprintf(c.log, "serve: skip project %s: %v\n", proj.Slug, err)
			}
			continue
		}
		passCtx, cancel := context.WithTimeout(ctx, timeout)
		err = stepRunner(passCtx, proj.Slug, intake, pipeline, worker)
		cancel()
		if err != nil && ctx.Err() == nil && c.log != nil {
			fmt.Fprintf(c.log, "serve: project %s pass error: %v\n", proj.Slug, err)
		}
	}
	return nil
}

func (c *multiProjectCoordinator) listRunnableProjects(ctx context.Context) ([]project.Project, error) {
	store, err := state.Open(ctx, c.cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	defer store.DB().Close()
	all, err := project.New(store, clock.System{}).List(ctx)
	if err != nil {
		return nil, err
	}
	var out []project.Project
	for _, p := range all {
		if p.State == project.StateActive && strings.TrimSpace(p.ManifestSHA256) != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

func roundRobinProjects(projects []project.Project, lastSlug string) []project.Project {
	sorted := slices.Clone(projects)
	slices.SortFunc(sorted, func(a, b project.Project) int {
		return strings.Compare(a.Slug, b.Slug)
	})
	if len(sorted) <= 1 || lastSlug == "" {
		return sorted
	}
	idx := len(sorted)
	for i, p := range sorted {
		if p.Slug > lastSlug {
			idx = i
			break
		}
	}
	if idx == len(sorted) {
		return sorted
	}
	out := make([]project.Project, 0, len(sorted))
	out = append(out, sorted[idx:]...)
	out = append(out, sorted[:idx]...)
	return out
}

func runProjectOncePass(ctx context.Context, slug string, intake, pipeline, worker []string) error {
	var errs []error
	for _, stage := range []struct {
		name string
		fn   func(context.Context, []string) error
		args []string
	}{
		{"intake", runGHIntake, intake},
		{"pipeline", runGHPipeline, pipeline},
		{"worker", runWorker, worker},
	} {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		if err := stage.fn(ctx, stage.args); err != nil {
			errs = append(errs, fmt.Errorf("%s(%s): %w", stage.name, slug, err))
		}
	}
	return errors.Join(errs...)
}
