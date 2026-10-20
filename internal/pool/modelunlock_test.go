package pool

import (
	"testing"
	"time"
)

// TestUnlockModelsByModel 按模型解锁：只清该模型，其他模型与账号级冷却不动。
func TestUnlockModelsByModel(t *testing.T) {
	p := lockViewPool(t)
	far := time.Now().Add(2 * time.Hour)
	// 账号级冷却先写（它会清掉路由型模型冷却），再手搓模型级冷却。
	p.Cooldown("u2", CoolSoft, time.Hour, "429 rate limit")
	setModelCooldown(p, "u1", "hy3", far, "14003 model busy", false)
	setModelCooldown(p, "u2", "hy3", far, "model rate limit", false)
	setModelCooldown(p, "u1", "glm-5.3", far, "model rate limit", false)

	if n := p.UnlockModels("hy3", "", ""); n != 2 {
		t.Fatalf("unlocked=%d want 2", n)
	}
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.byUID["u1"].modelCooled(now, "hy3") || p.byUID["u2"].modelCooled(now, "hy3") {
		t.Fatal("hy3 应已解锁")
	}
	if !p.byUID["u1"].modelCooled(now, "glm-5.3") {
		t.Fatal("glm-5.3 不应被一并清除")
	}
	if p.byUID["u2"].until.IsZero() {
		t.Fatal("账号级冷却不应被模型解锁清掉")
	}
}

// TestUnlockModelsByAccount 按账号解锁 + 全部解锁；Hits 一并清零（退避从头计）。
func TestUnlockModelsByAccount(t *testing.T) {
	p := lockViewPool(t)
	far := time.Now().Add(time.Hour)
	setModelCooldown(p, "u1", "hy3", far, "x", false)
	setModelCooldown(p, "u2", "hy3", far, "x", false)

	if n := p.UnlockModels("hy3", "", "u1"); n != 1 {
		t.Fatalf("unlocked=%d want 1", n)
	}
	if rows := p.ModelLockView(); len(rows) != 1 || rows[0].Locked != 1 {
		t.Fatalf("u2 仍应锁着，got %+v", rows)
	}
	if n := p.UnlockModels("", "global", ""); n != 0 {
		t.Fatalf("realm 过滤失效：cn 账号不应被 global 解锁清除，got %d", n)
	}
	if n := p.UnlockModels("", "", ""); n != 1 {
		t.Fatalf("全部解锁 unlocked=%d want 1", n)
	}
	if rows := p.ModelLockView(); rows != nil {
		t.Fatalf("全部解锁后应无锁，got %+v", rows)
	}

	// 解锁后再次 14003：从 60s 起步，而非继承之前的命中阶梯。
	p.CooldownModelBusy("u1", "hy3", "14003 model busy")
	p.CooldownModelBusy("u1", "hy3", "14003 model busy") // 窗口内不延长
	p.mu.RLock()
	mc := p.byUID["u1"].modelCooldowns["hy3"]
	p.mu.RUnlock()
	if mc.Hits != 1 || time.Until(mc.Until) > 61*time.Second {
		t.Fatalf("解锁后应从头计：hits=%d until=%v", mc.Hits, time.Until(mc.Until))
	}
}
