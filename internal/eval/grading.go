package eval

// ScoreToGrade converts a 0-100 percentage to a letter grade.
// Mirrors site-inspector's grading.js scale.
func ScoreToGrade(score, max int) string {
	if max <= 0 {
		return "N/A"
	}
	pct := float64(score) * 100.0 / float64(max)
	switch {
	case pct >= 97:
		return "A+"
	case pct >= 93:
		return "A"
	case pct >= 90:
		return "A-"
	case pct >= 87:
		return "B+"
	case pct >= 83:
		return "B"
	case pct >= 80:
		return "B-"
	case pct >= 77:
		return "C+"
	case pct >= 73:
		return "C"
	case pct >= 70:
		return "C-"
	case pct >= 67:
		return "D+"
	case pct >= 63:
		return "D"
	case pct >= 60:
		return "D-"
	default:
		return "F"
	}
}
