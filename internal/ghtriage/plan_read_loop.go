package ghtriage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
)

// planWithReads implements a bounded multi-turn read loop where the model can request
// up to 4 turns of repo file reads before delivering the final plan.
func (p *Planner) planWithReads(ctx context.Context, repo string, snap Snapshot, candidate PlanningCandidate) (PlanOutput, *GroundedInput, domain.ID, error) {
	if p.grounding == nil {
		return PlanOutput{}, nil, "", errors.New("planner grounding is not configured")
	}
	cfg := p.grounding
	if cfg.Source.Repository != repo {
		return PlanOutput{}, nil, "", errors.New("configured source repository differs from issue repository")
	}
	if len(cfg.AllowedPrefixes) == 0 {
		return PlanOutput{}, nil, "", errors.New("read mode requires allowed prefixes")
	}

	tracked, err := repoworkspace.TrackedFiles(ctx, cfg.Source.LocalPath, cfg.Source.Commit, cfg.AllowedPrefixes)
	if err != nil {
		return PlanOutput{}, nil, "", err
	}
	regular := []string{}
	for path, isRegular := range tracked {
		if isRegular {
			regular = append(regular, path)
		}
	}
	sort.Strings(regular)

	budget := cfg.ContextBytes
	if budget <= 0 || budget > repoworkspace.MaxContextBytes {
		budget = repoworkspace.MaxContextBytes
	}

	totalBytes := 0
	filesMap := make(map[string]repoworkspace.File)

	// Turn 0 input: pinned Source.Paths contents read via repoworkspace.ReadFile
	for _, pth := range cfg.Source.Paths {
		rem := budget - totalBytes
		if rem <= 0 {
			return PlanOutput{}, nil, "", errors.New("pinned files exceed context byte budget")
		}
		f, err := repoworkspace.ReadFile(ctx, cfg.Source.LocalPath, cfg.Source.Commit, pth, rem)
		if err != nil {
			return PlanOutput{}, nil, "", fmt.Errorf("read pinned file %q: %w", pth, err)
		}
		filesMap[pth] = f
		totalBytes += len(f.Content)
	}

	under := func(pth string) bool {
		for _, part := range strings.Split(pth, "/") {
			if strings.EqualFold(part, ".git") {
				return false
			}
		}
		for _, prefix := range cfg.AllowedPrefixes {
			if strings.HasPrefix(pth, prefix) {
				return true
			}
		}
		return false
	}

	var prevResults []ReadResult
	var turnEntries []TurnLogEntry

	storeTurnLog := func() domain.ID {
		if len(turnEntries) == 0 {
			return ""
		}
		raw, err := json.Marshal(PlanTurnLog{
			CaseID: string(candidate.Case.ID),
			WorkID: string(candidate.Case.CurrentWorkID),
			Turns:  turnEntries,
		})
		if err != nil {
			return ""
		}
		obj, err := p.evidence.Put(ctx, bytes.NewReader(raw), evidence.Metadata{
			MediaType: "application/json",
			Kind:      KindPlanTurnLog,
		})
		if err != nil {
			return ""
		}
		return obj.ID
	}

	// missingPaths is set when a final plan came without allowed_paths and no
	// Owner list stands in for them: the next turn asks for them again.
	missingPaths := false

	// Maximum 5 model calls: turns 0, 1, 2, 3 (reads) and turn 4 (must be final plan).
	for turn := 0; turn < 5; turn++ {
		isFinalTurn := (turn == 4) || (totalBytes >= budget)

		question := PlanQuestion
		if len(cfg.AllowedPaths) == 0 {
			// PlanQuestion's schema has no allowed_paths: outside read mode a
			// separate file selection supplies them. Here the plan must.
			question += ReadModeAllowedPathsNote
		}
		if missingPaths {
			question += "\n\nYour last plan had no allowed_paths. Answer with the final plan again and include them."
		}
		if isFinalTurn {
			question += "\n\nFinal answer required. Do not request further files; answer now with the final plan JSON object only."
		} else {
			question += "\n\nYou may request files to read under allowed_prefixes by replying with {\"read_repo_file\": [\"path\", ...]} (at most 8 paths). When ready, reply with the final plan JSON object."
		}

		currentFiles := make([]repoworkspace.File, 0, len(filesMap))
		for _, f := range filesMap {
			currentFiles = append(currentFiles, f)
		}
		sort.Slice(currentFiles, func(i, j int) bool {
			return currentFiles[i].Path < currentFiles[j].Path
		})

		contextSnapshot := repoworkspace.Snapshot{
			Schema:     repoworkspace.Schema,
			Repository: repo,
			Commit:     cfg.Source.Commit,
			Files:      currentFiles,
		}

		if len(candidate.PriorReviews) > 0 && !strings.HasSuffix(question, PriorReviewsNote) {
			question += PriorReviewsNote
		}
		input := PlanInput{
			Schema:             PlanSchemaV2,
			PriorReviews:       candidate.PriorReviews,
			Question:           question,
			Snapshot:           snap,
			Decision:           candidate.Decision,
			DecisionEvidenceID: candidate.DecisionEvidenceID,
			Grounding: &GroundedInput{
				Context: contextSnapshot,
				Contract: PlanSource{
					BaseSHA:            cfg.Source.Commit,
					ValidationCommands: cloneCommands(cfg.ValidationCommands),
				},
			},
			TrackedFiles: regular,
			Turn:         turn,
			ReadResults:  prevResults,
		}

		output, err := p.model.Plan(ctx, input)
		if err != nil {
			return PlanOutput{}, nil, "", &planFailure{class: "model-exit", err: fmt.Errorf("plan model: %w", err), turnLogID: storeTurnLog()}
		}

		if output.IsReadRequest() {
			if isFinalTurn {
				return PlanOutput{}, nil, "", &planFailure{class: "validation", err: errors.New("read request on final turn when final answer was required"), turnLogID: storeTurnLog()}
			}

			prevResults = nil
			var pathsServed []string
			var pathsDenied []string

			for _, pth := range output.ReadRepoFile {
				isPinned := slices.Contains(cfg.Source.Paths, pth)
				if !isPinned && !under(pth) {
					pathsDenied = append(pathsDenied, pth)
					prevResults = append(prevResults, ReadResult{Path: pth, Error: fmt.Sprintf("path %q is not under allowed prefixes", pth)})
					continue
				}
				if !tracked[pth] {
					pathsDenied = append(pathsDenied, pth)
					prevResults = append(prevResults, ReadResult{Path: pth, Error: fmt.Sprintf("path %q is not a tracked regular file", pth)})
					continue
				}
				if _, already := filesMap[pth]; already {
					pathsServed = append(pathsServed, pth)
					prevResults = append(prevResults, ReadResult{Path: pth})
					continue
				}
				if len(filesMap) >= repoworkspace.MaxFiles {
					pathsDenied = append(pathsDenied, pth)
					prevResults = append(prevResults, ReadResult{Path: pth, Error: fmt.Sprintf("maximum context file limit of %d reached", repoworkspace.MaxFiles)})
					continue
				}
				rem := budget - totalBytes
				if rem <= 0 {
					pathsDenied = append(pathsDenied, pth)
					prevResults = append(prevResults, ReadResult{Path: pth, Error: "context byte budget exhausted"})
					continue
				}
				f, err := repoworkspace.ReadFile(ctx, cfg.Source.LocalPath, cfg.Source.Commit, pth, rem)
				if err != nil {
					pathsDenied = append(pathsDenied, pth)
					prevResults = append(prevResults, ReadResult{Path: pth, Error: err.Error()})
					continue
				}
				filesMap[pth] = f
				totalBytes += len(f.Content)
				pathsServed = append(pathsServed, pth)
				prevResults = append(prevResults, ReadResult{Path: pth})
			}

			turnEntries = append(turnEntries, TurnLogEntry{
				Turn:           turn,
				PathsRequested: append([]string(nil), output.ReadRepoFile...),
				PathsServed:    pathsServed,
				PathsDenied:    pathsDenied,
				Bytes:          totalBytes,
			})
			continue
		}

		// Final plan received.
		// If output.AllowedPaths is empty and cfg.AllowedPaths is present, fallback.
		if len(output.AllowedPaths) == 0 && len(cfg.AllowedPaths) > 0 {
			output.AllowedPaths = append([]string(nil), cfg.AllowedPaths...)
		}
		if len(output.AllowedPaths) == 0 && !isFinalTurn {
			missingPaths = true
			continue
		}

		// Validate allowed_paths against checkSelection rules
		if err := checkSelection(FileSelection{AllowedPaths: output.AllowedPaths}, cfg, tracked); err != nil {
			return PlanOutput{}, nil, "", &planFailure{class: "validation", err: fmt.Errorf("plan allowed_paths: %w", err), turnLogID: storeTurnLog()}
		}

		finalFiles := make([]repoworkspace.File, 0, len(filesMap))
		for _, f := range filesMap {
			finalFiles = append(finalFiles, f)
		}
		sort.Slice(finalFiles, func(i, j int) bool {
			return finalFiles[i].Path < finalFiles[j].Path
		})

		finalSnapshot := repoworkspace.Snapshot{
			Schema:     repoworkspace.Schema,
			Repository: repo,
			Commit:     cfg.Source.Commit,
			Files:      finalFiles,
		}

		rawContext, err := finalSnapshot.Canonical()
		if err != nil {
			return PlanOutput{}, nil, "", err
		}
		sum := sha256.Sum256(rawContext)
		obj, found, err := p.evidence.FindByContentHash(ctx, hex.EncodeToString(sum[:]), KindSourceContext)
		if err != nil {
			return PlanOutput{}, nil, "", err
		}
		if !found {
			if obj, err = p.evidence.Put(ctx, bytes.NewReader(rawContext), evidence.Metadata{MediaType: "application/json", Kind: KindSourceContext}); err != nil {
				return PlanOutput{}, nil, "", err
			}
		}

		contract := PlanSource{
			BaseSHA:            finalSnapshot.Commit,
			ContextEvidenceID:  obj.ID,
			ContextHash:        obj.ContentHash,
			AllowedPaths:       append([]string(nil), output.AllowedPaths...),
			ValidationCommands: cloneCommands(cfg.ValidationCommands),
		}

		grounding := &GroundedInput{
			Context:  finalSnapshot,
			Contract: contract,
		}

		return output, grounding, storeTurnLog(), nil
	}

	return PlanOutput{}, nil, "", &planFailure{class: "validation", err: errors.New("planning exceeded maximum allowed turns"), turnLogID: storeTurnLog()}
}
