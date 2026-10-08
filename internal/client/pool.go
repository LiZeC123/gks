package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/LiZeC123/gks/internal/log"
	"github.com/LiZeC123/gks/internal/mux"
)

// 会话池相关错误。
var (
	// ErrPoolTimeout 表示在 ConnectTimeout 内没等到可用会话。
	ErrPoolTimeout = errors.New("client: 等待可用会话超时")
	// ErrPoolClosed 表示池已关闭。
	ErrPoolClosed = errors.New("client: 会话池已关闭")
	// errNoCapacity 是内部哨兵：会话数已达上限。
	errNoCapacity = errors.New("client: 会话数已达上限")
)

// PoolConfig 是会话池参数（来自 client.pool 段）。
type PoolConfig struct {
	// ServerAddr 是服务端的 KCP/UDP 地址。
	ServerAddr string
	// Size 是保底会话数（池会持续维护这么多条健康会话）。
	Size int
	// MaxSessions 是硬上限，达到后 Acquire 会排队等待。
	MaxSessions int
	// IdleTimeout 是空闲会话回收阈值（只回收超出 Size 的部分）。
	IdleTimeout time.Duration
	// StartupJitter 是启动错峰上限：相邻保底会话的建立间隔在其内随机。
	StartupJitter time.Duration
	// ConnectTimeout 是 Acquire 等待可用会话的上限。
	ConnectTimeout time.Duration
	// BackoffMin / BackoffMax 是后台重建会话的指数退避区间。
	BackoffMin time.Duration
	BackoffMax time.Duration
}

func (c *PoolConfig) normalize() {
	if c.Size < 1 {
		c.Size = 1
	}
	if c.MaxSessions < c.Size {
		c.MaxSessions = c.Size
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 5 * time.Second
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = 500 * time.Millisecond
	}
	if c.BackoffMax < c.BackoffMin {
		c.BackoffMax = 30 * time.Second
	}
}

// PoolStats 是会话池的运行状态。
type PoolStats struct {
	Sessions    int
	InUse       int
	Idle        int
	Creating    int
	Waiters     int
	Rebuilds    uint64
	IdleTimeout time.Duration
}

type poolEntry struct {
	sess      *mux.Session
	inUse     bool
	idleSince time.Time
}

// SessionPool 是一组已认证 KCP 会话的复用池。
//
// 保守形态（dev.md §8 阶段 3/5 的折中）：每个时刻一条会话最多承载 1 条活跃流，
// 因此不需要流间公平调度，也不存在一条慢流拖死整条会话的问题；并发压力通过
// 扩充会话数（至 MaxSessions）而不是多路复用来承担。
//
// 会话生命周期：池负责创建、保底重建、空闲回收与关闭；Handler 只负责
// Acquire → 开流 → Release（或 Discard）。
type SessionPool struct {
	cfg    PoolConfig
	dialer Dialer
	opts   mux.Options
	log    *slog.Logger

	mu       sync.Mutex
	entries  map[*mux.Session]*poolEntry
	creating int
	waiters  int
	rebuilds uint64
	backoff  time.Duration
	closed   bool
	notify   chan struct{}

	onDropMu sync.Mutex
	onDrop   func(*mux.Session)

	startOnce sync.Once
	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// NewSessionPool 构造会话池。会话选项由 opts 提供（PSK/AEAD/上限/日志等）。
func NewSessionPool(cfg PoolConfig, dialer Dialer, opts mux.Options) *SessionPool {
	cfg.normalize()
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &SessionPool{
		cfg:     cfg,
		dialer:  dialer,
		opts:    opts,
		log:     logger.With("component", "pool"),
		entries: make(map[*mux.Session]*poolEntry),
		notify:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// SetOnDrop 注册会话被池丢弃/回收时的回调（用于清理按会话的资源，例如路由表）。
func (p *SessionPool) SetOnDrop(fn func(*mux.Session)) {
	p.onDropMu.Lock()
	p.onDrop = fn
	p.onDropMu.Unlock()
}

// Start 预热保底会话并启动后台维护（保底重建 + 空闲回收）。
func (p *SessionPool) Start(ctx context.Context) {
	p.startOnce.Do(func() {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.ensureMin(ctx, true)
			ticker := time.NewTicker(p.maintainInterval())
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-p.done:
					return
				case <-ticker.C:
					p.reclaimIdle()
					p.ensureMin(ctx, false)
				}
			}
		}()
		p.log.Info("会话池已启动",
			log.Event, "pool_start",
			"size", p.cfg.Size,
			"max_sessions", p.cfg.MaxSessions,
			"idle_timeout", p.cfg.IdleTimeout.String(),
			"server", p.cfg.ServerAddr,
		)
	})
}

