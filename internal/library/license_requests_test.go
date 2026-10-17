package library

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type licenseTestSettings struct {
	mu         sync.Mutex
	values     map[string]string
	writeError error
}

func (s *licenseTestSettings) GetSetting(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[key], nil
}
func (s *licenseTestSettings) SetSetting(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeError != nil {
		return s.writeError
	}
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[key] = value
	return nil
}

type licenseTestTransport func(*http.Request) (*http.Response, error)

func (f licenseTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func licenseTestRequest(t *testing.T, asin, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.example.test/1.0/content/"+asin+"/licenserequest", strings.NewReader(`{"quality":"High"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-adp-token", token)
	return req
}
func licenseTestResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

const throttledLicense = `{"content_license":{"status_code":"Denied","license_denial_reasons":[{"rejectionReason":"CustomerThrottled","validationType":"Ownership"}]}}`
const deniedTestLicense = `{"content_license":{"status_code":"Denied","license_denial_reasons":[{"rejectionReason":"RequesterEligibility","validationType":"Ownership"}]}}`
const grantedTestLicense = `{"content_license":{"status_code":"Granted"}}`

func TestLicenseCooldownBlocksOtherTitlesAndSurvivesRestart(t *testing.T) {
	settings := &licenseTestSettings{}
	now := time.Now()
	var requests atomic.Int32
	base := licenseTestTransport(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return licenseTestResponse(throttledLicense), nil
	})
	transport := newLicenseTransport(settings, "account-a", base)
	transport.now = func() time.Time { return now }
	_, err := transport.RoundTrip(licenseTestRequest(t, "B000FIRST", "first-token"))
	var cooldown *LicenseCooldownError
	if !errors.As(err, &cooldown) {
		t.Fatalf("got %v, want a cooldown", err)
	}
	// A new transport represents a restart or a rebuilt client after sign-in.
	restarted := newLicenseTransport(settings, "account-a", base)
	restarted.now = func() time.Time { return now }
	_, err = restarted.RoundTrip(licenseTestRequest(t, "B000OTHER", "fresh-token"))
	if !errors.As(err, &cooldown) {
		t.Fatalf("after restart: got %v, want a cooldown", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("sent %d license requests, want 1", requests.Load())
	}
	other := newLicenseTransport(settings, "account-b", base)
	other.now = func() time.Time { return now }
	_, _ = other.RoundTrip(licenseTestRequest(t, "B000OTHER", "other-account"))
	if requests.Load() != 2 {
		t.Fatal("one account's cooldown blocked another account")
	}
}

