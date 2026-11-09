package server

import (
	"testing"
	"time"
)

func TestPlannerWaitingCooldown(t *testing.T) {
	e := &Engine{}
	e.beginWaiting("2", time.Hour)
	if !e.inWaiting("2") {
		t.Fatal("冷却期内应处于 waiting")
	}
	e.clearWaiting("2")
	if e.inWaiting("2") {
		t.Fatal("clearWaiting 后不该还在 waiting")
	}
}

func TestPlannerWaitingExpires(t *testing.T) {
	e := &Engine{}
	e.beginWaiting("2", -time.Second) // 已过期
	if e.inWaiting("2") {
		t.Fatal("过期后应自动恢复规划")
	}
	if e.inWaiting("2") {
		t.Fatal("过期的标记应已被清除")
	}
}

func TestPlannerWaitingIsPerTask(t *testing.T) {
	e := &Engine{}
	e.beginWaiting("2", time.Hour)
	if e.inWaiting("3") {
		t.Fatal("waiting 不该串到别的任务")
	}
}

func TestIntentCountNilStore(t *testing.T) {
	if got := intentCount(nil); got != -1 {
		t.Fatalf("nil store 应返回 -1（跳过 waiting 判定），实际 %d", got)
	}
}
