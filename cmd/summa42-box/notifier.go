package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/notify"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// run-notifier tells the Owner what needs them (development plan, card
// M3-T3): it polls the Collective's state, queues each event once and sends
// the queue to a Discord webhook. It reads the state and writes only its own
// outbox table. See internal/notify for what a message may contain.

func parseNotifierFlags(args []string) (webhookFile string, interval time.Duration, once bool, err error) {
	flags := flag.NewFlagSet("run-notifier", flag.ContinueOnError)
	flags.StringVar(&webhookFile, "webhook-file", "", "absolute path of a private file (mode 0600) holding the Discord webhook address")
	flags.DurationVar(&interval, "interval", time.Minute, "interval between checks")
	flags.BoolVar(&once, "once", false, "check and send once, then exit")
	if err = flags.Parse(args); err != nil {
		return
	}
	if flags.NArg() != 0 || !filepath.IsAbs(webhookFile) {
		err = errors.New("run-notifier requires an absolute --webhook-file and no positional arguments")
		return
	}
	if interval < 10*time.Second {
		err = errors.New("run-notifier requires an --interval of at least 10s")
	}
	return
}

// notifierTick collects, queues and sends once, and reports in one line.
func notifierTick(ctx context.Context, store *state.Store, outbox *notify.Outbox, sender notify.Sender, now time.Time, log io.Writer) error {
	events, err := notify.Collect(ctx, store, now)
	if err != nil {
		return err
	}
	queued, err := outbox.Enqueue(ctx, events)
	if err != nil {
		return err
	}
	result, err := outbox.Deliver(ctx, sender)
	if queued > 0 || result != (notify.DeliveryResult{}) {
		fmt.Fprintf(log, "notifier: queued=%d delivered=%d not_sent=%d unknown=%d\n", queued, result.Delivered, result.NotSent, result.Unknown)
	}
	return err
}

func runNotifier(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	webhookFile, interval, once, err := parseNotifierFlags(args)
	if err != nil {
		return err
	}
	sender, err := notify.NewDiscordSender(webhookFile)
	if err != nil {
		return err
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return fmt.Errorf("load initialized Collective: %w", err)
	}
	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.DB().Close()
	outbox := notify.NewOutbox(store, clock.System{})
	for {
		if err := notifierTick(ctx, store, outbox, sender, time.Now().UTC(), os.Stderr); err != nil && ctx.Err() == nil {
			if once {
				return err
			}
			fmt.Fprintln(os.Stderr, "notifier:", err)
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
