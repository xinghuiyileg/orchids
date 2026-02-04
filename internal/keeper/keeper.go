package keeper

import (
	"fmt"
	"log"
	"sync"
	"time"

	"orchids-api/internal/clerk"
	"orchids-api/internal/store"
)

// AccountStatus 账号状态信息
type AccountStatus struct {
	AccountID    int64     `json:"account_id"`
	AccountName  string    `json:"account_name"`
	Email        string    `json:"email"`
	LastRefresh  time.Time `json:"last_refresh"`
	NextRefresh  time.Time `json:"next_refresh"`
	LastError    string    `json:"last_error,omitempty"`
	RefreshCount int       `json:"refresh_count"`
	IsHealthy    bool      `json:"is_healthy"`
}

// AccountKeeper 账号保活管理器
type AccountKeeper struct {
	store           *store.Store
	refreshInterval time.Duration
	stopCh          chan struct{}
	wg              sync.WaitGroup

	mu           sync.RWMutex
	lastRefresh  map[int64]time.Time
	lastError    map[int64]string
	refreshCount map[int64]int
}

const (
	DefaultRefreshInterval = 30 * time.Minute
	StartupConcurrency     = 5
	RefreshTimeout         = 30 * time.Second
)

// New 创建账号保活管理器
func New(s *store.Store) *AccountKeeper {
	return &AccountKeeper{
		store:           s,
		refreshInterval: DefaultRefreshInterval,
		stopCh:          make(chan struct{}),
		lastRefresh:     make(map[int64]time.Time),
		lastError:       make(map[int64]string),
		refreshCount:    make(map[int64]int),
	}
}

// Start 启动保活任务
func (k *AccountKeeper) Start() {
	log.Println("[AccountKeeper] 启动账号保活服务")

	// 启动时预热所有账号
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		k.WarmUp()
	}()

	// 启动定时刷新任务
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		k.runRefreshLoop()
	}()
}

// Stop 停止保活任务
func (k *AccountKeeper) Stop() {
	log.Println("[AccountKeeper] 停止账号保活服务")
	close(k.stopCh)
	k.wg.Wait()
	log.Println("[AccountKeeper] 账号保活服务已停止")
}

// WarmUp 预热所有启用账号
func (k *AccountKeeper) WarmUp() {
	log.Println("[AccountKeeper] 开始预热账号...")

	accounts, err := k.store.GetEnabledAccounts()
	if err != nil {
		log.Printf("[AccountKeeper] 获取账号失败: %v", err)
		return
	}

	if len(accounts) == 0 {
		log.Println("[AccountKeeper] 没有启用的账号")
		return
	}

	log.Printf("[AccountKeeper] 准备预热 %d 个账号", len(accounts))

	// 使用信号量控制并发
	sem := make(chan struct{}, StartupConcurrency)
	var wg sync.WaitGroup

	startTime := time.Now()
	successCount := 0
	failCount := 0
	var countMu sync.Mutex

	for _, acc := range accounts {
		wg.Add(1)
		go func(account *store.Account) {
			defer wg.Done()
			sem <- struct{}{}        // 获取信号量
			defer func() { <-sem }() // 释放信号量

			if err := k.refreshAccount(account); err != nil {
				countMu.Lock()
				failCount++
				countMu.Unlock()
				log.Printf("[AccountKeeper] 预热失败 %s (%s): %v", account.Name, account.Email, err)
			} else {
				countMu.Lock()
				successCount++
				countMu.Unlock()
				log.Printf("[AccountKeeper] 预热成功 %s (%s)", account.Name, account.Email)
			}
		}(acc)
	}

	wg.Wait()
	log.Printf("[AccountKeeper] 预热完成: 成功=%d, 失败=%d, 耗时=%v",
		successCount, failCount, time.Since(startTime))
}

// runRefreshLoop 运行定时刷新循环
func (k *AccountKeeper) runRefreshLoop() {
	ticker := time.NewTicker(k.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-k.stopCh:
			return
		case <-ticker.C:
			k.refreshAllAccounts()
		}
	}
}

