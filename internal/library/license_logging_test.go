package library

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mstrhakr/audplexus/internal/logging"
)

func TestLicenseLogsDistinguishNetworkRequestsFromCooldownSkipsWithoutSecrets(t *testing.T) {
	logging.Init("info", true, logging.FileConfig{})
	transport := newLicenseTransport(&licenseTestSettings{}, "synthetic-private-customer", licenseTestTransport(func(*http.Request) (*http.Response, error) {
		return licenseTestResponse(strings.Replace(throttledLicense, `"status_code":"Denied"`, `"private_fixture":"synthetic-private-license","status_code":"Denied"`, 1)), nil
	}))
	_, _ = transport.RoundTrip(licenseTestRequest(t, "B000000001", "synthetic-private-token"))
	_, _ = transport.RoundTrip(licenseTestRequest(t, "B000000002", "synthetic-private-token"))
	counts := map[string]int{}
	for _, entry := range logging.TailLogs(50) {
		parsed := logging.ParseLogLine(entry.Line)
		if parsed.Component != "audible_license" || parsed.Fields["account_key"] != transport.cooldownKey {
			continue
		}
		for _, secret := range []string{"synthetic-private-customer", "synthetic-private-token", "synthetic-private-license", "licenserequest", "api.example.test"} {
			if strings.Contains(entry.Line, secret) {
				t.Fatalf("license log exposed %q", secret)
			}
		}
		counts[parsed.Fields["license_event"]]++
	}
	for _, event := range []string{"request_sent", "request_result", "cooldown_started", "cooldown_skip"} {
		if counts[event] != 1 {
			t.Fatalf("event %s appeared %d times, want 1", event, counts[event])
		}
	}
}
