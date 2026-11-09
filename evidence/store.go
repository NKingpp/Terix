// Package evidence preserves finding evidence independently of disposable traffic.
package evidence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Store 是证据快照存储：blob 文件 + DB 引用。
//
// # 它现在只做「快照存储」，不再做「漏洞记录」
//
// 原本它还承担 Record / Bind 等「写漏洞 + 绑流量证据」的职责，那些随渗透功能删除
// （2026-10-29）。留下的能力都服务于**任务归档**：
//
//	CopySnapshots / WithInstalledSnapshots  归档时把 blob 打进/取出压缩包
//	Collect / RunGC                        清理不再被任何快照行引用的 blob
//	OpenBody / writeBody / hashPath         blob 的读写与路径计算
//
// 所以包名 evidence 仍然准确 —— 它存的就是 evidence 的 blob。
type Store struct {
	DB  *db.DB
	Dir string

	// filesMu 串行化**进程内**的文件操作（复制/安装/GC）。
	//
	// # 为什么需要它（advisory lock 删掉之后）
	//
	// 原先这三条路径都套在 WithEvidenceTx 里，靠 pg_advisory_xact_lock 挡住
	// 「GC 正在删、安装正在写」的竞争。SQLite 单写者下事务里做 fsync 会让全体
	// 写操作排队（§6.4.2 #2/#3/#4），所以事务被拆掉，那把锁也跟着没了。
	//
	// 它替代的只是「文件系统互斥」这一件事，**不是**数据库锁：
	// 持锁期间不做任何 IO 等待以外的数据库写（短事务各开各的）。
	//
	// # 局限
	//
	// 只在同进程内生效。多进程共享同一个 Dir 时（本项目的部署形态是单进程），
	// 仍要靠 staging + rename 的原子性 + 24h 宽限期兜底。
	// flags 见 filesMu 的注释：可取消的文件操作闸门（容量 1 = 互斥）。
	filesSem chan struct{}
}

