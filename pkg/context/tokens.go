package context

import (
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
)

// 本文件是 agentrix 的单一 token 估算实现，全包唯一口径。
//
// 为什么必须单点：估算值同时驱动三件事——压缩触发阈值（effectiveTokens）、
// 摘要预算（truncateSummary）、采样窗口（sampleWindow）。此前 context 与
// harness/core 各持一份 len([]rune(s))/2 + 1，两处一旦分叉，压缩决策与用量记账
// 就在用不同的尺子量同一段文本，任何调参与效果对比都失去意义。
// harness/core 现通过导出的 EstimateMessagesTokens / EstimateTextTokens 复用本实现。

const (
	// tokensPerCJKChar / tokensPerASCIIChar 是主流 BPE 分词器的经验系数，非精确常数。
	//
	// 为什么必须按字符类别分别加权：旧公式「约 2 字符 = 1 token」对两类主流内容同时失真——
	// 工具结果（JSON / 日志 / 代码）实际约 3.5–4 字符 = 1 token，被高估约 1.8 倍；
	// 中文对话实际约 1–1.5 字符 = 1 token，被低估约 2 倍。
	//
	// 高估的代价不止数字难看：effectiveTokens 虚高会让压缩在远未接近窗口时就触发，
	// 而每次触发都改写历史、作废 provider 侧的 KV 缓存前缀，省下的 token 不够付重建成本。
	//
	// 接入方若锁定特定模型，应以该模型 tokenizer 的实测值替换这两个系数。
	tokensPerCJKChar   = 0.7  // 约 1.4 个 CJK 字符 = 1 token
	tokensPerASCIIChar = 0.28 // 约 3.6 个 ASCII 字符 = 1 token

	// messageStructTokens 是单条消息的结构固定开销（role、分隔符等协议字段）。
	messageStructTokens = 4

	// multimodalTokensPerPart 是单个非文本消息分片（图片/音频/视频/文件）的预留 token 数。
	// 这类分片的实际开销由 provider 侧按分辨率等计算，框架层用固定预留量兜底，
	// 避免含图消息被估成 0 token 而绕过压缩阈值。
	multimodalTokensPerPart = 1024
)

// EstimateTextTokens 估算一段文本的 token 数，按 CJK / ASCII 码点数分别加权。
//
// 非 ASCII 一律按 CJK 权重计：emoji、西里尔字母等会被高估，但高估是安全方向
// （宁可早压不可爆窗），且这类字符在 Agent 上下文里占比极低。
//
// 按字节而非按 rune 遍历：大工具结果（数万字符）每轮压缩都要估算一次，
// 字节级扫描避开 UTF-8 解码开销，rune 总数交给有硬件加速的 RuneCountInString。
func EstimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	ascii := 0
	for i := 0; i < len(s); i++ {
		if s[i] < utf8.RuneSelf {
			ascii++
		}
	}
	nonASCII := utf8.RuneCountInString(s) - ascii

	t := int(float64(nonASCII)*tokensPerCJKChar + float64(ascii)*tokensPerASCIIChar)
	if t < 1 {
		return 1 // 非空文本至少 1 token，避免短串被估成 0 而绕过阈值
	}
	return t
}

// TextDensity 返回 s 的实测 token 密度（每字符的 token 数）。
//
// 用于把 token 预算反推为字符预算：按内容的实际构成换算，而不是假定某一种语言。
// 于是 JSON / 代码类工具结果能保留约 3.6 倍于中文的字符量，而两者都不会超预算。
// 见 sampleWindow。
func TextDensity(s string) float64 {
	runes := utf8.RuneCountInString(s)
	if runes == 0 {
		return tokensPerCJKChar // 无内容时取最保守密度
	}
	return float64(EstimateTextTokens(s)) / float64(runes)
}

// EstimateMessageTokens 估算单条消息的 token 数：
// 正文 + 推理内容 + 工具调用 + 多模态分片 + 结构开销。
func EstimateMessageTokens(m *schema.Message) int {
	if m == nil {
		return 0
	}
	n := EstimateTextTokens(m.Content)
	n += EstimateTextTokens(m.ReasoningContent)
	for _, tc := range m.ToolCalls {
		n += EstimateTextTokens(tc.Function.Name)
		n += EstimateTextTokens(tc.Function.Arguments)
	}
	n += estimateMultiContentTokens(m)
	return n + messageStructTokens
}

// EstimateMessagesTokens 估算消息序列的总 token 数。
func EstimateMessagesTokens(messages []*schema.Message) int {
	total := 0
	for _, m := range messages {
		total += EstimateMessageTokens(m)
	}
	return total
}

// estimateMultiContentTokens 估算多模态分片的开销。
// 非文本分片（图片/音频/视频/文件）按固定预留量计，文本分片按字符估算。
// 漏算会让含图消息被估成近乎 0 token，从而永远不触发压缩。
func estimateMultiContentTokens(m *schema.Message) int {
	n := 0
	// MultiContent 已废弃但仍被部分 provider 使用，一并计入
	for _, part := range m.MultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			n += EstimateTextTokens(part.Text)
			continue
		}
		n += multimodalTokensPerPart
	}
	for _, part := range m.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			n += EstimateTextTokens(part.Text)
			continue
		}
		n += multimodalTokensPerPart
	}
	return n
}