// refreshAllAccounts 刷新所有启用账号
func (k *AccountKeeper) refreshAllAccounts() {
	accounts, err := k.store.GetEnabledAccounts()
	if err != nil {
		log.Printf("[AccountKeeper] 获取账号失败: %v", err)
		return
	}

	log.Printf("[AccountKeeper] 开始定时刷新 %d 个账号", len(accounts))

	successCount := 0
	failCount := 0

	for _, acc := range accounts {
		select {
		case <-k.stopCh:
			return
		default:
		}

		if err := k.refreshAccount(acc); err != nil {
			failCount++
			// 刷新失败只记录日志，不禁用账号
		} else {
			successCount++
		}

		// 每个刷新之间间隔一小段时间，避免请求过快
		time.Sleep(100 * time.Millisecond)
	}

	log.Printf("[AccountKeeper] 定时刷新完成: 成功=%d, 失败=%d", successCount, failCount)
}

// refreshAccount 刷新单个账号的 Session
func (k *AccountKeeper) refreshAccount(acc *store.Account) error {
	info, err := clerk.FetchAccountInfo(acc.ClientCookie)
	if err != nil {
		k.mu.Lock()
		k.lastError[acc.ID] = err.Error()
		k.mu.Unlock()
		return fmt.Errorf("刷新账号 %s 失败: %w", acc.Name, err)
	}

	k.mu.Lock()
	k.lastRefresh[acc.ID] = time.Now()
	k.lastError[acc.ID] = ""
	k.refreshCount[acc.ID]++
	k.mu.Unlock()

	_ = info
	return nil
}

// RefreshAccountByID 手动刷新指定账号
func (k *AccountKeeper) RefreshAccountByID(id int64) error {
	acc, err := k.store.GetAccount(id)
	if err != nil {
		return fmt.Errorf("获取账号失败: %w", err)
	}
	return k.refreshAccount(acc)
}

// GetStatus 获取所有账号的保活状态
func (k *AccountKeeper) GetStatus() []AccountStatus {
	accounts, err := k.store.GetEnabledAccounts()
	if err != nil {
		return nil
	}

	k.mu.RLock()
	defer k.mu.RUnlock()

	statuses := make([]AccountStatus, 0, len(accounts))
	now := time.Now()

	for _, acc := range accounts {
		lastRefresh := k.lastRefresh[acc.ID]
		lastError := k.lastError[acc.ID]
		refreshCount := k.refreshCount[acc.ID]

		nextRefresh := lastRefresh.Add(k.refreshInterval)
		if lastRefresh.IsZero() {
			nextRefresh = now
		}

		statuses = append(statuses, AccountStatus{
			AccountID:    acc.ID,
			AccountName:  acc.Name,
			Email:        acc.Email,
			LastRefresh:  lastRefresh,
			NextRefresh:  nextRefresh,
			LastError:    lastError,
			RefreshCount: refreshCount,
			IsHealthy:    lastError == "" && !lastRefresh.IsZero(),
		})
	}

	return statuses
}

// GetHealthyCount 获取健康账号数量
func (k *AccountKeeper) GetHealthyCount() (healthy, total int) {
	statuses := k.GetStatus()
	total = len(statuses)
	for _, s := range statuses {
		if s.IsHealthy {
			healthy++
		}
	}
	return
}

// RefreshAll 立即刷新所有账号（用于手动触发）
func (k *AccountKeeper) RefreshAll() {
	log.Println("[AccountKeeper] 手动触发刷新所有账号")
	k.refreshAllAccounts()
}

// MarkAccountActive 标记账号为活跃状态（请求成功时调用）
func (k *AccountKeeper) MarkAccountActive(accountID int64) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.lastRefresh[accountID].IsZero() {
		k.lastRefresh[accountID] = time.Now()
	}
	k.lastError[accountID] = ""
}

// CleanupDeletedAccounts 清理已删除账号的状态数据
func (k *AccountKeeper) CleanupDeletedAccounts(activeIDs map[int64]bool) {
	k.mu.Lock()
	defer k.mu.Unlock()

	for id := range k.lastRefresh {
		if !activeIDs[id] {
			delete(k.lastRefresh, id)
			delete(k.lastError, id)
			delete(k.refreshCount, id)
		}
	}
}
