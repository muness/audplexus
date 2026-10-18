package errs

import "strings"

// CleanDownloadError uses Audible's license formatter for license failures.
func CleanDownloadError(raw string) string {
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "license denied") || strings.Contains(lower, "customerthrottled") || strings.Contains(lower, "being throttled") {
		return CleanEntitlementReason(raw)
	}
	return CleanForDisplay(raw)
}
