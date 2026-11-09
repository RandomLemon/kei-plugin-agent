package summary

import (
	"slices"
	"time"
	"unicode/utf8"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

// Slice 按时间间隔和输入预算切片，正文预算优先，超限拆分最多保留 sliceOverlapMsgs 条重叠。
func Slice(messages []store.Message) ([][]store.Message, int) {
	messages = slices.Clone(messages)
	slices.SortStableFunc(messages, func(a, b store.Message) int {
		if c := a.SentAt.Compare(b.SentAt); c != 0 {
			return c
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	var all [][]store.Message
	var current []store.Message
	chars := 0
	for _, m := range messages {
		n := utf8.RuneCountInString(m.Text)
		if n > sliceMsgMaxChars {
			m.Text = truncate(m.Text, sliceMsgMaxChars-1) + "…"
			n = sliceMsgMaxChars
		}
		gap := len(current) > 0 && m.SentAt.Sub(current[len(current)-1].SentAt) > sliceGapMinutes*time.Minute
		if gap {
			all = append(all, current)
			current = nil
			chars = 0
		}
		if len(current) > 0 && (len(current) >= sliceMaxMsgs || chars+n > sliceMaxChars) {
			all = append(all, current)
			current = slices.Clone(current[max(0, len(current)-sliceOverlapMsgs):])
			chars = 0
			for _, v := range current {
				chars += utf8.RuneCountInString(v.Text)
			}
			// 重叠是上下文；优先让新消息满足切片预算。
			for len(current) > 0 && chars+n > sliceMaxChars {
				chars -= utf8.RuneCountInString(current[0].Text)
				current = current[1:]
			}
		}
		current = append(current, m)
		chars += n
	}
	if len(current) > 0 {
		all = append(all, current)
	}
	dropped := max(0, len(all)-jobMaxSlices)
	return all[:min(len(all), jobMaxSlices)], dropped
}
