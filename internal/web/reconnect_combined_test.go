package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mstrhakr/audplexus/internal/crypto"
	"github.com/mstrhakr/audplexus/internal/database"
	"github.com/mstrhakr/audplexus/internal/library"
	audible "github.com/mstrhakr/go-audible"
)

func TestCombinedAndroidCallbackDoesNotInheritIPhoneRefusal(t *testing.T) {
	for _, reason := range []string{"CustomerThrottled", "HTTP429"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			db := newWebTestDB(t)
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatal(err)
			}
			signingKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
			oldCredentials := audible.Credentials{ADPToken: "synthetic-iphone", AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour), CustomerID: "synthetic-customer", Marketplace: "us", DevicePrivateKey: signingKey, DeviceInfo: audible.DeviceInfo{DeviceType: audible.DeviceTypeID, DeviceSerialNumber: "0123456789abcdef0123456789abcdef"}}
			box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(oldCredentials)
			sealed, _ := box.Encrypt(raw)
			account := &database.AudibleAccount{ID: "same-account", Marketplace: "us", CustomerID: "synthetic-customer", Enabled: true, Credentials: sealed}
			if err := db.CreateAudibleAccount(ctx, account); err != nil {
				t.Fatal(err)
			}
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			licenses, registrations := 0, 0
			http.DefaultTransport = registrationTransport(func(request *http.Request) (*http.Response, error) {
				status := http.StatusOK
				header := make(http.Header)
				body := ""
				switch {
				case request.URL.Path == "/auth/register":
					registrations++
					var registration struct {
						Data map[string]any `json:"registration_data"`
					}
					if err := json.NewDecoder(request.Body).Decode(&registration); err != nil {
						t.Fatal(err)
					}
					if registration.Data["device_type"] != "A10KISP2GWF0E4" {
						t.Fatal("callback did not register Android")
					}
					reply := map[string]any{"response": map[string]any{"success": map[string]any{"tokens": map[string]any{"bearer": map[string]any{"access_token": "synthetic-android-access", "refresh_token": "synthetic-refresh", "expires_in": 3600}, "mac_dms": map[string]any{"adp_token": "synthetic-android", "device_private_key": signingKey}}, "extensions": map[string]any{"customer_info": map[string]any{"user_id": "synthetic-customer"}}}}}
					encoded, _ := json.Marshal(reply)
					body = string(encoded)
				case strings.HasSuffix(request.URL.Path, "/licenserequest"):
					licenses++
					if request.Header.Get("x-adp-token") == "synthetic-iphone" {
						body = `{"content_license":{"status_code":"Denied","license_denial_reasons":[{"rejectionReason":"CustomerThrottled"}]}}`
						if reason == "HTTP429" {
							status = http.StatusTooManyRequests
							header.Set("Retry-After", "7200")
						}
					} else {
						body = `{"content_license":{"status_code":"Granted","content_url":"https://cdn.example.test/book.aax"}}`
					}
				default:
					t.Fatalf("unexpected request: %s", request.URL)
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			s := &Server{db: db, router: gin.New(), accounts: library.NewAccountManager(ctx, db, box)}
			s.refreshPrimaryAudible()
			s.setupTemplates()
			s.onboarded.Store(true)
			s.router.POST("/auth/start", s.handleAuthStart)
			s.router.POST("/auth/callback", s.handleAuthCallback)
			_, err = s.accounts.ClientForAccount(account.ID).GetDownloadInfo(ctx, "B000000001")
			var pause *library.LicenseCooldownError
			if !errors.As(err, &pause) {
				t.Fatalf("initial refusal missing: %v", err)
			}
			post := func(path string, form url.Values) *httptest.ResponseRecorder {
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				response := httptest.NewRecorder()
				s.router.ServeHTTP(response, request)
				return response
			}
			response := post("/auth/start", url.Values{"account_id": {account.ID}, "marketplace": {"us"}, "device_profile": {"android"}})
			if response.Code != http.StatusOK {
				t.Fatalf("start: %d", response.Code)
			}
			pending := s.pendingAuth.entries[account.ID]
			response = post("/auth/callback", url.Values{"account_id": {account.ID}, "device_profile": {"android"}, "device_serial": {pending.deviceSerial}, "code_verifier": {pending.codeVerifier}, "redirect_url": {"https://www.audible.com/ap/maplanding?openid.oa2.authorization_code=synthetic-code"}})
			if response.Code != http.StatusOK || registrations != 1 {
				t.Fatalf("callback: %d, registrations: %d", response.Code, registrations)
			}
			saved, _ := db.GetAudibleAccount(ctx, account.ID)
			if !crypto.IsEncrypted(saved.Credentials) {
				t.Fatal("reconnect saved unencrypted credentials")
			}
			accounts := library.NewAccountManager(ctx, db, box)
			info, err := accounts.ClientForAccount(account.ID).GetDownloadInfo(ctx, "B000000001")
			if reason == "CustomerThrottled" {
				if err != nil || info == nil || licenses != 2 {
					t.Fatalf("Android blocked after callback: licenses=%d, error=%v", licenses, err)
				}
			} else if !errors.As(err, &pause) || licenses != 1 {
				t.Fatalf("HTTP 429 bypassed after callback: licenses=%d, error=%v", licenses, err)
			}
		})
	}
}
