package tool

import (
	"context"
	"fmt"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// newTestMCPServer 构造一个进程内 MCP server，提供两个工具：
//   - echo：原样返回输入文本（只读语义）
//   - add ：两数求和（模拟写操作语义，用于审批包装验证）
func newTestMCPServer() *server.MCPServer {
	svr := server.NewMCPServer("test-server", mcp.LATEST_PROTOCOL_VERSION)

	svr.AddTool(mcp.NewTool("echo",
		mcp.WithDescription("回显输入文本"),
		mcp.WithString("text", mcp.Required(), mcp.Description("要回显的文本")),
	), func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arg := request.Params.Arguments.(map[string]any)
		return mcp.NewToolResultText(fmt.Sprintf("%v", arg["text"])), nil
	})

	svr.AddTool(mcp.NewTool("add",
		mcp.WithDescription("两数求和"),
		mcp.WithNumber("x", mcp.Required(), mcp.Description("第一个数")),
		mcp.WithNumber("y", mcp.Required(), mcp.Description("第二个数")),
	), func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arg := request.Params.Arguments.(map[string]any)
		sum := arg["x"].(float64) + arg["y"].(float64)
		return mcp.NewToolResultText(fmt.Sprintf("%.2f", sum)), nil
	})

	return svr
}

// connectInProcess 用 in-process client 走与 ConnectMCP 相同的握手与拉取链路。
func connectInProcess(t *testing.T, toolNameList []string) *MCPConnection {
	t.Helper()
	cli, err := client.NewInProcessClient(newTestMCPServer())
	if err != nil {
		t.Fatalf("new in-process client: %v", err)
	}
	conn, err := connectMCPWithClient(context.Background(), "test", cli, toolNameList, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func toolNames(t *testing.T, conn *MCPConnection) map[string]bool {
	t.Helper()
	names := make(map[string]bool)
	for _, tl := range conn.Tools() {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("tool info: %v", err)
		}
		names[info.Name] = true
	}
	return names
}

// TestConnectMCP_Tools 验证握手后拉取到 server 暴露的全部工具。
func TestConnectMCP_Tools(t *testing.T) {
	conn := connectInProcess(t, nil)
	names := toolNames(t, conn)
	if !names["echo"] || !names["add"] {
		t.Fatalf("应拉取到 echo 与 add，实际 %v", names)
	}
}

// TestConnectMCP_ToolNameList 验证按名单过滤接入的工具子集。
func TestConnectMCP_ToolNameList(t *testing.T) {
	conn := connectInProcess(t, []string{"echo"})
	names := toolNames(t, conn)
	if len(names) != 1 || !names["echo"] {
		t.Fatalf("ToolNameList=[echo] 时只应接入 echo，实际 %v", names)
	}
}

// TestConnectMCP_Invoke 验证 MCP 工具经 eino 适配后可端到端调用。
func TestConnectMCP_Invoke(t *testing.T) {
	conn := connectInProcess(t, nil)
	for _, tl := range conn.Tools() {
		info, _ := tl.Info(context.Background())
		if info.Name != "add" {
			continue
		}
		inv, ok := tl.(einotool.InvokableTool)
		if !ok {
			t.Fatal("MCP 适配工具应实现 InvokableTool")
		}
		out, err := inv.InvokableRun(context.Background(), `{"x":1.5,"y":2.25}`)
		if err != nil {
			t.Fatalf("invoke add: %v", err)
		}
		if out != "3.75" {
			t.Fatalf("add(1.5,2.25) 应得 3.75，实际 %q", out)
		}
		return
	}
	t.Fatal("add 工具未接入")
}

// TestMCPTools_Governance 验证 MCP 工具与手写工具同权纳入 Catalog 治理：
// 写类工具登记 NeedsApproval 后被套审批包装；白名单过滤对 MCP 工具同样生效。
func TestMCPTools_Governance(t *testing.T) {
	ctx := context.Background()
	conn := connectInProcess(t, nil)

	catalog := NewCatalog()
	if err := catalog.Register(CommandSpec{
		ToolName: "echo", Resource: "test", Command: "echo",
		Exec: ExecInProcess,
	}); err != nil {
		t.Fatalf("register echo: %v", err)
	}
	if err := catalog.Register(CommandSpec{
		ToolName: "add", Resource: "test", Command: "add",
		Exec: ExecInProcess, NeedsApproval: true, ApprovalReason: "模拟写操作",
	}); err != nil {
		t.Fatalf("register add: %v", err)
	}

	// 审批包装：add 应被包上 ApprovalWrapper
	wrapped, err := WrapTools(ctx, catalog, conn.Tools())
	if err != nil {
		t.Fatalf("wrap tools: %v", err)
	}
	wrappedAdd := false
	for _, tl := range wrapped {
		if _, ok := tl.(*ApprovalWrapper); ok {
			wrappedAdd = true
		}
	}
	if !wrappedAdd {
		t.Fatal("登记 NeedsApproval 的 MCP 工具应被审批包装")
	}

	// 白名单过滤：只放行 test echo，add 应被过滤掉
	filtered, err := catalog.FilterTools(ctx, wrapped, []string{"test echo"})
	if err != nil {
		t.Fatalf("filter tools: %v", err)
	}
	for _, tl := range filtered {
		info, _ := tl.Info(ctx)
		if info.Name == "add" {
			t.Fatal("白名单未放行的 MCP 工具应被过滤")
		}
	}
	if len(filtered) != 1 {
		t.Fatalf("过滤后应只剩 echo，实际 %d 个", len(filtered))
	}
}

// TestNewMCPClient_InvalidConfig 验证配置校验：缺参数与未知传输方式应报错而非 panic。
func TestNewMCPClient_InvalidConfig(t *testing.T) {
	cases := []MCPServerConfig{
		{Name: "bad-stdio", Transport: MCPTransportStdio}, // 缺 Command
		{Name: "bad-http", Transport: MCPTransportHTTP},   // 缺 BaseURL
		{Name: "bad-transport", Transport: "grpc"},        // 未知传输
		{Name: "bad-sse", Transport: MCPTransportSSE},     // 缺 BaseURL
	}
	for _, cfg := range cases {
		if _, err := newMCPClient(cfg); err == nil {
			t.Errorf("配置 %+v 应报错", cfg)
		}
	}
}
