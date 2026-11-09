// Package store 定义记忆子系统的数据库模型、迁移与 gorm 日志适配。
package store

import "time"

// Conversation 是一个会话（群或私聊），conv_key 由 memory 包推导。
type Conversation struct {
	ConvKey    string `gorm:"primaryKey"`
	Type       string `gorm:"not null;default:''"` // group | private
	Platform   string `gorm:"not null;default:''"`
	BotID      string `gorm:"not null;default:''"`
	ChannelID  string `gorm:"not null;default:''"`
	PeerUserID string `gorm:"not null;default:''"`
	Name       string `gorm:"not null;default:''"`
	// ShareEnabled 自 M4 起生效；GORM 插入时跳过零值，写 false 只能用 Update。
	ShareEnabled bool `gorm:"not null;default:true"`
	LastMsgAt    *time.Time
}

// TableName 返回表名。
func (Conversation) TableName() string { return "mem_conversation" }

// Message 是一条原始消息；ID 是切片内的稳定次序。
type Message struct {
	ID          int64     `gorm:"primaryKey"`
	ConvKey     string    `gorm:"not null;default:''"`
	DedupeKey   string    `gorm:"not null;default:''"`
	MsgID       string    `gorm:"not null;default:''"`
	EventID     string    `gorm:"not null;default:''"`
	SenderID    string    `gorm:"not null;default:''"`
	SenderName  string    `gorm:"not null;default:''"`
	SenderIsBot bool      `gorm:"not null"`
	IsSelf      bool      `gorm:"not null"`
	Persona     string    `gorm:"not null;default:''"`
	Text        string    `gorm:"not null;default:''"`
	ReplyTo     string    `gorm:"not null;default:''"`
	Mentions    []string  `gorm:"type:jsonb;serializer:json;not null"`
	SentAt      time.Time `gorm:"not null"`
	LocalDate   time.Time `gorm:"type:date;not null"`
	ReceivedAt  time.Time `gorm:"not null"`
	Recalled    bool      `gorm:"not null"`
}

// TableName 返回表名。
func (Message) TableName() string { return "mem_message" }

// ConvDay 是会话某一本地日的消息计数与各阶段进度。
// 进度列一次建好避免后续改表，在 S4、S6 之前恒为零值。
type ConvDay struct {
	ConvKey          string    `gorm:"primaryKey"`
	LocalDate        time.Time `gorm:"primaryKey;type:date"`
	MsgCount         int       `gorm:"not null"`
	LastMsgID        int64     `gorm:"not null"`
	RolledMsgCount   int       `gorm:"not null"` // S6 使用
	RolledUptoMsgID  int64     `gorm:"not null"` // S6 使用
	RollingRuns      int       `gorm:"not null"` // S6 使用
	SummarizedUptoID *int64    // S4 使用；nil 表示未生成日摘要
}

// TableName 返回表名。
func (ConvDay) TableName() string { return "mem_conv_day" }

// LocalDate 把时刻 t 换算成 loc 下的摘要日，返回该日期的 UTC 零点，供 date 列存取。
// dayStart 是日界距零点的时长：本地当天 dayStart 至次日 dayStart 算同一天，0 即自然日。
func LocalDate(t time.Time, loc *time.Location, dayStart time.Duration) time.Time {
	y, m, d := t.In(loc).Add(-dayStart).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Job 是持久化后台任务，DedupeKey 保证调度幂等。
type Job struct {
	ID        int64 `gorm:"primaryKey"`
	Type      string
	DedupeKey string
	Payload   JobPayload `gorm:"type:jsonb;serializer:json"`
	Status    string
	Attempts  int
	RunAfter  time.Time
	LastError string
	Result    JobResult `gorm:"type:jsonb;serializer:json"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 返回表名。
func (Job) TableName() string { return "mem_job" }

// JobPayload 是任务参数。
type JobPayload struct {
	Run     int    `json:"run,omitempty"` // rolling_summary 对应的固定运行序号
	ConvKey string `json:"conv_key,omitempty"`
	Date    string `json:"date,omitempty"`
}

// JobResult 记录任务执行结果，失败时同样保存；重试时各次尝试累加，Days / FailedDays 为累计天次。
type JobResult struct {
	Days             int              `json:"days,omitempty"`
	FailedDays       int              `json:"failed_days,omitempty"`
	PromptTokens     int64            `json:"prompt_tokens,omitempty"`
	CompletionTokens int64            `json:"completion_tokens,omitempty"`
	Slices           int              `json:"slices,omitempty"`
	DroppedSlices    int              `json:"dropped_slices,omitempty"`
	FailedSlices     int              `json:"failed_slices,omitempty"`
	MergeFallbacks   int              `json:"merge_fallbacks,omitempty"`
	Deleted          map[string]int64 `json:"deleted,omitempty"`
}

// Summary 是一个会话在某天的日摘要；话题随摘要级联删除。
type Summary struct {
	ID                 int64 `gorm:"primaryKey"`
	ConvKey            string
	ScopeType          string
	LocalDate          time.Time `gorm:"type:date"`
	Kind               string
	Version            int
	Overview           string
	FromMsgID, ToMsgID int64
	MsgCount           int
	Model              string
	CreatedAt          time.Time
	Topics             []Topic `gorm:"constraint:OnDelete:CASCADE"`
}

// TableName 返回表名。
func (Summary) TableName() string { return "mem_summary" }

// Topic 是带来源与跨天稳定键的话题。
type Topic struct {
	ID                    int64 `gorm:"primaryKey"`
	SummaryID             int64
	ConvKey               string
	LocalDate             time.Time `gorm:"type:date"`
	TopicKey              string
	ContinuesFrom         string
	Title, Summary        string
	Participants          []string   `gorm:"type:jsonb;serializer:json"`
	KeyPoints             []KeyPoint `gorm:"type:jsonb;serializer:json"`
	Entities              []string   `gorm:"type:jsonb;serializer:json"`
	Importance            float64
	HasOpen               bool
	FirstMsgID, LastMsgID int64
	FirstAt, LastAt       time.Time
}

// TableName 返回表名。
func (Topic) TableName() string { return "mem_topic" }

// KeyPoint 是带说话人与消息引用的要点。
type KeyPoint struct {
	Type    string   `json:"type"`
	Text    string   `json:"text"`
	Speaker string   `json:"speaker"`
	Owner   string   `json:"owner,omitempty"`
	MsgIDs  []string `json:"msg_ids"`
}
