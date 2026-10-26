package climodel

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// commandCodeFree lists the Command Code model ids billed at $0. Any other id
// draws on the plan's small monthly pool, so it needs an explicit opt-in.
// ponytail: hardcoded list; the free deals are "while capacity lasts", update
// it when Command Code changes them.
var commandCodeFree = []string{
	"poolside/laguna-s-2.1-free",
	"reflection/beam:free",
	"stealth/glyph-cluster:free",
}

// commandCodeMaxTurns bounds one headless cmd run. Every turn resends the whole
// prompt (often 50k+ tokens), and the answer needs one; the slack covers a
// model that reaches for a read-only tool anyway.
const commandCodeMaxTurns = 4

// commandCodePreamble goes before every Command Code prompt: cmd is an agent
// with read-only tools in plan mode, and a model that explores its empty
// scratch directory burns turns and the plan's pool for nothing.
const commandCodePreamble = "Everything you need is in this message. Do not use any tools. " +
	"Reply with the requested JSON object only: no Markdown, no code fence, no other text.\n\n"

// looksLikeCommandCode catches the names the command-code package installs, so
// a cmd launched without CommandCodeModel cannot skip the guards below.
func looksLikeCommandCode(binary string) bool {
	base := filepath.Base(binary)
	return slices.Contains([]string{"cmd", "cmdc", "commandcode", "command-code"}, base) ||
		strings.Contains(binary, "command-code")
}

// commandCodeArgs is the whole argv of a Command Code run. The adapter builds
// it so nothing operator-supplied can drop plan mode, add --yolo or name a
// second model: cmd resolves repeated options by last value and accepts
// unknown ones, so filtering operator argv would leak. --skip-onboarding and -t
// are required headless: a fresh model user and a fresh scratch directory make
// cmd ask whether to trust the location and whether to run taste analysis, and
// there is nobody to answer.
func commandCodeArgs(model string) []string {
	return []string{"--skip-onboarding", "-t", "-p", "-m", model, "--output-format", "json",
		"--permission-mode", "plan", "--max-turns", fmt.Sprint(commandCodeMaxTurns),
		"--no-session", "--safe-mode", "--no-skills", "--no-auto-update"}
}

// checkCommandCode enforces the Command Code backend rules at construction:
// the model is named only through CommandCodeModel, free models unless paid
// ones are allowed, and zero data retention only on a model that offers it
// (free ones do not).
func checkCommandCode(config Config) error {
	model := config.CommandCodeModel
	if model == "" {
		if looksLikeCommandCode(config.Binary) {
			return errors.New("Command Code runs only through --model-commandcode=<model id>, which builds its arguments")
		}
		if config.ZDR {
			return errors.New("zero data retention is a Command Code option; set --model-commandcode")
		}
		return nil
	}
	if !looksLikeCommandCode(config.Binary) {
		return fmt.Errorf("--model-commandcode needs the Command Code binary (cmd), not %q", config.Binary)
	}
	if len(config.Args) > 0 {
		return errors.New("--model-arg is not accepted with --model-commandcode: the adapter builds the Command Code arguments")
	}
	if strings.HasPrefix(model, "-") || strings.ContainsAny(model, " \t\n") {
		return fmt.Errorf("invalid Command Code model id %q", model)
	}
	free := slices.Contains(commandCodeFree, model)
	if !free && !config.AllowPaid {
		return fmt.Errorf("Command Code model %q is not free; pass --model-allow-paid (env SUMMA42_ALLOW_PAID_MODELS=1) to spend the plan's pool", model)
	}
	if config.ZDR && free {
		return fmt.Errorf("free Command Code model %q has no zero data retention; use a paid model with --model-allow-paid", model)
	}
	return nil
}

// commandCodeAnswer extracts the answer from cmd's NDJSON output: the last
// line of type "result". Anything but a successful, non-empty result fails
// closed, so a rate limit or a backend error is never read as an answer.
// Model text cannot forge a result line: cmd JSON-escapes it inside events.
func commandCodeAnswer(out []byte) ([]byte, error) {
	type result struct {
		Type      string         `json:"type"`
		Subtype   string         `json:"subtype"`
		FinalText string         `json:"finalText"`
		Error     string         `json:"error"`
		Usage     map[string]int `json:"usage"`
	}
	var last *result
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), maxModelOutput)
	for scanner.Scan() {
		var r result
		if json.Unmarshal(scanner.Bytes(), &r) == nil && r.Type == "result" {
			last = &r
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("command code output: %w", err)
	}
	switch {
	case last == nil:
		return nil, errors.New("command code: no result line")
	case last.Subtype != "success" && providerRefused(last.Error):
		return nil, fmt.Errorf("command code: %w: %s", ghtriage.ErrModelUnavailable, last.Error)
	case last.Subtype != "success":
		return nil, fmt.Errorf("command code: %s: %s", last.Subtype, last.Error)
	case strings.TrimSpace(last.FinalText) == "":
		return nil, errors.New("command code: empty answer")
	}
	log.Printf("command code usage: input=%d output=%d cache_read=%d cache_write=%d",
		last.Usage["inputTokens"], last.Usage["outputTokens"], last.Usage["cacheReadTokens"], last.Usage["cacheWriteTokens"])
	return []byte(last.FinalText), nil
}
