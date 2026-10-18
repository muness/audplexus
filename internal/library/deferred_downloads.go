package library

import (
	"context"
	"errors"
	"time"

	"github.com/mstrhakr/audplexus/internal/database"
)

func (dm *DownloadManager) deferForLicenseCooldown(ctx context.Context, item *database.DownloadQueue, title string, err error) bool {
	var cooldown *LicenseCooldownError
	if !errors.As(err, &cooldown) {
		return false
	}
	item.Status = database.DownloadStatusDeferred
	item.Error = cooldown.Error()
	item.StartedAt = nil
	item.CompletedAt = nil
	if updateErr := dm.db.UpdateDownload(ctx, item); updateErr != nil {
		dlLog.Error().Err(updateErr).Msg("could not defer download during license pause")
		return true
	}
	dlLog.Warn().Err(err).Str("asin", item.ASIN).Time("retry_after", cooldown.Until).Msg("download deferred during Audplexus license pause")
	_ = dm.db.UpdateBookStatus(ctx, item.BookID, database.BookStatusQueued)
	dm.emit(DownloadEvent{ASIN: item.ASIN, BookID: item.BookID, Title: title, Type: "deferred", Stage: "queued", Error: item.Error})
	return true
}

func (dm *DownloadManager) resumeDownloadsAfterCooldown(ctx context.Context) {
	dm.resumeMu.Lock()
	defer dm.resumeMu.Unlock()
	if dm.accounts == nil {
		return
	}
	status := database.DownloadStatusDeferred
	items, err := dm.db.ListDownloads(ctx, &status)
	if err != nil {
		return
	}
	for _, item := range items {
		until, err := dm.accounts.licenseCooldownForBook(ctx, item.ASIN)
		if err != nil || time.Now().Before(until) {
			continue
		}
		if err := dm.db.ResumeDeferredDownload(ctx, item.ID); err != nil {
			dlLog.Warn().Err(err).Msg("could not resume deferred download")
		}
	}
}

func (dm *DownloadManager) handleDownloadFailure(ctx context.Context, item *database.DownloadQueue, title string, writer *fileDownloadWriter, err error) {
	writer.Cleanup()
	if dm.deferForLicenseCooldown(ctx, item, title, err) {
		return
	}
	dlLog.WithField("asin", item.ASIN).Error().Err(err).Msg("download failed")
	dm.cleanupDownloadFiles(item.ASIN)
	dm.failItem(ctx, item, title, err)
}
