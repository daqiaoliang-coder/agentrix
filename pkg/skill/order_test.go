package skill

import (
	"strings"
	"testing"
)

// buildLoader 按给定顺序注册若干技能。
func buildLoader(names ...string) *Loader {
	l := NewLoader()
	for _, n := range names {
		l.Register(&Skill{Name: n, Description: "desc-" + n})
	}
	return l
}

// frontmatterString 把 Frontmatter 产物拼成单一字符串，便于逐字节比较稳定性。
func frontmatterString(l *Loader) string {
	var sb strings.Builder
	for _, m := range l.Frontmatter() {
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}

// TestFrontmatterOrderIsStableAcrossCalls 验证 §2 的核心：多次调用 Frontmatter()
// 产物逐字节一致。Go 的 map 迭代顺序每次随机，若直接遍历 map，本断言必然间歇性失败——
// system prompt 前缀一变，provider 侧 KV 缓存全部作废。
func TestFrontmatterOrderIsStableAcrossCalls(t *testing.T) {
	// 刻意用乱序名称，排除「字典序恰好等于注册序」造成的假阳性
	l := buildLoader("zeta", "alpha", "mid", "beta", "omega", "gamma")
	first := frontmatterString(l)
	// 多次调用：Go map 迭代顺序在多次遍历间会变化，足以暴露未排序的实现
	for i := 0; i < 50; i++ {
		if got := frontmatterString(l); got != first {
			t.Fatalf("第 %d 次 Frontmatter 顺序漂移：\n首次=%q\n本次=%q", i, first, got)
		}
	}
}

// TestFrontmatterOrderFollowsRegistration 验证顺序恒等于注册顺序（而非字典序），
// 让接入方能主动把高频技能排在前面。
func TestFrontmatterOrderFollowsRegistration(t *testing.T) {
	l := buildLoader("zeta", "alpha", "mid")
	msgs := l.Frontmatter()
	if len(msgs) != 3 {
		t.Fatalf("应有 3 条 frontmatter，实际 %d", len(msgs))
	}
	// 注册顺序 zeta→alpha→mid；若按字典序会是 alpha→mid→zeta
	for i, want := range []string{"zeta", "alpha", "mid"} {
		if !strings.Contains(msgs[i].Content, want) {
			t.Errorf("第 %d 条应含技能名 %q，实际 %q", i, want, msgs[i].Content)
		}
	}
}

// TestRegisterOverwriteKeepsPosition 验证同名重复注册覆盖内容但不改变原有顺序。
func TestRegisterOverwriteKeepsPosition(t *testing.T) {
	l := buildLoader("a", "b", "c")
	l.Register(&Skill{Name: "b", Description: "new-desc"}) // 覆盖 b 的内容

	msgs := l.Frontmatter()
	if len(msgs) != 3 {
		t.Fatalf("重复注册不应新增条目，应有 3 条，实际 %d", len(msgs))
	}
	if !strings.Contains(msgs[1].Content, "b") {
		t.Errorf("覆盖后 b 应仍在第 2 位，实际 %q", msgs[1].Content)
	}
	if !strings.Contains(msgs[1].Content, "new-desc") {
		t.Errorf("覆盖未生效：%q", msgs[1].Content)
	}
}

// TestRegisterIgnoresInvalid 验证空名/nil 技能被安全忽略，不污染顺序表。
func TestRegisterIgnoresInvalid(t *testing.T) {
	l := NewLoader()
	l.Register(nil)
	l.Register(&Skill{Name: "", Description: "no-name"})
	l.Register(&Skill{Name: "ok", Description: "valid"})

	msgs := l.Frontmatter()
	if len(msgs) != 1 {
		t.Fatalf("应只保留 1 个有效技能，实际 %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "ok") {
		t.Errorf("有效技能丢失：%q", msgs[0].Content)
	}
}

// TestNamesReturnsFullSet 验证 Names() 返回全部已注册技能名（集合完整）。
//
// 注意：Names() 本身遍历 map、不保证顺序，这是有意的——它是「错误提示用可用清单」，
// 排序由调用方（tool/builtin/skill_tool.go 的 withSkillHint）负责。本次 §2 修复
// 只针对会进入 system prompt 前缀的 Frontmatter()，不涉及 Names()，故此处只校验集合。
func TestNamesReturnsFullSet(t *testing.T) {
	l := buildLoader("zeta", "alpha", "mid")
	names := l.Names()
	if len(names) != 3 {
		t.Fatalf("Names() 应返回 3 个，实际 %d: %v", len(names), names)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"zeta", "alpha", "mid"} {
		if !got[want] {
			t.Errorf("Names() 缺少 %q，实际 %v", want, names)
		}
	}
}
