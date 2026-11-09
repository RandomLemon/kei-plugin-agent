package summary

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/RandomLemon/kei-plugin-persona/internal/memory/store"
)

var placeholderPattern = regexp.MustCompile(`\[(图片|表情|文件|卡片|引用|消息)\]`)

// StripPlaceholders 删除消息中没有文本信息的占位符。
func StripPlaceholders(text string) string {
	return placeholderPattern.ReplaceAllString(text, "")
}

func filler(text string) bool {
	if strings.TrimSpace(StripPlaceholders(text)) == "" {
		return true
	}
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, text)
	if utf8.RuneCountInString(compact) <= 2 && strings.Contains(fillerWords, "|"+compact+"|") {
		return true
	}
	symbols, repeated := true, true
	runes := []rune(compact)
	for _, r := range runes {
		if !unicode.IsPunct(r) && !unicode.IsSpace(r) && !unicode.IsSymbol(r) && r != '\ufe0f' && r != '\u200d' {
			symbols = false
		}
		if r != runes[0] {
			repeated = false
		}
	}
	return symbols || len(runes) >= 3 && repeated
}

// Prefilter 删除噪音并折叠复读；低于真人信息阈值的切片返回空。
func Prefilter(messages []store.Message) []store.Message {
	var out []store.Message
	humans, chars := 0, 0
	for i := 0; i < len(messages); {
		m := messages[i]
		j := i + 1
		for j < len(messages) && messages[j].Text == m.Text {
			j++
		}
		if !filler(m.Text) {
			if !m.IsSelf && !m.SenderIsBot {
				humans++
				chars += utf8.RuneCountInString(m.Text)
			}
			if j-i > 1 {
				m.Text += fmt.Sprintf("（×%d）", j-i)
			}
			out = append(out, m)
		}
		i = j
	}
	if humans < 3 || chars < 60 {
		return nil
	}
	return out
}
