package board

import (
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

type Column string

const (
	ColumnIntake     Column = "INTAKE"
	ColumnTriage     Column = "TRIAGE"
	ColumnReady      Column = "READY"
	ColumnInProgress Column = "IN_PROGRESS"
	ColumnReview     Column = "REVIEW"
	ColumnDone       Column = "DONE"
	ColumnHeld       Column = "HELD"
)

func IsValidColumn(c Column) bool {
	switch c {
	case ColumnIntake, ColumnTriage, ColumnReady, ColumnInProgress, ColumnReview, ColumnDone, ColumnHeld:
		return true
	default:
		return false
	}
}

type Item struct {
	MissionID            domain.ID
	Source               string
	SourceID             string
	RevisionID           string
	Title                string
	Description          string
	RequiredCapabilities []string
	Grant                workflow.Grant
}

type Card struct {
	ID                   domain.ID
	MissionID            domain.ID
	Source               string
	SourceID             string
	RevisionID           string
	Column               Column
	Title                string
	Description          string
	RequiredCapabilities []string
	Grant                workflow.Grant
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type Move struct {
	ID          domain.ID
	CardID      domain.ID
	FromColumn  Column
	ToColumn    Column
	Actor       string
	Reason      string
	EvidenceIDs []domain.ID
	CreatedAt   time.Time
}

type MoveRequest struct {
	CardID      domain.ID
	ToColumn    Column
	Actor       string
	Reason      string
	EvidenceIDs []domain.ID
}

type HeldCardInfo struct {
	CardID   domain.ID `json:"card_id"`
	Source   string    `json:"source"`
	SourceID string    `json:"source_id"`
	Title    string    `json:"title"`
	Reason   string    `json:"reason"`
	HeldAt   time.Time `json:"held_at"`
}

type OwnerSummary struct {
	MissionID    domain.ID      `json:"mission_id"`
	TotalCards   int            `json:"total_cards"`
	ColumnCounts map[Column]int `json:"column_counts"`
	HeldCards    []HeldCardInfo `json:"held_cards"`
}

type Connector interface {
	Source() string
	ToItem(missionID domain.ID, raw any) (Item, error)
}
