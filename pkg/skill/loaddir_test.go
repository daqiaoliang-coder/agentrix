package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "release", "SKILL.md"), `---
name: release
description: 发布变更
---
# 发布

正文内容
`)
	writeFile(t, filepath.Join(dir, "release", "references", "canary.md"), "# 灰度细则\n")
	// 无 SKILL.md 的目录应被忽略
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}

	loader, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	names := loader.Names()
	if len(names) != 1 || names[0] != "release" {
		t.Fatalf("技能列表 = %v", names)
	}
	body, err := loader.LoadBody("release")
	if err != nil {
		t.Fatalf("load body: %v", err)
	}
	if body != "# 发布\n\n正文内容\n" {
		t.Errorf("正文 = %q", body)
	}
	ref, err := loader.LoadReference("release", "canary")
	if err != nil || ref != "# 灰度细则\n" {
		t.Errorf("reference = %q err=%v", ref, err)
	}
	fm := loader.Frontmatter()
	if len(fm) != 1 {
		t.Fatalf("frontmatter 条数 = %d", len(fm))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
