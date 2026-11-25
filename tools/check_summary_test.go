package tools

import (
	"reflect"
	"testing"
)

func TestSummarizeStatusesAndCategories(t *testing.T) {
	results := []CheckResult{
		{Entry: Entry{Name: "matched", Category: "a"}, Status: StatusOK},
		{Entry: Entry{Name: "unverified", Category: "a"}, Status: StatusUnpinned},
		{Entry: Entry{Name: "absent", Category: "a"}, Status: StatusMissing},
		{Entry: Entry{Name: "different", Category: "a"}, Status: StatusMismatch},
		{Entry: Entry{Name: "other", Category: "b"}, Status: StatusOK},
	}
	summary := Summarize(results, "a")
	if summary.Total() != 4 || summary.Present() != 3 || !summary.HasWarnings() {
		t.Fatalf("unexpected counts: %+v", summary)
	}
	if !reflect.DeepEqual(summary.Mismatch, []string{"different"}) {
		t.Fatalf("hash mismatch was not classified: %+v", summary)
	}
	if Summarize(results, "").Total() != 5 {
		t.Fatal("unfiltered summary lost an entry")
	}
	if results[2].Status != StatusMissing {
		t.Fatal("the input was modified")
	}
}

func TestSummarizeEmptyAndUnknown(t *testing.T) {
	empty := Summarize(nil, "")
	if empty.Total() != 0 || empty.HasWarnings() {
		t.Fatalf("unexpected empty summary: %+v", empty)
	}
	results := []CheckResult{{Entry: Entry{Name: "future", Category: "a"}, Status: Status(99)}}
	summary := Summarize(results, "a")
	if summary.Total() != 1 || summary.Present() != 0 || !summary.HasWarnings() {
		t.Fatalf("unknown status was hidden: %+v", summary)
	}
	filtered := Summarize(results, "b")
	if filtered.Total() != 0 {
		t.Fatal("unmatched category was included")
	}
}
