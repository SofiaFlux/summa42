package testutil

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// PolicyV1SetID and policyV1Module are the policy set every Collective
// initialized before policy_mvc_default_v2 still carries.
const (
	PolicyV1SetID  domain.ID = "policy_mvc_default_v1"
	policyV1Module           = "package summa42\n\ndefault decision := {\"outcome\": \"DENY\", \"reason_codes\": [\"default_deny\"]}\n\ndecision := {\"outcome\": \"ALLOW\", \"reason_codes\": [\"low_risk\"]} if {\n  input.risk == \"LOW\"\n  input.authority_valid == true\n}\n"
)

// UsePolicyV1 rewrites a freshly bootstrapped Collective's active policy set
// into the v1 row an existing install has (the
// capability profile is unchanged between v1 and v2), so upgrade paths can be tested.
func UsePolicyV1(t *testing.T, db *sql.DB) {
	t.Helper()
	digest := sha256.Sum256([]byte(policyV1Module))
	if _, err := db.Exec(
		`UPDATE policy_sets SET policy_set_id = ?, module = ?, policy_hash = ? WHERE active = 1`,
		PolicyV1SetID, []byte(policyV1Module), hex.EncodeToString(digest[:]),
	); err != nil {
		t.Fatal(err)
	}
}
