package identity

import (
	"net/http/httptest"
	"testing"
)

func TestNewAuthenticator(t *testing.T) {
	a, err := NewAuthenticator("k1:alice:tenant-a, k2:bob")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !a.Enabled() {
		t.Fatal("应启用认证")
	}

	req := httptest.NewRequest("POST", "/chat", nil)
	req.Header.Set(HeaderKey, "k1")
	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if p.ID != "alice" || p.Tenant != "tenant-a" {
		t.Errorf("主体不正确: %+v", p)
	}
	if got := p.Scope("s1"); got != "alice::s1" {
		t.Errorf("会话前缀 = %q，期望 alice::s1", got)
	}

	req.Header.Set(HeaderKey, "k2")
	p, err = a.Authenticate(req)
	if err != nil || p.Tenant != "" {
		t.Errorf("无 tenant 的条目解析失败: p=%+v err=%v", p, err)
	}

	req.Header.Set(HeaderKey, "bad")
	if _, err := a.Authenticate(req); err == nil {
		t.Error("无效 key 应返回错误")
	}

	req.Header.Del(HeaderKey)
	if _, err := a.Authenticate(req); err == nil {
		t.Error("缺少 key 应返回错误")
	}
}

func TestNewAuthenticatorInvalid(t *testing.T) {
	for _, spec := range []string{"k1", "k1:", "k1:alice:t:x", "k1:a,k1:b"} {
		if _, err := NewAuthenticator(spec); err == nil {
			t.Errorf("spec %q 应报错", spec)
		}
	}
}

func TestDevAuthenticator(t *testing.T) {
	a := DevAuthenticator()
	req := httptest.NewRequest("POST", "/chat", nil)
	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("dev 模式不应报错: %v", err)
	}
	if !p.Anonymous() || p.Scope("s1") != "s1" {
		t.Errorf("dev 模式应保持会话原样: %+v", p)
	}
	if p.Operator() != "anonymous" {
		t.Errorf("Operator = %q", p.Operator())
	}
}
