// Package agent 是 kei 的「LLM 人格代理」进程内插件：在群聊里按人格预设偶尔插话。
//
// 接入方式：宿主空导入本包并配置 plugins.agent.enabled: true。
package agent

import "github.com/RandomLemon/kei/pkg/bot"

// 确保 Plugin 满足 bot.Plugin。
var _ bot.Plugin = (*Plugin)(nil)

func init() { bot.RegisterPlugin(&Plugin{}) }

// Metadata 返回插件元信息。
func (p *Plugin) Metadata() bot.Metadata {
	return bot.Metadata{
		Name:        "agent",
		Version:     "v0.1.0",
		Author:      "RandomLemon",
		Description: "LLM 人格代理：在群聊中按人格预设偶尔参与对话",
		Permissions: []bot.Permission{bot.PermNetwork, bot.PermStorage, bot.PermSendMessage},
	}
}
