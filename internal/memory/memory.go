// Package memory 是插件的记忆子系统：连接 PostgreSQL，采集并存储原始消息。
package memory

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/jobs"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

const (
	ingestWriteTimeout = 5 * time.Second
	flushTimeout       = 2 * time.Second
	pingTimeout        = 5 * time.Second
	migrateTimeout     = 30 * time.Second
	connectBackoffMin  = time.Second
	connectBackoffMax  = 60 * time.Second
	// maxFutureSkew 是消息时间允许超前写入时刻的上限，超过时改用写入时刻。
	maxFutureSkew = 5 * time.Minute
)

// Config 是记忆子系统的配置，由根包从 memory_* 键组装并校验（数值均为正数）。
type Config struct {
	ContextTimeout                                      time.Duration
	ContextMaxChars                                     int
	LLMBaseURL, LLMAPIKey, LLMModelSmall, LLMModelLarge string
	LLMHeaders                                          map[string]string
	LLMTimeout                                          time.Duration
	LLMJSONMode                                         bool
	DailyAt                                             string
	MessageRetentionDays                                int
	SummaryRetentionDays                                int
	DatabaseDSN                                         string
	MaxOpenConns                                        int
	Location                                            *time.Location // 取自 random_timezone
	QueueSize                                           int
	IngestBatch                                         int
	IngestInterval                                      time.Duration
}

// Service 是记忆子系统的入口：后台连库、迁移，并批量写入入队的原始消息。
type Service struct {
	cfg        Config
	log        *slog.Logger
	queue      chan Record
	httpClient *http.Client

	// 注入缝：New 填默认值，同包测试可改小。
	tickInterval       time.Duration
	workerStopTimeout  time.Duration
	workerIdleWait     time.Duration
	now                func() time.Time
	dayStart           time.Duration // 摘要日日界，由 DailyAt 解析
	ingestWriteTimeout time.Duration
	flushTimeout       time.Duration
	pingTimeout        time.Duration
	migrateTimeout     time.Duration
	connectBackoffMin  time.Duration
	connectBackoffMax  time.Duration
	retryBackoff       []time.Duration // 批量写入失败后的重试退避，长度即重试次数

	// mu 保护以下字段；Enqueue 持读锁，保证 Stop 关闭准入后不再有记录入队。
	db      *gorm.DB // mu 保护；就绪后只读，Stop 后不再准入读取。
	mu      sync.RWMutex
	state   string
	started bool
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{} // 后台协程退出时关闭

	jobsPending   atomic.Int64
	jobsRunning   atomic.Int64
	jobsDead      atomic.Int64
	written       atomic.Int64
	duplicates    atomic.Int64
	rejected      atomic.Int64
	dropped       atomic.Int64
	failedBatches atomic.Int64
}

// New 创建 Service；不连库，连库在 Start 后进行。
func New(cfg Config, log *slog.Logger, httpClient *http.Client) *Service {
	if cfg.DailyAt == "" {
		cfg.DailyAt = "04:00"
	}
	if cfg.MessageRetentionDays == 0 {
		cfg.MessageRetentionDays = 60
	}
	if cfg.SummaryRetentionDays == 0 {
		cfg.SummaryRetentionDays = 90
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		tickInterval: tickInterval, workerStopTimeout: workerStopTimeout,
		cfg:                cfg,
		log:                log,
		queue:              make(chan Record, cfg.QueueSize),
		httpClient:         httpClient,
		now:                time.Now,
		dayStart:           parseDayStart(cfg.DailyAt),
		ingestWriteTimeout: ingestWriteTimeout,
		flushTimeout:       flushTimeout,
		pingTimeout:        pingTimeout,
		migrateTimeout:     migrateTimeout,
		connectBackoffMin:  connectBackoffMin,
		connectBackoffMax:  connectBackoffMax,
		retryBackoff:       []time.Duration{time.Second, 2 * time.Second},
		state:              StateConnecting,
	}
}

// Start 启动后台协程（连库 → 迁移 → 写入）并立即返回。ctx 须为插件级 ctx。
// 重复调用或 Stop 之后调用不生效。
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.run(runCtx)
}

