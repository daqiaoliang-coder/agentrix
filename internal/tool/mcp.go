package tool

import (
	"context"
	"encoding/json"
	"fmt"

	einomcp "github.com/cloudwego/eino-ext/components/tool/mcp"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCPTransport 标识 MCP server 的传输方式。
type MCPTransport string

const (
	// MCPTransportStdio 拉起子进程经标准 IO 通信（本地工具型 server）。
	MCPTransportStdio MCPTransport = "stdio"
	// MCPTransportHTTP 是现行推荐的远程传输（Streamable HTTP）。
	MCPTransportHTTP MCPTransport = "http"
	// MCPTransportSSE 兼容旧版 server 的 SSE 传输。
	MCPTransportSSE MCPTransport = "sse"
)

// MCPServerConfig 描述一个 MCP server 的连接参数。
type MCPServerConfig struct {
	Name      string            // 标识名，仅用于错误信息定位
	Transport MCPTransport      // stdio / http / sse
	Command   string            // stdio：可执行文件，如 "npx"
	Args      []string          // stdio：命令参数
	Env       []string          // stdio：附加环境变量，"KEY=VALUE" 形式
	BaseURL   string            // http/sse：服务地址
	Headers   map[string]string // http/sse：请求头（如 Authorization）
	// ToolNameList 只接入列出的工具；空表示接入该 server 的全部工具。
	ToolNameList []string
}

// MCPConnection 持有到 MCP server 的存活连接及其导出的工具集。
//
// 连接即生命周期：stdio 传输下连接持有子进程，进程退出前应 Close。
// 已取出的工具在连接存续期间可反复调用。
type MCPConnection struct {
	name  string
	cli   client.MCPClient
	tools []einotool.BaseTool
}

// Tools 返回从 server 拉取并适配好的 eino 工具集。
//
// 产物可直接并入 SceneConfig.Tools：与手写工具同等待遇，
// 可登记 Catalog 获得命令白名单过滤与 NeedsApproval 审批包装。
func (c *MCPConnection) Tools() []einotool.BaseTool { return c.tools }

// Close 断开连接（stdio 下同时终止子进程）。
func (c *MCPConnection) Close() error { return c.cli.Close() }

// ConnectMCP 建立到单个 MCP server 的连接：启动传输 → 握手 → 拉取工具清单。
//
// 调用时机：进程启动 / scene 注册时一次性建立，然后把 Tools() 并入
// SceneConfig.Tools。不要在每次 NewAgent 时重连——工具 schema 已在
// Connect 时固化，Agent 每次请求重建装配只复用已取回的工具集，
// 重连只会白白付出握手与 ListTools 开销。
func ConnectMCP(ctx context.Context, cfg MCPServerConfig) (*MCPConnection, error) {
	cli, err := newMCPClient(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := connectMCPWithClient(ctx, cfg.Name, cli, cfg.ToolNameList, cfg.Headers)
	if err != nil {
		_ = cli.Close()
		return nil, err
	}
	return conn, nil
}

// connectMCPWithClient 是 ConnectMCP 的核心，与传输方式解耦：
// 调用方提供已创建的 client（测试可注入 in-process client），
// 此处完成启动、握手与工具拉取。失败时由调用方负责关闭 client。
func connectMCPWithClient(
	ctx context.Context,
	name string,
	cli *client.Client,
	toolNameList []string,
	headers map[string]string,
) (*MCPConnection, error) {
	if err := cli.Start(ctx); err != nil {
		return nil, fmt.Errorf("mcp %s: start: %w", name, err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "agentrix", Version: "1.0.0"}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		return nil, fmt.Errorf("mcp %s: initialize: %w", name, err)
	}

	tools, err := einomcp.GetTools(ctx, &einomcp.Config{
		Cli:           cli,
		ToolNameList:  toolNameList,
		CustomHeaders: headers,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp %s: list tools: %w", name, err)
	}
	return &MCPConnection{name: name, cli: cli, tools: unwrapTextResults(tools)}, nil
}

// unwrapTextResults 给每个 MCP 工具套上结果解包层。
//
// eino-ext 适配器把整个 CallToolResult 序列化输出，模型看到的是
// {"content":[{"type":"text","text":"3.75"}]} 这样的信封，而项目内
// 手写工具的约定是直接返回领域数据（"3.75"）。信封既浪费 token，
// 也让两种工具的结果形态不一致。单条 text 内容时解包为纯文本；
// 混合内容（图片/多段）保留原信封，不丢信息。
func unwrapTextResults(tools []einotool.BaseTool) []einotool.BaseTool {
	out := make([]einotool.BaseTool, 0, len(tools))
	for _, tl := range tools {
		inv, ok := tl.(einotool.InvokableTool)
		if !ok {
			out = append(out, tl)
			continue
		}
		out = append(out, &mcpTextTool{BaseTool: tl, inv: inv})
	}
	return out
}

// mcpTextTool 透传工具元信息，仅在调用成功后解包单条 text 结果。
type mcpTextTool struct {
	einotool.BaseTool
	inv einotool.InvokableTool
}

func (t *mcpTextTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...einotool.Option) (string, error) {
	out, err := t.inv.InvokableRun(ctx, argumentsInJSON, opts...)
	if err != nil {
		return out, err
	}
	return unwrapMCPText(out), nil
}

func unwrapMCPText(out string) string {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return out
	}
	if len(result.Content) == 1 && result.Content[0].Type == "text" {
		return result.Content[0].Text
	}
	return out
}

// newMCPClient 按传输方式创建底层 MCP client。
func newMCPClient(cfg MCPServerConfig) (*client.Client, error) {
	switch cfg.Transport {
	case MCPTransportStdio:
		if cfg.Command == "" {
			return nil, fmt.Errorf("mcp %s: stdio transport requires Command", cfg.Name)
		}
		return client.NewStdioMCPClient(cfg.Command, cfg.Env, cfg.Args...)
	case MCPTransportHTTP:
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("mcp %s: http transport requires BaseURL", cfg.Name)
		}
		opts := []transport.StreamableHTTPCOption{}
		if len(cfg.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(cfg.Headers))
		}
		return client.NewStreamableHttpClient(cfg.BaseURL, opts...)
	case MCPTransportSSE:
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("mcp %s: sse transport requires BaseURL", cfg.Name)
		}
		opts := []transport.ClientOption{}
		if len(cfg.Headers) > 0 {
			opts = append(opts, transport.WithHeaders(cfg.Headers))
		}
		return client.NewSSEMCPClient(cfg.BaseURL, opts...)
	default:
		return nil, fmt.Errorf("mcp %s: unknown transport %q (want stdio|http|sse)", cfg.Name, cfg.Transport)
	}
}
