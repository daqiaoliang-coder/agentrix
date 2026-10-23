-- agentrix 工具结果溢出存储建表脚本
--
-- 上下文压缩淘汰老工具结果时，把原文落入本表，上下文中只保留可回读的 stub。
-- 进程内实现会随 Turn 结束丢失，持久化后 read_result 可跨 Turn/跨副本回读。
--
-- 执行：mysql -u root -p agentrix < 005_spill.sql

CREATE TABLE IF NOT EXISTS `ai_tool_spill` (
  `ref`        VARCHAR(191) NOT NULL COMMENT '稳定引用（toolresult:<tool_call_id>）',
  `content`    LONGTEXT     NOT NULL COMMENT '工具结果原文',
  `created_at` DATETIME(3)  NOT NULL,
  `updated_at` DATETIME(3)  NOT NULL,
  PRIMARY KEY (`ref`),
  KEY `idx_updated_at` (`updated_at`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci
  COMMENT = '被压缩淘汰的工具结果原文';
