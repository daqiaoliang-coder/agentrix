-- agentrix 双轨事件存储建表脚本
--
-- 双轨设计：
--   ai_raw_history   : append-only 审计事件流（"实际发生了什么"），只增不改不删
--   ai_session_state : 每会话单行工作记忆（"模型需要记住什么"），CAS 乐观锁更新
--
-- 执行：mysql -u root -p agentrix < 001_init.sql
-- 要求：MySQL 5.7.8+（JSON 列类型）

CREATE TABLE IF NOT EXISTS `ai_raw_history` (
  `id`           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键，全局有序，用于范围查询和游标',
  `session_id`   VARCHAR(64)     NOT NULL                COMMENT '会话 ID（分区键）',
  `seq`          BIGINT UNSIGNED NOT NULL                COMMENT '会话内序号，从 0 递增，保证顺序',
  `role`         VARCHAR(16)     NOT NULL                COMMENT '消息角色：user / assistant / tool / system',
  `content`      MEDIUMTEXT      NULL                    COMMENT '消息正文',
  `tool_calls`   JSON            NULL                    COMMENT 'assistant 工具调用列表：[{"id","name","arguments"}]',
  `tool_call_id` VARCHAR(64)     NULL                    COMMENT 'tool 消息的配对 ID，对应 tool_calls[].id',
  `extra`        JSON            NULL                    COMMENT '扩展元数据：token 用量 / finish_reason / tool_name',
  `create_time`  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '写入时间戳（append-only，不加 ON UPDATE）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_session_seq` (`session_id`, `seq`),
  KEY `idx_tool_call_id` (`tool_call_id`),
  KEY `idx_create_time` (`create_time`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci
  COMMENT = 'Agent 原始历史：append-only 审计事件流（只增不改不删）';

CREATE TABLE IF NOT EXISTS `ai_session_state` (
  `session_id`     VARCHAR(64)     NOT NULL                COMMENT '会话 ID，主键',
  `version`        VARCHAR(8)      NOT NULL DEFAULT '1'    COMMENT '状态 schema 版本号，用于版本迁移',
  `history_cursor` BIGINT UNSIGNED NOT NULL DEFAULT 0    COMMENT 'RawHistory 游标：上次处理到的最大记录 seq',
  `memory`         JSON            NULL                    COMMENT '跨轮记忆摘要 {summary, token_budget, compressed_turns}',
  `skill_runtimes` JSON            NULL                    COMMENT 'Skill 运行时状态',
  `todo`           JSON            NULL                    COMMENT 'Todo 列表快照',
  `reminder`       JSON            NULL                    COMMENT 'Reminder 状态',
  `hitl_state`     JSON            NULL                    COMMENT 'HITL 状态',
  `version_lock`   BIGINT UNSIGNED NOT NULL DEFAULT 0    COMMENT 'CAS 乐观锁版本号，每次更新 +1',
  `create_time`    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '首次写入时间',
  `update_time`    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '最后更新时间',
  PRIMARY KEY (`session_id`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  COLLATE = utf8mb4_unicode_ci
  COMMENT = 'Agent 会话工作记忆：每会话单行，结构化状态 + CAS 乐观锁';
