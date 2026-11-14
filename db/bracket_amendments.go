package db

import "strings"

// BracketAmendment 是一次止盈止损改价的审计记录。
//
// # 为什么需要它
//
// 改 TP/SL 是同步动作，不经过 orders_draft / orders_execution ——
// 那两张表里不会留下任何痕迹。而「谁把止损从 161 挪到 158」恰恰是
// 事后最需要回答的问题（挪止损是最容易造成大额亏损的操作）。
type BracketAmendment struct {
	ID        int64
	AccountID int64
	InstID    string
	PosSide   string
	AlgoID    string
	OldTp     string
	OldSl     string
	NewTp     string
	NewSl     string
	Operator  string // agent key 或 human
	Reason    string
	// Result 是结果分类：amended（原子改价成功）| replaced（撤旧挂新）| failed
	Result string
	Error  string
}

// InsertBracketAmendment 落一条改价审计。
//
// 返回错误不阻断调用方：审计写失败不该让风控动作失败（把保护改对的
// 价值高于留痕），但调用方应把它记进日志。
func (d *DB) InsertBracketAmendment(a BracketAmendment) error {
	if strings.TrimSpace(a.InstID) == "" {
		return nil // 没有标的就没有可追溯性，不值得写一行垃圾
	}
	_, err := d.Exec(`INSERT INTO bracket_amendments
(account_id, inst_id, pos_side, algo_id, old_tp, old_sl, new_tp, new_sl,
 operator, reason, result, error)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		nullIfEmptyI64(a.AccountID), strings.TrimSpace(a.InstID),
		nullIfBlank(a.PosSide), nullIfBlank(a.AlgoID),
		nullIfBlank(a.OldTp), nullIfBlank(a.OldSl),
		nullIfBlank(a.NewTp), nullIfBlank(a.NewSl),
		strings.TrimSpace(a.Operator), nullIfBlank(a.Reason),
		strings.TrimSpace(a.Result), nullIfBlank(a.Error))
	return err
}
