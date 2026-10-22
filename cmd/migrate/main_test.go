package main

import "testing"

func TestSplitStatements(t *testing.T) {
	sql := `-- comment; with semicolon
CREATE TABLE a (id INT);
CREATE TABLE b (
  note VARCHAR(10) DEFAULT 'x;y'
);
`
	got := splitStatements(sql)
	if len(got) != 2 {
		t.Fatalf("语句数 = %d, got %#v", len(got), got)
	}
	if got[0] != "CREATE TABLE a (id INT)" {
		t.Errorf("第一条 = %q", got[0])
	}
	if !contains(got[1], "'x;y'") {
		t.Errorf("引号内分号被错误拆分: %q", got[1])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
