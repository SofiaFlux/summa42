package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

func validPipelineArgs(t *testing.T) []string {
	return []string{
		"--mission", "mission-1", "--workspace-root", "/work", "--repository", "o/r",
		"--base-branch", "main", "--credential-file", "/secret/token",
		"--source-repo", "/src", "--source-repository", "o/r",
		"--source-sha", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--allowed-prefix", "docs/",
		"--validation-command-json", `["go","test","./..."]`,
		"--model-binary", "/bin/producer", "--model-arg", "-p",
		"--review-model-binary", "/bin/reviewer", "--review-model-arg", "--strict",
		"--once",
	}
}

// fakePipeline records every stage call in order and fails or busies the
// stages named in fail/busy.
type fakePipeline struct {
	calls []string
	argv  map[string][][]string
	fail  map[string]error
	cases []domain.ID
}

func (f *fakePipeline) install(p *pipeline) {
	f.argv = map[string][][]string{}
	wrap := func(stages []pipelineStage) {
		for i := range stages {
			name := stages[i].name
			stages[i].run = func(_ context.Context, args []string) error {
				f.calls = append(f.calls, name)
				f.argv[name] = append(f.argv[name], args)
				return f.fail[name]
			}
		}
	}
	wrap(p.stages)
	wrap(p.perCase)
	p.implementCases = func(context.Context) ([]domain.ID, error) { return f.cases, nil }
}

func parsedPipeline(t *testing.T, args []string) (*pipeline, *bytes.Buffer) {
	t.Helper()
	p, _, err := parsePipelineFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	p.stderr = &stderr
	return p, &stderr
}

func TestPipelineStageErrorDoesNotSkipLaterStages(t *testing.T) {
	p, stderr := parsedPipeline(t, validPipelineArgs(t))
	f := &fakePipeline{fail: map[string]error{"plan": errors.New("model timed out\nsecond line")}}
	f.install(p)
	if err := p.pass(context.Background()); err == nil {
		t.Fatal("a pass with a failed stage returned nil")
	}
	reviews := f.argv["plan-review"]
	if len(reviews) != 1 {
		t.Fatalf("plan review calls = %d, want 1 after the plan stage failed", len(reviews))
	}
	if !slices.Contains(reviews[0], "--model-binary=/bin/reviewer") || slices.Contains(reviews[0], "--model-binary=/bin/producer") {
		t.Fatalf("plan review argv %v does not use the review model", reviews[0])
	}
	if !slices.Contains(reviews[0], "--model-user=nobody") {
		t.Fatalf("plan review argv %v does not forward the model user", reviews[0])
	}
	want := []string{"recover", "triage-driver", "worker", "triage-review", "plan", "plan-review"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("stages = %v, want %v", f.calls, want)
	}
	if got := stderr.String(); got != "stage=plan case=- err=model timed out second line\n" {
		t.Fatalf("stderr = %q, want one stage line", got)
	}
}

