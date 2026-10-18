package library

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mstrhakr/audplexus/internal/database"
	audible "github.com/mstrhakr/go-audible"
)

func syntheticAudibleCredentials(t *testing.T) *audible.Credentials {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &audible.Credentials{
		DeviceInfo: audible.DeviceInfo{DeviceType: audible.DeviceTypeID, DeviceSerialNumber: "0123456789abcdef0123456789abcdef"},
		ADPToken:   "synthetic-token", AccessToken: "synthetic-access", CustomerID: "synthetic-customer",
		ExpiresAt:        time.Now().Add(time.Hour),
		DevicePrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
	}
}

func TestSyncRefreshesOneThousandTitlesWithoutRequestingLicenses(t *testing.T) {
	ctx := context.Background()
	service := newTestSyncService(t)
	items := make([]map[string]string, 1000)
	for i := range items {
		items[i] = map[string]string{"asin": fmt.Sprintf("B%09d", i), "title": fmt.Sprintf("Synthetic title %d", i), "content_type": "audiobook"}
	}
	body, err := json.Marshal(map[string]any{"items": items, "total_results": len(items)})
	if err != nil {
		t.Fatal(err)
	}
	var licenseRequests int
	client := audible.NewClientWithHTTP(audible.MarketplaceUS, &http.Client{Transport: licenseTestTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/licenserequest") {
			licenseRequests++
			return licenseTestResponse(throttledLicense), nil
		}
		return licenseTestResponse(string(body)), nil
	})})
	client.SetCredentials(syntheticAudibleCredentials(t))
	service.client = client
	for pass := 0; pass < 2; pass++ {
		added, err := service.doAudibleSync(ctx, &database.SyncHistory{})
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if pass == 0 {
			want = len(items)
		}
		if added != want {
			t.Fatalf("sync %d added %d titles, want %d", pass+1, added, want)
		}
		if pass == 0 {
			// Refresh must preserve both a saved file and a historical denial.
			complete, _ := service.db.GetBookByASIN(ctx, "B000000000")
			complete.Status = database.BookStatusComplete
			complete.FilePath = "/library/saved.m4b"
			complete.FileSize = 123
			if err := service.db.UpsertBook(ctx, complete); err != nil {
				t.Fatal(err)
			}
			denied, _ := service.db.GetBookByASIN(ctx, "B000000001")
			denied.Status = database.BookStatusUnavailable
			denied.UnavailableReason = "historical denial"
			if err := service.db.UpsertBook(ctx, denied); err != nil {
				t.Fatal(err)
			}
		}
	}
	if licenseRequests != 0 {
		t.Fatalf("sync requested %d licenses, want zero", licenseRequests)
	}
	saved, _ := service.db.GetBookByASIN(ctx, "B000000000")
	denied, _ := service.db.GetBookByASIN(ctx, "B000000001")
	if saved.Status != database.BookStatusComplete || saved.FilePath != "/library/saved.m4b" || saved.FileSize != 123 {
		t.Fatalf("sync changed saved file metadata: %+v", saved)
	}
	if denied.Status != database.BookStatusUnavailable || denied.UnavailableReason != "historical denial" {
		t.Fatalf("sync changed historical denial: %+v", denied)
	}
}

