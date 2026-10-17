package database

import "context"

// DownloadStatusDeferred keeps a throttled download out of the worker claim query.
const DownloadStatusDeferred DownloadStatus = "deferred"

// WaitingDownloads includes queued downloads and downloads waiting for Audible.
func WaitingDownloads(ctx context.Context, db Database) ([]DownloadQueue, error) {
	pending := DownloadStatusPending
	items, err := db.ListDownloads(ctx, &pending)
	if err != nil {
		return nil, err
	}
	deferred := DownloadStatusDeferred
	waiting, err := db.ListDownloads(ctx, &deferred)
	if err != nil {
		return nil, err
	}
	return append(items, waiting...), nil
}
