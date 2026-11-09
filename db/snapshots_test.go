package db

import (
	"errors"
	"testing"
)

// snapshotsSetup opens a DB and returns its SnapshotStore. Skips if no PG.
func snapshotsSetup(t *testing.T) (*DB, *SnapshotStore) {
	t.Helper()
	d := testOpen(t)
	return d, d.Snapshots()
}

// TestSnapshotRoundtripAndDedup 验证写入 → 读回 → 同秒重复写入命中去重。
func TestSnapshotRoundtripAndDedup(t *testing.T) {
	d, store := snapshotsSetup(t)

	source := uniqueName(t, "src")
	defer d.Exec(`DELETE FROM snapshots WHERE source = $1`, source) //nolint:errcheck

	in := SnapshotInput{
		Source:      source,
		Endpoint:    "/quote",
		ParamsHash:  "hash-abc",
		CollectedAt: "2026-03-01T09:30:15Z",
		BodyRef:     "s3://bucket/key.json",
		Checksum:    "sha256:deadbeef",
	}
	snap, created, err := store.InsertSnapshot(in)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !created {
		t.Error("expected created = true for first insert")
	}
	if snap.ID == 0 || snap.Source != source || snap.Checksum != in.Checksum {
		t.Errorf("unexpected snapshot: %+v", snap)
	}
	if snap.BodyRef == nil || *snap.BodyRef != in.BodyRef {
		t.Errorf("body_ref = %v", snap.BodyRef)
	}

	// 同一秒(毫秒不同)重复写入必须命中去重,返回同一条记录且 created=false。
	snap2, created2, err := store.InsertSnapshot(SnapshotInput{
		Source:      source,
		Endpoint:    "/quote",
		ParamsHash:  "hash-abc",
		CollectedAt: "2026-03-01T09:30:15.750Z",
		BodyRef:     "s3://bucket/other.json",
		Checksum:    "sha256:other",
	})
	if err != nil {
		t.Fatalf("insert dup: %v", err)
	}
	if created2 {
		t.Error("expected created = false for same-second duplicate")
	}
	if snap2.ID != snap.ID {
		t.Errorf("dedup failed: id %d != %d", snap2.ID, snap.ID)
	}
	// 命中去重时应返回**已存在**的记录,不能被第二次的入参覆盖。
	if snap2.Checksum != in.Checksum {
		t.Errorf("dedup overwrote checksum: %q", snap2.Checksum)
	}

	// 下一秒是不同记录。
	snap3, created3, err := store.InsertSnapshot(SnapshotInput{
		Source:      source,
		Endpoint:    "/quote",
		ParamsHash:  "hash-abc",
		CollectedAt: "2026-03-01T09:30:16Z",
		Checksum:    "sha256:nextsecond",
	})
	if err != nil {
		t.Fatalf("insert next second: %v", err)
	}
	if !created3 {
		t.Error("expected created = true for next-second insert")
	}
	if snap3.ID == snap.ID {
		t.Error("next-second insert must not dedup into previous record")
	}
	// 未传 body_ref 时必须为 NULL。
	if snap3.BodyRef != nil {
		t.Errorf("expected nil body_ref, got %v", *snap3.BodyRef)
	}

	got, err := store.GetSnapshotByDedupKey(source, "/quote", "hash-abc", "2026-03-01T09:30:15.100Z")
	if err != nil {
		t.Fatalf("get by dedup key: %v", err)
	}
	if got.ID != snap.ID {
		t.Errorf("dedup key lookup: got id %d, want %d", got.ID, snap.ID)
	}

	list, err := store.ListSnapshotsBySource(source, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(list))
	}
	// 最新在前。
	if list[0].ID != snap3.ID {
		t.Errorf("list not ordered by collected_at desc: first id %d", list[0].ID)
	}
}

// TestSnapshotErrors 覆盖必填校验与查不到。
func TestSnapshotErrors(t *testing.T) {
	_, store := snapshotsSetup(t)

	cases := []struct {
		name string
		in   SnapshotInput
	}{
		{"no source", SnapshotInput{Endpoint: "/x", ParamsHash: "h", CollectedAt: "2026-01-01T00:00:00Z", Checksum: "c"}},
		{"no endpoint", SnapshotInput{Source: "s", ParamsHash: "h", CollectedAt: "2026-01-01T00:00:00Z", Checksum: "c"}},
		{"no params_hash", SnapshotInput{Source: "s", Endpoint: "/x", CollectedAt: "2026-01-01T00:00:00Z", Checksum: "c"}},
		{"no checksum", SnapshotInput{Source: "s", Endpoint: "/x", ParamsHash: "h", CollectedAt: "2026-01-01T00:00:00Z"}},
		{"no collected_at", SnapshotInput{Source: "s", Endpoint: "/x", ParamsHash: "h", Checksum: "c"}},
	}
	for _, c := range cases {
		if _, _, err := store.InsertSnapshot(c.in); !errors.Is(err, ErrSnapshotInvalid) {
			t.Errorf("%s: expected ErrSnapshotInvalid, got %v", c.name, err)
		}
	}

	if _, err := store.GetSnapshotByDedupKey("nope-src", "/x", "h", "2026-01-01T00:00:00Z"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("expected ErrSnapshotNotFound, got %v", err)
	}
	if _, err := store.GetSnapshotByDedupKey("", "/x", "h", "2026-01-01T00:00:00Z"); !errors.Is(err, ErrSnapshotInvalid) {
		t.Errorf("expected ErrSnapshotInvalid for empty source, got %v", err)
	}
}
