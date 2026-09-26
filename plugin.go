package agent

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

// Plugin 是 agent 插件的实例状态。
//
// 实例在 init() 时构造（此时无配置、无依赖），运行期依赖只能在 Setup/Start
// 阶段装配。全部可变状态都挂在本结构上，禁止包级可变状态。
type Plugin struct {
	cfg   *config
	api   bot.BotAPI
	log   *slog.Logger
	store bot.Storage
	hc    *http.Client

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.RWMutex
	channels map[string]*channelState

	sem      *semaphore
	replyIDs *idRing

	// 注入缝：单测替换为桩。
	now       func() time.Time
	randFloat func() float64
	completer completer

	statSkips     atomic.Int64
	statLLMErrors atomic.Int64
}

// Setup 读取 PluginContext、校验配置并注册规则。
//
// 配置非法或缺少 network 权限时返回错误，kei 会阻止启动。
func (p *Plugin) Setup(ctx context.Context, reg bot.Registrar) error {
	pc, ok := bot.PluginContextFrom(ctx)
	if !ok {
		return errors.New("agent: 缺少 PluginContext")
	}
	return p.setup(pc, reg)
}

// setup 是 Setup 的可测内核：直接接收已解析的 PluginContext。
func (p *Plugin) setup(pc bot.PluginContext, reg bot.Registrar) error {
	if pc.HTTPClient == nil {
		return errors.New("agent: 需要 network 权限")
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
	p.hc = pc.HTTPClient
	p.now = time.Now
	p.randFloat = rand.Float64
	p.channels = make(map[string]*channelState)
	p.sem = newSemaphore(cfg.limitsMaxConcurrent)
	p.replyIDs = newIDRing(replyIDRingSize)
	p.completer = newOpenAIClient(cfg, pc.HTTPClient, log)

	reg.OnEvent(bot.EventMessage, p.handleGroupMessage,
		bot.WithKind(bot.MessageGroup), bot.WithPriority(0), bot.WithID("agent:group"))

	reg.OnCommand("agent", p.handleCommand,
		bot.WithAdmin(), bot.WithPriority(100), bot.WithID("agent:admin"))
	return nil
}

// Start 保存插件级 ctx 并启动后台能力。
func (p *Plugin) Start(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)
	return nil
}

// Stop 取消插件级 ctx、停止全部定时器并等待后台协程退出；幂等。
func (p *Plugin) Stop(ctx context.Context) error {
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
		st.stopTimer()
	}
	p.wg.Wait()
	return nil
}
