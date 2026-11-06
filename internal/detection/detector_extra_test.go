package detection_test

import (
	"context"
	"testing"

	"github.com/SofiaFlux/summa42/internal/containment"
	"github.com/SofiaFlux/summa42/internal/domain"
)

func TestDetector_DetectRunawayDeniedLoop(t *testing.T) {
	ctx := context.Background()
	store, contSvc, detector := setupDetectionSuite(t)
	attemptID := domain.ID("att_det_4")
	taskID := domain.ID("task_det_4")
	seedAttempt(t, store, attemptID, taskID)

	// Repeated denied operations (e.g. hitting rate-limits / policy denials in rapid succession)
	for i := 0; i < 5; i++ {
		err := detector.ReportDeniedActionLoop(ctx, attemptID, taskID, "cloud.launch_cluster")
		if err != nil {
			t.Fatalf("report denied loop: %v", err)
		}
	}

	rec, err := contSvc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if rec.LadderState != containment.StateSuspectedRogue {
		t.Fatalf("expected StateSuspectedRogue after runaway denied loop, got %s", rec.LadderState)
	}
}

func TestDetector_DetectAuditTampering(t *testing.T) {
	ctx := context.Background()
	store, contSvc, detector := setupDetectionSuite(t)
	attemptID := domain.ID("att_det_5")
	taskID := domain.ID("task_det_5")
	seedAttempt(t, store, attemptID, taskID)

	// Tampering with evidence or dropping logs -> immediate TERMINATE
	err := detector.ReportAuditTampering(ctx, attemptID, taskID, "/evidence/blobs/sha256/override")
	if err != nil {
		t.Fatalf("report audit tampering: %v", err)
	}

	rec, err := contSvc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if rec.LadderState != containment.StateSuspectedRogue {
		t.Fatalf("expected instant StateSuspectedRogue on audit tampering, got %s", rec.LadderState)
	}
}
