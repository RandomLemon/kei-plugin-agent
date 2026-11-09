package summary

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

func sample() []store.Message {
	var result []store.Message
	for n := range 6 {
		result = append(result, store.Message{ID: int64(n + 1), MsgID: fmt.Sprint(n + 1), SenderID: fmt.Sprintf("id%d", n%2), SenderName: []string{"张三", "李四"}[n%2], Text: fmt.Sprintf("第%d条：周末聚餐选择城东餐厅，预算人均一百元，大家可以讨论具体时间和交通。", n), SentAt: time.Date(2026, 11, 7, 9, n, 0, 0, time.UTC)})
	}
	return result
}

func TestSlicing(t *testing.T) {
	msgs := sample()
	msgs[3].SentAt = msgs[2].SentAt.Add(time.Hour)
	msgs = msgs[:4]
	chunks, dropped := Slice(msgs)
	if len(chunks) != 2 || dropped != 0 {
		t.Fatalf("切片 %d/%d", len(chunks), dropped)
	}
	msgs = nil
	for n := range 205 {
		msgs = append(msgs, store.Message{ID: int64(n), Text: "短", SentAt: time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)})
	}
	chunks, _ = Slice(msgs)
	if len(chunks) != 2 || len(chunks[0]) != 200 || len(chunks[1]) != 15 || chunks[1][0].ID != 190 {
		t.Fatalf("重叠 %+v", chunks)
	}
	msgs = nil
	for n := range 82 {
		msgs = append(msgs, store.Message{ID: int64(n), Text: "内容", SentAt: time.Unix(int64(n)*3600, 0)})
	}
	chunks, dropped = Slice(msgs)
	if len(chunks) != 80 || dropped != 2 {
		t.Fatalf("切片上限 %d/%d", len(chunks), dropped)
	}
}

func TestPrefilter(t *testing.T) {
	for _, text := range []string{"[图片] [引用]", "😀！", "？？？", "哈哈哈", "好的", "ok", "嗯嗯"} {
		if !filler(text) {
			t.Errorf("噪音未过滤 %s", text)
		}
	}
	messages := sample()
	if got := Prefilter(messages); len(got) != 6 {
		t.Fatalf("有效消息丢失 %d", len(got))
	}
	for n := range messages {
		messages[n].SenderIsBot = true
	}
	if Prefilter(messages) != nil {
		t.Fatal("机器人计入真人阈值")
	}
	messages = sample()
	messages[1].Text = messages[0].Text
	got := Prefilter(messages)
	if len(got) != 5 || !strings.Contains(got[0].Text, "×2") {
		t.Fatal("复读未折叠")
	}
	messages = sample()
	for n := range messages {
		messages[n].Text = "短句"
	}
	if Prefilter(messages) != nil {
		t.Fatal("短文本切片未丢弃")
	}
}

func TestNormalization(t *testing.T) {
	msgs := sample()
	msgs[2].MsgID = ""
	in := NewInput(msgs, store.Conversation{Type: "group", Name: "群"}, time.Now(), time.UTC)
	out := SliceOutput{Topics: []SliceTopic{{Title: "u1 联系 u2", Summary: "邮箱 x@y.com 电话 13812345678 证件 123456789012345678", Participants: []string{"u1", "bot", "bad"}, Importance: 2, KeyPoints: []Point{
		{Type: "todo", Text: "u1 去订餐", Speaker: "u1", Owner: "bad", Refs: []int{1, 3, 99}},
		{Type: "decision", Text: "错误归属", Speaker: "u2", Refs: []int{1}},
		{Type: "info", Text: "无引用", Speaker: "u1", Refs: []int{99}},
	}}}}
	topics := in.Normalize(out, msgs, msgs)
	if len(topics) != 1 {
		t.Fatal(topics)
	}
	topic := topics[0]
	if topic.Title != "张三 联系 李四" || strings.Contains(topic.Summary, "13812345678") || strings.Contains(topic.Summary, "x@y.com") || strings.Contains(topic.Summary, "123456789012345678") {
		t.Fatalf("文本未处理 %+v", topic)
	}
	if len(topic.Participants) != 1 || len(topic.KeyPoints) != 1 || topic.KeyPoints[0].Speaker != "id0" || topic.KeyPoints[0].Owner != "" || topic.KeyPoints[0].MsgIDs[1] != "mem:3" || topic.Importance != 1 || !topic.HasOpen {
		t.Fatalf("规范化 %+v", topic)
	}
	if topic.FirstMsgID != 1 || topic.LastMsgID != 6 {
		t.Fatal("范围未取完整切片")
	}
	if !strings.HasPrefix(in.RenderSlice(msgs), "[会话] 群聊「群」") {
		t.Fatal("切片输入缺少群聊标签")
	}
	private := NewInput(msgs, store.Conversation{Type: "private", Name: "张三"}, time.Now(), time.UTC)
	if !strings.HasPrefix(private.RenderSlice(msgs), "[会话] 私聊「张三」") {
		t.Fatal("切片输入缺少私聊标签")
	}
	if got := in.clean(strings.Repeat("字", 15)+"13812345678", 20); strings.Contains(got, "138") {
		t.Fatalf("截断后残留手机号片段：%s", got)
	}
	in.Names["u12"] = "王五"
	if got := in.clean("u12 与 u1", 60); got != "王五 与 张三" {
		t.Fatal(got)
	}
	if (&SliceOutput{}).Validate() == nil || (&SliceOutput{Topics: []SliceTopic{}}).Validate() != nil {
		t.Fatal("结构校验错误")
	}
}

