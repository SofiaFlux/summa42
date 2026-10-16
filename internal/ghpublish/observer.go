package ghpublish

import (
	"context"
	"errors"
	"fmt"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	"strings"
)

type CIState string

const (
	CIStateSuccess CIState = "SUCCESS"
	CIStatePending CIState = "PENDING"
	CIStateFailed  CIState = "FAILED"
)

type CheckRunStatus struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadSHA    string `json:"head_sha"`
}

type CIObservation struct {
	HeadSHA string           `json:"head_sha"`
	State   CIState          `json:"state"`
	Runs    []CheckRunStatus `json:"runs"`
}

type ReviewState string

const (
	ReviewApproved         ReviewState = "APPROVED"
	ReviewChangesRequested ReviewState = "CHANGES_REQUESTED"
	ReviewCommented        ReviewState = "COMMENTED"
	ReviewDismissed        ReviewState = "DISMISSED"
)

type ReviewStatus struct {
	User      string      `json:"user"`
	State     ReviewState `json:"state"`
	CommitSHA string      `json:"commit_sha"`
}

type ReviewObservation struct {
	HeadSHA          string         `json:"head_sha"`
	Approved         bool           `json:"approved"`
	ChangesRequested bool           `json:"changes_requested"`
	Reviews          []ReviewStatus `json:"reviews"`
}

type Observer struct {
	c *client
}

func NewObserver(c Config) (*Observer, error) {
	cl, err := newClient(c)
	if err != nil {
		return nil, err
	}
	return &Observer{c: cl}, nil
}

func (o *Observer) ObserveCI(ctx context.Context, headSHA string, requiredChecks []string) (CIObservation, error) {
	if o == nil || o.c == nil {
		return CIObservation{}, errors.New("observer not configured")
	}
	if !repoworkspace.FullCommit(headSHA) {
		return CIObservation{}, errors.New("invalid commit SHA for CI observation")
	}

	var payload struct {
		TotalCount int `json:"total_count"`
		CheckRuns  []struct {
			Name       string  `json:"name"`
			Status     string  `json:"status"`
			Conclusion *string `json:"conclusion"`
			HeadSHA    string  `json:"head_sha"`
		} `json:"check_runs"`
	}

	path := fmt.Sprintf("/commits/%s/check-runs", headSHA)
	if _, err := o.c.request(ctx, "GET", path, nil, &payload); err != nil {
		return CIObservation{}, fmt.Errorf("query check-runs: %w", err)
	}

	runs := make([]CheckRunStatus, 0, len(payload.CheckRuns))
	seenRequired := make(map[string]bool)
	hasPending := false
	hasFailure := false

	for _, cr := range payload.CheckRuns {
		conclusion := ""
		if cr.Conclusion != nil {
			conclusion = *cr.Conclusion
		}
		status := CheckRunStatus{
			Name:       cr.Name,
			Status:     cr.Status,
			Conclusion: conclusion,
			HeadSHA:    cr.HeadSHA,
		}
		runs = append(runs, status)

		if cr.Status != "completed" {
			hasPending = true
		} else {
			switch conclusion {
			case "success", "skipped", "neutral":
				// passing or non-blocking
			default:
				hasFailure = true
			}
		}

		for _, req := range requiredChecks {
			if cr.Name == req {
				seenRequired[req] = (cr.Status == "completed" && (conclusion == "success" || conclusion == "skipped"))
			}
		}
	}

	for _, req := range requiredChecks {
		if !seenRequired[req] {
			hasPending = true
		}
	}

	state := CIStateSuccess
	if hasFailure {
		state = CIStateFailed
	} else if hasPending || len(runs) == 0 {
		state = CIStatePending
	}

	return CIObservation{
		HeadSHA: headSHA,
		State:   state,
		Runs:    runs,
	}, nil
}

func (o *Observer) ObserveReviews(ctx context.Context, pullNumber int, headSHA string) (ReviewObservation, error) {
	if o == nil || o.c == nil {
		return ReviewObservation{}, errors.New("observer not configured")
	}
	if pullNumber <= 0 || !repoworkspace.FullCommit(headSHA) {
		return ReviewObservation{}, errors.New("invalid PR number or head SHA for review observation")
	}

	var rawReviews []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		State    string `json:"state"`
		CommitID string `json:"commit_id"`
	}

	path := fmt.Sprintf("/pulls/%d/reviews", pullNumber)
	if _, err := o.c.request(ctx, "GET", path, nil, &rawReviews); err != nil {
		return ReviewObservation{}, fmt.Errorf("query PR reviews: %w", err)
	}

	approved := false
	changesRequested := false
	reviews := make([]ReviewStatus, 0, len(rawReviews))

	for _, r := range rawReviews {
		st := ReviewStatus{
			User:      r.User.Login,
			State:     ReviewState(strings.ToUpper(r.State)),
			CommitSHA: r.CommitID,
		}
		reviews = append(reviews, st)

		if st.State == ReviewChangesRequested {
			changesRequested = true
		}
		if st.State == ReviewApproved && st.CommitSHA == headSHA {
			approved = true
		}
	}

	return ReviewObservation{
		HeadSHA:          headSHA,
		Approved:         approved,
		ChangesRequested: changesRequested,
		Reviews:          reviews,
	}, nil
}
