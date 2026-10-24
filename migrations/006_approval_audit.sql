-- agentrix 审批审计建表脚本
--
-- 检查点（002_checkpoint.sql）只承载 eino 图状态且成功后删除，无法回答
-- 「谁批准了哪次写操作」。本表独立记录每次审批的请求现场与最终决策：
--   pending   → 审批已发起，等待人工决策
--   approved  → 审批通过（写操作随后执行）
--   rejected  → 审批被拒绝（写操作未执行）
--   abandoned → 用户发起新 Turn，挂起审批被放弃
--
-- 执行：mysql -u root -p agentrix < 006_approval_audit.sql

CREATE TABLE IF NOT EXISTS `ai_approval_audit` (
  `interrupt_id` VARCHAR(128) NOT NULL                COMMENT '中断点 ID（一次审批一条记录）',
  `scene`        VARCHAR(64)  NOT NULL DEFAULT ''     COMMENT '场景 key',
  `session_id`   VARCHAR(128) NOT NULL DEFAULT ''     COMMENT '会话 ID（含认证主体前缀）',
  `turn_id`      VARCHAR(64)  NOT NULL DEFAULT ''     COMMENT '发起审批的 Turn',
  `tool_name`    VARCHAR(128) NOT NULL DEFAULT ''     COMMENT '待执行的工具名',
  `tool_call_id` VARCHAR(64)  NOT NULL DEFAULT ''     COMMENT '模型工具调用 ID',
  `arguments`    MEDIUMTEXT   NULL                    COMMENT '工具参数原文（审批依据）',
  `reason`       VARCHAR(512) NOT NULL DEFAULT ''     COMMENT '审批提示语',
  `status`       VARCHAR(16)  NOT NULL                COMMENT 'pending / approved / rejected / abandoned',
  `operator`     VARCHAR(128) NOT NULL DEFAULT ''     COMMENT '决策人（来自认证主体，不信任请求体）',
  `comment`      VARCHAR(1024) NOT NULL DEFAULT ''    COMMENT '审批意见',
  `requested_at` DATETIME(3)  NOT NULL                COMMENT '审批发起时间',
  `decided_at`   DATETIME(3)  NULL                    COMMENT '决策/放弃时间',
  PRIMARY KEY (`interrupt_id`),
  KEY `idx_session_requested` (`session_id`, `requested_at`),
  KEY `idx_status_requested` (`status`, `requested_at`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci
  COMMENT = '审批审计：谁对哪次写操作做了什么决策';