// Stop 关闭准入，等待后台协程在 flushTimeout 内写完队列中的记录并关闭连接池。幂等。
// ctx 到期时不再等待。
func (s *Service) Stop(ctx context.Context) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	wasReady := s.state == StateReady
	s.state = StateStopped
	cancel, done := s.cancel, s.done
	s.mu.Unlock()

	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-ctx.Done():
			s.log.Warn("persona memory: 等待后台协程退出超时", "operation", "memory_stop", "err", ctx.Err())
			return
		}
	}
	// 就绪过的写循环已在 flush 中处理队列；从未就绪时队列里的记录只能丢弃。
	if n := len(s.queue); !wasReady && n > 0 {
		s.log.Warn("persona memory: 数据库未就绪，丢弃队列中的记录", "operation", "memory_stop", "dropped", n)
	}
}

// Stats 返回当前计数快照。
func (s *Service) Stats() Stats {
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()
	return Stats{
		JobsPending: s.jobsPending.Load(), JobsRunning: s.jobsRunning.Load(), JobsDead: s.jobsDead.Load(),
		State:         state,
		Written:       s.written.Load(),
		Duplicates:    s.duplicates.Load(),
		Rejected:      s.rejected.Load(),
		Dropped:       s.dropped.Load(),
		FailedBatches: s.failedBatches.Load(),
		QueueLen:      len(s.queue),
		QueueCap:      cap(s.queue),
	}
}

// run 是唯一的后台协程：连库、迁移成功后进入写循环；ctx 取消（Stop）时写完剩余记录并关闭连接池。
func (s *Service) run(ctx context.Context) {
	defer close(s.done)

	db, err := s.connect(ctx)
	if err != nil {
		return
	}
	defer s.closeDB(db)

	if !s.migrate(ctx, db) {
		return
	}
	for {
		opCtx, cancel := context.WithTimeout(ctx, ingestWriteTimeout)
		err := (jobs.Queue{DB: db}).Reset(opCtx, s.now().UTC())
		cancel()
		if err == nil {
			break
		}
		if ctx.Err() == nil {
			s.log.Warn("persona memory: 恢复中断任务失败", "operation", "memory_job_reset", "err", err)
		}
		if !sleepCtx(ctx, s.connectBackoffMin) {
			return
		}
	}
	stopJobs := s.startJobs(ctx, db)
	defer stopJobs()
	s.mu.Lock()
	s.db = db
	s.mu.Unlock()
	s.setState(StateReady)
	s.writeLoop(ctx, db)
}

// connect 打开连接池并 Ping 直到成功；失败按指数退避重试，ctx 取消时返回错误。
func (s *Service) connect(ctx context.Context) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(s.cfg.DatabaseDSN), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               store.NewLogger(s.log),
	})
	if err != nil {
		// 解析错误会带上连接串，这里不输出 err，避免泄露 DSN。
		s.log.Error("persona memory: DSN 无法解析，记忆子系统不可用", "operation", "memory_connect")
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	if s.cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(s.cfg.MaxOpenConns)
	}

	backoff := s.connectBackoffMin
	for {
		pingCtx, cancel := context.WithTimeout(ctx, s.pingTimeout)
		err := sqlDB.PingContext(pingCtx)
		cancel()
		if err == nil {
			return db, nil
		}
		if ctx.Err() != nil {
			break
		}
		s.log.Warn("persona memory: 连接数据库失败", "operation", "memory_connect", "err", err)
		if !sleepCtx(ctx, backoff) {
			break
		}
		backoff = min(backoff*2, s.connectBackoffMax)
	}
	_ = sqlDB.Close()
	return nil, ctx.Err()
}

// migrate 执行迁移；失败时置为「迁移失败」且不再重试。ctx 取消导致的失败不改状态。
func (s *Service) migrate(ctx context.Context, db *gorm.DB) bool {
	mctx, cancel := context.WithTimeout(ctx, s.migrateTimeout)
	defer cancel()
	if err := store.Migrate(mctx, db); err != nil {
		if ctx.Err() == nil {
			s.log.Error("persona memory: 迁移失败", "operation", "memory_migrate", "err", err)
			s.setState(StateMigrateFailed)
		}
		return false
	}
	return true
}

// setState 更新连库状态；已停止后保持「已停止」。
func (s *Service) setState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.state = state
	}
}

// parseDayStart 把 HH:MM 换算成距零点的时长；格式已由根包配置校验。
func parseDayStart(dailyAt string) time.Duration {
	t, _ := time.Parse("15:04", dailyAt)
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
}

func (s *Service) closeDB(db *gorm.DB) {
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// sleepCtx 等待 d；ctx 先取消时返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
