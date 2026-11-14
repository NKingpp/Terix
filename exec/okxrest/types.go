// Package okxrest 是最小的 OKX v5 REST 客户端：只用标准库，不依赖仓库任何包。
//
// # 为什么单独成包而不是塞进 exec
//
//  1. 签名器需要被 httptest 单测，不需要数据库；
//  2. exec 目前只提供 ChannelSubmitter 抽象（现货语义），swap 的
//     instType/tdMode/posSide/reduceOnly/attachAlgoOrds 与它错位；
//  3. 纯标准库 → 天然无循环依赖，不触发 scripts/check-vaultd-isolation.sh
//     的 3 组断言。
package okxrest

// Credentials 是一次调用所需的全部鉴权信息。
//
// Demo 必须与 key 的环境一致：错配时 OKX 报 50101，文案像「key 无效」
// 而实际是环境错了。
type Credentials struct {
	APIKey     string
	SecretKey  string
	Passphrase string
	Demo       bool
	// BaseURL 为空则用 https://www.okx.com。
	// 单独可配是为了让「网络环境需要镜像」的部署不改代码。
	BaseURL string
}

// AlgoAttach 是随主单一起提交的止盈止损（attachAlgoOrds 的元素）。
//
// px 传 "-1" 表示触发后按市价成交 —— 这是 OKX 的约定，不是笔误。
type AlgoAttach struct {
	TpTriggerPx     string `json:"tpTriggerPx,omitempty"`
	TpOrdPx         string `json:"tpOrdPx,omitempty"`
	TpTriggerPxType string `json:"tpTriggerPxType,omitempty"`
	SlTriggerPx     string `json:"slTriggerPx,omitempty"`
	SlOrdPx         string `json:"slOrdPx,omitempty"`
	SlTriggerPxType string `json:"slTriggerPxType,omitempty"`
}

// OrderRequest 是 POST /api/v5/trade/order 的请求体。
type OrderRequest struct {
	InstID      string       `json:"instId"`
	TdMode      string       `json:"tdMode"`            // cross | isolated | cash
	Side        string       `json:"side"`              // buy | sell
	PosSide     string       `json:"posSide,omitempty"` // long | short | net
	OrdType     string       `json:"ordType"`           // market | limit
	Sz          string       `json:"sz"`
	Px          string       `json:"px,omitempty"`
	ClOrdID     string       `json:"clOrdId"`
	ReduceOnly  bool         `json:"reduceOnly,omitempty"`
	AttachAlgos []AlgoAttach `json:"attachAlgoOrds,omitempty"`
}

// OrderAck 是下单成功后 data[0] 的内容。
type OrderAck struct {
	OrdID   string `json:"ordId"`
	ClOrdID string `json:"clOrdId"`
	SCode   string `json:"sCode"`
	SMsg    string `json:"sMsg"`
	TS      string `json:"ts"`
}

// OrderState 是查询单笔订单的返回（GET /api/v5/trade/order）。
type OrderState struct {
	OrdID     string `json:"ordId"`
	ClOrdID   string `json:"clOrdId"`
	InstID    string `json:"instId"`
	State     string `json:"state"` // live | partially_filled | filled | canceled
	AccFillSz string `json:"accFillSz"`
	AvgPx     string `json:"avgPx"`
	UTime     string `json:"uTime"`
}

// AlgoOrder 是算法单（TP/SL）的挂单记录。
//
// InstID 字段名在 OKX 侧是 instId；State 只有 live/effective/canceled 三种，
// 只有 live/effective 才算「保护还在」。
type AlgoOrder struct {
	AlgoID      string `json:"algoId"`
	InstID      string `json:"instId"`
	Side        string `json:"side"`
	PosSide     string `json:"posSide"`
	Sz          string `json:"sz"`
	OrdType     string `json:"ordType"`
	TpTriggerPx string `json:"tpTriggerPx"`
	SlTriggerPx string `json:"slTriggerPx"`
	State       string `json:"state"`
}

// Position 是 GET /api/v5/account/positions 的一项。
//
// Pos 是净持仓（可负）；posSide 在 net 模式下恒为 "net"。
type Position struct {
	InstID   string `json:"instId"`
	PosSide  string `json:"posSide"`
	Pos      string `json:"pos"`
	AvailPos string `json:"availPos"`
	AvgPx    string `json:"avgPx"`
	Upl      string `json:"upl"`
	Lever    string `json:"lever"`
	MgnMode  string `json:"mgnMode"`
	MarkPx   string `json:"markPx"`
	CTime    string `json:"cTime"`
	UTime    string `json:"uTime"`
}

// AccountConfig 是 GET /api/v5/account/config 的关键字段子集。
//
// PosMode 决定 posSide 的取值：net_mode 下用 "net"，long_short_mode 下用
// long/short —— 这套语义错了会让下单直接被 51000 拒。
type AccountConfig struct {
	UID     string `json:"uid"`
	AcctLv  string `json:"acctLv"`
	PosMode string `json:"posMode"` // net_mode | long_short_mode
}

// AlgoRequest 是 POST /api/v5/trade/order-algo 的请求体（oco = 同时挂 TP 与 SL）。
type AlgoRequest struct {
	InstID        string `json:"instId"`
	TdMode        string `json:"tdMode"`
	Side          string `json:"side"`
	PosSide       string `json:"posSide,omitempty"`
	OrdType       string `json:"ordType"` // oco | conditional
	CloseFraction string `json:"closeFraction,omitempty"`
	ReduceOnly    bool   `json:"reduceOnly,omitempty"`
	TpTriggerPx   string `json:"tpTriggerPx,omitempty"`
	TpOrdPx       string `json:"tpOrdPx,omitempty"`
	SlTriggerPx   string `json:"slTriggerPx,omitempty"`
	SlOrdPx       string `json:"slOrdPx,omitempty"`
}
