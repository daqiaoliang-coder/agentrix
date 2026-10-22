// Package identity 提供最小可用的调用方身份：从请求头解析 API Key，
// 映射为稳定主体标识，并以主体为前缀隔离会话与 artifact 命名空间。
//
// 生产路径：设置 AGENTRIX_API_KEYS（格式 key:userID[:tenant],...），
// 客户端通过 X-Agentrix-Key 携带 API Key。
// 未配置时进入匿名开发模式（ID 为空），行为与旧版本一致。
package identity

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// HeaderKey 是客户端携带 API Key 的请求头。
const HeaderKey = "X-Agentrix-Key"

// ErrUnauthorized 表示请求缺少或携带了无效的 API Key。
var ErrUnauthorized = errors.New("unauthorized: invalid or missing api key")

// Principal 是认证后的调用方身份。
// ID 为空表示匿名开发模式（未配置 AGENTRIX_API_KEYS）。
type Principal struct {
	ID     string
	Tenant string
}

// Anonymous 报告是否为匿名开发模式。
func (p Principal) Anonymous() bool { return p.ID == "" }

// Operator 返回写入审计字段的操作者标识。
func (p Principal) Operator() string {
	if p.ID == "" {
		return "anonymous"
	}
	return p.ID
}

// Scope 把用户输入的会话 ID 映射到该主体独有的命名空间。
// 匿名模式保持原样，保证本地开发与既有调用方行为不变。
func (p Principal) Scope(sessionID string) string {
	if p.ID == "" {
		return sessionID
	}
	return p.ID + "::" + sessionID
}

// Authenticator 是静态 API Key 表。
type Authenticator struct {
	keys map[string]Principal
}

// NewAuthenticator 按 key:userID[:tenant],... 构造认证器。
// 任一条目格式非法即返回错误，避免带病上线。
func NewAuthenticator(spec string) (*Authenticator, error) {
	a := &Authenticator{keys: make(map[string]Principal)}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return a, nil
	}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("invalid AGENTRIX_API_KEYS entry %q: want key:userID[:tenant]", entry)
		}
		key, user := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if key == "" || user == "" {
			return nil, fmt.Errorf("invalid AGENTRIX_API_KEYS entry %q: empty key or user", entry)
		}
		var tenant string
		if len(parts) == 3 {
			tenant = strings.TrimSpace(parts[2])
		}
		if _, dup := a.keys[key]; dup {
			return nil, fmt.Errorf("duplicate api key in AGENTRIX_API_KEYS")
		}
		a.keys[key] = Principal{ID: user, Tenant: tenant}
	}
	return a, nil
}

// FromEnv 读取并解析 AGENTRIX_API_KEYS。
func FromEnv() (*Authenticator, error) {
	return NewAuthenticator(os.Getenv("AGENTRIX_API_KEYS"))
}

// DevAuthenticator 返回禁用认证的认证器：全部请求映射为匿名主体。
func DevAuthenticator() *Authenticator {
	return &Authenticator{keys: map[string]Principal{}}
}

// Enabled 报告是否配置了任何 API Key。
func (a *Authenticator) Enabled() bool { return a != nil && len(a.keys) > 0 }

// Authenticate 从请求头解析主体。认证未启用时返回匿名主体。
func (a *Authenticator) Authenticate(r *http.Request) (Principal, error) {
	if !a.Enabled() {
		return Principal{}, nil
	}
	key := strings.TrimSpace(r.Header.Get(HeaderKey))
	if key == "" {
		return Principal{}, ErrUnauthorized
	}
	p, ok := a.keys[key]
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	return p, nil
}
