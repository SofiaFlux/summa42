package control

import "encoding/json"

func (t TaskDTO) MarshalJSON() ([]byte, error) {
	type purposeDTO struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	type wireTask struct {
		ID                   any        `json:"id"`
		ParentTaskID         any        `json:"parent_task_id,omitempty"`
		Purpose              purposeDTO `json:"purpose"`
		State                any        `json:"state"`
		CurrentAttemptID     any        `json:"current_attempt_id,omitempty"`
		CurrentFence         int64      `json:"current_fence"`
		AcceptanceCriteria   []string   `json:"acceptance_criteria,omitempty"`
		RequiredCapabilities []string   `json:"required_capabilities,omitempty"`
		RequiredEnforcement  any        `json:"required_enforcement"`
		AuthorityCeiling     []string   `json:"authority_ceiling,omitempty"`
		ResourceEnvelopeID   any        `json:"resource_envelope_id,omitempty"`
		Priority             int        `json:"priority"`
		EarliestStart        any        `json:"earliest_start,omitempty"`
		Deadline             any        `json:"deadline,omitempty"`
		CreatedAt            any        `json:"created_at,omitempty"`
		UpdatedAt            any        `json:"updated_at,omitempty"`
	}
	return json.Marshal(wireTask{
		ID: t.ID, ParentTaskID: t.ParentTaskID,
		Purpose: purposeDTO{Kind: string(t.Purpose.Kind), ID: string(t.Purpose.ID)},
		State:   t.State, CurrentAttemptID: t.CurrentAttemptID, CurrentFence: t.CurrentFence,
		AcceptanceCriteria: t.AcceptanceCriteria, RequiredCapabilities: t.RequiredCapabilities,
		RequiredEnforcement: t.RequiredEnforcement, AuthorityCeiling: t.AuthorityCeiling,
		ResourceEnvelopeID: t.ResourceEnvelopeID, Priority: t.Priority,
		EarliestStart: t.EarliestStart, Deadline: t.Deadline, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	})
}
