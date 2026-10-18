package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mstrhakr/audplexus/internal/database"
	"github.com/mstrhakr/audplexus/internal/library"
)

func serverWithLicensePause(t *testing.T) (*Server, time.Time) {
	t.Helper()
	db := newWebTestDB(t)
	ctx := context.Background()
	account := &database.AudibleAccount{ID: "synthetic-account", DisplayName: "Personal Audible", Marketplace: "us", Enabled: true, Credentials: []byte(`{"adp_token":"synthetic-token"}`)}
	if err := db.CreateAudibleAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	value, _ := json.Marshal(map[string]any{"until": until, "delay": int64(time.Hour), "reason": "http_429"})
	key := fmt.Sprintf("audible_license_http429_%x", sha256.Sum256([]byte("us:"+account.ID)))
	if err := db.SetSetting(ctx, key, string(value)); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db, accounts: library.NewAccountManager(ctx, db, nil), router: gin.New()}
	server.setupTemplates()
	return server, until
}

func TestAccountIndicatorShowsThrottleWithoutWaitingJobs(t *testing.T) {
	server, until := serverWithLicensePause(t)
	response := httptest.NewRecorder()
	server.router.GET("/api/audible/status", server.handleAudibleAccountStatus)
	server.router.ServeHTTP(response, httptest.NewRequest("GET", "/api/audible/status", nil))
	html := response.Body.String()
	for _, text := range []string{"Personal Audible: license requests paused", "Next Audplexus attempt:", "Audible downloads paused", until.Format(time.RFC3339)} {
		if !strings.Contains(html, text) {
			t.Fatalf("account indicator omitted %q: %s", text, html)
		}
	}
	if !strings.Contains(html, "HTTP 429") || strings.Contains(html, "CustomerThrottled") {
		t.Fatal("HTTP 429 account warning used a registration refusal explanation")
	}
	if strings.Contains(html, "All systems operational") {
		t.Fatal("throttled account was reported healthy")
	}
	view := server.accountsForView(context.Background())
	if len(view) != 1 || !view[0].LicensePausedUntil.Equal(until) {
		t.Fatalf("Settings omitted account cooldown: %+v", view)
	}
	key := fmt.Sprintf("audible_license_http429_%x", sha256.Sum256([]byte("us:synthetic-account")))
	if err := server.db.SetSetting(context.Background(), key, `{"until":"2000-01-01T00:00:00Z","delay":1}`); err != nil {
		t.Fatal(err)
	}
	remaining, err := server.accounts.LicenseCooldowns(context.Background())
	if err != nil || len(remaining) != 0 {
		t.Fatalf("expired cooldown remains visible: %v, %v", remaining, err)
	}
}

func TestBookAndDashboardShowDeferredIssueAndIgnoreStaleFailures(t *testing.T) {
	server, until := serverWithLicensePause(t)
	ctx := context.Background()
	queued := &database.Book{ASIN: "B000000001", Title: "Synthetic queued book", Status: database.BookStatusQueued}
	complete := &database.Book{ASIN: "B000000002", Title: "Synthetic completed book", Status: database.BookStatusComplete}
	for _, book := range []*database.Book{queued, complete} {
		if err := server.db.UpsertBook(ctx, book); err != nil {
			t.Fatal(err)
		}
	}
	message := "Audible returned CustomerThrottled for this device registration. Audplexus paused its license requests; reconnecting may help. Retry after " + until.Format(time.RFC3339)
	for _, item := range []database.DownloadQueue{
		{BookID: queued.ID, ASIN: queued.ASIN, Status: database.DownloadStatusDeferred, Error: message},
		{BookID: complete.ID, ASIN: complete.ASIN, Status: database.DownloadStatusFailed, Error: "old failure"},
	} {
		if err := server.db.EnqueueDownload(ctx, &item); err != nil {
			t.Fatal(err)
		}
		if err := server.db.UpdateDownload(ctx, &item); err != nil {
			t.Fatal(err)
		}
	}
	issues := server.bookDownloadIssues(ctx, []database.Book{*queued, *complete})
	if _, ok := issues[complete.ID]; ok {
		t.Fatal("completed book shows a stale error")
	}
	if !issues[queued.ID].Deferred || issues[queued.ID].AccountName != "Personal Audible" {
		t.Fatalf("book lost account issue: %+v", issues)
	}
	manager := library.NewDownloadManager(server.db, nil, nil, nil, nil, t.TempDir(), t.TempDir(), "m4b", false, 1, 1, 1, nil, nil)
	manager.SetAccountManager(server.accounts)
	snapshot := manager.PipelineSnapshot(ctx)
	if len(snapshot.Items) != 1 || !snapshot.Items[0].Deferred || snapshot.Items[0].AccountName != "Personal Audible" || !strings.Contains(snapshot.Items[0].Error, "Retry after") {
		t.Fatalf("live snapshot lost throttle details: %+v", snapshot.Items)
	}
	for _, target := range []struct {
		name string
		data gin.H
	}{
		{"library_row.html", gin.H{"Book": queued, "BookIssue": issues[queued.ID], "BookAction": libraryBookAction{}}},
		{"book_detail_panel.html", gin.H{"BookFileCount": 0, "Book": queued, "BookIssue": issues[queued.ID], "BookAction": libraryBookAction{}}},
		{"dashboard_downloads.html", server.getDashboardDownloadsData(ctx)},
	} {
		t.Run(target.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			if err := server.router.HTMLRender.Instance(target.name, target.data).Render(response); err != nil {
				t.Fatal(err)
			}
			html := response.Body.String()
			for _, text := range []string{"license requests paused", "CustomerThrottled", "Automatic retry", until.Format(time.RFC3339), "Personal Audible"} {
				if !strings.Contains(html, text) {
					t.Fatalf("%s omitted %q", target.name, text)
				}
			}
		})
	}
}

func TestFailedBookShowsSavedErrorWithoutRawHTML(t *testing.T) {
	issue := issueForDownload(database.DownloadQueue{Status: database.DownloadStatusFailed, Error: "download failed: <b>disk full</b>"})
	if issue.Label != "Download failed" || !strings.Contains(issue.Message, "disk full") || strings.Contains(issue.Message, "<b>") {
		t.Fatalf("bad failure display: %+v", issue)
	}
}

func TestStatusAPIReportsAccountThrottleAsAnIssueWithoutFailedBooks(t *testing.T) {
	server, _ := serverWithLicensePause(t)
	server.sync = library.NewSyncService(server.db, nil, t.TempDir())
	server.router.GET("/api/sync/status.json", server.handleSyncStatusJSON)
	response := httptest.NewRecorder()
	server.router.ServeHTTP(response, httptest.NewRequest("GET", "/api/sync/status.json", nil))
	var status struct {
		HasIssues bool                             `json:"has_issues"`
		Failed    int                              `json:"books_failed"`
		Cooldowns []library.AccountLicenseCooldown `json:"audible_account_cooldowns"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.HasIssues || status.Failed != 0 || len(status.Cooldowns) != 1 {
		t.Fatalf("status API hid account throttle: %s", response.Body.String())
	}
}
