package database

import (
	"context"
	"testing"
)

func TestDeferredDownloadCanBeCancelledAndCannotBeResumedAfterwards(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	book := &Book{ASIN: "B000000001", Title: "Synthetic title", Status: BookStatusQueued}
	if err := db.UpsertBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	item := &DownloadQueue{BookID: book.ID, ASIN: book.ASIN, Status: DownloadStatusDeferred}
	if err := db.EnqueueDownload(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := db.CancelDownload(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ResumeDeferredDownload(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	status := DownloadStatusCancelled
	rows, err := db.ListDownloads(ctx, &status)
	if err != nil || len(rows) != 1 {
		t.Fatalf("cancelled rows = %v, error = %v", rows, err)
	}
	next, err := db.GetNextPendingDownload(ctx)
	if err != nil || next != nil {
		t.Fatalf("cancelled item became runnable: %v, %v", next, err)
	}
}

func TestRetryAllDoesNotDuplicateDeferredDownload(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	book := &Book{ASIN: "B000000001", Title: "Synthetic title", Status: BookStatusQueued}
	if err := db.UpsertBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	for _, status := range []DownloadStatus{DownloadStatusFailed, DownloadStatusDeferred} {
		if err := db.EnqueueDownload(ctx, &DownloadQueue{BookID: book.ID, ASIN: book.ASIN, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	count, err := db.RetryAllDownloads(ctx)
	if err != nil || count != 0 {
		t.Fatalf("retried %d titles, want zero: %v", count, err)
	}
}
