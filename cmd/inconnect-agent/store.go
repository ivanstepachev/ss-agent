package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

const (
	slotStatusFree      = "free"
	slotStatusUsed      = "used"
	slotStatusReserved  = "reserved"
	serverPSKPrefix     = "server_psk_shard_"
	legacyServerPSKKey  = "server_psk"
)

var (
	errNoFreePorts  = errors.New("no_free_ports")
	errSlotNotFound = errors.New("slot_not_found")
	errSlotNotInUse = errors.New("slot_not_used")
	errSlotReserved = errors.New("slot_reserved")
	errSlotFree     = errors.New("slot_free")
	schemaStatement = `
CREATE TABLE IF NOT EXISTS slots (
    port            INTEGER PRIMARY KEY,
    password        TEXT NOT NULL,
    status          TEXT NOT NULL,
    user_id         TEXT,
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL,
    shard_id        INTEGER NOT NULL DEFAULT 1
);`
	metadataSchema = `
CREATE TABLE IF NOT EXISTS metadata (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_at  DATETIME NOT NULL
);`
)

// Slot represents a single allocation entry.
type Slot struct {
	ID       int
	ShardID  int
	Password string
	Status   string
	UserID   sql.NullString
}

type SlotStore struct {
	db              *sql.DB
	serverPasswords map[int]string
}

func NewSlotStore(db *sql.DB) *SlotStore {
	return &SlotStore{
		db:              db,
		serverPasswords: make(map[int]string),
	}
}

func (s *SlotStore) Init(ctx context.Context, cfg Config, shards []ShardDefinition) error {
	if _, err := s.db.ExecContext(ctx, schemaStatement); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, metadataSchema); err != nil {
		return fmt.Errorf("create metadata schema: %w", err)
	}
	if err := s.ensureShardColumn(ctx); err != nil {
		return err
	}
	if err := s.ensureSlots(ctx, shards); err != nil {
		return err
	}
	return s.ensureServerPasswords(ctx, shards)
}

func (s *SlotStore) ensureShardColumn(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(slots)`)
	if err != nil {
		return fmt.Errorf("table info: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan table info: %w", err)
		}
		if name == "shard_id" {
			return nil
		}
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE slots ADD COLUMN shard_id INTEGER NOT NULL DEFAULT 1`); err != nil {
		return fmt.Errorf("add shard_id column: %w", err)
	}
	return nil
}

