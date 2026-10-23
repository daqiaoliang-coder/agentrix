-- agentrix artifact 持久化建表脚本
--
-- artifact 是模型通过 write_artifact 产出的结构化结果（计划/表格/JSON 等），
-- 此前仅存进程内 map，重启即丢。本表按会话归属持久化，支持跨重启与多副本查询。
--
-- 执行：mysql -u root -p agentrix < 004_artifact.sql

CREATE TABLE IF NOT EXISTS `ai_artifact` (
  `id`         VARCHAR(64)  NOT NULL COMMENT '产物 ID（art_<nanotime>_<seq>）',
  `session_id` VARCHAR(128) NOT NULL COMMENT '会话 ID（含认证主体前缀）',
  `turn_id`    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '产生该产物的 Turn',
  `type`       VARCHAR(32)  NOT NULL COMMENT 'text/table/json/plan/chart/custom',
  `title`      VARCHAR(255) NOT NULL DEFAULT '' COMMENT '产物标题',
  `content`    LONGTEXT     NOT NULL COMMENT '结构化内容（JSON 序列化）',
  `metadata`   JSON         NULL COMMENT '附加元数据',
  `created_at` DATETIME(3)  NOT NULL,
  `updated_at` DATETIME(3)  NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_session_created` (`session_id`, `created_at`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci
  COMMENT = 'Agent 产物持久化';
