package tools

// CheckSummary classifies the results shared by startup logs and doctor.
type CheckSummary struct {
	OK       []string
	Unpinned []string
	Missing  []string
	Mismatch []string
	Unknown  []string
}

// Summarize keeps input order and optionally filters by category.
// An empty category includes all entries.
func Summarize(results []CheckResult, category string) CheckSummary {
	var summary CheckSummary
	for _, result := range results {
		if category != "" && result.Entry.Category != category {
			continue
		}
		switch result.Status {
		case StatusOK:
			summary.OK = append(summary.OK, result.Entry.Name)
		case StatusUnpinned:
			summary.Unpinned = append(summary.Unpinned, result.Entry.Name)
		case StatusMissing:
			summary.Missing = append(summary.Missing, result.Entry.Name)
		case StatusMismatch:
			summary.Mismatch = append(summary.Mismatch, result.Entry.Name)
		default:
			summary.Unknown = append(summary.Unknown, result.Entry.Name)
		}
	}
	return summary
}

// Present counts files that exist, including files with a mismatched hash.
func (s CheckSummary) Present() int {
	return len(s.OK) + len(s.Unpinned) + len(s.Mismatch)
}

func (s CheckSummary) Total() int {
	return s.Present() + len(s.Missing) + len(s.Unknown)
}

func (s CheckSummary) HasWarnings() bool {
	return len(s.Missing) > 0 || len(s.Mismatch) > 0 || len(s.Unknown) > 0
}
