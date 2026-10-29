package persona

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

const (
	// replyIDRingSize 是「最近发送消息 ID 环」的容量，用于引用寻址判定。
	replyIDRingSize = 128
	// storageTimeout 是单次 Storage 读写的超时。
	storageTimeout = time.Second
)

// Plugin 是 persona 插件的实例状态。
//
// 实例在 init() 时构造（此时无配置、无依赖），运行期依赖只能在 Setup/Start
// 阶段装配。全部可变状态都挂在本结构上，禁止包级可变状态。
type Plugin struct {
	cfg   *config
	api   bot.BotAPI
	log   *slog.Logger
	store bot.Storage
	// imgClient 是图片下载客户端（派生自 PluginContext.HTTPClient 的 transport，
	// 装了地址限制拨号器）；单测可整体替换为桩。
	imgClient *http.Client

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// writeMu 保护 closing/mcpRefreshing 与写队列；写穿透、刷新、任务准入与
	// Stop 用它串行化关闭判定。
	writeMu sync.Mutex
	// closing 为真表示 Stop 已开始，此后不再接受新的写穿透、任务或刷新。
	closing       bool
	mcpRefreshing bool
	// writeCond/writeQueue/writerDone 构成单写协程：所有 Storage.Set 按提交顺序
	// 串行落库，避免同一键的旧快照后完成而覆盖新值。Stop 先关闭准入并等待
	// writerDone，再取消 p.ctx。
	writeCond  *sync.Cond
	writeQueue []writeOp
	writerDone chan struct{}

	mu       sync.RWMutex
	channels map[string]*channelState

	policyMu sync.RWMutex
	policy   policyState

	sem      *semaphore
	replyIDs *idRing

	// 注入缝：单测替换为桩。
	now       func() time.Time
	randFloat func() float64
	completer completer
	toolsMu   sync.RWMutex
	tools     []tool // 当前 MCP 工具目录（mcp.md §13.3）
	mcp       *mcpClient

	statSkips     atomic.Int64
	statLLMErrors atomic.Int64
}

// Setup 读取 PluginContext、校验配置并注册规则。
//
// 配置非法或缺少 network 权限时返回错误，kei 会阻止启动。
func (p *Plugin) Setup(ctx context.Context, reg bot.Registrar) error {
	pc, ok := bot.PluginContextFrom(ctx)
	if !ok {
		return errors.New("persona: 缺少 PluginContext")
	}
	return p.setup(pc, reg)
}

// setup 是 Setup 的可测内核：直接接收已解析的 PluginContext。
func (p *Plugin) setup(pc bot.PluginContext, reg bot.Registrar) error {
	if pc.HTTPClient == nil {
		return errors.New("persona: 需要 network 权限")
	}
	cfg, err := loadConfig(pc.Config)
	if err != nil {
		return err
	}
	log := pc.Logger
	if log == nil {
		log = slog.Default()
	}
	p.cfg = cfg
	p.api = pc.Bot
	p.log = log
	p.store = pc.Storage
	p.imgClient = newVisionClient(pc.HTTPClient)
	p.now = time.Now
	p.randFloat = rand.Float64
	p.channels = make(map[string]*channelState)
	p.policy = policyState{
		groupMode:   cfg.groupPolicy,
		groupList:   cfg.groupList,
		privateMode: cfg.privatePolicy,
		privateList: cfg.privateList,
	}
	p.sem = newSemaphore(cfg.limitsMaxConcurrent)
	p.replyIDs = newIDRing(replyIDRingSize)
	p.completer = newOpenAIClient(cfg, pc.HTTPClient, log)
	p.tools = nil
	if cfg.mcpURL != "" {
		p.mcp = newMCPClient(cfg, pc.HTTPClient)
	}

	reg.OnEvent(bot.EventMessage, p.handleGroupMessage,
		bot.WithKind(bot.MessageGroup), bot.WithPriority(0), bot.WithID("persona:group"))

	reg.OnEvent(bot.EventMessage, p.handlePrivateMessage,
		bot.WithKind(bot.MessagePrivate), bot.WithPriority(0), bot.WithID("persona:private"))

	reg.OnCommand("agent", p.handleCommand,
		bot.WithAdmin(), bot.WithPriority(100), bot.WithID("persona:admin"))
	return nil
}

