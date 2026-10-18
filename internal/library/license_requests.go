package library

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mstrhakr/audplexus/internal/logging"
)

var licenseLog = logging.Component("audible_license")

const (
	firstLicenseCooldown   = 15 * time.Minute
	longestLicenseCooldown = 24 * time.Hour
	deniedLicenseTTL       = 5 * time.Minute
)

type licenseSettings interface {
	GetSetting(context.Context, string) (string, error)
	SetSetting(context.Context, string, string) error
}

// LicenseCooldownError reports when another license request may be attempted.
type LicenseCooldownError struct {
	Until  time.Time
	Reason string
}

func licensePauseMessage(reason string) string {
	if reason == "http_429" {
		return "Audible returned HTTP 429. Audplexus paused license requests for this customer."
	}
	return "Audible returned CustomerThrottled for this device registration. Audplexus paused its license requests; reconnecting may help."
}

func (e *LicenseCooldownError) Error() string {
	return licensePauseMessage(e.Reason) + " Retry after " + e.Until.UTC().Format(time.RFC3339)
}

type licenseCooldown struct {
	Reason string        `json:"reason"`
	Until  time.Time     `json:"until"`
	Delay  time.Duration `json:"delay"`
}

type licenseReply struct {
	response http.Response
	body     []byte
}

func (r *licenseReply) copyFor(req *http.Request) *http.Response {
	response := r.response
	response.Request = req
	response.Header = r.response.Header.Clone()
	response.Body = io.NopCloser(bytes.NewReader(r.body))
	return &response
}

type licenseCall struct {
	done  chan struct{}
	reply *licenseReply
	err   error
}

type deniedLicense struct {
	reply *licenseReply
	until time.Time
}

// customerLicenseGate serializes requests across registrations for one customer.
// HTTP 429 pauses are shared even if saving the pause fails.
type customerLicenseGate struct {
	permit        chan struct{}
	mu            sync.Mutex
	localCooldown licenseCooldown
}

// licenseTransport guards only license POSTs. Library reads and CDN transfers
// use the underlying transport without waiting for the license permit.
type licenseTransport struct {
	settings      licenseSettings
	cooldownKey   string
	rateLimitKey  string
	customer      *customerLicenseGate
	base          http.RoundTripper
	now           func() time.Time
	mu            sync.Mutex
	calls         map[string]*licenseCall
	denied        map[string]deniedLicense
	localCooldown licenseCooldown
}

func newLicenseTransport(settings licenseSettings, identity string, base http.RoundTripper) *licenseTransport {
	return newRegistrationLicenseTransport(settings, identity, identity, base, &customerLicenseGate{permit: make(chan struct{}, 1)})
}

func newRegistrationLicenseTransport(settings licenseSettings, customer, registration string, base http.RoundTripper, gate *customerLicenseGate) *licenseTransport {
	return &licenseTransport{
		customer:     gate,
		rateLimitKey: fmt.Sprintf("audible_license_http429_%x", sha256.Sum256([]byte(customer))),
		settings:     settings,
		cooldownKey:  fmt.Sprintf("audible_license_registration_%x", sha256.Sum256([]byte(registration))),
		base:         base,
		now:          time.Now,
		calls:        make(map[string]*licenseCall),
		denied:       make(map[string]deniedLicense),
	}
}

func (t *licenseTransport) savedCooldown(ctx context.Context, key string) (licenseCooldown, error) {
	raw, err := t.settings.GetSetting(ctx, key)
	if err != nil {
		return licenseCooldown{}, fmt.Errorf("read Audible license pause: %w", err)
	}
	var pause licenseCooldown
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &pause); err != nil {
			return pause, fmt.Errorf("read Audible license pause: %w", err)
		}
	}
	return pause, nil
}

