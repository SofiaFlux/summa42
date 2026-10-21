package runtime

import (
	"runtime/debug"
	"strings"

	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/runmanifest"
)

func resolveBuildMetadata(explicitVersion, explicitCommit string) runmanifest.BuildMetadata {
	version := strings.TrimSpace(explicitVersion)
	commit := strings.TrimSpace(explicitCommit)
	if version != "" && commit != "" {
		return runmanifest.BuildMetadata{RuntimeVersion: version, RuntimeCommit: commit}
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return runmanifest.BuildMetadata{RuntimeVersion: version, RuntimeCommit: commit}
	}
	if version == "" {
		candidate := strings.TrimSpace(info.Main.Version)
		if candidate != "" && candidate != "(devel)" {
			version = candidate
		}
	}
	if commit == "" {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				commit = strings.TrimSpace(setting.Value)
				break
			}
		}
	}
	return runmanifest.BuildMetadata{RuntimeVersion: version, RuntimeCommit: commit}
}

type policyMetadataSource interface {
	Metadata() (policy.OPAMetadata, error)
}

func capturePolicySnapshot(engine policy.PolicyEngine) (runmanifest.PolicySnapshot, error) {
	source, ok := engine.(policyMetadataSource)
	if !ok {
		return runmanifest.PolicySnapshot{}, nil
	}
	metadata, err := source.Metadata()
	if err != nil {
		return runmanifest.PolicySnapshot{}, err
	}
	return runmanifest.PolicySnapshot{
		PolicySetID:            metadata.PolicySetID,
		PolicySetHash:          metadata.PolicySetHash,
		PolicyCapabilitiesHash: metadata.PolicyCapabilitiesHash,
	}, nil
}