// Start 保存插件级 ctx、同步加载名单策略并启动后台能力。
//
// kei 传给 Start 的 ctx 是「阶段上下文」：阶段函数返回后立即被 cancel
// （见 internal/pluginmgr/manager.go 的 run）。因此这里用 context.WithoutCancel
// 摘掉它的取消与超时、只保留值，再由 Stop 经 p.cancel 终止全部后台工作；
// 直接用阶段 ctx 会让所有异步链路（懒加载、写穿透、LLM、发送）当场失效。
func (p *Plugin) Start(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(context.WithoutCancel(ctx))
	p.loadPolicy(ctx)
	if p.mcp != nil {
		tools, err := p.loadMCPTools(ctx)
		if err != nil {
			p.log.Warn("persona: MCP 工具目录加载失败，启动中止",
				"operation", "mcp_load", "success", false, "err", err)
			p.cancel()
			cleanupCtx, cancel := context.WithTimeout(ctx, p.cfg.mcpTimeout)
			defer cancel()
			if cleanupErr := p.mcp.close(cleanupCtx); cleanupErr != nil {
				p.log.Warn("persona: MCP 会话清理失败", "err", cleanupErr)
			}
			return err
		}
		p.replaceMCPTools(tools)
	}
	p.writeMu.Lock()
	p.writeCond = sync.NewCond(&p.writeMu)
	p.writerDone = make(chan struct{})
	p.writeMu.Unlock()
	go p.writeLoop()
	return nil
}

// writeOp 是一次写穿透任务；按提交顺序由单写协程执行。
type writeOp struct {
	message string
	fn      func(ctx context.Context) error
	attrs   []any
}

// startTask 在关闭准入锁内登记一个 p.wg 任务，避免 Stop 开始后再 Add。
func (p *Plugin) startTask() bool {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.closing {
		return false
	}
	p.wg.Add(1)
	return true
}

// writeLoop 串行执行写穿透队列，直到 Stop 置 closing 且队列排空。
func (p *Plugin) writeLoop() {
	defer close(p.writerDone)
	for {
		p.writeMu.Lock()
		for len(p.writeQueue) == 0 && !p.closing {
			p.writeCond.Wait()
		}
		if len(p.writeQueue) == 0 {
			p.writeMu.Unlock()
			return
		}
		op := p.writeQueue[0]
		p.writeQueue[0] = writeOp{}
		p.writeQueue = p.writeQueue[1:]
		p.writeMu.Unlock()

		ctx, cancel := context.WithTimeout(p.ctx, storageTimeout)
		err := op.fn(ctx)
		cancel()
		if err != nil {
			p.log.Warn(op.message, append(op.attrs, "err", err)...)
		}
	}
}

// persist 把一次写穿透加入队列：按提交顺序串行执行，单次带 storageTimeout 超时，
// 失败只 warn。Stop 之后拒绝新写入；Stop 先关闭准入并等待 writerDone（队列排空），
// 再取消 p.ctx，避免最后一次写入被当场取消。
func (p *Plugin) persist(op string, fn func(ctx context.Context) error, attrs ...any) {
	p.writeMu.Lock()
	if p.closing || p.writeCond == nil || p.ctx == nil || p.store == nil {
		p.writeMu.Unlock()
		return
	}
	p.writeQueue = append(p.writeQueue, writeOp{
		message: op,
		fn:      fn,
		attrs:   append([]any(nil), attrs...),
	})
	p.writeCond.Signal()
	p.writeMu.Unlock()
}

// Stop 标记关闭、排空写队列，再取消插件级 ctx、停掉全部批处理并等待后台
// 协程退出；幂等。
func (p *Plugin) Stop(ctx context.Context) error {
	// 先拒绝新写入并让单写协程把队列排空，再取消 p.ctx：写穿透的 ctx 派生自
	// p.ctx，先取消会让持久化后端上的最后一次写入被丢弃（见 persist）。每次
	// 写入自带 storageTimeout，故这里的等待有界。
	p.writeMu.Lock()
	p.closing = true
	writerDone := p.writerDone
	if p.writeCond != nil {
		p.writeCond.Broadcast()
	}
	p.writeMu.Unlock()
	if writerDone != nil {
		<-writerDone
	}

	if p.cancel != nil {
		p.cancel()
	}
	p.mu.RLock()
	states := make([]*channelState, 0, len(p.channels))
	for _, st := range p.channels {
		states = append(states, st)
	}
	p.mu.RUnlock()
	for _, st := range states {
		st.stopBatch()
	}
	p.wg.Wait()
	if p.mcp != nil {
		cleanupCtx, cancel := context.WithTimeout(ctx, p.cfg.mcpTimeout)
		defer cancel()
		return p.mcp.close(cleanupCtx)
	}
	return nil
}
