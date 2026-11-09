package company

import (
	"context"
	"fmt"
)

type ProjectionTranslation struct {
	Kind           EntityKind     `json:"kind"`
	Name           string         `json:"name"`
	Status         string         `json:"status"`
	Attributes     map[string]any `json:"attributes"`
	ExposureAmount int64          `json:"exposure_amount"`
}

// ProjectionAdapter translates external projections (CRM, ERP, cloud, Git)
// into canonical CompanyState entity representations without coupling kernel or company
// logic to proprietary external APIs.
type ProjectionAdapter interface {
	SourceSystem() string
	Translate(ctx context.Context, externalID string, payload map[string]any) (ProjectionTranslation, error)
}

type CRMAdapter struct{}

func (a *CRMAdapter) SourceSystem() string {
	return "CRM"
}

func (a *CRMAdapter) Translate(ctx context.Context, externalID string, payload map[string]any) (ProjectionTranslation, error) {
	name, _ := payload["name"].(string)
	if name == "" {
		name = fmt.Sprintf("CRM Customer %s", externalID)
	}
	status, _ := payload["status"].(string)
	if status == "" {
		status = "ACTIVE"
	}
	var exposure int64
	if exp, ok := payload["contract_value"].(float64); ok {
		exposure = int64(exp)
	} else if exp, ok := payload["contract_value"].(int64); ok {
		exposure = exp
	} else if exp, ok := payload["contract_value"].(int); ok {
		exposure = int64(exp)
	}
	return ProjectionTranslation{
		Kind:           KindCustomer,
		Name:           name,
		Status:         status,
		Attributes:     payload,
		ExposureAmount: exposure,
	}, nil
}

type ERPAdapter struct{}

func (a *ERPAdapter) SourceSystem() string {
	return "ERP"
}

func (a *ERPAdapter) Translate(ctx context.Context, externalID string, payload map[string]any) (ProjectionTranslation, error) {
	name, _ := payload["description"].(string)
	if name == "" {
		name = fmt.Sprintf("ERP Account %s", externalID)
	}
	status, _ := payload["status"].(string)
	if status == "" {
		status = "SETTLED"
	}
	var exposure int64
	if exp, ok := payload["payable_balance"].(float64); ok {
		exposure = int64(exp)
	} else if exp, ok := payload["payable_balance"].(int64); ok {
		exposure = exp
	} else if exp, ok := payload["payable_balance"].(int); ok {
		exposure = int64(exp)
	}
	return ProjectionTranslation{
		Kind:           KindCommitment,
		Name:           name,
		Status:         status,
		Attributes:     payload,
		ExposureAmount: exposure,
	}, nil
}

type CloudAdapter struct{}

func (a *CloudAdapter) SourceSystem() string {
	return "CLOUD"
}

func (a *CloudAdapter) Translate(ctx context.Context, externalID string, payload map[string]any) (ProjectionTranslation, error) {
	name, _ := payload["resource_name"].(string)
	if name == "" {
		name = fmt.Sprintf("Cloud Asset %s", externalID)
	}
	status, _ := payload["health"].(string)
	if status == "" {
		status = "HEALTHY"
	}
	return ProjectionTranslation{
		Kind:           KindAsset,
		Name:           name,
		Status:         status,
		Attributes:     payload,
		ExposureAmount: 0,
	}, nil
}

type GitAdapter struct{}

func (a *GitAdapter) SourceSystem() string {
	return "GIT"
}

func (a *GitAdapter) Translate(ctx context.Context, externalID string, payload map[string]any) (ProjectionTranslation, error) {
	name, _ := payload["repo_name"].(string)
	if name == "" {
		name = fmt.Sprintf("Git Service %s", externalID)
	}
	status, _ := payload["compliance_status"].(string)
	if status == "" {
		status = "COMPLIANT"
	}
	return ProjectionTranslation{
		Kind:           KindProductService,
		Name:           name,
		Status:         status,
		Attributes:     payload,
		ExposureAmount: 0,
	}, nil
}
