package session

import (
	"context"
	"sync"
	"time"
)

// Leaser 提供会话级执行租约：同一会话同一时刻只允许一个 Turn（Run/Resume）执行。
//
// 租约带 TTL，持有方须在过期前续约；进程崩溃后租约自然过期，其他副本可接管。
// owner 由调用方为每次执行生成唯一值，续约与释放只对本 owner 生效，
// 避免过期后被接管的租约被原持有方误删。
type Leaser interface {
	// AcquireLease 在租约空闲、已过期或已由 owner 持有时获取成功；否则返回 false。
	AcquireLease(ctx context.Context, sessionID, owner string, ttl time.Duration) (bool, error)
	// RenewLease 延长 owner 持有的租约；租约已被他人接管时返回 false。
	RenewLease(ctx context.Context, sessionID, owner string, ttl time.Duration) (bool, error)
	// ReleaseLease 释放 owner 持有的租约；非 owner 持有时为 no-op。
	ReleaseLease(ctx context.Context, sessionID, owner string) error
}

// MemoryLeaser 是进程内租约实现，仅能保证单进程内互斥。
type MemoryLeaser struct {
	mu     sync.Mutex
	leases map[string]memoryLease
	now    func() time.Time
}

type memoryLease struct {
	owner    string
	expireAt time.Time
}

func NewMemoryLeaser() *MemoryLeaser {
	return &MemoryLeaser{leases: make(map[string]memoryLease), now: time.Now}
}

func (l *MemoryLeaser) AcquireLease(_ context.Context, sessionID, owner string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if cur, ok := l.leases[sessionID]; ok && cur.owner != owner && now.Before(cur.expireAt) {
		return false, nil
	}
	l.leases[sessionID] = memoryLease{owner: owner, expireAt: now.Add(ttl)}
	return true, nil
}

func (l *MemoryLeaser) RenewLease(_ context.Context, sessionID, owner string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.leases[sessionID]
	if !ok || cur.owner != owner {
		return false, nil
	}
	l.leases[sessionID] = memoryLease{owner: owner, expireAt: l.now().Add(ttl)}
	return true, nil
}

func (l *MemoryLeaser) ReleaseLease(_ context.Context, sessionID, owner string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.leases[sessionID]; ok && cur.owner == owner {
		delete(l.leases, sessionID)
	}
	return nil
}
