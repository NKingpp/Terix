package db

import "errors"

// ErrOrderInvalid 表示入参非法（方向、数量、模式、状态转换等）。
//
// 原来定义在 paper_orders.go 里，纸面模式删除后移到这里 —— 它被
// 执行账户参数、授权、台账等多处复用，早已不专属纸面。
var ErrOrderInvalid = errors.New("invalid order params")
