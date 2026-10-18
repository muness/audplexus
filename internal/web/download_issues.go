package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mstrhakr/audplexus/internal/database"
	"github.com/mstrhakr/audplexus/internal/errs"
	"github.com/mstrhakr/audplexus/internal/library"
)

type downloadIssue struct {
	Label       string
	Message     string
	Deferred    bool
	RetryAt     string
	RetryLabel  string
	AccountName string
}

func issueForDownload(item database.DownloadQueue) downloadIssue {
	issue := downloadIssue{Label: "Download failed", Message: errs.CleanDownloadError(item.Error)}
	if item.Status != database.DownloadStatusDeferred {
		return issue
	}
	issue.Label = "Waiting for Audible — account throttled"
	issue.Deferred = true
	issue.Message = "Audible is throttling download licenses (CustomerThrottled)."
	const prefix = "Retry after "
	if _, value, ok := strings.Cut(item.Error, prefix); ok {
		if until, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
			issue.RetryAt = until.Format(time.RFC3339)
			issue.RetryLabel = until.UTC().Format("Jan 2, 15:04 MST")
		}
	}
	return issue
}

func (s *Server) bookDownloadIssues(ctx context.Context, books []database.Book) map[int64]downloadIssue {
	statuses := make(map[int64]database.BookStatus, len(books))
	for _, book := range books {
		statuses[book.ID] = book.Status
	}
	latest := map[int64]database.DownloadQueue{}
	for _, status := range []database.DownloadStatus{database.DownloadStatusDeferred, database.DownloadStatusFailed} {
		rows, err := s.db.ListDownloads(ctx, &status)
		if err != nil {
			webLog.Warn().Err(err).Msg("could not load download issues")
			continue
		}
		for _, row := range rows {
			bookStatus := statuses[row.BookID]
			if (status == database.DownloadStatusDeferred && bookStatus != database.BookStatusQueued) || (status == database.DownloadStatusFailed && bookStatus != database.BookStatusFailed) {
				continue
			}
			if previous, ok := latest[row.BookID]; !ok || row.ID > previous.ID {
				latest[row.BookID] = row
			}
		}
	}
	issues := make(map[int64]downloadIssue, len(latest))
	for id, row := range latest {
		issues[id] = s.downloadIssue(ctx, row)
	}
	return issues
}

func (s *Server) audibleLicenseCooldowns(ctx context.Context) ([]library.AccountLicenseCooldown, error) {
	if s.accounts == nil {
		return nil, nil
	}
	return s.accounts.LicenseCooldowns(ctx)
}

func (s *Server) handleAudibleAccountStatus(c *gin.Context) {
	cooldowns, err := s.audibleLicenseCooldowns(c.Request.Context())
	c.HTML(http.StatusOK, "audible_account_status.html", gin.H{"AccountCooldowns": cooldowns, "StatusUnavailable": err != nil, "Sidebar": s.computeSidebar(c.Request.Context()), "RefreshSidebar": true})
}

func (s *Server) downloadIssue(ctx context.Context, item database.DownloadQueue) downloadIssue {
	issue := issueForDownload(item)
	if issue.Deferred && s.accounts != nil {
		owner, _ := s.db.GetBookAccount(ctx, item.ASIN)
		if owner == "" {
			owner = s.accounts.PrimaryAccountID()
		}
		for _, account := range s.accounts.EnabledAccounts() {
			if account.ID == owner {
				issue.AccountName = account.Name
				break
			}
		}
	}
	return issue
}
