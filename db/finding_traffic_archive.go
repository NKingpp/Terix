package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func ArchiveEvidenceSnapshots(snapshot *TaskArchiveSnapshot) ([]TrafficEvidenceSnapshot, error) {
	var out []TrafficEvidenceSnapshot
	if rawRowCount(snapshot.Tables["traffic_evidence_snapshots"]) == 0 {
		return out, nil
	}
	if snapshot.FormatVersion < 3 {
		return nil, ErrTaskArchiveFormatMismatch
	}
	err := json.Unmarshal(snapshot.Tables["traffic_evidence_snapshots"], &out)
	return out, err
}

func restoreFindingTrafficTx(tx *sql.Tx, snapshot *TaskArchiveSnapshot) error {
	snapshots, err := ArchiveEvidenceSnapshots(snapshot)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, v := range snapshots {
		if allowed[v.ID] {
			return errors.New("duplicate archived evidence snapshot")
		}
		allowed[v.ID] = true
		if err = InsertEvidenceSnapshotTx(tx, v); err != nil {
			return err
		}
	}
	rows, err := decodeArchiveRows(snapshot.Tables["finding_traffic_bindings"])
	if err != nil {
		return err
	}
	if len(rows) > 0 && snapshot.FormatVersion < 3 {
		return ErrTaskArchiveFormatMismatch
	}
	for _, row := range rows {
		fid, ok := jsonInt64(row["finding_id"])
		if !ok {
			return errors.New("invalid archived evidence finding id")
		}
		sid, _ := row["snapshot_id"].(string)
		role, _ := row["role"].(string)
		if !allowed[sid] || !ValidTrafficRole(role) {
			return errors.New("invalid archived evidence binding")
		}
		var owned bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM findings WHERE id=$1 AND task_id=$2)`, fid, snapshot.TaskID).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return fmt.Errorf("evidence references finding outside archived task: %d", fid)
		}
	}
	if len(rows) > 0 {
		// PG 用 json_populate_recordset 把 JSON 数组直接灌进表；SQLite 没有这个函数。
		// 逐行插入：绑定集合已经校验过（allowed + 归属任务），这里只需保证原子性
		// —— 调用方已在事务里，所以逐行 Exec 与批量插入等价，只是多几次语句准备。
		for _, row := range rows {
			fid, _ := jsonInt64(row["finding_id"])
			sid, _ := row["snapshot_id"].(string)
			role, _ := row["role"].(string)
			note, _ := row["note"].(string)
			position, _ := jsonInt64(row["position"])
			createdAt, _ := row["created_at"].(string)
			if createdAt == "" {
				createdAt = NowISO()
			}
			if _, err = tx.Exec(`INSERT INTO finding_traffic_bindings(finding_id,snapshot_id,role,note,position,created_at)
VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT (finding_id, snapshot_id) DO NOTHING`,
				fid, sid, role, note, position, createdAt); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`UPDATE traffic_evidence_snapshots SET unreferenced_at=NULL WHERE EXISTS(SELECT 1 FROM finding_traffic_bindings b WHERE b.snapshot_id=traffic_evidence_snapshots.id)`)
	}
	return err
}

func normalizeArchivedFindingVersions(raw json.RawMessage) (json.RawMessage, error) {
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		for _, key := range []string{"evidence_version", "report_evidence_version"} {
			if row[key] == nil {
				row[key] = 0
			}
		}
	}
	return json.Marshal(rows)
}
