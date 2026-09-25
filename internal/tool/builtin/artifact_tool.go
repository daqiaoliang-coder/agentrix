package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/daqiaoliang-coder/agentrix/internal/projection"
)

// 框架工具名常量化：signalCallback 按名字识别 artifact 写入并补发
// artifact_new / artifact_updated 信号，字符串散落两处易漂移。
const (
	ToolWriteArtifact = "write_artifact"
	ToolReadArtifact  = "read_artifact"
)

// validArtifactTypes 是模型侧可传的 type 取值全集。
var validArtifactTypes = map[string]projection.ArtifactType{
	string(projection.ArtifactText):   projection.ArtifactText,
	string(projection.ArtifactTable):  projection.ArtifactTable,
	string(projection.ArtifactJSON):   projection.ArtifactJSON,
	string(projection.ArtifactPlan):   projection.ArtifactPlan,
	string(projection.ArtifactChart):  projection.ArtifactChart,
	string(projection.ArtifactCustom): projection.ArtifactCustom,
}

// WriteArtifactTool 把 artifact 落盘能力暴露为模型可调用的框架工具。
// 会话/轮次归属不经参数传递，而是从 Agent 注入的 TurnScope 取回——
// 让模型自己填 session_id 只会引入伪造归属的可能。
type WriteArtifactTool struct {
	store projection.ArtifactStore
}

func NewWriteArtifactTool(store projection.ArtifactStore) *WriteArtifactTool {
	return &WriteArtifactTool{store: store}
}

type writeArtifactArgs struct {
	// ID 留空创建新 artifact；填写则更新既有 artifact（覆盖 type/title/content）
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// writeArtifactResult 是工具返回给模型的 JSON 契约，signalCallback 也按它
// 解析并发射 artifact 信号。字段变更须两处同步。
type writeArtifactResult struct {
	ArtifactID string `json:"artifact_id"`
	SessionID  string `json:"session_id"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Updated    bool   `json:"updated"`
}

func (t *WriteArtifactTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: ToolWriteArtifact,
		Desc: "把本轮产出的结构化成果（报告、计划、表格、图表数据等）保存为 artifact，供会话后续轮次与外部系统读取。" +
			"传 id 时更新既有 artifact，否则创建新 artifact。content 必须是合法 JSON 字符串。",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"id": {
				Type:     schema.String,
				Desc:     "要更新的 artifact ID；留空表示创建新 artifact",
				Required: false,
			},
			"type": {
				Type:     schema.String,
				Desc:     "产物类型：text / table / json / plan / chart / custom",
				Required: true,
			},
			"title": {
				Type:     schema.String,
				Desc:     "产物标题，展示用",
				Required: true,
			},
			"content": {
				Type:     schema.String,
				Desc:     "产物内容，必须是合法 JSON 字符串（纯文本可写成 JSON 字符串字面量）",
				Required: true,
			},
		}),
	}, nil
}

func (t *WriteArtifactTool) InvokableRun(
	ctx context.Context,
	argumentsInJSON string,
	_ ...tool.Option,
) (string, error) {
	if t.store == nil {
		return "", fmt.Errorf("write_artifact: artifact store not configured")
	}
	scope, ok := projection.TurnScopeFrom(ctx)
	if !ok || scope.SessionID == "" {
		return "", fmt.Errorf("write_artifact: turn scope unavailable (tool must run inside an Agent turn)")
	}
	var args writeArtifactArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("invalid write_artifact arguments: %w", err)
	}
	typ, ok := validArtifactTypes[strings.TrimSpace(args.Type)]
	if !ok {
		return "", fmt.Errorf("write_artifact: invalid type %q; valid: text, table, json, plan, chart, custom", args.Type)
	}
	if strings.TrimSpace(args.Title) == "" {
		return "", fmt.Errorf("write_artifact: title is required")
	}
	if !json.Valid([]byte(args.Content)) {
		return "", fmt.Errorf("write_artifact: content must be valid JSON")
	}
	content := json.RawMessage(args.Content)

	var (
		art     *projection.Artifact
		err     error
		updated bool
	)
	if strings.TrimSpace(args.ID) != "" {
		// 更新路径：只允许覆盖本会话内的 artifact，防止跨会话篡改
		art, err = t.store.Get(ctx, args.ID)
		if err != nil {
			return "", fmt.Errorf("write_artifact: %w", err)
		}
		if art.SessionID != scope.SessionID {
			return "", fmt.Errorf("write_artifact: artifact %s belongs to another session", args.ID)
		}
		art.Type = typ
		art.Title = args.Title
		art.Content = content
		updated = true
	} else {
		art, err = projection.NewArtifact(scope.SessionID, scope.TurnID, typ, args.Title, content)
		if err != nil {
			return "", fmt.Errorf("write_artifact: %w", err)
		}
	}
	if err := t.store.Save(ctx, art); err != nil {
		return "", fmt.Errorf("write_artifact: save: %w", err)
	}

	out, _ := json.Marshal(writeArtifactResult{
		ArtifactID: art.ID,
		SessionID:  art.SessionID,
		Type:       string(art.Type),
		Title:      art.Title,
		Updated:    updated,
	})
	return string(out), nil
}

// ReadArtifactTool 按 ID 回读 artifact，供模型引用前轮产出。
type ReadArtifactTool struct {
	store projection.ArtifactStore
}

func NewReadArtifactTool(store projection.ArtifactStore) *ReadArtifactTool {
	return &ReadArtifactTool{store: store}
}

func (t *ReadArtifactTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: ToolReadArtifact,
		Desc: "按 ID 读取 artifact 的完整内容（含类型、标题、正文与更新时间）。",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"id": {
				Type:     schema.String,
				Desc:     "artifact ID（write_artifact 返回的 artifact_id）",
				Required: true,
			},
		}),
	}, nil
}

func (t *ReadArtifactTool) InvokableRun(
	ctx context.Context,
	argumentsInJSON string,
	_ ...tool.Option,
) (string, error) {
	if t.store == nil {
		return "", fmt.Errorf("read_artifact: artifact store not configured")
	}
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("invalid read_artifact arguments: %w", err)
	}
	if strings.TrimSpace(args.ID) == "" {
		return "", fmt.Errorf("read_artifact: id is required")
	}
	art, err := t.store.Get(ctx, args.ID)
	if err != nil {
		return "", fmt.Errorf("read_artifact: %w", err)
	}
	out, _ := json.Marshal(art)
	return string(out), nil
}
