package operations

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/SofiaFlux/summa42/internal/testutil"
)

func TestDispatchPolicyLookupsFailWhenRecordsAreMissing(t *testing.T) {
	store := testutil.OpenStore(t)
	ctx := context.Background()
	if _, _, err := preparedPolicyInput(ctx, store.DB(), "missing-operation"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("risk lookup error = %v, want sql.ErrNoRows", err)
	}
	if _, err := controlDescriptorType(ctx, store.DB(), "missing-slot"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("descriptor lookup error = %v, want sql.ErrNoRows", err)
	}
}