func (t *licenseTransport) cooldown(ctx context.Context) (licenseCooldown, error) {
	registration, err := t.savedCooldown(ctx, t.cooldownKey)
	if err != nil {
		return registration, err
	}
	customer, err := t.savedCooldown(ctx, t.rateLimitKey)
	if err != nil {
		return customer, err
	}
	t.mu.Lock()
	if t.localCooldown.Until.After(registration.Until) {
		registration = t.localCooldown
	}
	t.mu.Unlock()
	t.customer.mu.Lock()
	if t.customer.localCooldown.Until.After(customer.Until) {
		customer = t.customer.localCooldown
	}
	t.customer.mu.Unlock()
	// Keep expired backoff for the next refusal of the same kind, but an active
	// pause always wins over an expired one.
	if customer.Until.After(registration.Until) {
		return customer, nil
	}
	return registration, nil
}

func (t *licenseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/licenserequest") {
		return t.base.RoundTrip(req)
	}
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	// Token changes invalidate title denials. Registration identity scopes the refusal pause.
	key := fmt.Sprintf("%s|%x|%x", req.URL.String(), sha256.Sum256([]byte(req.Header.Get("x-adp-token"))), sha256.Sum256(body))
	t.mu.Lock()
	if call, ok := t.calls[key]; ok {
		t.mu.Unlock()
		licenseLog.Info().Str("account_key", t.rateLimitKey).Str("registration_key", t.cooldownKey).Str("license_event", "shared").Msg("concurrent Audible license request shared")
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-call.done:
		}
		if call.err != nil {
			return nil, call.err
		}
		return call.reply.copyFor(req), nil
	}
	call := &licenseCall{done: make(chan struct{})}
	t.calls[key] = call
	t.mu.Unlock()
	call.reply, call.err = t.requestLicense(req, key)
	t.mu.Lock()
	delete(t.calls, key)
	close(call.done)
	t.mu.Unlock()
	if call.err != nil {
		return nil, call.err
	}
	return call.reply.copyFor(req), nil
}

