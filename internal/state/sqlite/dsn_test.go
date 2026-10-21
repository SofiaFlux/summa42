package sqlite

import (
	"strings"
	"testing"
)

func TestLocalFileDSNUsesImmediateTransactions(t *testing.T) {
	dsn, err := localFileDSN(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "_txlock=immediate") {
		t.Fatalf("dsn %q lacks _txlock=immediate", dsn)
	}
}
