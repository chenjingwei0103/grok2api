package audit

import "strings"

// NormalizeReasoningEffort accepts only bounded, non-sensitive audit values.
// Provider adapters must resolve client aliases before calling this function.
func NormalizeReasoningEffort(value string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(value)); normalized {
	case "auto", "none", "minimal", "low", "medium", "high", "xhigh", "max", "fixed":
		return normalized
	default:
		return ""
	}
}