func TestPipelineSelectsImplementCasesByGoConstant(t *testing.T) {
	ctx := context.Background()
	f := newTriageDriverFixture(t, initializedCollective(t))
	defer f.Close()
	ensure := func(object, kind string) domain.ID {
		caps := []string{"workspace.repo.write"}
		c, err := f.cases.Ensure(ctx, workflowcase.Observation{
			MissionID: f.mission, Source: ghtriage.SourceGitHub, ObjectID: object, RevisionID: "r1", EvidenceID: "ev",
			FirstWork: workflow.WorkProposal{Kind: kind, RequiredCapabilities: caps, AuthorityCeiling: caps, ProposedActions: caps},
			Grant:     workflow.Grant{Capabilities: caps, Actions: caps}, MaxSteps: 3, RemainingBudget: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	implement := ensure("o/r#1", ghtriage.ImplementationWorkKind)
	ensure("o/r#2", ghtriage.PlanReviewWorkKind)
	ensure("o/r#3", "github.issue.implementation") // the reconciler's misspelt kind
	got, err := implementationCases(ctx, f.cases, f.mission)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []domain.ID{implement}) {
		t.Fatalf("selected %v, want only %s", got, implement)
	}

	p, _ := parsedPipeline(t, validPipelineArgs(t))
	fake := &fakePipeline{cases: got}
	fake.install(p)
	if err := p.pass(ctx); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"implement", "code-review", "publish"} {
		calls := fake.argv[stage]
		if len(calls) != 1 || !slices.Equal(calls[0][:2], []string{"--case", string(implement)}) {
			t.Fatalf("%s argv = %v, want one call for %s", stage, calls, implement)
		}
	}
	if !slices.Contains(fake.argv["code-review"][0], "--model-binary=/bin/reviewer") {
		t.Fatalf("code review argv %v does not use the review model", fake.argv["code-review"][0])
	}
	if !slices.Contains(fake.argv["implement"][0], "--model-binary=/bin/producer") {
		t.Fatalf("implement argv %v does not use the producer model", fake.argv["implement"][0])
	}
	if !slices.Contains(fake.argv["plan"][0], "--allowed-prefix=docs/") {
		t.Fatalf("plan argv %v does not carry the grounding flags", fake.argv["plan"][0])
	}
}

func TestPipelineRequiresDistinctReviewModel(t *testing.T) {
	same := validPipelineArgs(t)
	same[slices.Index(same, "/bin/reviewer")] = "/bin/producer"
	same[slices.Index(same, "--strict")] = "-p"
	_, _, err := parsePipelineFlags(same)
	if err == nil || !strings.Contains(err.Error(), "review model must differ from producer model") {
		t.Fatalf("identical review model: err = %v", err)
	}
	// Both left at their codex defaults is the same model too.
	defaults := slices.DeleteFunc(validPipelineArgs(t), func(s string) bool {
		return strings.Contains(s, "model") || s == "/bin/producer" || s == "/bin/reviewer" || s == "-p" || s == "--strict"
	})
	if _, _, err := parsePipelineFlags(defaults); err == nil || !strings.Contains(err.Error(), "review model must differ") {
		t.Fatalf("defaulted models: err = %v", err)
	}
	if _, _, err := parsePipelineFlags(validPipelineArgs(t)); err != nil {
		t.Fatalf("distinct models rejected: %v", err)
	}
}

func TestPipelineExitCodeReflectsFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		fail map[string]error
		ok   bool
	}{
		"clean":         {ok: true},
		"busy":          {fail: map[string]error{"implement": errBusy, "plan-review": errBusy}, ok: true},
		"case moved":    {fail: map[string]error{"publish": fmt.Errorf("x: %w", ghtriage.ErrCaseNotAtStage)}, ok: true},
		"worker failed": {fail: map[string]error{"worker": errors.New("boom")}},
		"recovery":      {fail: map[string]error{"recover": errors.New("db locked")}},
		"code review":   {fail: map[string]error{"code-review": errors.New("held")}},
	} {
		t.Run(name, func(t *testing.T) {
			p, stderr := parsedPipeline(t, validPipelineArgs(t))
			f := &fakePipeline{fail: tc.fail, cases: []domain.ID{"case-1"}}
			f.install(p)
			err := p.pass(context.Background())
			if (err == nil) != tc.ok {
				t.Fatalf("pass err = %v, want ok=%v (stderr %q)", err, tc.ok, stderr)
			}
			if code := exitStatus(err, &bytes.Buffer{}); (code == 0) != tc.ok || (!tc.ok && code != 1) {
				t.Fatalf("exit = %d", code)
			}
			if len(f.calls) != 9 {
				t.Fatalf("stages run = %v, want all nine", f.calls)
			}
		})
	}
	for _, args := range [][]string{
		slices.DeleteFunc(validPipelineArgs(t), func(s string) bool { return s == "--once" }),
		append(validPipelineArgs(t), "--interval", "1m"),
		append(slices.DeleteFunc(validPipelineArgs(t), func(s string) bool { return s == "--once" }), "--interval", "0s"),
	} {
		if _, _, err := parsePipelineFlags(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestRunGHImplementBusyExitsThree(t *testing.T) {
	err := implementOutcome(ghtriage.ImplementationResult{Busy: true})
	var stderr bytes.Buffer
	if code := exitStatus(err, &stderr); code != 3 {
		t.Fatalf("busy implement exit = %d, want 3", code)
	}
	if stderr.String() != "busy\n" {
		t.Fatalf("stderr = %q, want busy", stderr.String())
	}
	if err := implementOutcome(ghtriage.ImplementationResult{}); exitStatus(err, &bytes.Buffer{}) != 0 {
		t.Fatalf("idle implement err = %v", err)
	}
}

// Every stage's argv must pass that stage's own flag parser, so a flag rename
// in a run-gh-* command fails here rather than at runtime every pass.
func TestPipelineStageArgvParsesWithEachStageParser(t *testing.T) {
	p, _ := parsedPipeline(t, validPipelineArgs(t))
	modelIs := func(name, want, got string) {
		if got != want {
			t.Fatalf("%s model binary = %q, want %q", name, got, want)
		}
	}
	parsers := map[string]func([]string) error{
		"recover": func(args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("recover takes no argv, got %v", args)
			}
			return nil
		},
		"triage-driver": func(args []string) error { _, err := parseGHTriageDriverFlags(args); return err },
		"worker": func(args []string) error {
			once, root, cred, _, lease, model, err := parseWorkerArgs(args)
			if err == nil && (!once || root != "/work" || lease != pipelineWorkerLease(model.Timeout) || cred != "/secret/token") {
				err = fmt.Errorf("worker once=%v root=%q lease=%v credential=%q", once, root, lease, cred)
			}
			modelIs("worker", "/bin/producer", model.Binary)
			return err
		},
		"triage-review": func(args []string) error {
			_, model, err := parseGHTriageReviewFlags(args)
			modelIs("triage-review", "/bin/reviewer", model.Binary)
			return err
		},
		"plan": func(args []string) error {
			_, grounding, model, err := parseGHPlanFlags(args)
			if err == nil && grounding == nil {
				err = errors.New("plan got no grounding")
			}
			modelIs("plan", "/bin/producer", model.Binary)
			return err
		},
		"plan-review": func(args []string) error {
			_, model, err := parseGHPlanReviewFlags(args)
			modelIs("plan-review", "/bin/reviewer", model.Binary)
			return err
		},
		"implement": func(args []string) error {
			_, _, _, model, err := parseGHImplementFlags(args)
			modelIs("implement", "/bin/producer", model.Binary)
			return err
		},
		"code-review": func(args []string) error {
			_, _, model, err := parseGHCodeReviewFlags(args)
			modelIs("code-review", "/bin/reviewer", model.Binary)
			return err
		},
		"publish": func(args []string) error { _, _, _, _, _, err := parseGHPublishFlags(args); return err },
	}
	check := func(s pipelineStage, args []string) {
		parse, ok := parsers[s.name]
		if !ok {
			t.Fatalf("stage %s has no parser in this test", s.name)
		}
		if err := parse(args); err != nil {
			t.Fatalf("stage %s argv %v: %v", s.name, args, err)
		}
		delete(parsers, s.name)
	}
	for _, s := range p.stages {
		check(s, s.args)
	}
	for _, s := range p.perCase {
		check(s, slices.Concat([]string{"--case", "case-1"}, s.args))
	}
	if len(parsers) != 0 {
		t.Fatalf("stages missing from the pipeline: %v", parsers)
	}
	for _, s := range p.stages {
		lease := pipelineWorkerLease(30 * time.Minute)
		if s.name == "worker" && s.timeout >= lease {
			t.Fatalf("worker stage timeout %v is not below its lease %v", s.timeout, lease)
		}
		// Every stage that wraps a model call outlives the default 30m model timeout.
		switch s.name {
		case "worker", "triage-review", "plan", "plan-review":
			if s.timeout < 30*time.Minute {
				t.Fatalf("stage %s timeout %v is shorter than the 30m model timeout", s.name, s.timeout)
			}
		}
	}
}

func TestPipelineRequiresPublicationAndSourceFlagsAtStartup(t *testing.T) {
	for flag, bad := range map[string]string{
		"--source-repo": "relative", "--workspace-root": "relative", "--credential-file": "relative",
		"--base-branch": "", "--repository": "",
	} {
		args := validPipelineArgs(t)
		args[slices.Index(args, flag)+1] = bad
		if _, _, err := parsePipelineFlags(args); err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s=%q: err = %v", flag, bad, err)
		}
		missing := validPipelineArgs(t)
		i := slices.Index(missing, flag)
		missing = slices.Delete(missing, i, i+2)
		if _, _, err := parsePipelineFlags(missing); err == nil {
			t.Fatalf("missing %s accepted", flag)
		}
	}
}
