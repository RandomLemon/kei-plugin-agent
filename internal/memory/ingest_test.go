package memory

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/memtest"
	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

const testWait = 5 * time.Second

func shanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func testConfig(t *testing.T, dsn string) Config {
	return Config{
		DatabaseDSN:    dsn,
		MaxOpenConns:   4,
		Location:       shanghai(t),
		QueueSize:      100,
		IngestBatch:    10,
		IngestInterval: 50 * time.Millisecond,
	}
}

// startService 启动 Service 并等待就绪，测试结束时停止。tweak 在 Start 之前调整配置与注入缝。
func startService(t *testing.T, cfg Config, tweak func(*Service)) *Service {
	t.Helper()
	s := New(cfg, discardLogger(), nil)
	if tweak != nil {
		tweak(s)
	}
	s.Start(t.Context())
	t.Cleanup(func() { s.Stop(context.WithoutCancel(t.Context())) })
	waitFor(t, func() bool { return s.Stats().State == StateReady }, "Service 就绪")
	return s
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stopNow(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testWait)
	defer cancel()
	s.Stop(ctx)
}

func groupRecord(msgID string) Record {
	return Record{
		Conv:   Conv{Type: ConvTypeGroup, Platform: "qq", BotID: "b1", ChannelID: "g1"},
		MsgID:  msgID,
		At:     time.Now().UTC(),
		Sender: Sender{ID: "u1", Name: "Alice"},
		Text:   "hello " + msgID,
	}
}

