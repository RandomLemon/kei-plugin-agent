package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPolicyAllowMatrix(t *testing.T) {
	cases := []struct {
		name string
		mode string
		list []string
		id   string
		want bool
	}{
		{"off 命中", policyOff, []string{"g1"}, "g1", false},
		{"off 空名单", policyOff, nil, "g1", false},
		{"open 空名单", policyOpen, nil, "g1", true},
		{"open 有名单", policyOpen, []string{"g1"}, "g9", true},
		{"whitelist 命中", policyWhitelist, []string{"g1", "g2"}, "g2", true},
		{"whitelist 未命中", policyWhitelist, []string{"g1"}, "g9", false},
		{"whitelist 空名单", policyWhitelist, nil, "g1", false},
		{"blacklist 命中", policyBlacklist, []string{"g1"}, "g1", false},
		{"blacklist 未命中", policyBlacklist, []string{"g1"}, "g9", true},
		{"blacklist 空名单", policyBlacklist, nil, "g1", true},
		{"未知模式", "bogus", []string{"g1"}, "g1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (policyState{groupMode: tc.mode, groupList: tc.list}).allowsGroup(tc.id); got != tc.want {
				t.Errorf("allowsGroup(%q) = %v, want %v", tc.id, got, tc.want)
			}
			if got := (policyState{privateMode: tc.mode, privateList: tc.list}).allowsPrivate(tc.id); got != tc.want {
				t.Errorf("allowsPrivate(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestPolicyCommandOutputs(t *testing.T) {
	env := newTestEnv(t, nil, nil)

	if got := env.command("policy"); got != "agent: group=open(0) · private=off(0)" {
		t.Fatalf("policy = %q", got)
	}
	if got := env.command("policy", "group", "whitelist"); got != "agent: group=whitelist(0)" {
		t.Fatalf("policy group whitelist = %q", got)
	}
	if got := env.command("policy"); got != "agent: group=whitelist(0) · private=off(0)" {
		t.Fatalf("policy after set = %q", got)
	}
	if got := env.command("list"); got != "agent: group=[] private=[]" {
		t.Fatalf("list = %q", got)
	}
	if got := env.command("list", "group", "add", "g1"); got != "agent: group=[g1]" {
		t.Fatalf("list add g1 = %q", got)
	}
	if got := env.command("list", "group", "add", "g2"); got != "agent: group=[g1 g2]" {
		t.Fatalf("list add g2 = %q", got)
	}
	if got := env.command("list", "group", "add", "g9"); got != "agent: group=[g1 g2 g9]" {
		t.Fatalf("list add g9 = %q", got)
	}
	if got := env.command("list", "group", "add", "g9"); got != "agent: group=[g1 g2 g9]" {
		t.Fatalf("list add 幂等 = %q", got)
	}
	if got := env.command("list", "group", "del", "g9"); got != "agent: group=[g1 g2]" {
		t.Fatalf("list del g9 = %q", got)
	}
	if got := env.command("list", "group", "del", "nope"); got != "agent: group=[g1 g2]" {
		t.Fatalf("list del 不存在应幂等 = %q", got)
	}
	if got := env.command("list", "private", "add", "u1"); got != "agent: private=[u1]" {
		t.Fatalf("list private add = %q", got)
	}
	if got := env.command("list"); got != "agent: group=[g1 g2] private=[u1]" {
		t.Fatalf("list = %q", got)
	}
	if got := env.command("list", "group"); got != "agent: group=[g1 g2]" {
		t.Fatalf("list group = %q", got)
	}
	if got := env.command("list", "private"); got != "agent: private=[u1]" {
		t.Fatalf("list private = %q", got)
	}
	if got := env.command("policy"); got != "agent: group=whitelist(2) · private=off(1)" {
		t.Fatalf("policy 计数 = %q", got)
	}
	if got := env.command("policy", "private", "blacklist"); got != "agent: private=blacklist(1)" {
		t.Fatalf("policy private blacklist = %q", got)
	}

	// 非法参数一律回用法。
	for _, args := range [][]string{
		{"policy", "group"},
		{"policy", "group", "bogus"},
		{"policy", "bogus", "open"},
		{"policy", "group", "off", "extra"},
		{"list", "bogus"},
		{"list", "group", "add"},
		{"list", "group", "add", ""},
		{"list", "group", "bogus", "x"},
		{"list", "bogus", "add", "g1"},
	} {
		if got := env.command(args...); got != agentUsage() {
			t.Fatalf("command %v = %q, want usage", args, got)
		}
	}
}

func TestPolicyPersistence(t *testing.T) {
	env := newTestEnv(t, nil, nil)

	env.command("policy", "group", "blacklist")
	waitStoreContains(t, env, policyKey, `"group_mode":"blacklist"`)
	waitStoreContains(t, env, policyKey, `"group_list":[]`)

	env.command("list", "private", "add", "u1")
	waitStoreContains(t, env, policyKey, `"private_list":["u1"]`)
}

func TestPolicyRestoreOnStart(t *testing.T) {
	mutate := func(c map[string]any) {
		c["group_policy"] = "open"
		c["group_list"] = []any{"cfg1"}
		c["private_policy"] = "off"
		c["private_list"] = []any{"cfgu"}
	}
	newEnv := func(t *testing.T, raw string) *testEnv {
		t.Helper()
		store := newFakeStorage()
		if raw != "" {
			if err := store.Set(context.Background(), policyKey, []byte(raw), 0); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		return newTestEnvWith(t, nil, mutate, store, nil)
	}
	assert := func(t *testing.T, env *testEnv, gm string, gl []string, pm string, pl []string) {
		t.Helper()
		env.p.policyMu.RLock()
		defer env.p.policyMu.RUnlock()
		if env.p.policy.groupMode != gm {
			t.Errorf("groupMode = %q, want %q", env.p.policy.groupMode, gm)
		}
		if !slices.Equal(env.p.policy.groupList, gl) {
			t.Errorf("groupList = %v, want %v", env.p.policy.groupList, gl)
		}
		if env.p.policy.privateMode != pm {
			t.Errorf("privateMode = %q, want %q", env.p.policy.privateMode, pm)
		}
		if !slices.Equal(env.p.policy.privateList, pl) {
			t.Errorf("privateList = %v, want %v", env.p.policy.privateList, pl)
		}
	}

	t.Run("合法覆盖", func(t *testing.T) {
		env := newEnv(t, `{"group_mode":"blacklist","group_list":["g1"],"private_mode":"whitelist","private_list":["u1"]}`)
		assert(t, env, "blacklist", []string{"g1"}, "whitelist", []string{"u1"})
	})
	t.Run("非法模式回落配置默认", func(t *testing.T) {
		env := newEnv(t, `{"group_mode":"bogus","private_mode":""}`)
		assert(t, env, "open", []string{"cfg1"}, "off", []string{"cfgu"})
	})
	t.Run("list 为 null 回落配置默认", func(t *testing.T) {
		env := newEnv(t, `{"group_mode":"whitelist","group_list":null,"private_list":null}`)
		assert(t, env, "whitelist", []string{"cfg1"}, "off", []string{"cfgu"})
	})
	t.Run("list 为 [] 采用空名单", func(t *testing.T) {
		env := newEnv(t, `{"group_list":[],"private_list":[]}`)
		assert(t, env, "open", nil, "off", nil)
	})
	t.Run("坏 JSON 回落配置默认", func(t *testing.T) {
		env := newEnv(t, `{oops`)
		assert(t, env, "open", []string{"cfg1"}, "off", []string{"cfgu"})
		if !env.cap.hasMsg("agent: 名单策略解析失败") {
			t.Error("want 解析失败 warn")
		}
	})
	t.Run("无覆盖", func(t *testing.T) {
		env := newEnv(t, "")
		assert(t, env, "open", []string{"cfg1"}, "off", []string{"cfgu"})
	})
}

// waitStoreContains 轮询等待 Storage 中某键的值包含给定子串。
func waitStoreContains(t *testing.T, env *testEnv, key, sub string) {
	t.Helper()
	if env.store == nil {
		t.Fatal("需要 fakeStorage")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v, ok := env.store.value(key); ok && strings.Contains(v, sub) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	v, _ := env.store.value(key)
	t.Fatalf("Storage[%s] = %q, want 含 %q", key, v, sub)
}
