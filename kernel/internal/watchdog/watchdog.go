// Package watchdog 提供进程健康巡检器（§12.2.3 / §10.5 / §13.3）。
//
// 对齐《理想架构.md》：
//   - 心跳周期 5s，连续 3 次无响应判定 crashed（§13.3）；
//   - busy 期间仅超出 maxBusyMs 且探活无响应才判定僵死（§10.5）；
//   - 空闲计时：达到 idleTimeout 触发 onIdle（§14.9 / §9.1）；
//   - 任何成功读到的协议行视为活性证据，不只 pong（§13.3）。
package watchdog

import (
	"context"
	"time"
)

// Status 巡检所需的单元状态快照（由调用方维护，仅被巡检 goroutine 读取）。
type Status struct {
	LastPong   time.Time // 最近一次成功读行（活性证据）
	LastActive time.Time // 最近一次活动（空闲依据）
	BusyUntil  time.Time // 非零 = 处于 busy，值为 busyDeadline（§10.1.2 修饰字段）
	Busy       bool
}

// Probe 供巡检调用的动作。
type Probe struct {
	Alive func() bool // 进程是否仍在读循环（true 表示未退出）
	Ping  func() bool // 强制探活（§10.5.1：短超时 2s），true=有响应
}

// Watchdog 单 goroutine 定时巡检器（§12.2.3：以单 goroutine 巡检）。
type Watchdog struct {
	Interval         time.Duration // 巡检周期（默认 1s）
	HeartbeatTimeout time.Duration // 心跳超时（默认 15s，约 3 个 5s 周期）
	IdleTimeout      time.Duration // 空闲回收阈值；<=0 禁用空闲计时
	BusyProbeTimeout time.Duration // busy 超时后的探活超时（§10.5：2s）；未设时由 Ping 实现自控

	OnCrashed func(reason string) // 判定僵死/心跳超时（reason ∈ E_BUSY_TIMEOUT / 心跳超时）
	OnIdle    func()              // 空闲到点（仅 IdleTimeout>0 时触发）
}

// Run 阻塞巡检直至 ctx 取消或进程退出（probe.Alive() 返回 false）。
// 每次周期调用 Tick；用于单单元独立巡检。
func (w *Watchdog) Run(ctx context.Context, st *Status, probe Probe) {
	interval := w.Interval
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if w.Tick(st, probe) {
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}
}

// Tick 执行一次巡检周期，返回 true 表示该单元已被判定终止（心跳超时/busy 僵死），
// 调用方应据此触发终止。供单 goroutine 集中巡检（§12.2.3：以单 goroutine 巡检，
// 逐单元喂 Tick）与独立巡检共用——行为一致，唯一区别是驱动方式。
func (w *Watchdog) Tick(st *Status, probe Probe) bool {
	hbTimeout := w.HeartbeatTimeout
	if hbTimeout <= 0 {
		hbTimeout = 15 * time.Second
	}
	if !probe.Alive() {
		return true
	}
	if w.checkBusy(st, probe) {
		return true
	}
	if time.Since(st.LastPong) > hbTimeout {
		if w.OnCrashed != nil {
			w.OnCrashed("heartbeat timeout")
		}
		return true
	}
	if w.IdleTimeout > 0 && !st.Busy &&
		time.Since(st.LastActive) > w.IdleTimeout {
		if w.OnIdle != nil {
			w.OnIdle()
		}
	}
	return false
}

// checkBusy §10.5：到达 busyUntil 仍未退出 busy → 强制探活（短超时 2s）；
// 无响应 → 判定僵死，强制终止，记 E_BUSY_TIMEOUT。返回 true 表示已终止。
func (w *Watchdog) checkBusy(st *Status, probe Probe) bool {
	if !st.Busy || st.BusyUntil.IsZero() {
		return false
	}
	if time.Now().Before(st.BusyUntil) {
		return false // 倒计时内：豁免空闲杀死与关闭策略
	}
	// 已超时：强制 ping 探活（§10.5.2）。
	ok := true
	if probe.Ping != nil {
		ok = probe.Ping()
	}
	if !ok {
		if w.OnCrashed != nil {
			w.OnCrashed("E_BUSY_TIMEOUT")
		}
		return true
	}
	// 有响应：视为仍忙碌，宿主可延长 busyUntil（由调用方更新）。
	return false
}
