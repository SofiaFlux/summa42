package board

import (
	"errors"
	"fmt"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

type GitHubConnector struct{}

func NewGitHubConnector() *GitHubConnector {
	return &GitHubConnector{}
}

func (c *GitHubConnector) Source() string {
	return "github"
}

func (c *GitHubConnector) ToItem(missionID domain.ID, raw any) (Item, error) {
	data, ok := raw.(map[string]any)
	if !ok {
		return Item{}, errors.New("github connector requires map[string]any payload")
	}
	repo, _ := data["repo"].(string)
	issueID := data["issue_id"]
	revisionID, _ := data["revision_id"].(string)
	title, _ := data["title"].(string)
	desc, _ := data["description"].(string)

	if strings.TrimSpace(repo) == "" || issueID == nil || strings.TrimSpace(revisionID) == "" {
		return Item{}, errors.New("github connector payload requires repo, issue_id, and revision_id")
	}

	sourceID := fmt.Sprintf("%s#%v", repo, issueID)
	return Item{
		MissionID:   missionID,
		Source:      c.Source(),
		SourceID:    sourceID,
		RevisionID:  revisionID,
		Title:       title,
		Description: desc,
	}, nil
}

type FakeADOConnector struct {
	orgURL string
}

func NewFakeADOConnector(orgURL string) *FakeADOConnector {
	return &FakeADOConnector{orgURL: orgURL}
}

func (c *FakeADOConnector) Source() string {
	return "azure_devops"
}

func (c *FakeADOConnector) ToItem(missionID domain.ID, raw any) (Item, error) {
	data, ok := raw.(map[string]any)
	if !ok {
		return Item{}, errors.New("ado connector requires map[string]any payload")
	}
	workItemID := data["work_item_id"]
	rev := data["rev"]
	title, _ := data["title"].(string)
	desc, _ := data["description"].(string)

	if workItemID == nil || rev == nil {
		return Item{}, errors.New("ado connector payload requires work_item_id and rev")
	}

	sourceID := fmt.Sprintf("%s/_workitems/edit/%v", c.orgURL, workItemID)
	revisionID := fmt.Sprintf("rev-%v", rev)
	return Item{
		MissionID:   missionID,
		Source:      c.Source(),
		SourceID:    sourceID,
		RevisionID:  revisionID,
		Title:       title,
		Description: desc,
		Grant:       workflow.Grant{Capabilities: []string{"read"}},
	}, nil
}
