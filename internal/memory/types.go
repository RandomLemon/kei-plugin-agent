package memory

import (
	"errors"
	"time"
)

// 会话类型，取值见 Conv.Type。
const (
	ConvTypeGroup   = "group"
	ConvTypePrivate = "private"
)

// 连库状态，取值见 Stats.State。
const (
	StateConnecting    = "连接中"
	StateReady         = "就绪"
	StateMigrateFailed = "迁移失败"
	StateStopped       = "已停止"
)

// Record 是一条原始消息。
type Record struct {
	Conv     Conv
	EventID  string
	MsgID    string
	At       time.Time // UTC
	Sender   Sender
	Self     bool
	Persona  string // 仅 Self=true 时填写
	Text     string
	ReplyTo  string
	Mentions []string
}

// Conv 描述消息所在会话；conv_key 由 memory 包推导。
type Conv struct {
	Type       string // ConvTypeGroup | ConvTypePrivate
	Platform   string
	BotID      string
	ChannelID  string
	PeerUserID string
	Name       string
}

// Sender 是消息发送者。
type Sender struct {
	ID    string
	Name  string
	IsBot bool
}

// Stats 是 /agent memory status 使用的计数快照。
type Stats struct {
	JobsPending, JobsRunning, JobsDead int64
	State                              string // StateConnecting | StateReady | StateMigrateFailed | StateStopped
	Written, Duplicates, Rejected      int64
	Dropped, FailedBatches             int64
	QueueLen, QueueCap                 int
}

// Scene 是插件根据会话状态构造的可信身份，模型不能修改。
type Scene struct{ Conv Conv }

// ContextRequest 描述近期上下文查询及正文预算。
type ContextRequest struct {
	Scene        Scene
	Queries      []string
	BudgetChars  int
	HistorySince time.Time // 短期历史窗口起点；零值时不跳过
}

// Block 是根包渲染的记忆块，Lines 已完成清洗和预算选择。
type Block struct {
	Title string
	Lines []string
}

// ContextResult 没有可用内容时 Blocks 为空。
type ContextResult struct{ Blocks []Block }

// ErrNotReady 表示数据库从未就绪或服务已停止。
var ErrNotReady = errors.New("memory: 未就绪")

// SummaryQuery 是当前会话的显式摘要查询，日期按摘要日解释。
type SummaryQuery struct {
	Scene    Scene
	DateFrom string
	DateTo   string
	Query    string
	Limit    int // 0 表示默认 8，其余限制在 1–10。
}

// SummaryItem 是显式查询返回的一条话题。
type SummaryItem struct {
	Date      string
	Kind      string
	Title     string
	Summary   string
	OpenItems []string
}

// ErrInvalidDateRange 表示日期格式、顺序或跨度不合法。
var ErrInvalidDateRange = errors.New("memory: 日期范围非法")
