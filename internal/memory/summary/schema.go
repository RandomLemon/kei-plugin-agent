package summary

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

// SliceOutput 是小模型的结构化输出。
type SliceOutput struct {
	Topics []SliceTopic `json:"topics"`
}

// SliceTopic 是规范化前的话题。
type SliceTopic struct {
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	Participants []string `json:"participants"`
	KeyPoints    []Point  `json:"key_points"`
	Entities     []string `json:"entities"`
	Importance   float64  `json:"importance"`
}

// Point 使用任务内代号引用说话人与消息。
type Point struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Speaker string `json:"speaker"`
	Owner   string `json:"owner"`
	Refs    []int  `json:"refs"`
}

// Validate 只校验顶层 topics；其余在来源校验时规范化。
func (o *SliceOutput) Validate() error {
	if o.Topics == nil {
		return fmt.Errorf("缺少 topics 数组")
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// 预编译的正则只读，可安全并发使用。
var (
	sensitivePatterns = []*regexp.Regexp{
		regexp.MustCompile(`\b1[3-9]\d{9}\b`),
		regexp.MustCompile(`\b\d{17}[\dXx]\b`),
		regexp.MustCompile(`[\w.+-]+@[\w-]+(\.[\w-]+)+`),
	}
	aliasPattern = regexp.MustCompile(`\b(u[0-9]+|bot)\b`)
)

// Redact 替换明确可识别的手机号、证件号与邮箱；须在截断前调用，避免截断后的残段漏匹配。
func Redact(s string) string {
	for _, pattern := range sensitivePatterns {
		s = pattern.ReplaceAllString(s, "[已隐去]")
	}
	return s
}

func importance(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return min(1, max(0, v))
}

// ShortSummary 返回第一句话，最多四十字，合并输入和读取共用。
func ShortSummary(text string) string {
	for i, r := range text {
		if strings.ContainsRune("。！？!?；", r) {
			return truncate(text[:i], 40)
		}
	}
	return truncate(text, 40)
}

func (i Input) clean(text string, limit int) string {
	text = aliasPattern.ReplaceAllStringFunc(text, func(alias string) string {
		if name, ok := i.Names[alias]; ok {
			return name
		}
		return alias
	})
	return truncate(Redact(text), limit)
}

// Normalize 校验引用与归属，并回填平台 ID；范围取整个原始切片。
func (i Input) Normalize(output SliceOutput, raw, filtered []store.Message) []store.Topic {
	refs := map[int]store.Message{}
	for _, m := range filtered {
		refs[i.Numbers[m.ID]] = m
	}
	var topics []store.Topic
	for _, candidate := range output.Topics[:min(len(output.Topics), 6)] {
		topic := store.Topic{Title: i.clean(candidate.Title, 20), Summary: i.clean(candidate.Summary, 120), Importance: importance(candidate.Importance), Entities: slices.Clone(candidate.Entities)}
		for _, alias := range candidate.Participants {
			if id, ok := i.Users[alias]; ok && alias != "bot" {
				topic.Participants = appendUnique(topic.Participants, id)
			}
		}
		for _, p := range candidate.KeyPoints {
			switch p.Type {
			case "decision", "todo", "open_question", "info":
			default:
				continue
			}
			point := store.KeyPoint{Type: p.Type, Text: i.clean(p.Text, 60)}
			for _, ref := range p.Refs {
				m, ok := refs[ref]
				if !ok {
					continue
				}
				id := m.MsgID
				if id == "" {
					id = "mem:" + strconv.FormatInt(m.ID, 10)
				}
				point.MsgIDs = appendUnique(point.MsgIDs, id)
				if i.Aliases[m.SenderID] == p.Speaker {
					point.Speaker = m.SenderID
				}
			}
			if point.Speaker == "" || len(point.MsgIDs) == 0 {
				continue
			}
			point.Owner = i.Users[p.Owner]
			topic.KeyPoints = append(topic.KeyPoints, point)
			topic.HasOpen = topic.HasOpen || p.Type == "todo" || p.Type == "open_question"
		}
		if len(topic.KeyPoints) == 0 && topic.Summary == "" {
			continue
		}
		if len(raw) > 0 {
			topic.FirstMsgID = raw[0].ID
			topic.LastMsgID = raw[len(raw)-1].ID
			topic.FirstAt = raw[0].SentAt
			topic.LastAt = raw[len(raw)-1].SentAt
		}
		topics = append(topics, topic)
	}
	return topics
}

func appendUnique(values []string, value string) []string {
	if !slices.Contains(values, value) {
		return append(values, value)
	}
	return values
}