func TestLicenseCooldownExpiresAndBacksOff(t *testing.T) {
	settings := &licenseTestSettings{}
	now := time.Now()
	var requests int
	transport := newLicenseTransport(settings, "account", licenseTestTransport(func(*http.Request) (*http.Response, error) {
		requests++
		if requests < 3 {
			return licenseTestResponse(throttledLicense), nil
		}
		return licenseTestResponse(grantedTestLicense), nil
	}))
	transport.now = func() time.Time { return now }
	_, err := transport.RoundTrip(licenseTestRequest(t, "B000FIRST", "token"))
	var first *LicenseCooldownError
	if !errors.As(err, &first) {
		t.Fatal(err)
	}
	now = first.Until.Add(time.Second)
	_, err = transport.RoundTrip(licenseTestRequest(t, "B000FIRST", "token"))
	var second *LicenseCooldownError
	if !errors.As(err, &second) {
		t.Fatal(err)
	}
	if second.Until.Sub(now) != 2*firstLicenseCooldown {
		t.Fatal("repeated throttling did not extend the cooldown")
	}
	now = second.Until.Add(time.Second)
	response, err := transport.RoundTrip(licenseTestRequest(t, "B000FIRST", "token"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	cooldown, err := transport.cooldown(context.Background())
	if err != nil || !cooldown.Until.IsZero() {
		t.Fatalf("successful probe left cooldown: %v, %v", cooldown, err)
	}
}

func TestDeniedLicensesExpireAndFreshCredentialsSkipTheCache(t *testing.T) {
	now := time.Now()
	var requests int
	transport := newLicenseTransport(&licenseTestSettings{}, "account", licenseTestTransport(func(*http.Request) (*http.Response, error) {
		requests++
		return licenseTestResponse(deniedTestLicense), nil
	}))
	transport.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		response, err := transport.RoundTrip(licenseTestRequest(t, "B000TITLE", "old-token"))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	if requests != 1 {
		t.Fatalf("repeated denial made %d requests", requests)
	}
	response, err := transport.RoundTrip(licenseTestRequest(t, "B000TITLE", "new-token"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if requests != 2 {
		t.Fatal("fresh credentials reused an old title denial")
	}
	now = now.Add(deniedLicenseTTL + time.Second)
	response, err = transport.RoundTrip(licenseTestRequest(t, "B000TITLE", "new-token"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if requests != 3 {
		t.Fatal("expired denial was not checked again")
	}
}

func TestLicenseRequestsShareConcurrentResults(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	transport := newLicenseTransport(&licenseTestSettings{}, "account", licenseTestTransport(func(*http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		return licenseTestResponse(grantedTestLicense), nil
	}))
	results := make(chan error, 2)
	request := func() {
		response, err := transport.RoundTrip(licenseTestRequest(t, "B000TITLE", "token"))
		if err == nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if string(body) != grantedTestLicense {
				err = errors.New("shared response body was consumed by another caller")
			}
		}
		results <- err
	}
	go request()
	<-started
	go request()
	select {
	case err := <-results:
		t.Fatalf("request completed before the shared response: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("sent %d concurrent license requests, want 1", requests.Load())
	}
}

func TestLicenseCooldownDoesNotBlockLibraryReads(t *testing.T) {
	transport := newLicenseTransport(&licenseTestSettings{}, "account", licenseTestTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/licenserequest") {
			return licenseTestResponse(throttledLicense), nil
		}
		return licenseTestResponse(`{"items":[]}`), nil
	}))
	_, _ = transport.RoundTrip(licenseTestRequest(t, "B000TITLE", "token"))
	req, _ := http.NewRequest(http.MethodGet, "https://api.example.test/1.0/library", nil)
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestLicenseCooldownRemainsInMemoryIfSavingFails(t *testing.T) {
	settings := &licenseTestSettings{writeError: errors.New("database unavailable")}
	var requests int
	transport := newLicenseTransport(settings, "account", licenseTestTransport(func(*http.Request) (*http.Response, error) {
		requests++
		return licenseTestResponse(throttledLicense), nil
	}))
	_, err := transport.RoundTrip(licenseTestRequest(t, "B000FIRST", "token"))
	if err == nil {
		t.Fatal("lost the cooldown write error")
	}
	_, err = transport.RoundTrip(licenseTestRequest(t, "B000OTHER", "token"))
	var cooldown *LicenseCooldownError
	if !errors.As(err, &cooldown) || requests != 1 {
		t.Fatalf("database failure allowed another request: count=%d, error=%v", requests, err)
	}
}

func TestHTTP429HonorsRetryAfter(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, retryAfter := range []string{"7200", now.Add(2 * time.Hour).UTC().Format(http.TimeFormat)} {
		t.Run(retryAfter, func(t *testing.T) {
			transport := newLicenseTransport(&licenseTestSettings{}, "account", licenseTestTransport(func(*http.Request) (*http.Response, error) {
				response := licenseTestResponse(`{}`)
				response.StatusCode = http.StatusTooManyRequests
				response.Header.Set("Retry-After", retryAfter)
				return response, nil
			}))
			transport.now = func() time.Time { return now }
			_, err := transport.RoundTrip(licenseTestRequest(t, "B000000001", "token"))
			var cooldown *LicenseCooldownError
			if !errors.As(err, &cooldown) || !cooldown.Until.Equal(now.Add(2*time.Hour)) {
				t.Fatalf("Retry-After %q produced %v", retryAfter, err)
			}
		})
	}
}

func TestExpiredCooldownAllowsOnlyOneProbeWhenAudibleStillThrottles(t *testing.T) {
	now := time.Now()
	var requests atomic.Int32
	transport := newLicenseTransport(&licenseTestSettings{}, "account", licenseTestTransport(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return licenseTestResponse(throttledLicense), nil
	}))
	transport.now = func() time.Time { return now }
	_, err := transport.RoundTrip(licenseTestRequest(t, "B000000001", "token"))
	var cooldown *LicenseCooldownError
	if !errors.As(err, &cooldown) {
		t.Fatal(err)
	}
	now = cooldown.Until.Add(time.Second)
	var workers sync.WaitGroup
	for i := 0; i < 50; i++ {
		req := licenseTestRequest(t, fmt.Sprintf("B%09d", i), "token")
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := transport.RoundTrip(req)
			var cooldown *LicenseCooldownError
			if !errors.As(err, &cooldown) {
				t.Errorf("request bypassed renewed cooldown: %v", err)
			}
		}()
	}
	workers.Wait()
	if requests.Load() != 2 {
		t.Fatalf("made %d requests, want initial request and one probe", requests.Load())
	}
}

func TestCancelledLicenseRequestDoesNotReachAudible(t *testing.T) {
	transport := newLicenseTransport(&licenseTestSettings{}, "account", licenseTestTransport(func(*http.Request) (*http.Response, error) { t.Fatal("sent a cancelled request"); return nil, nil }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := transport.RoundTrip(licenseTestRequest(t, "B000000001", "token").WithContext(ctx))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
}
