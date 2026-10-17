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
)

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
type LicenseCooldownError struct{ Until time.Time }

func (e *LicenseCooldownError) Error() string {
	return "Audible is throttling download licenses (CustomerThrottled). Retry after " + e.Until.UTC().Format(time.RFC3339)
}

type licenseCooldown struct {
	Until time.Time     `json:"until"`
	Delay time.Duration `json:"delay"`
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

// licenseTransport guards only license POSTs. Library reads and CDN transfers
// use the underlying transport without waiting for the license permit.
type licenseTransport struct {
	settings      licenseSettings
	cooldownKey   string
	base          http.RoundTripper
	now           func() time.Time
	permit        chan struct{}
	mu            sync.Mutex
	calls         map[string]*licenseCall
	denied        map[string]deniedLicense
	localCooldown licenseCooldown
}

func newLicenseTransport(settings licenseSettings, identity string, base http.RoundTripper) *licenseTransport {
	return &licenseTransport{
		settings:    settings,
		cooldownKey: fmt.Sprintf("audible_license_cooldown_%x", sha256.Sum256([]byte(identity))),
		base:        base,
		now:         time.Now,
		permit:      make(chan struct{}, 1),
		calls:       make(map[string]*licenseCall),
		denied:      make(map[string]deniedLicense),
	}
}

func (t *licenseTransport) cooldown(ctx context.Context) (licenseCooldown, error) {
	raw, err := t.settings.GetSetting(ctx, t.cooldownKey)
	if err != nil {
		return licenseCooldown{}, fmt.Errorf("read Audible license cooldown: %w", err)
	}
	var cooldown licenseCooldown
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &cooldown); err != nil {
			return cooldown, fmt.Errorf("read Audible license cooldown: %w", err)
		}
	}
	t.mu.Lock()
	if t.localCooldown.Until.After(cooldown.Until) {
		cooldown = t.localCooldown
	}
	t.mu.Unlock()
	return cooldown, nil
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
	// Credential changes invalidate title denials, but not the account cooldown.
	key := fmt.Sprintf("%s|%x|%x", req.URL.String(), sha256.Sum256([]byte(req.Header.Get("x-adp-token"))), sha256.Sum256(body))
	t.mu.Lock()
	if call, ok := t.calls[key]; ok {
		t.mu.Unlock()
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
	case t.permit <- struct{}{}:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	defer func() { <-t.permit }()
	cooldown, err := t.cooldown(req.Context())
	if err != nil {
		return nil, err
	}
	if t.now().Before(cooldown.Until) {
		return nil, &LicenseCooldownError{Until: cooldown.Until}
	}
	for cachedKey, cached := range t.denied {
		if !t.now().Before(cached.until) {
			delete(t.denied, cachedKey)
		}
	}
	if cached, ok := t.denied[key]; ok {
		return cached.reply, nil
	}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
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
	if throttled {
		delay := cooldown.Delay * 2
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
		cooldown = licenseCooldown{Until: t.now().Add(delay), Delay: delay}
		t.mu.Lock()
		t.localCooldown = cooldown
		t.mu.Unlock()
		raw, _ := json.Marshal(cooldown)
		// Keep the stop in effect even if the database write fails.
		if err := t.settings.SetSetting(req.Context(), t.cooldownKey, string(raw)); err != nil {
			return nil, errors.Join(&LicenseCooldownError{Until: cooldown.Until}, fmt.Errorf("save Audible license cooldown: %w", err))
		}
		return nil, &LicenseCooldownError{Until: cooldown.Until}
	}
	if strings.EqualFold(result.License.Status, "Denied") {
		t.denied[key] = deniedLicense{reply: reply, until: t.now().Add(deniedLicenseTTL)}
	}
	if strings.EqualFold(result.License.Status, "Granted") && cooldown.Delay > 0 {
		if err := t.settings.SetSetting(req.Context(), t.cooldownKey, ""); err != nil {
			return nil, fmt.Errorf("clear Audible license cooldown: %w", err)
		}
		t.mu.Lock()
		t.localCooldown = licenseCooldown{}
		t.mu.Unlock()
	}
	return reply, nil
}