func (s *SlotStore) ensureSlots(ctx context.Context, shards []ShardDefinition) error {
	total := 0
	for _, sh := range shards {
		total += sh.SlotCount
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for slotID := 1; slotID <= total; slotID++ {
		pwd, err := generatePassword()
		if err != nil {
			return fmt.Errorf("generate password: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO slots (port, password, status, created_at, updated_at, shard_id)
             VALUES (?, ?, ?, ?, ?, 1)
             ON CONFLICT(port) DO NOTHING`,
			slotID,
			pwd,
			slotStatusFree,
			now,
			now,
		); err != nil {
			return fmt.Errorf("seed slot %d: %w", slotID, err)
		}
	}

	offset := 0
	for _, sh := range shards {
		start := offset + 1
		end := offset + sh.SlotCount
		if _, err := tx.ExecContext(ctx, `
UPDATE slots
SET shard_id = ?
WHERE port BETWEEN ? AND ?`,
			sh.ID, start, end); err != nil {
			return fmt.Errorf("assign shard %d: %w", sh.ID, err)
		}
		offset = end
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seed tx: %w", err)
	}
	return nil
}

func (s *SlotStore) ensureServerPasswords(ctx context.Context, shards []ShardDefinition) error {
	for _, sh := range shards {
		key := fmt.Sprintf("%s%d", serverPSKPrefix, sh.ID)
		var fallback string
		if sh.ID == 1 {
			fallback = legacyServerPSKKey
		}
		psk, err := s.ensureServerPassword(ctx, key, fallback)
		if err != nil {
			return err
		}
		s.serverPasswords[sh.ID] = psk
	}
	return nil
}

func (s *SlotStore) AllocateSlot(ctx context.Context, userID string) (*Slot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("begin allocate tx: %w", err)
	}
	defer tx.Rollback()

	slot := &Slot{}
	row := tx.QueryRowContext(ctx, `
SELECT port, password, shard_id FROM slots
WHERE status = ?
ORDER BY port
LIMIT 1`, slotStatusFree)
	if err := row.Scan(&slot.ID, &slot.Password, &slot.ShardID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNoFreePorts
		}
		return nil, fmt.Errorf("select free slot: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var userValue interface{}
	if userID != "" {
		userValue = userID
	}
	res, err := tx.ExecContext(ctx, `
UPDATE slots
SET status = ?, user_id = ?, updated_at = ?
WHERE port = ? AND status = ?`,
		slotStatusUsed,
		userValue,
		now,
		slot.ID,
		slotStatusFree,
	)
	if err != nil {
		return nil, fmt.Errorf("update slot %d: %w", slot.ID, err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return nil, errors.New("slot allocation conflict")
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit allocate tx: %w", err)
	}
	slot.Status = slotStatusUsed
	return slot, nil
}

func (s *SlotStore) ReserveSlot(ctx context.Context, slotID int) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
UPDATE slots
SET status = ?, user_id = NULL, updated_at = ?
WHERE port = ? AND status = ?`,
		slotStatusReserved,
		now,
		slotID,
		slotStatusUsed,
	)
	if err != nil {
		return fmt.Errorf("reserve slot %d: %w", slotID, err)
	}
	rows, _ := res.RowsAffected()
	if rows == 1 {
		return nil
	}

	status, err := s.slotStatus(ctx, slotID)
	if err != nil {
		return err
	}
	switch status {
	case slotStatusFree:
		return errSlotFree
	case slotStatusReserved:
		return errSlotReserved
	case "":
		return errSlotNotFound
	default:
		return errSlotNotInUse
	}
}

func (s *SlotStore) slotStatus(ctx context.Context, slotID int) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM slots WHERE port = ?`, slotID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errSlotNotFound
	}
	if err != nil {
		return "", fmt.Errorf("fetch slot status: %w", err)
	}
	return status, nil
}

func (s *SlotStore) RotateReserved(ctx context.Context, shardID int) (int, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return 0, fmt.Errorf("begin rotate tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT port FROM slots WHERE status = ? AND shard_id = ? ORDER BY port`, slotStatusReserved, shardID)
	if err != nil {
		return 0, fmt.Errorf("select reserved slots: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var slotID int
		if err := rows.Scan(&slotID); err != nil {
			return 0, fmt.Errorf("scan reserved slot: %w", err)
		}
		pwd, err := generatePassword()
		if err != nil {
			return 0, fmt.Errorf("generate password for %d: %w", slotID, err)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `
UPDATE slots
SET password = ?, status = ?, updated_at = ?
WHERE port = ?`,
			pwd,
			slotStatusFree,
			now,
			slotID,
		); err != nil {
			return 0, fmt.Errorf("update reserved slot %d: %w", slotID, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate reserved slots: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit rotate tx: %w", err)
	}
	return count, nil
}

func (s *SlotStore) SlotsByShard(ctx context.Context, shardID int, expected int) ([]Slot, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT port, password, status, user_id, shard_id
FROM slots
WHERE shard_id = ?
ORDER BY port
LIMIT ?`, shardID, expected)
	if err != nil {
		return nil, fmt.Errorf("select shard slots: %w", err)
	}
	defer rows.Close()

	var slots []Slot
	for rows.Next() {
		var slot Slot
		if err := rows.Scan(&slot.ID, &slot.Password, &slot.Status, &slot.UserID, &slot.ShardID); err != nil {
			return nil, fmt.Errorf("scan slot: %w", err)
		}
		slots = append(slots, slot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate slots: %w", err)
	}
	if len(slots) != expected {
		return nil, fmt.Errorf("expected %d slots for shard %d, found %d", expected, shardID, len(slots))
	}
	return slots, nil
}

func generatePassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

func (s *SlotStore) ensureServerPassword(ctx context.Context, key, legacy string) (string, error) {
	load := func(k string) (string, error) {
		var value string
		err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key = ?`, k).Scan(&value)
		if err != nil {
			return "", err
		}
		return value, nil
	}

	value, err := load(key)
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("load server password for %s: %w", key, err)
	}

	if legacy != "" {
		if legacyValue, legacyErr := load(legacy); legacyErr == nil {
			if err := s.upsertServerPassword(ctx, key, legacyValue); err != nil {
				return "", err
			}
			return legacyValue, nil
		} else if legacyErr != nil && !errors.Is(legacyErr, sql.ErrNoRows) {
			return "", fmt.Errorf("load legacy server password %s: %w", legacy, legacyErr)
		}
	}

	psk, err := generatePassword()
	if err != nil {
		return "", fmt.Errorf("generate server password: %w", err)
	}
	if err := s.upsertServerPassword(ctx, key, psk); err != nil {
		return "", err
	}
	return psk, nil
}

func (s *SlotStore) upsertServerPassword(ctx context.Context, key, value string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO metadata (key, value, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, now); err != nil {
		return fmt.Errorf("store server password for %s: %w", key, err)
	}
	return nil
}

func (s *SlotStore) ServerPassword(shardID int) string {
	return s.serverPasswords[shardID]
}