func countMessages(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&store.Message{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// lockMessages 在另一事务里锁住 mem_message 阻塞写入，返回释放函数（测试结束时也会释放）。
func lockMessages(t *testing.T, db *gorm.DB) (release func()) {
	t.Helper()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := tx.Exec(`LOCK TABLE mem_message IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { tx.Rollback() }) }
	t.Cleanup(release)
	return release
}

func TestIngestDedupe(t *testing.T) {
	dsn := memtest.NewDB(t)
	s := startService(t, testConfig(t, dsn), nil)
	db := memtest.Open(t, dsn)

	eventOnly := groupRecord("")
	eventOnly.EventID = "e1"
	badConv := groupRecord("m-bad")
	badConv.Conv.ChannelID = ""
	noKeys := groupRecord("")
	for _, r := range []Record{groupRecord("m1"), eventOnly, badConv, noKeys, groupRecord("m3")} {
		s.Enqueue(r)
	}
	waitFor(t, func() bool { st := s.Stats(); return st.Written == 3 && st.Rejected == 2 }, "第一批写入")

	// 第二批：同一 MsgID（含同群另一机器人收到的同一条消息）与同一 EventID 都按重复处理。
	otherBot := groupRecord("m1")
	otherBot.Conv.BotID = "b2"
	for _, r := range []Record{groupRecord("m1"), otherBot, eventOnly} {
		s.Enqueue(r)
	}
	waitFor(t, func() bool { return s.Stats().Duplicates == 3 }, "重复计数")

	st := s.Stats()
	if st.Written != 3 || st.Rejected != 2 {
		t.Fatalf("Stats = %+v", st)
	}
	var keys []string
	if err := db.Model(&store.Message{}).Order("dedupe_key").Pluck("dedupe_key", &keys).Error; err != nil {
		t.Fatal(err)
	}
	if want := []string{"e:e1", "m:m1", "m:m3"}; !slices.Equal(keys, want) {
		t.Fatalf("dedupe_key = %v, want %v", keys, want)
	}
	var day store.ConvDay
	if err := db.First(&day, "conv_key = ?", "group:qq:g1").Error; err != nil {
		t.Fatal(err)
	}
	if day.MsgCount != 3 {
		t.Fatalf("重复消息不应累加 msg_count，实际 %d", day.MsgCount)
	}
}

func TestIngestPrivateConvAndCleanText(t *testing.T) {
	dsn := memtest.NewDB(t)
	s := startService(t, testConfig(t, dsn), nil)
	db := memtest.Open(t, dsn)

	mentions := []string{"u2"}
	s.Enqueue(Record{
		Conv:     Conv{Type: ConvTypePrivate, Platform: "qq", BotID: "b1", PeerUserID: "u1", ChannelID: "c1"},
		MsgID:    "m1",
		At:       time.Now().UTC(),
		Sender:   Sender{ID: "u1", Name: "Al\x00ice"},
		Text:     "a\x00b\xff",
		Mentions: mentions,
	})
	mentions[0] = "changed" // 入队后修改原切片不影响写入
	waitFor(t, func() bool { return s.Stats().Written == 1 }, "私聊写入")

	var c store.Conversation
	if err := db.First(&c, "conv_key = ?", "private:qq:b1:u1").Error; err != nil {
		t.Fatal(err)
	}
	if c.Type != ConvTypePrivate || c.BotID != "b1" || c.PeerUserID != "u1" || c.ChannelID != "" {
		t.Fatalf("conversation = %+v", c)
	}
	var m store.Message
	if err := db.First(&m, "conv_key = ?", "private:qq:b1:u1").Error; err != nil {
		t.Fatal(err)
	}
	if m.Text != "ab\uFFFD" || m.SenderName != "Alice" || !slices.Equal(m.Mentions, []string{"u2"}) {
		t.Fatalf("message text=%q sender_name=%q mentions=%v", m.Text, m.SenderName, m.Mentions)
	}
}

func TestIngestConversationAndDay(t *testing.T) {
	dsn := memtest.NewDB(t)
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) // 上海 11-04 20:00
	s := startService(t, testConfig(t, dsn), func(s *Service) {
		s.now = func() time.Time { return base }
	})
	db := memtest.Open(t, dsn)

	rec := func(msgID string, at time.Time, name string) Record {
		r := groupRecord(msgID)
		r.At = at
		r.Conv.Name = name
		return r
	}
	conv := func() store.Conversation {
		var c store.Conversation
		if err := db.First(&c, "conv_key = ?", "group:qq:g1").Error; err != nil {
			t.Fatal(err)
		}
		return c
	}

	s.Enqueue(rec("m1", base.Add(-2*time.Hour), "群A"))
	waitFor(t, func() bool { return s.Stats().Written == 1 }, "m1")

	// 不带名字的记录不清空群名；更早的消息不回退 last_msg_at。
	s.Enqueue(rec("m2", base.Add(-time.Hour), ""))
	s.Enqueue(rec("m3", base.Add(-3*time.Hour), ""))
	waitFor(t, func() bool { return s.Stats().Written == 3 }, "m2、m3")
	c := conv()
	if c.Name != "群A" || c.BotID != "" || c.ChannelID != "g1" || !c.ShareEnabled {
		t.Fatalf("conversation = %+v", c)
	}
	if c.LastMsgAt == nil || !c.LastMsgAt.Equal(base.Add(-time.Hour)) {
		t.Fatalf("last_msg_at = %v, want %v", c.LastMsgAt, base.Add(-time.Hour))
	}

	// 超前 4 分钟保留；超前 10 分钟钳制为写入时刻；上海 11-04 03:00 未过 04:00 日界，归入 11-03。
	s.Enqueue(rec("m4", base.Add(4*time.Minute), "群B"))
	s.Enqueue(rec("m5", base.Add(10*time.Minute), ""))
	s.Enqueue(rec("m6", time.Date(2026, 11, 3, 19, 0, 0, 0, time.UTC), ""))
	waitFor(t, func() bool { return s.Stats().Written == 6 }, "m4–m6")

	sentAt := func(msgID string) time.Time {
		var m store.Message
		if err := db.First(&m, "msg_id = ?", msgID).Error; err != nil {
			t.Fatal(err)
		}
		return m.SentAt
	}
	if got := sentAt("m4"); !got.Equal(base.Add(4 * time.Minute)) {
		t.Fatalf("m4 sent_at = %v", got)
	}
	if got := sentAt("m5"); !got.Equal(base) {
		t.Fatalf("m5 sent_at = %v, want 钳制为 %v", got, base)
	}
	c = conv()
	if c.Name != "群B" || !c.LastMsgAt.Equal(base.Add(4*time.Minute)) {
		t.Fatalf("conversation = %+v, last_msg_at = %v", c, c.LastMsgAt)
	}

	var days []store.ConvDay
	if err := db.Order("local_date").Find(&days, "conv_key = ?", "group:qq:g1").Error; err != nil {
		t.Fatal(err)
	}
	wantDays := []struct {
		date  time.Time
		count int
	}{
		{time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), 1},
		{time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), 5},
	}
	if len(days) != len(wantDays) {
		t.Fatalf("conv_day = %+v", days)
	}
	for i, want := range wantDays {
		d := days[i]
		var maxID int64
		if err := db.Model(&store.Message{}).Where("local_date = ?", want.date).
			Select("max(id)").Scan(&maxID).Error; err != nil {
			t.Fatal(err)
		}
		if !d.LocalDate.Equal(want.date) || d.MsgCount != want.count || d.LastMsgID != maxID || d.SummarizedUptoID != nil {
			t.Fatalf("conv_day[%d] = %+v, want date=%v count=%d last_msg_id=%d", i, d, want.date, want.count, maxID)
		}
	}
}

func TestIngestFullBatchWritesImmediately(t *testing.T) {
	dsn := memtest.NewDB(t)
	cfg := testConfig(t, dsn)
	cfg.IngestBatch = 3
	cfg.IngestInterval = time.Hour
	s := startService(t, cfg, nil)

	for i := range 3 {
		s.Enqueue(groupRecord(fmt.Sprintf("m%d", i)))
	}
	waitFor(t, func() bool { return s.Stats().Written == 3 }, "满批写入")

	s.Enqueue(groupRecord("m-partial"))
	time.Sleep(200 * time.Millisecond)
	if got := s.Stats().Written; got != 3 {
		t.Fatalf("未满批且 interval 未到时 Written = %d, want 3", got)
	}
}

func TestIngestPartialBatchAfterInterval(t *testing.T) {
	dsn := memtest.NewDB(t)
	cfg := testConfig(t, dsn)
	cfg.IngestBatch = 100
	s := startService(t, cfg, nil)

	s.Enqueue(groupRecord("m1"))
	waitFor(t, func() bool { return s.Stats().Written == 1 }, "interval 到期写入")
}

func TestEnqueueQueueFull(t *testing.T) {
	s := New(Config{QueueSize: 2, IngestBatch: 10, IngestInterval: time.Second}, discardLogger(), nil)
	for i := range 2 {
		if !s.Enqueue(groupRecord(fmt.Sprintf("m%d", i))) {
			t.Fatalf("第 %d 条应入队成功", i+1)
		}
	}
	begin := time.Now()
	if s.Enqueue(groupRecord("m-full")) {
		t.Fatal("队列满时 Enqueue 应返回 false")
	}
	if elapsed := time.Since(begin); elapsed > 50*time.Millisecond {
		t.Fatalf("Enqueue 阻塞了 %v", elapsed)
	}
	if st := s.Stats(); st.Dropped != 1 || st.QueueLen != 2 || st.QueueCap != 2 {
		t.Fatalf("Stats = %+v", st)
	}
}

func TestIngestRetryThenDrop(t *testing.T) {
	dsn := memtest.NewDB(t)
	cfg := testConfig(t, dsn)
	cfg.IngestBatch = 1
	writeTimeout := 200 * time.Millisecond
	s := startService(t, cfg, func(s *Service) {
		s.ingestWriteTimeout = writeTimeout
		s.retryBackoff = []time.Duration{20 * time.Millisecond, 40 * time.Millisecond}
	})
	db := memtest.Open(t, dsn)
	release := lockMessages(t, db)

	begin := time.Now()
	s.Enqueue(groupRecord("m1"))
	waitFor(t, func() bool { return s.Stats().FailedBatches == 1 }, "重试后丢弃")
	// 首次写入加 2 次重试，每次都等满写入超时。
	if elapsed := time.Since(begin); elapsed < 3*writeTimeout {
		t.Fatalf("耗时 %v，少于 3 次写入超时", elapsed)
	}
	release()

	time.Sleep(100 * time.Millisecond)
	if st := s.Stats(); st.Written != 0 || st.FailedBatches != 1 {
		t.Fatalf("Stats = %+v", st)
	}
	if n := countMessages(t, db); n != 0 {
		t.Fatalf("丢弃的批次不应落库，实际 %d 条", n)
	}
}

func TestStopFlushesQueue(t *testing.T) {
	dsn := memtest.NewDB(t)
	cfg := testConfig(t, dsn)
	cfg.IngestBatch = 2
	cfg.IngestInterval = time.Hour
	s := startService(t, cfg, nil)
	db := memtest.Open(t, dsn)

	// 5 条：前 4 条凑满两批立即写，最后 1 条留在批里由 Stop 写入。
	for i := range 5 {
		s.Enqueue(groupRecord(fmt.Sprintf("m%d", i)))
	}
	stopNow(t, s)

	if got := s.Stats().Written; got != 5 {
		t.Fatalf("Written = %d, want 5", got)
	}
	if n := countMessages(t, db); n != 5 {
		t.Fatalf("落库 %d 条, want 5", n)
	}
}

func TestStopReturnsAfterFlushTimeoutWhenLocked(t *testing.T) {
	dsn := memtest.NewDB(t)
	cfg := testConfig(t, dsn)
	cfg.IngestBatch = 1 // 第一条立即开始写并被锁阻塞，其余留在队列
	flushTimeout := 300 * time.Millisecond
	s := startService(t, cfg, func(s *Service) { s.flushTimeout = flushTimeout })
	db := memtest.Open(t, dsn)
	lockMessages(t, db)

	for i := range 3 {
		s.Enqueue(groupRecord(fmt.Sprintf("m%d", i)))
	}
	time.Sleep(100 * time.Millisecond) // 让写协程进入被阻塞的写入

	begin := time.Now()
	stopNow(t, s)
	elapsed := time.Since(begin)
	if elapsed < flushTimeout || elapsed > flushTimeout+700*time.Millisecond {
		t.Fatalf("Stop 耗时 %v，应在 flushTimeout(%v) 后不久返回", elapsed, flushTimeout)
	}
	select {
	case <-s.done:
	default:
		t.Fatal("Stop 返回后后台协程仍未退出")
	}
	if got := s.Stats().Written; got != 0 {
		t.Fatalf("Written = %d, want 0", got)
	}
}

func TestConcurrentEnqueueAndStop(t *testing.T) {
	dsn := memtest.NewDB(t)
	cfg := testConfig(t, dsn)
	cfg.QueueSize = 1000
	// 放宽 flush 上界：-race 下写完约 800 条，慢环境里默认 2s 可能不够。
	s := startService(t, cfg, func(s *Service) { s.flushTimeout = 10 * time.Second })
	db := memtest.Open(t, dsn)

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 100 {
				if s.Enqueue(groupRecord(fmt.Sprintf("m%d-%d", g, i))) {
					accepted.Add(1)
				}
			}
		})
	}
	wg.Go(func() {
		time.Sleep(5 * time.Millisecond)
		stopNow(t, s)
	})
	wg.Wait()
	stopNow(t, s)

	st := s.Stats()
	if st.Written != accepted.Load() || st.Dropped != 0 {
		t.Fatalf("Stats = %+v, accepted = %d", st, accepted.Load())
	}
	if n := countMessages(t, db); n != st.Written {
		t.Fatalf("落库 %d 条, Written = %d", n, st.Written)
	}
}
