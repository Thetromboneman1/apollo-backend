package repository_test

import (
	"testing"

	"github.com/christianselig/apollo-backend/internal/domain"
	"github.com/christianselig/apollo-backend/internal/repository"
	"github.com/christianselig/apollo-backend/internal/testhelper"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func NewTestPostgresWatcher(t *testing.T) domain.WatcherRepository {
	t.Helper()

	ctx := t.Context()
	conn := testhelper.NewTestPgxConn(t)

	tx, err := conn.Begin(ctx)
	require.NoError(t, err)

	repo := repository.NewPostgresWatcher(tx)

	t.Cleanup(func() {
		_ = tx.Rollback(ctx)
	})

	return repo
}

func TestPostgresWatcher_GetByID(t *testing.T) {
	t.Parallel()
}

func TestPostgresWatcher_AccountScopedMutationsRequireCurrentAssociation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	conn := testhelper.NewTestPgxConn(t)
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	const (
		apns = "account-scoped-watcher-device"
		rid  = "t2_scoped"
	)

	var deviceID int64
	require.NoError(t, tx.QueryRow(ctx,
		`INSERT INTO devices (apns_token, sandbox) VALUES ($1, false) RETURNING id`, apns,
	).Scan(&deviceID))

	var accountID int64
	require.NoError(t, tx.QueryRow(ctx,
		`INSERT INTO accounts (reddit_account_id, username) VALUES ($1, $2) RETURNING id`, rid, "scoped-watcher-user",
	).Scan(&accountID))

	_, err = tx.Exec(ctx,
		`INSERT INTO devices_accounts (device_id, account_id) VALUES ($1, $2)`, deviceID, accountID,
	)
	require.NoError(t, err)

	repo := repository.NewPostgresWatcher(tx)
	watcher := &domain.Watcher{
		Label:     "before",
		DeviceID:  deviceID,
		AccountID: accountID,
		Type:      domain.UserWatcher,
		WatcheeID: 1,
	}
	require.NoError(t, repo.Create(ctx, watcher))

	watcher.Label = "current-association"
	require.NoError(t, repo.UpdateForDeviceAndAccount(ctx, watcher, apns, rid))

	var label string
	require.NoError(t, tx.QueryRow(ctx, `SELECT label FROM watchers WHERE id = $1`, watcher.ID).Scan(&label))
	require.Equal(t, "current-association", label)

	_, err = tx.Exec(ctx,
		`DELETE FROM devices_accounts WHERE device_id = $1 AND account_id = $2`, deviceID, accountID,
	)
	require.NoError(t, err)

	watcher.Label = "stale-association"
	require.ErrorIs(t, repo.UpdateForDeviceAndAccount(ctx, watcher, apns, rid), domain.ErrNotFound)
	require.ErrorIs(t, repo.DeleteForDeviceAndAccount(ctx, watcher.ID, apns, rid), domain.ErrNotFound)

	require.NoError(t, tx.QueryRow(ctx, `SELECT label FROM watchers WHERE id = $1`, watcher.ID).Scan(&label))
	require.Equal(t, "current-association", label)

	_, err = tx.Exec(ctx,
		`INSERT INTO devices_accounts (device_id, account_id) VALUES ($1, $2)`, deviceID, accountID,
	)
	require.NoError(t, err)
	require.NoError(t, repo.DeleteForDeviceAndAccount(ctx, watcher.ID, apns, rid))

	err = tx.QueryRow(ctx, `SELECT label FROM watchers WHERE id = $1`, watcher.ID).Scan(&label)
	require.ErrorIs(t, err, pgx.ErrNoRows)
}
