package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mstrhakr/audplexus/internal/database"
	"github.com/mstrhakr/audplexus/internal/library"
	audible "github.com/mstrhakr/go-audible"
)

func TestDiagnosticsRequestsLicenseOnlyWhenAsked(t *testing.T) {
	ctx := context.Background()
	db := newWebTestDB(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := json.Marshal(&audible.Credentials{
		ADPToken: "synthetic-token", AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour),
		DevicePrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
	})
	if err != nil {
		t.Fatal(err)
	}
	account := &database.AudibleAccount{ID: "synthetic-account", DisplayName: "Synthetic account", Marketplace: "us", Credentials: credentials, Enabled: true}
	if err := db.CreateAudibleAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	manager := library.NewAccountManager(ctx, db, nil)
	var licenseRequests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/licenserequest") {
			licenseRequests++
			_, _ = w.Write([]byte(`{"content_license":{"status_code":"Denied","license_denial_reasons":[{"rejectionReason":"CustomerThrottled"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"item":{"asin":"B000000001","title":"Synthetic title","content_type":"audiobook"}}`))
	}))
	defer upstream.Close()
	manager.ClientForAccount(account.ID).SetAPIEndpoint(upstream.URL)
	server := &Server{db: db, accounts: manager}
	for attempt, query := range []string{"", "?check_license=true", "?check_license=true", ""} {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/diagnostics/asin/B000000001"+query, nil)
		c.Params = gin.Params{{Key: "asin", Value: "B000000001"}}
		server.handleDiagnosticsASINProbe(c)
		if response.Code != http.StatusOK {
			t.Fatalf("diagnostics status %d: %s", response.Code, response.Body.String())
		}
		var body struct {
			Accounts []diagnosticsAccountProbe `json:"accounts"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Accounts) != 1 || !body.Accounts[0].Found || body.Accounts[0].EntitlementChecked != (query != "") {
			t.Fatalf("diagnostics reported wrong check state: %s", response.Body.String())
		}
		wantRequests := 1
		if attempt == 0 {
			wantRequests = 0
		}
		if licenseRequests != wantRequests {
			t.Fatalf("diagnostics sent %d licenses after lookup %d, want %d", licenseRequests, attempt+1, wantRequests)
		}
	}
}

func TestPipelineTemplateShowsDeferredRetryTime(t *testing.T) {
	server := &Server{router: gin.New()}
	server.setupTemplates()
	response := httptest.NewRecorder()
	err := server.router.HTMLRender.Instance("downloads.html", gin.H{
		"Page":           "downloads",
		"Active":         []database.DownloadQueue{},
		"RecentErrors":   []database.DownloadQueue{},
		"Complete":       []database.DownloadQueue{},
		"DownloadTitles": map[string]string{},
		"ActiveTab":      "waiting",
		"Pending":        []database.DownloadQueue{{ASIN: "B000000001", Status: database.DownloadStatusDeferred, Error: "Retry after 2030-01-01T00:00:00Z"}},
	}).Render(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Body.String(), "Retry after 2030-01-01T00:00:00Z") {
		t.Fatal("waiting table omitted the deferred retry time")
	}
}
