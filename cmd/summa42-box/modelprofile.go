package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/modelrun"
)

func init() {
	subcommands["model-probe"] = runModelProbe
	subcommands["model-profile-draft"] = runModelProfileDraft
	subcommands["model-profile-adopt"] = runModelProfileAdopt
}

// runModelProbe sends one trivial request through the configured model and
// reports whether a well-formed answer came back. Run it once per harness and
// model before putting the harness in a pipeline.
func runModelProbe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("model-probe", flag.ContinueOnError)
	registerTriageModelFlags(flags, "model binary to probe")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := triageModelConfig(flags)
	if err != nil {
		return err
	}
	adapter, err := climodel.New(cfg)
	if err != nil {
		return err
	}
	start := time.Now()
	if err := adapter.Probe(ctx); err != nil {
		return fmt.Errorf("probe failed after %s: %w", time.Since(start).Round(time.Millisecond), err)
	}
	fmt.Printf("probe ok in %s: %s\n", time.Since(start).Round(time.Millisecond), describeModel(cfg))
	return nil
}

func describeModel(cfg climodel.Config) string {
	switch {
	case cfg.Profile != "":
		return "profile " + cfg.Profile + " model " + cfg.ProfileModel
	case cfg.CommandCodeModel != "":
		return "command code model " + cfg.CommandCodeModel
	}
	return "binary " + cfg.Binary
}

// profileHelpTimeout bounds one --help run.
const profileHelpTimeout = 30 * time.Second

// runModelProfileDraft asks the configured model to write a profile for a new
// harness from its --help output and stores it, validated, in the pending
// directory (not loaded by anything). The owner then probes it with
// model-probe --model-profile-dir=<pending dir> and adopts it.
func runModelProfileDraft(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("model-profile-draft", flag.ContinueOnError)
	registerTriageModelFlags(flags, "drafting model binary")
	target := flags.String("binary", "", "the harness to profile (a bare name found on PATH)")
	var helpArgs modelArgs
	flags.Var(&helpArgs, "help-args", "arguments before --help for one more help text, repeatable as one flag per word (e.g. --help-args=run for `tool run --help`)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*target) == "" || filepath.Base(*target) != *target {
		return errors.New("--binary must be a bare executable name")
	}
	cfg, err := triageModelConfig(flags)
	if err != nil {
		return err
	}
	drafter, err := climodel.New(cfg)
	if err != nil {
		return err
	}
	resolved, err := exec.LookPath(*target)
	if err != nil {
		return fmt.Errorf("%s is not on PATH: %w", *target, err)
	}
	helps := map[string]string{}
	prefixes := [][]string{nil}
	if len(helpArgs.args) > 0 {
		prefixes = append(prefixes, helpArgs.args)
	}
	for _, sub := range prefixes {
		text, err := helpText(ctx, cfg.RunAsUser, resolved, sub)
		if err != nil {
			return err
		}
		helps[strings.TrimSpace(*target+" "+strings.Join(sub, " "))+" --help"] = text
	}
	draftCtx, cancel := context.WithTimeout(ctx, drafter.Timeout())
	defer cancel()
	profile, err := drafter.DraftProfile(draftCtx, *target, helps)
	if err != nil {
		return err
	}
	if !contains(profile.Binaries, *target) {
		return fmt.Errorf("the drafted profile runs %v, not %s", profile.Binaries, *target)
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	active := filepath.Join(home, "model-profiles")
	pending := filepath.Join(active, "pending")
	if err := os.MkdirAll(pending, 0o700); err != nil {
		return err
	}
	for _, dir := range []string{active, pending} {
		if _, err := climodel.FindProfile(profile.Name, dir); err == nil {
			return fmt.Errorf("a profile named %q already exists in %s; remove it first", profile.Name, dir)
		}
	}
	raw, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(pending, profile.Name+".json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	fmt.Printf("drafted %s\n%s\n\nRead it, then: summa42-box model-probe --model-profile=%s --model-profile-dir=%s --model-name=<id> --model-user=<user>\nand: summa42-box model-profile-adopt --name=%s\n", path, raw, profile.Name, pending, profile.Name)
	return nil
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// helpText runs binary --help as the model user, so an unknown executable never
// runs as the Box's owner.
func helpText(ctx context.Context, runAs, binary string, prefix []string) (string, error) {
	user, err := climodel.ResolveRunAsUser(runAs)
	if err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, profileHelpTimeout)
	defer cancel()
	cmd, err := modelrun.Command(runCtx, modelrun.Spec{RunAs: user, Binary: binary, Args: append(append([]string{}, prefix...), "--help"), Timeout: profileHelpTimeout, Scratch: true})
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil && out.Len() == 0 {
		return "", fmt.Errorf("%s --help: %w", binary, err)
	}
	if out.Len() > 64<<10 {
		out.Truncate(64 << 10)
	}
	return out.String(), nil
}

// runModelProfileAdopt moves a pending profile into the active directory after
// validating it again. It is the owner's decision, so it needs --yes.
func runModelProfileAdopt(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("model-profile-adopt", flag.ContinueOnError)
	name := flags.String("name", "", "profile name in the pending directory")
	yes := flags.Bool("yes", false, "confirm that you read the profile and probed it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*yes {
		return errors.New("adopting a profile lets the Box launch that harness; read the pending file, probe it, then pass --yes")
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	active := filepath.Join(home, "model-profiles")
	// The bytes that were validated are the bytes installed: nothing is renamed
	// from a directory another process could change between check and use.
	profile, raw, err := climodel.ReadProfileFile(filepath.Join(active, "pending", *name+".json"))
	if err != nil {
		return err
	}
	if profile.Name != *name {
		return fmt.Errorf("pending file %s.json holds profile %q", *name, profile.Name)
	}
	if _, err := climodel.FindProfile(*name, active); err == nil {
		return fmt.Errorf("profile %q already exists (built-in or active)", *name)
	}
	target := filepath.Join(active, *name+".json")
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		os.Remove(target)
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	fmt.Printf("adopted %s\n", *name)
	return nil
}