// maintainInterval 由空闲回收阈值推导维护间隔（测试里可用很小的时间）。
func (p *SessionPool) maintainInterval() time.Duration {
	d := time.Second
	if p.cfg.IdleTimeout > 0 {
		d = p.cfg.IdleTimeout / 4
	}
	if d < 50*time.Millisecond {
		d = 50 * time.Millisecond
	}
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

// Acquire 取一条已认证的空闲会话（并标记为使用中）。
func (p *SessionPool) Acquire(ctx context.Context) (*mux.Session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(p.cfg.ConnectTimeout)
	var lastErr error

	for {
		if sess := p.takeIdle(); sess != nil {
			return sess, nil
		}

		sess, err := p.tryCreate(ctx, true)
		switch {
		case err == nil:
			return sess, nil
		case errors.Is(err, ErrPoolClosed):
			return nil, ErrPoolClosed
		case errors.Is(err, errNoCapacity):
			// 达到上限：等别人归还
		default:
			lastErr = err
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			if lastErr != nil {
				return nil, fmt.Errorf("%w: %v", ErrPoolTimeout, lastErr)
			}
			return nil, ErrPoolTimeout
		}
		if remaining > 200*time.Millisecond {
			remaining = 200 * time.Millisecond
		}

		p.mu.Lock()
		ch := p.notify
		p.waiters++
		p.mu.Unlock()

		select {
		case <-ch:
		case <-p.done:
			p.decWaiters()
			return nil, ErrPoolClosed
		case <-ctx.Done():
			p.decWaiters()
			return nil, ctx.Err()
		case <-time.After(remaining):
		}
		p.decWaiters()
	}
}

// Release 归还会话；会话已失效或流未清空时直接丢弃。
func (p *SessionPool) Release(sess *mux.Session) {
	if sess == nil {
		return
	}
	p.mu.Lock()
	e, ok := p.entries[sess]
	if !ok || p.closed || p.isDead(sess) || sess.NumStreams() != 0 {
		delete(p.entries, sess)
		p.mu.Unlock()
		_ = sess.Close()
		p.notifyDrop(sess)
		p.signal()
		return
	}
	e.inUse = false
	e.idleSince = time.Now()
	p.mu.Unlock()
	p.signal()
}

// Discard 丢弃一条不可用的会话（例如开流失败、等待回包期间失效）。
func (p *SessionPool) Discard(sess *mux.Session) {
	if sess == nil {
		return
	}
	p.mu.Lock()
	delete(p.entries, sess)
	p.mu.Unlock()
	_ = sess.Close()
	p.notifyDrop(sess)
	p.signal()
}

// Close 关闭池内所有会话并停止后台维护。
func (p *SessionPool) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		sessions := make([]*mux.Session, 0, len(p.entries))
		for s := range p.entries {
			sessions = append(sessions, s)
		}
		p.entries = make(map[*mux.Session]*poolEntry)
		p.signalLocked()
		p.mu.Unlock()

		close(p.done)
		for _, s := range sessions {
			_ = s.Close()
			p.notifyDrop(s)
		}
		p.wg.Wait()
	})
}

// Stats 返回池状态快照。
func (p *SessionPool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := PoolStats{
		Sessions:    len(p.entries),
		Creating:    p.creating,
		Waiters:     p.waiters,
		Rebuilds:    p.rebuilds,
		IdleTimeout: p.cfg.IdleTimeout,
	}
	for _, e := range p.entries {
		if e.inUse {
			st.InUse++
		} else {
			st.Idle++
		}
	}
	return st
}

// ---- 内部实现 ----

func (p *SessionPool) takeIdle() *mux.Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.inUse {
			continue
		}
		if p.isDead(e.sess) || e.sess.NumStreams() != 0 {
			continue
		}
		e.inUse = true
		e.idleSince = time.Time{}
		return e.sess
	}
	return nil
}

// tryCreate 新建一条会话并登记。inUse 表示由 Acquire 直接占用（预热/重建时为 false）。
func (p *SessionPool) tryCreate(ctx context.Context, inUse bool) (*mux.Session, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	if len(p.entries)+p.creating >= p.cfg.MaxSessions {
		p.mu.Unlock()
		return nil, errNoCapacity
	}
	p.creating++
	p.mu.Unlock()

	sess, err := p.dial(ctx)

	p.mu.Lock()
	p.creating--
	if err != nil {
		p.signalLocked()
		p.mu.Unlock()
		return nil, err
	}
	if p.closed {
		p.mu.Unlock()
		_ = sess.Close()
		p.notifyDrop(sess)
		return nil, ErrPoolClosed
	}
	e := &poolEntry{sess: sess, inUse: inUse}
	if !inUse {
		e.idleSince = time.Now()
	}
	p.entries[sess] = e
	total := len(p.entries)
	p.signalLocked()
	p.mu.Unlock()

	p.watch(sess)
	p.log.Info("新建会话",
		log.Event, "pool_session_new",
		log.SessionID, sess.Conv(),
		"sessions", total,
	)
	return sess, nil
}