func (t *licenseTransport) requestLicense(req *http.Request, key string) (*licenseReply, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	select {
	case t.customer.permit <- struct{}{}:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	defer func() { <-t.customer.permit }()
	cooldown, err := t.cooldown(req.Context())
	if err != nil {
		return nil, err
	}
	if t.now().Before(cooldown.Until) {
		licenseLog.Info().Str("account_key", t.rateLimitKey).Str("registration_key", t.cooldownKey).Str("license_event", "cooldown_skip").Str("reason", cooldown.Reason).Time("retry_after", cooldown.Until).Msg("Audible license request skipped during cooldown")
		return nil, &LicenseCooldownError{Until: cooldown.Until, Reason: cooldown.Reason}
	}
	for cachedKey, cached := range t.denied {
		if !t.now().Before(cached.until) {
			delete(t.denied, cachedKey)
		}
	}
	if cached, ok := t.denied[key]; ok {
		licenseLog.Info().Str("account_key", t.rateLimitKey).Str("registration_key", t.cooldownKey).Str("license_event", "denial_cache_hit").Msg("cached Audible license denial reused")
		return cached.reply, nil
	}
	started := time.Now()
	licenseLog.Info().Str("account_key", t.rateLimitKey).Str("registration_key", t.cooldownKey).Str("license_event", "request_sent").Msg("Audible license request sent")
	response, err := t.base.RoundTrip(req)
	if err != nil {
		licenseLog.Info().Str("account_key", t.rateLimitKey).Str("registration_key", t.cooldownKey).Str("license_event", "request_result").Str("result", "transport_error").Dur("elapsed", time.Since(started)).Msg("Audible license request finished")
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		licenseLog.Info().Str("account_key", t.rateLimitKey).Str("registration_key", t.cooldownKey).Str("license_event", "request_result").Str("result", "response_read_error").Dur("elapsed", time.Since(started)).Msg("Audible license request finished")
		return nil, err
	}
	reply := &licenseReply{response: *response, body: body}
	reply.response.Body = nil
	var result struct {
		License struct {
			Status  string `json:"status_code"`
			Reasons []struct {
				Code string `json:"rejectionReason"`
			} `json:"license_denial_reasons"`
		} `json:"content_license"`
	}
	_ = json.Unmarshal(body, &result)
	throttled := response.StatusCode == http.StatusTooManyRequests
	for _, reason := range result.License.Reasons {
		throttled = throttled || strings.EqualFold(reason.Code, "CustomerThrottled")
	}
	resultName := "unknown"
	if response.StatusCode == http.StatusTooManyRequests {
		resultName = "http_429"
	} else if throttled {
		resultName = "customer_throttled"
	} else if strings.EqualFold(result.License.Status, "Granted") {
		resultName = "granted"
	} else if strings.EqualFold(result.License.Status, "Denied") {
		resultName = "denied"
	}
	licenseLog.Info().Str("account_key", t.rateLimitKey).Str("registration_key", t.cooldownKey).Str("license_event", "request_result").Str("result", resultName).Int("http_status", response.StatusCode).Dur("elapsed", time.Since(started)).Msg("Audible license request finished")
	if throttled {
		reason := "customer_throttled"
		pauseKey := t.cooldownKey
		if response.StatusCode == http.StatusTooManyRequests {
			reason = "http_429"
			pauseKey = t.rateLimitKey
		}
		previous, err := t.savedCooldown(req.Context(), pauseKey)
		if err != nil {
			return nil, err
		}
		if cooldown.Reason == reason && cooldown.Delay > previous.Delay {
			previous = cooldown
		}
		delay := previous.Delay * 2
		if delay < firstLicenseCooldown {
			delay = firstLicenseCooldown
		}
		if delay > longestLicenseCooldown {
			delay = longestLicenseCooldown
		}
		if seconds, parseErr := time.ParseDuration(response.Header.Get("Retry-After") + "s"); parseErr == nil && seconds > delay {
			delay = seconds
		}
		if until, parseErr := http.ParseTime(response.Header.Get("Retry-After")); parseErr == nil && until.Sub(t.now()) > delay {
			delay = until.Sub(t.now())
		}
		cooldown = licenseCooldown{Until: t.now().Add(delay), Delay: delay, Reason: reason}
		licenseLog.Warn().Str("account_key", t.rateLimitKey).Str("registration_key", t.cooldownKey).Str("license_event", "cooldown_started").Str("reason", reason).Time("retry_after", cooldown.Until).Dur("delay", delay).Msg("Audible license cooldown started")
		if reason == "http_429" {
			t.customer.mu.Lock()
			t.customer.localCooldown = cooldown
			t.customer.mu.Unlock()
		} else {
			t.mu.Lock()
			t.localCooldown = cooldown
			t.mu.Unlock()
		}
		raw, _ := json.Marshal(cooldown)
		// Keep the stop in effect even if the database write fails.
		if err := t.settings.SetSetting(req.Context(), pauseKey, string(raw)); err != nil {
			return nil, errors.Join(&LicenseCooldownError{Until: cooldown.Until, Reason: cooldown.Reason}, fmt.Errorf("save Audible license cooldown: %w", err))
		}
		return nil, &LicenseCooldownError{Until: cooldown.Until, Reason: cooldown.Reason}
	}
	if strings.EqualFold(result.License.Status, "Denied") {
		t.denied[key] = deniedLicense{reply: reply, until: t.now().Add(deniedLicenseTTL)}
	}
	if strings.EqualFold(result.License.Status, "Granted") && cooldown.Delay > 0 {
		for _, key := range []string{t.cooldownKey, t.rateLimitKey} {
			if err := t.settings.SetSetting(req.Context(), key, ""); err != nil {
				return nil, fmt.Errorf("clear Audible license pause: %w", err)
			}
		}
		t.customer.mu.Lock()
		t.customer.localCooldown = licenseCooldown{}
		t.customer.mu.Unlock()
		t.mu.Lock()
		t.localCooldown = licenseCooldown{}
		t.mu.Unlock()
		licenseLog.Info().Str("account_key", t.rateLimitKey).Str("registration_key", t.cooldownKey).Str("license_event", "cooldown_cleared").Msg("Audible license cooldown cleared after a grant")
	}
	return reply, nil
}
