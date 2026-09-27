package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/RandomLemon/kei/pkg/bot"
)

// policyKey 是插件级名单策略的 Storage 键。
const policyKey = "agent:policy"

// scopeGroup / scopePrivate 是 /agent policy|list 的两个作用域。
const (
	scopeGroup   = "group"
	scopePrivate = "private"
)

// policyState 是插件级名单策略的运行期状态（配置默认值 + Storage 覆盖）。
type policyState struct {
	groupMode   string
	groupList   []string
	privateMode string
	privateList []string
}

// policyValue 是名单策略持久化后的 JSON 形状。
type policyValue struct {
	GroupMode   string   `json:"group_mode"`
	GroupList   []string `json:"group_list"`
	PrivateMode string   `json:"private_mode"`
	PrivateList []string `json:"private_list"`
}

// forScope 返回指定作用域的模式与名单；作用域非法时返回 ok=false。
func (s policyState) forScope(scope string) (string, []string, bool) {
	switch scope {
	case scopeGroup:
		return s.groupMode, slices.Clone(s.groupList), true
	case scopePrivate:
		return s.privateMode, slices.Clone(s.privateList), true
	}
	return "", nil, false
}

// snapshot 返回策略的持久化形状：列表为拷贝且保证非 nil（序列化成 [] 而不是 null）。
func (s policyState) snapshot() policyValue {
	return policyValue{
		GroupMode:   s.groupMode,
		GroupList:   ensureNonNil(slices.Clone(s.groupList)),
		PrivateMode: s.privateMode,
		PrivateList: ensureNonNil(slices.Clone(s.privateList)),
	}
}

// allowsGroup 判断群聊（按频道 ID）是否放行。
func (s policyState) allowsGroup(channelID string) bool {
	return allowByMode(s.groupMode, s.groupList, channelID)
}

// allowsPrivate 判断私聊（按发送者 ID）是否放行。
func (s policyState) allowsPrivate(userID string) bool {
	return allowByMode(s.privateMode, s.privateList, userID)
}

// allowByMode 按模式判定 id 是否放行；未知模式返回 true（放行下限）。
//
// off = 全部拒绝；open = 全部放行；whitelist = 仅名单内；blacklist = 名单外。
func allowByMode(mode string, list []string, id string) bool {
	switch mode {
	case policyOff:
		return false
	case policyOpen:
		return true
	case policyWhitelist:
		return slices.Contains(list, id)
	case policyBlacklist:
		return !slices.Contains(list, id)
	default:
		return true
	}
}

// ensureNonNil 保证切片非 nil，便于序列化成 [] 而不是 null。
func ensureNonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// policyAllowsGroup 判断群聊频道是否放行。
func (p *Plugin) policyAllowsGroup(channelID string) bool {
	p.policyMu.RLock()
	defer p.policyMu.RUnlock()
	return p.policy.allowsGroup(channelID)
}

// policyAllowsPrivate 判断私聊发送者是否放行。
func (p *Plugin) policyAllowsPrivate(userID string) bool {
	p.policyMu.RLock()
	defer p.policyMu.RUnlock()
	return p.policy.allowsPrivate(userID)
}

// loadPolicy 同步读取插件级名单策略覆盖；失败只 warn，保留配置默认值。
//
// 逐字段覆盖：模式为空或非法时不覆盖；列表为 null/缺键时不覆盖，
// 为 [] 时覆盖为空名单（表示「用户把名单清空了」）。
func (p *Plugin) loadPolicy(ctx context.Context) {
	if p.store == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	raw, err := p.store.Get(cctx, policyKey)
	if errors.Is(err, bot.ErrNotFound) {
		return
	}
	if err != nil {
		p.log.Warn("agent: 读取名单策略失败", "err", err)
		return
	}
	var v policyValue
	if err := json.Unmarshal(raw, &v); err != nil {
		p.log.Warn("agent: 名单策略解析失败", "err", err)
		return
	}

	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	if v.GroupMode != "" && validPolicyMode(v.GroupMode) {
		p.policy.groupMode = v.GroupMode
	}
	if v.GroupList != nil {
		p.policy.groupList = slices.Clone(v.GroupList)
	}
	if v.PrivateMode != "" && validPolicyMode(v.PrivateMode) {
		p.policy.privateMode = v.PrivateMode
	}
	if v.PrivateList != nil {
		p.policy.privateList = slices.Clone(v.PrivateList)
	}
}