func (p *SessionPool) dial(ctx context.Context) (*mux.Session, error) {
	conn, err := p.dialer.Dial(p.cfg.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("连接服务端 %s: %w", p.cfg.ServerAddr, err)
	}
	sess, err := mux.DialSession(ctx, conn, p.opts)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("会话认证失败: %w", err)
	}
	return sess, nil
}

// watch 监听会话失效，及时从池中摘除并唤醒等待者。
func (p *SessionPool) watch(sess *mux.Session) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		select {
		case <-sess.Done():
		case <-p.done:
			return
		}
		p.mu.Lock()
		_, existed := p.entries[sess]
		delete(p.entries, sess)
		p.signalLocked()
		p.mu.Unlock()
		if existed {
			p.notifyDrop(sess)
			p.log.Warn("会话失效，已从池中移除",
				log.Event, "pool_session_dead",
				log.SessionID, sess.Conv(),
			)
		}
	}()
}

func (p *SessionPool) reclaimIdle() {
	if p.cfg.IdleTimeout <= 0 {
		return
	}
	now := time.Now()
	var toClose []*mux.Session

	p.mu.Lock()
	healthy := 0
	for _, e := range p.entries {
		if !p.isDead(e.sess) {
			healthy++
		}
	}
	for _, e := range p.entries {
		if e.inUse || p.isDead(e.sess) || e.idleSince.IsZero() {
			continue
		}
		if healthy <= p.cfg.Size {
			break // 保持保底数量
		}
		if now.Sub(e.idleSince) >= p.cfg.IdleTimeout {
			toClose = append(toClose, e.sess)
			delete(p.entries, e.sess)
			healthy--
		}
	}
	p.mu.Unlock()

	for _, s := range toClose {
		_ = s.Close()
		p.notifyDrop(s)
		p.log.Info("回收空闲会话",
			log.Event, "pool_session_reclaim",
			log.SessionID, s.Conv(),
		)
	}
	if len(toClose) > 0 {
		p.signal()
	}
}

// ensureMin 把健康会话数补到 Size；stagger 为 true 时按 StartupJitter 错峰。
func (p *SessionPool) ensureMin(ctx context.Context, stagger bool) {
	for {
		if !p.needMore() {
			return
		}
		if wait := p.nextWait(stagger); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-p.done:
				return
			case <-time.After(wait):
			}
		}
		_, err := p.tryCreate(ctx, false)
		p.recordCreateResult(err)
		if errors.Is(err, ErrPoolClosed) {
			return
		}
	}
}

func (p *SessionPool) needMore() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	healthy := 0
	for _, e := range p.entries {
		if !p.isDead(e.sess) {
			healthy++
		}
	}
	return healthy+p.creating < p.cfg.Size
}

func (p *SessionPool) nextWait(stagger bool) time.Duration {
	p.mu.Lock()
	backoff := p.backoff
	p.mu.Unlock()
	if backoff > 0 {
		return jitter(backoff)
	}
	if stagger && p.cfg.StartupJitter > 0 {
		return time.Duration(rand.Int63n(int64(p.cfg.StartupJitter) + 1))
	}
	return 0
}

func (p *SessionPool) recordCreateResult(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.backoff = 0
		return
	}
	p.rebuilds++
	next := p.backoff * 2
	if next < p.cfg.BackoffMin {
		next = p.cfg.BackoffMin
	}
	if next > p.cfg.BackoffMax {
		next = p.cfg.BackoffMax
	}
	p.backoff = next
}

func (p *SessionPool) isDead(sess *mux.Session) bool {
	select {
	case <-sess.Done():
		return true
	default:
		return false
	}
}

func (p *SessionPool) signal() {
	p.mu.Lock()
	p.signalLocked()
	p.mu.Unlock()
}

// signalLocked 广播「状态变化」：关闭旧 channel 并换新的，唤醒所有等待者。
func (p *SessionPool) signalLocked() {
	close(p.notify)
	p.notify = make(chan struct{})
}

func (p *SessionPool) decWaiters() {
	p.mu.Lock()
	if p.waiters > 0 {
		p.waiters--
	}
	p.mu.Unlock()
}

func (p *SessionPool) notifyDrop(sess *mux.Session) {
	p.onDropMu.Lock()
	fn := p.onDrop
	p.onDropMu.Unlock()
	if fn != nil {
		fn(sess)
	}
}

// jitter 在 ±25% 内抖动，避免多个客户端同时重建（雪崩）。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	quarter := int64(d) / 4
	if quarter <= 0 {
		return d
	}
	out := d + time.Duration(rand.Int63n(2*quarter+1)-quarter)
	if out <= 0 {
		return d
	}
	return out
}
