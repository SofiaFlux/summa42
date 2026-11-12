package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/emergency"
	"github.com/SofiaFlux/summa42/internal/notify"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type notifierSender struct{ sent []string }

func (s *notifierSender) Send(_ context.Context, message string) (notify.Outcome, error) {
	s.sent = append(s.sent, message)
	return notify.Delivered, nil
}

// One notifier pass over a real Collective: the Owner freezes it through the
// emergency service, the notifier says so once, and later passes say nothing
// more about the same freeze.
func TestNotifierReportsAFreezeOnce(t *testing.T) {
	cfg := initializedCollective(t)
	ctx := context.Background()
	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	outbox := notify.NewOutbox(store, clock.System{})
	sender := &notifierSender{}
	var log bytes.Buffer

	if err := notifierTick(ctx, store, outbox, sender, time.Now().UTC(), &log); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("a quiet Collective produced messages: %q", sender.sent)
	}

	service := emergency.NewService(store, clock.System{}, cfg.OwnerPrincipalID)
	if err := service.EnsureDefaults(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := service.TriggerBreakGlass(ctx, cfg.OwnerPrincipalID, "a reason that is not sent"); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 3; pass++ {
		if err := notifierTick(ctx, store, outbox, sender, time.Now().UTC(), &log); err != nil {
			t.Fatal(err)
		}
	}
	if len(sender.sent) != 1 || !strings.Contains(sender.sent[0], "FROZEN") {
		t.Fatalf("messages after a freeze and three passes: %q", sender.sent)
	}
	if strings.Contains(sender.sent[0], "a reason that is not sent") {
		t.Fatalf("the message carries the free-text reason: %q", sender.sent[0])
	}
	if !strings.Contains(log.String(), "queued=1 delivered=1") {
		t.Fatalf("log: %q", log.String())
	}
}

func TestNotifierFlags(t *testing.T) {
	if _, interval, once, err := parseNotifierFlags([]string{"--webhook-file", "/etc/summa42/discord_webhook"}); err != nil || interval != time.Minute || once {
		t.Fatalf("defaults: %v %v %v", interval, once, err)
	}
	for name, args := range map[string][]string{
		"no file":        {},
		"relative file":  {"--webhook-file", "discord_webhook"},
		"short interval": {"--webhook-file", "/w", "--interval", "1s"},
		"stray argument": {"--webhook-file", "/w", "extra"},
	} {
		if _, _, _, err := parseNotifierFlags(args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