// savePolicy 写穿透持久化名单策略（异步、带超时、失败只 warn）。
func (p *Plugin) savePolicy() {
	if p.ctx == nil || p.store == nil {
		return
	}
	p.policyMu.RLock()
	v := p.policy.snapshot()
	p.policyMu.RUnlock()

	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ctx, cancel := context.WithTimeout(p.ctx, storageTimeout)
		defer cancel()
		if err := p.store.Set(ctx, policyKey, data, 0); err != nil {
			p.log.Warn("agent: 写入名单策略失败", "err", err)
		}
	}()
}

// setPolicyMode 设置 scope（"group"/"private"）的模式并写穿透；scope 或 mode 非法时返回 false 且不修改。
func (p *Plugin) setPolicyMode(scope, mode string) bool {
	if !validPolicyMode(mode) {
		return false
	}
	p.policyMu.Lock()
	switch scope {
	case scopeGroup:
		if p.policy.groupMode == mode {
			p.policyMu.Unlock()
			return true
		}
		p.policy.groupMode = mode
	case scopePrivate:
		if p.policy.privateMode == mode {
			p.policyMu.Unlock()
			return true
		}
		p.policy.privateMode = mode
	default:
		p.policyMu.Unlock()
		return false
	}
	p.policyMu.Unlock()
	p.savePolicy()
	return true
}

// addPolicyID 把 id 加入 scope 名单（已存在则不变）并写穿透；scope 非法或 id 为空时不修改。
func (p *Plugin) addPolicyID(scope, id string) {
	if id == "" {
		return
	}
	p.policyMu.Lock()
	changed := false
	switch scope {
	case scopeGroup:
		if !slices.Contains(p.policy.groupList, id) {
			p.policy.groupList = append(p.policy.groupList, id)
			changed = true
		}
	case scopePrivate:
		if !slices.Contains(p.policy.privateList, id) {
			p.policy.privateList = append(p.policy.privateList, id)
			changed = true
		}
	default:
		p.policyMu.Unlock()
		return
	}
	p.policyMu.Unlock()
	if changed {
		p.savePolicy()
	}
}

// delPolicyID 从 scope 名单移除 id（不存在则不变）并写穿透；scope 非法时不修改。
func (p *Plugin) delPolicyID(scope, id string) {
	if id == "" {
		return
	}
	p.policyMu.Lock()
	changed := false
	switch scope {
	case scopeGroup:
		if i := slices.Index(p.policy.groupList, id); i >= 0 {
			p.policy.groupList = slices.Delete(p.policy.groupList, i, i+1)
			changed = true
		}
	case scopePrivate:
		if i := slices.Index(p.policy.privateList, id); i >= 0 {
			p.policy.privateList = slices.Delete(p.policy.privateList, i, i+1)
			changed = true
		}
	default:
		p.policyMu.Unlock()
		return
	}
	p.policyMu.Unlock()
	if changed {
		p.savePolicy()
	}
}

// policyReport 渲染 `/agent policy`：`agent: group=open(0) · private=off(0)`。
func (p *Plugin) policyReport() string {
	p.policyMu.RLock()
	groupMode, groupList, _ := p.policy.forScope(scopeGroup)
	privateMode, privateList, _ := p.policy.forScope(scopePrivate)
	p.policyMu.RUnlock()
	return "agent: group=" + groupMode + "(" + strconv.Itoa(len(groupList)) + ")" +
		" · private=" + privateMode + "(" + strconv.Itoa(len(privateList)) + ")"
}

// policyScopeReport 渲染单侧：`agent: group=whitelist(2)`。
func (p *Plugin) policyScopeReport(scope string) string {
	p.policyMu.RLock()
	mode, list, ok := p.policy.forScope(scope)
	p.policyMu.RUnlock()
	if !ok {
		return agentUsage()
	}
	return "agent: " + scope + "=" + mode + "(" + strconv.Itoa(len(list)) + ")"
}

// listReport 渲染 `/agent list`：`agent: group=[g1 g2] private=[u1]`（空名单为 `[]`）。
func (p *Plugin) listReport() string {
	p.policyMu.RLock()
	_, groupList, _ := p.policy.forScope(scopeGroup)
	_, privateList, _ := p.policy.forScope(scopePrivate)
	p.policyMu.RUnlock()
	return "agent: group=" + formatIDList(groupList) + " private=" + formatIDList(privateList)
}

// listScopeReport 渲染单侧：`agent: group=[g1 g2]`。
func (p *Plugin) listScopeReport(scope string) string {
	p.policyMu.RLock()
	_, list, ok := p.policy.forScope(scope)
	p.policyMu.RUnlock()
	if !ok {
		return agentUsage()
	}
	return "agent: " + scope + "=" + formatIDList(list)
}

// formatIDList 把 ID 列表渲染成 `[id1 id2]`（空名单为 `[]`）。
func formatIDList(list []string) string {
	return "[" + strings.Join(list, " ") + "]"
}