func TestMerge(t *testing.T) {
	date := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	source := []store.Topic{
		{Title: "聚餐", Summary: "城东餐厅", Importance: 0.8, KeyPoints: []store.KeyPoint{{Type: "todo", Text: "原文", Speaker: "id0"}}},
		{Title: "交通", Summary: "坐地铁", Importance: 0.6, KeyPoints: []store.KeyPoint{{Type: "info", Text: "另一要点", Speaker: "id1"}}},
		{Title: "遗漏重要", Summary: "保留", Importance: 0.4}, {Title: "闲聊", Summary: "忽略", Importance: 0.1},
	}
	known := []store.Topic{{TopicKey: "stable"}}
	output := MergeOutput{Topics: []MergedTopic{
		{From: []string{"s1", "bad"}, KeyPoints: []string{"s1.0", "s1.0", "s2.0"}, Continues: "k1"},
		{From: []string{"s1", "s2"}, KeyPoints: []string{"s2.0"}, Continues: "k1"},
	}}
	topics, overview := Merge(output, source, known, "c", date, false)
	if len(topics) != 3 || topics[0].TopicKey != "stable" || topics[1].TopicKey == "stable" || topics[2].TopicKey != "topic:c:20261107:3" {
		t.Fatalf("合并 %+v", topics)
	}
	if len(topics[0].KeyPoints) != 1 || topics[0].KeyPoints[0].Text != "原文" || topics[0].KeyPoints[0].Speaker != "id0" || !topics[0].HasOpen || overview == "" {
		t.Fatal("归属/概况错误")
	}
	fallback, overview := Merge(MergeOutput{}, source, known, "c", date, true)
	if len(fallback) != 4 || overview != "聚餐；交通；遗漏重要" || fallback[0].ContinuesFrom != "" {
		t.Fatal("兜底错误")
	}
	input := renderMerge(source, nil)
	if !strings.Contains(input, `"text":"原文"`) || strings.Contains(input, "id0") || strings.Contains(input, "speaker") {
		t.Fatalf("合并输入要点应只含类型与原文：%s", input)
	}
	merged, _ := Merge(MergeOutput{Overview: "电话13812345678", Topics: []MergedTopic{{From: []string{"s1"}, Title: strings.Repeat("字", 15) + "13812345678"}}}, source[:1], nil, "c", date, false)
	if strings.Contains(merged[0].Title, "138") {
		t.Fatalf("合并标题截断前未脱敏：%s", merged[0].Title)
	}
	if ShortSummary("一句话。第二句") != "一句话" {
		t.Fatal("短摘要错误")
	}
}

func TestSlicingCharacterBudgetShrinksOverlap(t *testing.T) {
	var messages []store.Message
	for n := range 5 {
		messages = append(messages, store.Message{ID: int64(n + 1), Text: strings.Repeat("消息正文", 500), SentAt: time.Date(2026, 11, 7, 9, n, 0, 0, time.UTC)})
	}
	chunks, dropped := Slice(messages)
	if len(chunks) != 2 || dropped != 0 || len(chunks[0]) != 4 || len(chunks[1]) != 4 || chunks[1][0].ID != 2 || chunks[1][3].ID != 5 {
		t.Fatalf("字数拆分重叠不正确：%+v", chunks)
	}
	for _, chunk := range chunks {
		chars := 0
		for _, message := range chunk {
			chars += utf8.RuneCountInString(message.Text)
		}
		if chars > sliceMaxChars {
			t.Fatalf("正文预算超限：%d", chars)
		}
	}
}

func TestSlicingLongMessageSharesContextAndPreservesOriginal(t *testing.T) {
	messages := sample()[:3]
	original := strings.Repeat("长公告正文", 1800)
	messages[1].Text = original
	chunks, dropped := Slice(messages)
	if len(chunks) != 1 || dropped != 0 || len(chunks[0]) != 3 {
		t.Fatalf("长消息未与前后对话同片：%d", len(chunks))
	}
	text := chunks[0][1].Text
	if utf8.RuneCountInString(text) != sliceMsgMaxChars || !strings.HasSuffix(text, "…") || !strings.HasPrefix(original, strings.TrimSuffix(text, "…")) {
		t.Fatal("单条截断不符合预算或未保留开头")
	}
	if len(Prefilter(chunks[0])) != 3 {
		t.Fatal("长消息和前后真人消息被预过滤丢弃")
	}
	if messages[1].Text != original {
		t.Fatal("修改了调用方的原始正文")
	}
}