// lockFiles 取文件操作闸门，返回释放函数。
//
// ctx 若在等待期间被取消则返回 ctx.Err() —— 因为 Mutex 本身不可取消，
// 用一个带缓冲的 token 通道实现「可取消的等待」。
func (s *Store) lockFiles(ctx context.Context, op string) (func(), error) {
	select {
	case s.filesSem <- struct{}{}:
		return func() { <-s.filesSem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("evidence %s: 等待文件闸门时被取消: %w", op, ctx.Err())
	}
}

// New 构造一个证据快照存储。
//
// 不再接 *traffic.Traffic：那个字段只被已删的 ReadEvidence（证据正文读取）用过。
func New(pg *db.DB, dir string) *Store {
	return &Store{
		DB:       pg,
		Dir:      dir,
		filesSem: make(chan struct{}, 1),
	}
}

func hashPath(dir, hash string) (string, error) {
	if len(hash) != 64 {
		return "", errors.New("invalid evidence hash")
	}
	if _, err := hex.DecodeString(hash); err != nil || strings.ToLower(hash) != hash {
		return "", errors.New("invalid evidence hash")
	}
	return filepath.Join(dir, "blobs", hash[:2], hash+".bin"), nil
}

func verifyFile(path, hash string, length int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != length || hex.EncodeToString(h.Sum(nil)) != hash {
		return fmt.Errorf("证据正文校验失败: %s", hash)
	}
	return nil
}

// A new body becomes visible only after a durable write. Failed SQL commits may
// leave unreferenced files; GC reaps those after a full day's grace period.
func (s *Store) writeBody(r io.Reader, expectedLength int64, expectedHash string) (hash string, err error) {
	stage := filepath.Join(s.Dir, ".staging")
	if err = os.MkdirAll(stage, 0o700); err != nil {
		return
	}
	f, err := os.CreateTemp(stage, "body-")
	if err != nil {
		return "", err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		return "", err
	}
	hash = hex.EncodeToString(h.Sum(nil))
	if n != expectedLength {
		return "", fmt.Errorf("正文不完整: 预期 %d 字节，读取 %d 字节", expectedLength, n)
	}
	if expectedHash != "" && expectedHash != hash {
		return "", errors.New("原始流量正文哈希不匹配")
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	path, err := hashPath(s.Dir, hash)
	if err != nil {
		return "", err
	}
	if _, err = os.Stat(path); err == nil {
		if err = verifyFile(path, hash, n); err != nil {
			return "", err
		}
		// Refresh the grace period for a restored but not-yet-committed body.
		now := time.Now()
		return hash, os.Chtimes(path, now, now)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer d.Close()

	// 目录 fsync 在 Windows 上不可用:FlushFileBuffers 对目录句柄返回
	// ERROR_ACCESS_DENIED(Win32 限制,非 Go 问题)。POSIX 用它持久化 rename
	// 元数据,Windows 无等价操作 —— 这是能力缺失,不是可忽略的优化。
	//
	// 取舍:Windows 上跳过目录 fsync,崩溃一致性弱于 Linux(NTFS metadata
	// journal 提供较弱但存在的保证)。改动前是「Windows 上每次写入都失败」,
	// 跳过是净改善 —— 但不是「修好了」,是「降级可用」。
	// 若未来需要 Windows 生产部署,必须改用 journal 文件方案,不能只靠这个分支。
	// 详见《KNOWN-LIMITATIONS》L1（docs 仓库：quantex/KNOWN-LIMITATIONS.md）。
	if runtime.GOOS == "windows" {
		// 显式记录降级事实,不静默跳过:运维需要能从日志里查到
		// 「这台机器上证据持久化是弱保证」,否则会误以为已强持久化。
		slog.Warn("evidence: 跳过目录 fsync(Windows 无 FlushFileBuffers 目录支持),崩溃一致性降级",
			"dir", filepath.Dir(path), "limitation", "L1")
		return hash, nil
	}
	return hash, d.Sync()
}

// OpenBody may be called without holding the evidence lock; see Binding.
func (s *Store) OpenBody(snapshot db.TrafficEvidenceSnapshot, side string) (*os.File, int64, error) {
	hash, length := snapshot.ReqHash, snapshot.ReqLen
	if side == "response" {
		hash, length = snapshot.RespHash, snapshot.RespLen
	} else if side != "request" {
		return nil, 0, errors.New("side 必须为 request 或 response")
	}
	path, err := hashPath(s.Dir, hash)
	if err != nil {
		return nil, 0, err
	}
	if err = verifyFile(path, hash, length); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path)
	return f, length, err
}

// CopySnapshots is used by both report downloads and portable task archives.
// The destination owns real copies, never links into either disposable store.
//
// # 为什么不套事务
//
// 它只做文件 IO，一行 SQL 都不写。以前套 WithEvidenceTx 是为了借 advisory lock
// 保护文件系统，而事务里做 fsync 会让 SQLite 的全体写操作排队到 busy_timeout
// 耗尽（见 ARCH-SQLite-Migration.md §6.4.2 #2）。现在用进程内的 filesMu 串行化
// 文件操作 + staging/rename 保原子，不再占数据库写锁。
func (s *Store) CopySnapshots(ctx context.Context, snapshots []db.TrafficEvidenceSnapshot, dest string) error {
	unlock, err := s.lockFiles(ctx, "copy")
	if err != nil {
		return err
	}
	defer unlock()
	return s.copySnapshots(snapshots, dest)
}

// copySnapshots 逐份复制 blob 到 dest。
//
// 复制走 dest/.staging + os.Rename：中途失败时目标目录里不会出现半截文件
// （半个 .bin 会被 verifyFile 判成 hash 不匹配，进而让整个归档安装失败）。
func (s *Store) copySnapshots(snapshots []db.TrafficEvidenceSnapshot, dest string) error {
	stage := filepath.Join(dest, ".staging")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(stage) // 清理失败残留；成功路径里已被 rename 走空
	for _, v := range snapshots {
		for _, side := range []string{"request", "response"} {
			if err := func() error {
				f, length, err := s.OpenBody(v, side)
				if err != nil {
					return err
				}
				defer f.Close()
				hash := v.ReqHash
				if side == "response" {
					hash = v.RespHash
				}
				target, err := hashPath(dest, hash)
				if err != nil {
					return err
				}
				if _, statErr := os.Stat(target); statErr == nil {
					// 已存在：内容相同才算幂等成功，否则说明 dest 被污染。
					return verifyFile(target, hash, length)
				} else if !os.IsNotExist(statErr) {
					return statErr
				}
				tmp, err := os.CreateTemp(stage, "copy-")
				if err != nil {
					return err
				}
				tmpName := tmp.Name()
				defer os.Remove(tmpName) // rename 成功后是 no-op
				n, copyErr := io.Copy(tmp, f)
				syncErr := tmp.Sync()
				closeErr := tmp.Close()
				if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
					return err
				}
				if n != length {
					return fmt.Errorf("证据正文复制不完整: 预期 %d 字节，写入 %d 字节", length, n)
				}
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					return err
				}
				return os.Rename(tmpName, target)
			}(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) InstallSnapshots(ctx context.Context, snapshots []db.TrafficEvidenceSnapshot, source string) error {
	return s.WithInstalledSnapshots(ctx, snapshots, source, func() error { return nil })
}

// WithInstalledSnapshots pins installed bodies until the metadata restore finishes.
//
// # 事务边界（P0）
//
// 文件安装（读归档包、writeBody 的 io.Copy + fsync）全部在 restore() 之前完成，
// **不占任何数据库写锁**。restore() 自己按需要开短事务。
//
// # 为什么还要 holding filesMu 跨过 restore()
//
// filesMu 是进程内的文件操作闸门，不是数据库锁。它挡住的是「GC 在安装到一半
// 时把刚写好的 blob 删掉」。它不阻塞任何数据库写 —— 所以不会把
// 「文件 IO 在事务里」那个问题搬回来。
func (s *Store) WithInstalledSnapshots(ctx context.Context, snapshots []db.TrafficEvidenceSnapshot, source string, restore func() error) error {
	unlock, err := s.lockFiles(ctx, "install")
	if err != nil {
		return err
	}
	defer unlock()
	for _, v := range snapshots {
		if v.ID != db.TrafficSnapshotID(v) {
			return errors.New("归档证据快照元数据哈希不匹配")
		}
		for _, body := range []struct {
			hash   string
			length int64
		}{{v.ReqHash, v.ReqLen}, {v.RespHash, v.RespLen}} {
			if err := func() error {
				path, err := hashPath(source, body.hash)
				if err != nil {
					return err
				}
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				// writeBody 内部走 .staging + rename + fsync，文件层原子。
				_, err = s.writeBody(f, body.length, body.hash)
				return err
			}(); err != nil {
				return err
			}
		}
	}
	return restore()
}

// Collect 回收不再被引用的 blob。
//
// # 三段式（P0：文件遍历移出事务）
//
//  1. 短写事务：把无引用的快照标 unreferenced、删过期行。
//  2. 事务外：重新读一次引用集合（拿到最新值，不占写锁）。
//  3. 事务外：WalkDir 遍历，删「未被引用且超过宽限期」的文件。
//
// # 为什么宽限期是唯一真正的保护
//
// 一个 body 的生命周期是「先落盘、后插引用行」。两次写之间的窗口里它看起来
// 是孤儿，任何引用集合（事务内还是事务外读的）都看不见那条还没写的行。
// 所以删不删只由 mtime 决定 —— gcGracePeriod 必须显著大于一次归档的耗时。
// 事务外重读引用集合只是收窄「读完之后新增引用」的窗口，不是保护本身。
func (s *Store) Collect(ctx context.Context, now time.Time) error {
	unlock, err := s.lockFiles(ctx, "collect")
	if err != nil {
		return err
	}
	defer unlock()

	if err := s.collectRows(ctx, now); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	refs, err := s.referencedHashes(ctx)
	if err != nil {
		return err
	}
	for _, root := range []string{filepath.Join(s.Dir, "blobs"), filepath.Join(s.Dir, ".staging")} {
		walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if os.IsNotExist(walkErr) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if refs[strings.TrimSuffix(entry.Name(), ".bin")] {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if now.Sub(info.ModTime()) < gcGracePeriod {
				return nil
			}
			return os.Remove(path)
		})
		if walkErr != nil {
			return walkErr
		}
	}
	return nil
}

// gcGracePeriod 是 blob 的宽限期。
//
// 一个 body 先落盘、之后才被 DB 行引用；两次写之间的窗口里它是「孤儿」。
// 宽限期必须显著大于任何一次归档/记录操作的耗时，否则会在正常写入路径里
// 删掉正在被引用的文件。
const gcGracePeriod = 24 * time.Hour

// collectRows 在短事务里完成所有数据库改动（标记 + 删过期行）。
func (s *Store) collectRows(ctx context.Context, now time.Time) error {
	return s.DB.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE traffic_evidence_snapshots SET unreferenced_at=$1
WHERE unreferenced_at IS NULL AND NOT EXISTS(SELECT 1 FROM finding_traffic_bindings b WHERE b.snapshot_id=traffic_evidence_snapshots.id)`, db.FormatISO(now)); err != nil {
			return err
		}
		// 用 <= 而不是 <：时间列是毫秒精度的 TEXT，PG 版的 `<` 靠微秒精度避免
		// 边界相等，而这里「标记时刻 == 阈值」在秒级测试里是常见情形，
		// 严格 `<` 会让宽限期永远差一毫秒不生效。语义上「刚好满 24 小时」
		// 本来就该回收。
		_, err := tx.ExecContext(ctx, `DELETE FROM traffic_evidence_snapshots
WHERE unreferenced_at<=$1
AND NOT EXISTS(SELECT 1 FROM finding_traffic_bindings b WHERE b.snapshot_id=traffic_evidence_snapshots.id)`, db.FormatISO(now.Add(-gcGracePeriod)))
		return err
	})
}

// referencedHashes 取当前所有被快照行引用的 hash（事务外读，不占写锁）。
func (s *Store) referencedHashes(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT req_hash FROM traffic_evidence_snapshots
UNION SELECT resp_hash FROM traffic_evidence_snapshots`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := map[string]bool{}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		if hash != "" {
			refs[hash] = true
		}
	}
	return refs, rows.Err()
}

func (s *Store) RunGC(ctx context.Context) {
	timer := time.NewTicker(time.Hour)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			if err := s.Collect(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[evidence] cleanup failed: %v", err)
			}
		}
	}
}