func TestDownloadDefersOnSDKThrottleAndPreservesExistingFiles(t *testing.T) {
	ctx := context.Background()
	service := newTestSyncService(t)
	book := &database.Book{ASIN: "B000000002", Title: "Synthetic title", Status: database.BookStatusQueued}
	if err := service.db.UpsertBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	item := &database.DownloadQueue{BookID: book.ID, ASIN: book.ASIN}
	if err := service.db.EnqueueDownload(ctx, item); err != nil {
		t.Fatal(err)
	}
	transport := newLicenseTransport(service.db, "account", licenseTestTransport(func(*http.Request) (*http.Response, error) { return licenseTestResponse(throttledLicense), nil }))
	client := audible.NewClientWithHTTP(audible.MarketplaceUS, &http.Client{Transport: transport})
	client.SetCredentials(syntheticAudibleCredentials(t))
	directory := t.TempDir()
	file := filepath.Join(directory, book.ASIN+".aax")
	if err := os.WriteFile(file, []byte("existing partial download"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := NewDownloadManager(service.db, client, nil, nil, nil, t.TempDir(), directory, "m4b", false, 1, 1, 1, nil, nil)
	writer := &fileDownloadWriter{asin: book.ASIN, downloadDir: directory}
	_, err := client.DownloadBook(ctx, book.ASIN, writer)
	if err == nil {
		t.Fatal("synthetic throttle did not reach the SDK")
	}
	manager.handleDownloadFailure(ctx, item, book.Title, writer, err)
	deferred := database.DownloadStatusDeferred
	waiting, err := service.db.ListDownloads(ctx, &deferred)
	if err != nil || len(waiting) != 1 {
		t.Fatalf("deferred downloads: %v, %v", waiting, err)
	}
	if waiting[0].CompletedAt != nil || !strings.Contains(waiting[0].Error, "Retry after") {
		t.Fatalf("missing retry state: %+v", waiting[0])
	}
	queued, _ := service.db.GetBook(ctx, book.ID)
	if queued.Status != database.BookStatusQueued {
		t.Fatalf("book status = %s, want queued", queued.Status)
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "existing partial download" {
		t.Fatalf("throttle changed existing file: %q, %v", data, err)
	}
	next, err := service.db.GetNextPendingDownload(ctx)
	if err != nil || next != nil {
		t.Fatalf("worker reclaimed deferred download: %v, %v", next, err)
	}
}

func TestAccountReloadAndDatabaseReopenKeepLicenseCooldown(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := database.NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	credentials := syntheticAudibleCredentials(t)
	raw, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	account := &database.AudibleAccount{ID: "account-a", DisplayName: "Synthetic account", Marketplace: "us", CustomerID: credentials.CustomerID, Credentials: raw, Enabled: true}
	if err := db.CreateAudibleAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	manager := NewAccountManager(ctx, db, nil)
	guard := manager.accountLicenses[account.ID]
	var requests int
	guard.base = licenseTestTransport(func(*http.Request) (*http.Response, error) {
		requests++
		return licenseTestResponse(throttledLicense), nil
	})
	_, err = manager.ClientForAccount(account.ID).GetDownloadInfo(ctx, "B000000002")
	var cooldown *LicenseCooldownError
	if !errors.As(err, &cooldown) {
		t.Fatalf("SDK lost cooldown error: %v", err)
	}
	credentials.AccessToken = "refreshed-synthetic-access"
	account.Credentials, _ = json.Marshal(credentials)
	if err := db.UpdateAudibleAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = manager.ClientForAccount(account.ID).GetDownloadInfo(ctx, "B000000003")
	if !errors.As(err, &cooldown) || requests != 1 {
		t.Fatalf("token refresh bypassed registration pause: requests=%d, error=%v", requests, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := database.NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	manager = NewAccountManager(ctx, reopened, nil)
	guard = manager.accountLicenses[account.ID]
	guard.base = licenseTestTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("restart bypassed saved cooldown")
		return nil, nil
	})
	_, err = manager.ClientForAccount(account.ID).GetDownloadInfo(ctx, "B000000004")
	if !errors.As(err, &cooldown) {
		t.Fatalf("restart lost cooldown: %v", err)
	}
}

func TestDeferredDownloadLeavesOtherJobsRunnableAndResumesAfterCooldown(t *testing.T) {
	ctx := context.Background()
	service := newTestSyncService(t)
	credentials := syntheticAudibleCredentials(t)
	raw, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	account := &database.AudibleAccount{ID: "account-a", DisplayName: "Synthetic account", Marketplace: "us", CustomerID: credentials.CustomerID, Credentials: raw, Enabled: true}
	if err := service.db.CreateAudibleAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountManager(ctx, service.db, nil)
	guard := accounts.accountLicenses[account.ID]
	now := time.Now()
	guard.now = func() time.Time { return now }
	guard.base = licenseTestTransport(func(*http.Request) (*http.Response, error) { return licenseTestResponse(throttledLicense), nil })
	_, err = accounts.ClientForAccount(account.ID).GetDownloadInfo(ctx, "B000000001")
	var cooldown *LicenseCooldownError
	if !errors.As(err, &cooldown) {
		t.Fatal(err)
	}
	manager := NewDownloadManager(service.db, nil, nil, nil, nil, t.TempDir(), t.TempDir(), "m4b", false, 1, 1, 1, nil, nil)
	manager.accounts = accounts
	for i := 1; i <= 2; i++ {
		book := &database.Book{ASIN: fmt.Sprintf("B%09d", i), Title: "Synthetic title", Status: database.BookStatusQueued}
		if err := service.db.UpsertBook(ctx, book); err != nil {
			t.Fatal(err)
		}
		item := &database.DownloadQueue{BookID: book.ID, ASIN: book.ASIN}
		if err := service.db.EnqueueDownload(ctx, item); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if err := service.db.SetBookAccount(ctx, book.ASIN, account.ID); err != nil {
				t.Fatal(err)
			}
			manager.failItem(ctx, item, book.Title, fmt.Errorf("SDK download failed: %w", cooldown))
		}
	}
	manager.resumeDownloadsAfterCooldown(ctx)
	next, err := service.db.GetNextPendingDownload(ctx)
	if err != nil || next == nil || next.ASIN != "B000000002" {
		t.Fatalf("deferred job blocked another job: %v, %v", next, err)
	}
	now = cooldown.Until.Add(time.Second)
	manager.resumeDownloadsAfterCooldown(ctx)
	next, err = service.db.GetNextPendingDownload(ctx)
	if err != nil || next == nil || next.ASIN != "B000000001" {
		t.Fatalf("expired cooldown did not resume job: %v, %v", next, err)
	}
	manager.resumeDownloadsAfterCooldown(ctx)
	status := database.DownloadStatusActive
	active, err := service.db.ListDownloads(ctx, &status)
	if err != nil || len(active) != 2 {
		t.Fatalf("resume changed claimed jobs: %v, %v", active, err)
	}
}

func TestAndroidReconnectUsesNewRegistrationWithoutClearingHTTP429(t *testing.T) {
	for _, refusal := range []string{"CustomerThrottled", "HTTP429"} {
		t.Run(refusal, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "reconnect.db")
			db, err := database.NewSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.Migrate(); err != nil {
				t.Fatal(err)
			}
			credentials := syntheticAudibleCredentials(t)
			raw, _ := json.Marshal(credentials)
			account := &database.AudibleAccount{ID: "same-account", DisplayName: "Synthetic account", Marketplace: "us", CustomerID: credentials.CustomerID, Credentials: raw, Enabled: true}
			if err := db.CreateAudibleAccount(ctx, account); err != nil {
				t.Fatal(err)
			}
			accounts := NewAccountManager(ctx, db, nil)
			oldRegistration := accounts.accountLicenses[account.ID]
			requests := 0
			oldRegistration.base = licenseTestTransport(func(*http.Request) (*http.Response, error) {
				requests++
				response := licenseTestResponse(throttledLicense)
				if refusal == "HTTP429" {
					response.StatusCode = http.StatusTooManyRequests
					response.Header.Set("Retry-After", "7200")
				}
				return response, nil
			})
			_, err = accounts.ClientForAccount(account.ID).GetDownloadInfo(ctx, "B000000001")
			var pause *LicenseCooldownError
			if !errors.As(err, &pause) {
				t.Fatalf("missing initial pause: %v", err)
			}
			oldUntil := pause.Until

			// Refreshing tokens on the same device must not bypass either pause.
			refreshed := *credentials
			refreshed.AccessToken = "synthetic-refreshed-access"
			refreshed.ADPToken = "synthetic-refreshed-adp"
			refreshedClient := audible.NewClient(audible.MarketplaceUS)
			refreshedClient.SetCredentials(&refreshed)
			if err := accounts.PersistClientCredentials(ctx, account.ID, refreshedClient); err != nil {
				t.Fatal(err)
			}
			if err := accounts.Reload(ctx); err != nil {
				t.Fatal(err)
			}
			_, err = accounts.ClientForAccount(account.ID).GetDownloadInfo(ctx, "B000000002")
			if !errors.As(err, &pause) || requests != 1 {
				t.Fatalf("refresh bypassed pause: requests=%d, error=%v", requests, err)
			}

			book := &database.Book{ASIN: "B000000001", Title: "Synthetic book", Status: database.BookStatusQueued}
			if err := db.UpsertBook(ctx, book); err != nil {
				t.Fatal(err)
			}
			if err := db.SetBookAccount(ctx, book.ASIN, account.ID); err != nil {
				t.Fatal(err)
			}
			item := &database.DownloadQueue{BookID: book.ID, ASIN: book.ASIN}
			if err := db.EnqueueDownload(ctx, item); err != nil {
				t.Fatal(err)
			}
			downloads := NewDownloadManager(db, nil, nil, nil, nil, t.TempDir(), t.TempDir(), "m4b", false, 1, 1, 1, nil, nil)
			downloads.accounts = accounts
			downloads.failItem(ctx, item, book.Title, pause)

			// This is the persistence entry point used after a successful sign-in.
			android := *credentials
			android.ADPToken = "synthetic-new-android-adp"
			android.DeviceInfo = audible.DeviceInfo{DeviceType: "A10KISP2GWF0E4", DeviceSerialNumber: "0123456789abcdef0123"}
			androidClient := audible.NewClient(audible.MarketplaceUS)
			androidClient.SetCredentials(&android)
			if err := accounts.PersistClientCredentials(ctx, account.ID, androidClient); err != nil {
				t.Fatal(err)
			}
			if err := accounts.Reload(ctx); err != nil {
				t.Fatal(err)
			}
			rows, _ := db.ListAudibleAccounts(ctx)
			if len(rows) != 1 || rows[0].ID != account.ID {
				t.Fatal("reconnect created a different account")
			}
			oldPause, err := oldRegistration.cooldown(ctx)
			if err != nil || !oldPause.Until.Equal(oldUntil) {
				t.Fatal("reconnect erased the old pause instead of scoping it")
			}

			// Reopening the database rules out an in-memory-only bypass.
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = database.NewSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			accounts = NewAccountManager(ctx, db, nil)
			downloads.db = db
			downloads.accounts = accounts
			downloads.resumeDownloadsAfterCooldown(ctx)
			waitingStatus := database.DownloadStatusDeferred
			waiting, _ := db.ListDownloads(ctx, &waitingStatus)
			if refusal == "CustomerThrottled" && len(waiting) != 0 {
				t.Fatal("old iPhone pause kept the Android job deferred")
			}
			if refusal == "HTTP429" && len(waiting) != 1 {
				t.Fatal("reconnect bypassed the customer HTTP 429 pause")
			}
			guard := accounts.accountLicenses[account.ID]
			guard.base = licenseTestTransport(func(*http.Request) (*http.Response, error) {
				requests++
				return licenseTestResponse(`{"content_license":{"status_code":"Granted","content_url":"https://cdn.example.test/book.aax","drm_type":"Adrm"}}`), nil
			})
			info, err := accounts.ClientForAccount(account.ID).GetDownloadInfo(ctx, book.ASIN)
			if refusal == "CustomerThrottled" {
				if err != nil || info == nil || requests != 2 {
					t.Fatalf("Android inherited iPhone pause: requests=%d, error=%v", requests, err)
				}
			} else if !errors.As(err, &pause) || requests != 1 || !pause.Until.Equal(oldUntil) {
				t.Fatalf("Android bypassed HTTP 429 Retry-After: requests=%d, error=%v", requests, err)
			}
		})
	}
}
