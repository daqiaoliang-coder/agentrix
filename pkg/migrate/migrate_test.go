package migrate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitStatements(t *testing.T) {
	sqlText := `-- 注释行应被忽略
CREATE TABLE a (id INT);
INSERT INTO a (v) VALUES ('x;y');
-- 行内注释后仍应继续
CREATE TABLE ` + "`b`" + ` (name VARCHAR(10));
`
	got := SplitStatements(sqlText)
	want := []string{
		"CREATE TABLE a (id INT)",
		"INSERT INTO a (v) VALUES ('x;y')",
		"CREATE TABLE `b` (name VARCHAR(10))",
	}
	if len(got) != len(want) {
		t.Fatalf("语句数量不符: got %d %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条语句不符:\n got: %q\nwant: %q", i+1, got[i], want[i])
		}
	}
}

func TestListSQLSortsAndFilters(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"002_b.sql", "001_a.sql", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := ListSQL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("应只列出 2 个 SQL 文件，实际 %d: %v", len(files), files)
	}
	if filepath.Base(files[0]) != "001_a.sql" || filepath.Base(files[1]) != "002_b.sql" {
		t.Errorf("文件未按名称排序: %v", files)
	}
}
