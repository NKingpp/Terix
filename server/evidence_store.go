package server

import (
	"path/filepath"

	"github.com/Autumn-27/artex/evidence"
)

// evidenceStore 返回证据快照存储。
//
// # 为什么这个文件存在
//
// 它原先定义在 server/finding_traffic.go（漏洞-流量绑定 handler 文件）里 ——
// 但「构造 evidence.Store」和「漏洞绑定」没有任何关系。
//
// 而 finding_traffic.go 已随渗透功能删除（2026-10-29），
// 但 evidence.Store 仍在用：
//   - task_archives.go 的 CopySnapshots / WithInstalledSnapshots（任务归档）
//   - server.go 的 RunGC（快照清理）
//
// 所以把它搬到这里，而不是随那个文件删掉。
//
// ⚠️ 曾经在 finding_traffic.go 里的位置，是「函数定义在语义无关的文件里」的典型 ——
// 那让「删这个文件」的判断得出完全错误的结论（以为删的是「漏洞绑定」，
// 实际删的是「被 3 处依赖的构造函数」）。
//
// # 判据
//
// 列删除清单时，对每个要删的文件先跑：
//
//	grep -nE "^func \(s \*Server\)" <文件>
//
// 逐个数「哪些函数的调用方不在本次删除清单里」—— 那些必须先搬。
func (s *Server) evidenceStore() *evidence.Store {
	return evidence.New(s.m.pg, filepath.Join(s.m.dir, "evidence"))
}
