package main

import "testing"

func TestBlockCasesNeedsAReasonAndCases(t *testing.T) {
	for _, args := range [][]string{{"case_1"}, {"--reason", " ", "case_1"}, {"--reason", "incident"}} {
		if _, _, err := parseBlockCasesFlags(args); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
	reason, ids, err := parseBlockCasesFlags([]string{"--reason", "incident", "case_1", "case_2"})
	if err != nil || reason != "incident" || len(ids) != 2 || ids[1] != "case_2" {
		t.Fatalf("got %q %v %v", reason, ids, err)
	}
}
